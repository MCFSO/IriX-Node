// 六维度质量测试：并发安全（race）、可靠性、高可用、HTTP 压测与长稳。
// 运行：go test -race ./... -v（本文件全部测试需配合 -race 才有完整意义）

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClient 压测与冒烟共用的 HTTP 客户端：大连接池复用，避免 Windows 下
// 高并发短连接把 TIME_WAIT 端口池打满（报 "Only one usage of each socket
// address"）；http.DefaultClient 默认 MaxIdleConnsPerHost=2 不满足压测场景。
var testClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: func() *http.Transport {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = 256
		tr.MaxIdleConnsPerHost = 64
		return tr
	}(),
}

// newTestDaemon 创建临时数据目录的守护进程（配对码已就绪，认证关闭路径不测）。
func newTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	if err := d.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	return d, dir
}

// newTestServer 启动带认证的 httptest 服务器。
func newTestServer(d *Daemon) *httptest.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

// newTestServerWithHandler 用指定 handler 启动 httptest 服务器（用于测试中间件）。
func newTestServerWithHandler(h http.Handler) *httptest.Server {
	return httptest.NewServer(h)
}

// doReq 发起带 apikey 的请求并读取响应。
func doReq(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// sampleUUID 生成固定格式 uuid。
func sampleUUID(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
}

// sampleInst 构造一个标准实例（cwd 指向临时目录）。
func sampleInst(i int, cwd string) *Instance {
	cfg := InstanceConfig{
		Nickname:     fmt.Sprintf("实例-%d", i),
		StartCommand: "ping 127.0.0.1 -t",
		StopCommand:  "stop",
		Cwd:          cwd,
		Type:         "universal",
	}
	inst := NewInstance(sampleUUID(i), cfg)
	inst.Config.CreateDatetime = int64(i)
	return inst
}

// ---------------------------------------------------------------------------
// 高并发：数据竞争检测
// ---------------------------------------------------------------------------

// TestConcurrentInstanceCRUD 并发创建/更新/删除实例。
// 注意：真实缺陷是并发 Save 全量写盘会互相覆盖 —— 本测试同时校验持久化不丢数据。
func TestConcurrentInstanceCRUD(t *testing.T) {
	d, dir := newTestDaemon(t)
	const goroutines = 32
	const perGoroutine = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				inst := sampleInst(g*perGoroutine+i, dir)
				if err := d.Add(inst); err != nil {
					t.Errorf("Add 失败: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// 内存中必然完整
	d.mu.Lock()
	inMem := len(d.Instances)
	d.mu.Unlock()
	if inMem != goroutines*perGoroutine {
		t.Errorf("内存实例数 %d != 期望 %d（并发丢失更新）", inMem, goroutines*perGoroutine)
	}

	// 磁盘必须与内存一致（Save 已原子写 + 串行化，不允许丢失更新）
	disk := loadInstanceCount(t, dir)
	if disk != inMem {
		t.Errorf("磁盘实例数 %d != 内存 %d：持久化丢失更新", disk, inMem)
	}
	// 磁盘文件必须是合法 JSON（损坏即失败）
	raw, err := os.ReadFile(filepath.Join(dir, "instances.json"))
	if err != nil {
		t.Fatalf("读取 instances.json 失败: %v", err)
	}
	var list []PersistedInstance
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("instances.json 被并发写坏（非法 JSON）: %v", err)
	}

	// 并发删除
	var wg2 sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg2.Add(1)
		go func(g int) {
			defer wg2.Done()
			base := g * perGoroutine
			for i := 0; i < perGoroutine; i++ {
				inst := d.Find(sampleUUID(base + i))
				if inst == nil {
					continue
				}
				if err := d.Remove(inst.InstanceUuid, false); err != nil {
					t.Errorf("Remove 失败: %v", err)
					return
				}
			}
		}(g)
	}
	wg2.Wait()
	d.mu.Lock()
	left := len(d.Instances)
	d.mu.Unlock()
	if left != 0 {
		t.Errorf("删除后内存剩余 %d 个实例", left)
	}
}

// TestConcurrentReads 高并发只读路径（List/Detail/CountRunning/Find）。
func TestConcurrentReads(t *testing.T) {
	d, dir := newTestDaemon(t)
	for i := 0; i < 200; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = d.List()
				_ = d.CountRunning()
				_ = d.Find(sampleUUID(0))
				if inst := d.Find(sampleUUID(0)); inst != nil {
					_ = inst.Detail()
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestLogBufferSemantics 环形缓冲的顺序、截断与 Tail 边界语义。
func TestLogBufferSemantics(t *testing.T) {
	// 未写满：Tail 返回全部且保持顺序
	b := NewLogBuffer(100)
	b.Write([]byte("abc"))
	b.Write([]byte("def"))
	if got := b.Tail(0); got != "abcdef" {
		t.Fatalf("未写满时 Tail 应为 abcdef，实际 %q", got)
	}
	if b.Len() != 6 {
		t.Fatalf("Len 应为 6，实际 %d", b.Len())
	}

	// 写满并覆盖：只保留最后 maxBytes 字节，顺序正确
	b2 := NewLogBuffer(10)
	for i := 0; i < 6; i++ {
		b2.Write([]byte("ab")) // 共 12 字节
	}
	if b2.Len() != 10 {
		t.Fatalf("写满后 Len 应为 10，实际 %d", b2.Len())
	}
	if got := b2.Tail(0); got != "ababababab" {
		t.Fatalf("覆盖后内容应为 ababababab，实际 %q", got)
	}

	// 覆盖后顺序仍为写入顺序
	b3 := NewLogBuffer(5)
	b3.Write([]byte("12345"))
	b3.Write([]byte("67"))
	if got := b3.Tail(0); got != "34567" {
		t.Fatalf("覆盖后应保留最后 5 字节 34567，实际 %q", got)
	}

	// 单次写入超过容量：只保留尾部
	b4 := NewLogBuffer(4)
	b4.Write([]byte("abcdefgh"))
	if got := b4.Tail(0); got != "efgh" {
		t.Fatalf("超长写入应保留尾部 efgh，实际 %q", got)
	}

	// Tail 截断：只返回请求的字节数
	b5 := NewLogBuffer(4096)
	b5.Write([]byte(strings.Repeat("z", 3000)))
	if got := b5.Tail(1); len(got) != 1024 {
		t.Fatalf("Tail(1) 应返回 1024 字节，实际 %d", len(got))
	}

	// 覆盖状态下的 Tail 截断（跨环形边界）
	b6 := NewLogBuffer(8)
	b6.Write([]byte("12345678"))
	b6.Write([]byte("9A")) // 逻辑内容 3456789A
	if got := b6.Tail(0); got != "3456789A" {
		t.Fatalf("跨边界内容应为 3456789A，实际 %q", got)
	}
	// 容量绝不超过 maxBytes
	if cap(b6.buf) > 8 {
		t.Fatalf("容量应不超过 maxBytes，实际 cap=%d", cap(b6.buf))
	}
}

// TestConcurrentLogBuffer 并发写读日志环形缓冲。
func TestConcurrentLogBuffer(t *testing.T) {
	buf := NewLogBuffer(64 * 1024)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			line := bytes.Repeat([]byte(fmt.Sprintf("goroutine-%d ", g)), 50)
			for i := 0; i < 5000; i++ {
				_, _ = buf.Write(line)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = buf.Tail(16)
		}
	}()
	wg.Wait()
	if buf.Len() > 64*1024 {
		t.Errorf("缓冲超过上限: %d", buf.Len())
	}
}

// TestConcurrentTickets 并发票据创建与读取（低于上限）。
func TestConcurrentTickets(t *testing.T) {
	ts := NewTicketStore()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				p := ts.Create("u", "/tmp", "")
				if p == "" {
					t.Errorf("票据被拒绝")
					return
				}
				if tk := ts.Get(p); tk == nil {
					t.Errorf("票据刚创建即不可用")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestTicketLimit 票据数量上限：超过 maxTickets 后拒绝新票据。
func TestTicketLimit(t *testing.T) {
	ts := NewTicketStore()
	for i := 0; i < maxTickets+10; i++ {
		ts.Create("u", "/tmp", "")
	}
	ts.mu.Lock()
	n := len(ts.tickets)
	ts.mu.Unlock()
	if n > maxTickets {
		t.Fatalf("票据数 %d 超过上限 %d", n, maxTickets)
	}
	if p := ts.Create("u", "/tmp", ""); p != "" {
		t.Fatalf("满额后仍创建成功")
	}
	t.Logf("[验证] 票据上限生效: 最多 %d 张", n)
}

// ---------------------------------------------------------------------------
// 高可靠：持久化、崩溃恢复、票据、认证
// ---------------------------------------------------------------------------

// loadInstanceCount 从磁盘统计实例数。
func loadInstanceCount(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "instances.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("读取失败: %v", err)
	}
	var list []PersistedInstance
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return len(list)
}

// TestPersistenceRoundTrip 持久化往返：Add -> Save -> 模拟重启（新 Daemon Load）-> 数据完整。
func TestPersistenceRoundTrip(t *testing.T) {
	d, dir := newTestDaemon(t)
	for i := 0; i < 50; i++ {
		if err := d.Add(sampleInst(i, dir)); err != nil {
			t.Fatalf("Add 失败: %v", err)
		}
	}
	// 模拟进程重启：重新 NewDaemon + Load
	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatalf("重启后 Load 失败: %v", err)
	}
	if len(d2.Instances) != 50 {
		t.Fatalf("重启后实例数 %d != 50", len(d2.Instances))
	}
	for i := 0; i < 50; i++ {
		inst := d2.Find(sampleUUID(i))
		if inst == nil {
			t.Fatalf("重启后丢失实例 %d", i)
		}
		if inst.Config.Nickname != fmt.Sprintf("实例-%d", i) {
			t.Fatalf("重启后配置不一致: %s", inst.Config.Nickname)
		}
		if inst.Config.Crlf != 2 || inst.Config.IE != "utf-8" {
			t.Fatalf("默认值未补齐: %+v", inst.Config)
		}
	}
}

// TestCorruptInstancesFileBehavior 实例文件损坏时守护进程能否自愈。
func TestCorruptInstancesFileBehavior(t *testing.T) {
	dir := t.TempDir()
	// 写入半个 JSON（模拟崩溃中断写盘）
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), []byte(`[{"instanceUuid":"abc","config":`), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDaemon(dir, "test-key")
	if err := d.Load(); err != nil {
		t.Fatalf("损坏文件应容错继续，Load 返回错误: %v", err)
	}
	// 损坏文件应被备份而非丢失
	matches, _ := filepath.Glob(filepath.Join(dir, "instances.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("应有 1 个损坏备份文件，实际 %d: %v", len(matches), matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil || !strings.Contains(string(raw), "abc") {
		t.Fatalf("备份文件内容不完整: %v", err)
	}
	// 继续使用（Save 后磁盘恢复合法 JSON）
	if err := d.Save(); err != nil {
		t.Fatalf("容错后 Save 失败: %v", err)
	}
	if got := loadInstanceCount(t, dir); got != 0 {
		t.Fatalf("损坏文件按空列表处理，Save 后应为 0 个实例，实际 %d", got)
	}
}

// TestSaveFilePermission 检查持久化文件权限。
func TestSaveFilePermission(t *testing.T) {
	d, _ := newTestDaemon(t)
	if err := d.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	info, err := os.Stat(d.instanceFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[观察] instances.json 权限: %v（原子写：临时文件 + rename）", info.Mode())
	// 不应残留临时文件
	if _, err := os.Stat(d.instanceFile() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("原子写后残留临时文件: %v", err)
	}
}

// TestTicketExpiryReclaim 票据过期后不可用、清理循环可回收。
func TestTicketExpiryReclaim(t *testing.T) {
	ts := NewTicketStore()
	p := ts.Create("u", "/tmp", "/")
	ts.mu.Lock()
	ts.tickets[p].expires = time.Now().Add(-time.Minute) // 模拟过期
	ts.mu.Unlock()
	if tk := ts.Get(p); tk != nil {
		t.Fatalf("过期票据仍可用")
	}
	if n := ts.cleanupOnceForTest(); n != 1 {
		t.Fatalf("清理应移除 1 个票据，实际 %d", n)
	}
	if len(ts.tickets) != 0 {
		t.Fatalf("清理后仍有票据")
	}
}

// cleanupOnceForTest 执行一轮清理，返回移除数量。
func (ts *ticketStore) cleanupOnceForTest() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	now := time.Now()
	for k, v := range ts.tickets {
		if now.After(v.expires) {
			delete(ts.tickets, k)
			n++
		}
	}
	return n
}

// TestPairingAuth 配对码正确性、错误码拒绝、恒定时间比较。
func TestPairingAuth(t *testing.T) {
	d, dir := newTestDaemon(t)
	code, isNew, err := d.LoadPairing()
	if err != nil {
		t.Fatalf("LoadPairing 失败: %v", err)
	}
	if !isNew || len(code) != PairingDigits {
		t.Fatalf("初次应生成 %d 位配对码，得到 %q isNew=%v", PairingDigits, code, isNew)
	}

	// 模拟重启读取哈希
	d2 := NewDaemon(dir, "")
	_, isNew, err = d2.LoadPairing()
	if err != nil || isNew {
		t.Fatalf("二次启动不应再生成新码: isNew=%v err=%v", isNew, err)
	}
	req := &http.Request{Header: http.Header{}, URL: &url.URL{}}
	if d2.authOK(req) {
		t.Fatalf("无凭证不应通过")
	}
	q := req.URL.Query()
	q.Set("apikey", "00000000000000000000")
	req.URL.RawQuery = q.Encode()
	if d2.authOK(req) {
		t.Fatalf("错误配对码不应通过")
	}
	q.Set("apikey", code)
	req.URL.RawQuery = q.Encode()
	if !d2.authOK(req) {
		t.Fatalf("正确配对码应通过")
	}

	// 固定密钥模式（APIKey 非空）下配对码无效，仅固定密钥可通过
	d3 := NewDaemon(dir, "fixed-key")
	if d3.authOK(req) {
		t.Fatalf("固定密钥模式下配对码不应通过")
	}
	q.Set("apikey", "fixed-key")
	req.URL.RawQuery = q.Encode()
	if !d3.authOK(req) {
		t.Fatalf("固定密钥应通过")
	}

	// 恒定时间比较耗时稳定性（粗略探测，不做硬断言）
	start := time.Now()
	checkPairing(code+"0", d2.PairingHash)
	t1 := time.Since(start)
	start = time.Now()
	checkPairing(code, d2.PairingHash)
	t2 := time.Since(start)
	t.Logf("[观察] 恒定时间比较: 错误码 %s vs 正确码 %s", t1, t2)
}

// crashCommand 返回一个立即以非零码退出的命令（按平台）。
func crashCommand() string {
	if runtime.GOOS == "windows" {
		return "cmd /c exit 1"
	}
	return "sh -c 'exit 1'"
}

// longRunCommand 返回一个长驻进程命令（按平台）。
func longRunCommand() string {
	if runtime.GOOS == "windows" {
		return "ping 127.0.0.1 -t"
	}
	return "sleep 1000"
}

// TestAutoRestartCrashLoop 崩溃循环应被防抖停止（10 秒窗口内最多 3 次自动重启）。
func TestAutoRestartCrashLoop(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	inst := NewInstance("crash-loop-uuid", InstanceConfig{
		Nickname:     "崩溃循环",
		StartCommand: crashCommand(),
		Cwd:          dir,
		EventTask:    EventTask{AutoRestart: true},
	})
	d.Instances = append(d.Instances, inst)
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	// 等待防抖停止
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		s := inst.Status
		inst.mu.Unlock()
		if s == StatusStopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	inst.mu.Lock()
	status, attempts := inst.Status, inst.arAttempts
	inst.mu.Unlock()
	if status != StatusStopped {
		t.Fatalf("崩溃循环应被防抖停止，状态=%d", status)
	}
	if attempts != 0 {
		t.Fatalf("防抖停止后计数应复位，实际 %d", attempts)
	}
	t.Logf("[验证] 崩溃循环经防抖后停止，未无限重启")
}

// TestAutoRestartKillNoRestart 主动 kill 不应触发自动重启。
func TestAutoRestartKillNoRestart(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	inst := NewInstance("kill-uuid", InstanceConfig{
		Nickname:     "常驻",
		StartCommand: longRunCommand(),
		Cwd:          dir,
		EventTask:    EventTask{AutoRestart: true},
	})
	d.Instances = append(d.Instances, inst)
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	// 等待进入运行态
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		running := inst.Status == StatusRunning && inst.Proc != nil && inst.Proc.IsRunning()
		inst.mu.Unlock()
		if running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	inst.mu.Lock()
	running := inst.Status == StatusRunning && inst.Proc != nil && inst.Proc.IsRunning()
	inst.mu.Unlock()
	if !running {
		t.Fatalf("实例未进入运行态")
	}

	// 通过 API kill
	srv := newTestServer(d)
	code, body := doReq(t, srv.URL+"/api/protected_instance/kill?apikey=test-key&uuid="+inst.InstanceUuid)
	srv.Close()
	if code != http.StatusOK {
		t.Fatalf("kill 返回 %d: %s", code, body)
	}
	time.Sleep(500 * time.Millisecond)
	inst.mu.Lock()
	status, proc := inst.Status, inst.Proc
	inst.mu.Unlock()
	if status != StatusStopped {
		t.Fatalf("kill 后应为 Stopped，实际 %d（可能被自动重启）", status)
	}
	if proc != nil {
		t.Fatalf("kill 后 Proc 应清空，避免误触发自动重启")
	}
	t.Logf("[验证] 主动 kill 不触发 AutoRestart")
}

// TestPathSecurity 路径越界防护（安全可靠性）。
// 注意：反斜杠与盘符路径仅在被视为目录分隔符的平台上构成越界，
// 故 `..\x`、`C:/Windows` 只在 Windows 上断言被拒绝。
func TestPathSecurity(t *testing.T) {
	base := t.TempDir()
	evil := []string{"../x", "a/../../b", "/../x"}
	if runtime.GOOS == "windows" {
		evil = append(evil, `..\x`, `/..\x`, "C:/Windows")
	}
	for _, target := range evil {
		if _, err := NormalizePath(base, target); err == nil {
			t.Errorf("路径 %q 应被拒绝", target)
		}
	}
	if p, err := NormalizePath(base, "sub/file.txt"); err != nil || !strings.HasPrefix(p, base) {
		t.Errorf("合法路径被误拒绝: %v %v", p, err)
	}
	// 以 / 开头表示 cwd 根（跨平台一致）：/uploads 应解析到 cwd/uploads
	p, err := NormalizePath(base, "/uploads")
	if err != nil || filepath.Clean(p) != filepath.Join(base, "uploads") {
		t.Errorf("/ 前缀应解析为 cwd 根下路径: %v %v", p, err)
	}
	// cwd 本身及其绝对路径（Windows 盘符路径）也应被接受
	if p, err := NormalizePath(base, base); err != nil || p != base {
		t.Errorf("cwd 本身应被接受: %v %v", p, err)
	}
	// cwd 内子目录的绝对路径应原样接受
	subAbs := filepath.Join(base, "sub")
	if p, err := NormalizePath(base, subAbs); err != nil || filepath.Clean(p) != subAbs {
		t.Errorf("cwd 内子目录绝对路径应被接受: %v %v", p, err)
	}
	// 单独的 "/" 表示 cwd 根
	if p, err := NormalizePath(base, "/"); err != nil || filepath.Clean(p) != filepath.Clean(base) {
		t.Errorf("/ 应解析为 cwd: %v %v", p, err)
	}
	if runtime.GOOS == "windows" {
		if _, err := NormalizePath(base, `C:\x`); err == nil {
			t.Errorf("盘符路径越界应被拒绝")
		}
	}
}

// TestHTTPRoutes 全路由冒烟：每个端点至少返回 200 且 JSON 合法（高可靠性巡检）。
func TestHTTPRoutesSmoke(t *testing.T) {
	d, dir := newTestDaemon(t)
	if err := d.Add(sampleInst(1, dir)); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(d)
	defer srv.Close()

	routes := map[string]string{
		"GET /api/overview":                         srv.URL + "/api/overview?apikey=test-key",
		"GET /api/service/remote_service_instances": srv.URL + "/api/service/remote_service_instances?apikey=test-key&daemonId=x&page=1&page_size=10",
		"GET /api/instance":                         srv.URL + "/api/instance?apikey=test-key&daemonId=x&uuid=" + sampleUUID(1),
		"GET /api/protected_instance/outputlog":     srv.URL + "/api/protected_instance/outputlog?apikey=test-key&daemonId=x&uuid=" + sampleUUID(1),
	}
	for name, url := range routes {
		code, body := doReq(t, url)
		if code != http.StatusOK {
			t.Errorf("%s 返回 %d: %s", name, code, body)
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Errorf("%s 响应非合法 JSON: %v", name, err)
		}
		if resp["status"] != float64(200) {
			t.Errorf("%s status 字段异常: %v", name, resp["status"])
		}
	}
	// 未认证访问：HTTP 状态码与 body.status 一致（403），供监控/WAF 识别失败
	code, body := doReq(t, srv.URL+"/api/overview")
	if code != http.StatusForbidden {
		t.Errorf("未认证应返回 HTTP 403，实际 %d", code)
	}
	var resp map[string]any
	_ = json.Unmarshal(body, &resp)
	if resp["status"] != float64(403) {
		t.Errorf("未认证 body.status 应为 403，实际 %v", resp["status"])
	}
}

// ---------------------------------------------------------------------------
// 高并发 HTTP 压测（高吞吐/低延迟评估，吞吐数据用 t.Log 输出）
// ---------------------------------------------------------------------------

// runLoad 并发压测指定 URL，返回统计结果。
type loadResult struct {
	total    int
	ok       int
	errs     int
	elapsed  time.Duration
	latency  []float64
	status5x int
	errMsgs  []string
}

func runLoad(t *testing.T, url string, concurrency, requests int) loadResult {
	t.Helper()
	client := testClient
	var wg sync.WaitGroup
	start := time.Now()
	res := loadResult{}
	var mu sync.Mutex
	perWorker := requests / concurrency
	latCh := make(chan float64, requests)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				begin := time.Now()
				req, err := http.NewRequest(http.MethodGet, url, nil)
				if err != nil {
					mu.Lock()
					res.errs++
					mu.Unlock()
					continue
				}
				resp, err := client.Do(req)
				dt := float64(time.Since(begin).Microseconds())
				latCh <- dt
				mu.Lock()
				res.total++
				if err != nil {
					res.errs++
					if len(res.errMsgs) < 5 {
						res.errMsgs = append(res.errMsgs, err.Error())
					}
				} else {
					if resp.StatusCode == http.StatusOK {
						res.ok++
					} else if resp.StatusCode >= 500 {
						res.status5x++
					}
					// 必须读完响应体再关闭，连接才能归还连接池复用；
					// 直接 Close 未读完的 body 会让每个请求都新建连接，
					// Windows 下 TIME_WAIT 端口池会被打满
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(latCh)
	res.elapsed = time.Since(start)
	for l := range latCh {
		res.latency = append(res.latency, l)
	}
	sort.Float64s(res.latency)
	return res
}

// reportLoad 输出压测简报。
func reportLoad(t *testing.T, name string, r loadResult) {
	t.Helper()
	if r.total == 0 {
		t.Fatalf("%s 无任何请求完成", name)
	}
	pct := func(p float64) float64 {
		if len(r.latency) == 0 {
			return 0
		}
		i := int(math.Ceil(p*float64(len(r.latency)))) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(r.latency) {
			i = len(r.latency) - 1
		}
		return r.latency[i] / 1000.0
	}
	qps := float64(r.total) / r.elapsed.Seconds()
	t.Logf("[压测] %-40s 共%d 成功%d 错误%d 5xx%d | QPS=%.0f | 平均=%.2fms p50=%.2fms p95=%.2fms p99=%.2fms 总耗时=%.2fs",
		name, r.total, r.ok, r.errs, r.status5x, qps,
		avgMs(r.latency), pct(0.5), pct(0.95), pct(0.99), r.elapsed.Seconds())
	for _, m := range r.errMsgs {
		t.Logf("[压测错误样例] %s", m)
	}
}

func avgMs(lat []float64) float64 {
	if len(lat) == 0 {
		return 0
	}
	var sum float64
	for _, v := range lat {
		sum += v
	}
	return sum / float64(len(lat)) / 1000.0
}

// TestLoadOverview 高并发打概览接口。
func TestLoadOverview(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	const concurrency = 64
	const requests = 3200
	res := runLoad(t, srv.URL+"/api/overview?apikey=test-key", concurrency, requests)
	reportLoad(t, "GET /api/overview ×3200", res)
	if res.errs > 0 {
		t.Errorf("压测出现 %d 个错误", res.errs)
	}
	if res.status5x > 0 {
		t.Errorf("压测出现 %d 个 5xx", res.status5x)
	}
}

// TestLoadInstanceList 高并发打实例列表接口。
func TestLoadInstanceList(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, dir := newTestDaemon(t)
	for i := 0; i < 100; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}
	srv := newTestServer(d)
	defer srv.Close()

	res := runLoad(t, srv.URL+"/api/service/remote_service_instances?apikey=test-key&page=1&page_size=100", 64, 3200)
	reportLoad(t, "GET instance_list ×3200 (100实例)", res)
	if res.errs > 0 {
		t.Errorf("压测出现 %d 个错误", res.errs)
	}
}

// TestLoadFileList 高并发打文件列表接口（50 文件目录）。
func TestLoadFileList(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	d.Add(inst)
	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), []byte("x"), 0o644)
	}
	srv := newTestServer(d)
	defer srv.Close()

	url := srv.URL + "/api/files/list?apikey=test-key&uuid=" + inst.InstanceUuid + "&page=1&page_size=50"
	res := runLoad(t, url, 64, 3200)
	reportLoad(t, "GET /api/files/list ×3200", res)
	if res.errs > 0 {
		t.Errorf("压测出现 %d 个错误", res.errs)
	}
}

// TestConcurrentMixedHTTP 混合读写压测：并发创建与列表查询（模拟真实面板轮询 + 操作）。
func TestConcurrentMixedHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, dir := newTestDaemon(t)
	if err := d.Add(sampleInst(1, dir)); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(d)
	defer srv.Close()

	var wg sync.WaitGroup
	var errCount int32
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				var body io.Reader
				var req *http.Request
				var err error
				if g%2 == 0 {
					cfg := map[string]any{
						"nickname":     fmt.Sprintf("压测-%d", g*100+i),
						"startCommand": "ping 127.0.0.1 -t",
						"cwd":          dir,
					}
					payload, _ := json.Marshal(cfg)
					body = bytes.NewReader(payload)
					req, err = http.NewRequest(http.MethodPost, srv.URL+"/api/instance?apikey=test-key", body)
				} else {
					req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/service/remote_service_instances?apikey=test-key&page=1&page_size=100", nil)
				}
				if err != nil {
					continue
				}
				resp, err := testClient.Do(req)
				if err != nil {
					atomic.AddInt32(&errCount, 1)
					continue
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				var rsp struct {
					Status float64 `json:"status"`
				}
				_ = json.Unmarshal(raw, &rsp)
				// MCSM 约定：HTTP 恒 200，业务状态在 body.status
				if rsp.Status != 200 {
					atomic.AddInt32(&errCount, 1)
				}
			}
		}(g)
	}
	wg.Wait()
	if errCount > 0 {
		t.Errorf("混合读写压测出现 %d 个失败请求", errCount)
	}
	// 持久化必须与内存一致：初始 1 个 + 偶数 goroutine(4 个) 各创建 100 个 = 401
	want := 1 + 4*100
	disk := loadInstanceCount(t, dir)
	d.mu.Lock()
	mem := len(d.Instances)
	d.mu.Unlock()
	if disk != want || mem != want {
		t.Errorf("并发创建后 磁盘=%d 内存=%d，期望均=%d", disk, mem, want)
	}
	t.Logf("[验证] 混合读写并发创建后磁盘/内存实例数一致: %d", disk)
}

// ---------------------------------------------------------------------------
// 高可用：长稳运行、goroutine 泄漏、内存稳定
// ---------------------------------------------------------------------------

// TestLongRunStability 长时间混合请求 + 资源回收检查。
func TestLongRunStability(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, dir := newTestDaemon(t)
	for i := 0; i < 50; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}

	// 借用全局票据存储前先备份并清空，避免污染其他测试
	oldTickets := tickets
	tickets = NewTicketStore()
	defer func() { tickets = oldTickets }()

	srv := newTestServer(d)
	base := srv.URL
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()

	var errCount int32
	// 混合请求：overview / 列表 / 详情 / 文件列表 / 票据申请
	end := time.Now().Add(5 * time.Second)
	var reqCount int32
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			client := testClient
			for time.Now().Before(end) {
				var url string
				switch w % 4 {
				case 0:
					url = base + "/api/overview?apikey=test-key"
				case 1:
					url = base + "/api/service/remote_service_instances?apikey=test-key&page=1&page_size=50"
				case 2:
					url = base + "/api/instance?apikey=test-key&uuid=" + sampleUUID(w)
				case 3:
					req, _ := http.NewRequest(http.MethodPost, base+"/api/files/download?apikey=test-key&file_name=instances.json&uuid="+sampleUUID(w), nil)
					resp, err := client.Do(req)
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
					continue
				}
				resp, err := client.Get(url)
				if err != nil {
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					atomic.AddInt32(&errCount, 1)
					continue
				}
				atomic.AddInt32(&reqCount, 1)
			}
		}(w)
	}
	wg.Wait()
	srv.Close()
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	leaked := runtime.NumGoroutine() - baseGoroutines
	t.Logf("[长稳] 5 秒混合压测完成请求 %d，非 200 响应 %d，结束后 goroutine 变化 = %+d（正值可能为泄漏）", reqCount, errCount, leaked)
	if leaked > 30 {
		t.Errorf("疑似 goroutine 泄漏: +%d", leaked)
	}
}

// TestResolveBind 监听地址解析：-bind 显式指定优先，环境变量与默认值回退。
func TestResolveBind(t *testing.T) {
	cases := []struct {
		flag, env, want string
	}{
		{"", "", "127.0.0.1"},                  // 默认
		{"", "1", "0.0.0.0"},                   // 环境变量开启全部网卡
		{"", "0", "127.0.0.1"},                 // 环境变量关闭
		{"192.168.1.5", "", "192.168.1.5"},     // flag 优先
		{"0.0.0.0", "0", "0.0.0.0"},            // flag 覆盖环境变量
		{"::", "", "::"},                       // IPv6
		{"  192.168.1.5  ", "", "192.168.1.5"}, // 去空白
	}
	for _, c := range cases {
		if got := resolveBind(c.flag, c.env); got != c.want {
			t.Fatalf("resolveBind(%q, %q) = %q, 期望 %q", c.flag, c.env, got, c.want)
		}
	}
}
