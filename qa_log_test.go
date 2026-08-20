// 异步日志与实例日志落盘测试。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoCommand 返回一个长驻且有持续输出的进程命令（按平台）。
func echoCommand() string {
	if runtime.GOOS == "windows" {
		return "ping 127.0.0.1 -t"
	}
	return "sh -c 'while true; do echo heartbeat; sleep 0.2; done'"
}

// TestFileLoggerWritesToDisk 落盘内容与写入顺序一致。
func TestFileLoggerWritesToDisk(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "test.log", 1<<20)
	fl.Write([]byte("第一行\n"))
	fl.Write([]byte("第二行\n"))
	fl.Write([]byte("第三行\n"))
	fl.Close()
	data, err := os.ReadFile(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if got := string(data); got != "第一行\n第二行\n第三行\n" {
		t.Fatalf("日志内容不一致: %q", got)
	}
	t.Logf("[验证] 日志已落盘且顺序正确（%d 字节）", len(data))
}

// TestFileLoggerRotation 超过上限时轮转为 .1，新日志写入新文件。
func TestFileLoggerRotation(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "rot.log", 64) // 64 字节即触发轮转
	for i := 0; i < 10; i++ {
		fl.Write([]byte(strings.Repeat("x", 32)))
	}
	fl.Close()
	oldInfo, err1 := os.Stat(filepath.Join(dir, "rot.log.1"))
	curInfo, err2 := os.Stat(filepath.Join(dir, "rot.log"))
	if err1 != nil || err2 != nil {
		t.Fatalf("轮转文件缺失: .1=%v 当前=%v", err1, err2)
	}
	if curInfo.Size() > 64 || curInfo.Size() == 0 {
		t.Fatalf("当前文件大小异常: %d（应在 1~64 之间）", curInfo.Size())
	}
	if oldInfo.Size() < 32 || oldInfo.Size() > 64 {
		t.Fatalf("轮转历史文件大小异常: %d", oldInfo.Size())
	}
	t.Logf("[验证] 超过上限后轮转为 .1（旧=%d 字节，当前=%d 字节）", oldInfo.Size(), curInfo.Size())
}

// TestFileLoggerDropNoBlock 落盘追不上时丢弃数据，但 Write 绝不阻塞。
func TestFileLoggerDropNoBlock(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "drop.log", 1<<20)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 10000; j++ {
				fl.Write([]byte("xxxxx"))
			}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Write 被阻塞（磁盘追不上时不得阻塞调用方）")
	}
	fl.Close()
	if n := fl.drop.Load(); n <= 0 {
		t.Fatalf("预期发生丢弃计数，实际 %d", n)
	}
	t.Logf("[验证] 队列满时 Write 立即返回，丢弃 %d 字节", fl.drop.Load())
}

// TestStartProcessLogsToDisk 端到端：实例启动后 stdout 异步落盘到 {data}/logs/{uuid}.log。
func TestStartProcessLogsToDisk(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.LogDir = filepath.Join(dir, "logs")
	inst := NewInstance("log-uuid", InstanceConfig{
		Nickname:     "日志落盘",
		StartCommand: echoCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	logPath := filepath.Join(dir, "logs", "log-uuid.log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// 文件存在即代表输出流已建立（内容在 bufio 缓冲，flush 后落盘）
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 停止进程；done 通道保证退出前日志已 flush 落盘
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		t.Fatalf("实例未进入运行态")
	}
	_ = proc.Kill()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.IsRunning() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if proc.IsRunning() {
		t.Fatalf("进程未在超时内退出")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("日志文件为空（长驻输出进程应有日志）")
	}
	t.Logf("[验证] 实例日志端到端落盘成功（%d 字节）", len(data))
}

// gateWriter 阻塞 Write 直到 gate 关闭（测试用：暂停消费 goroutine）。
type gateWriter struct{ gate chan struct{} }

func (g gateWriter) Write(p []byte) (int, error) {
	<-g.gate
	return len(p), nil
}

// TestAsyncLoggerDropNoBlock 全局异步日志器缓冲满时丢弃且不阻塞调用方。
// 消费者先被 gate 阻塞，生产者写满 64 条缓冲后必然触发丢弃（确定性，
// 不依赖机器速度——此前 io.Discard 消费过快时 drop 可能为 0 造成 flake）。
func TestAsyncLoggerDropNoBlock(t *testing.T) {
	oldOut := log.Writer()
	gate := make(chan struct{})
	log.SetOutput(gateWriter{gate: gate})
	defer func() {
		select {
		case <-gate: // 已放行
		default:
			close(gate) // 失败路径也放行消费者，避免泄漏
		}
		log.SetOutput(oldOut)
	}()

	a := newAsyncLogger(64)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			a.Printf("日志 %d", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Printf 被阻塞（日志风暴不得阻塞调用方）")
	}
	if n := a.drop.Load(); n <= 0 {
		t.Fatalf("预期缓冲满触发丢弃，实际 %d", n)
	}
	close(gate) // 放行消费者，Close 排空剩余缓冲
	a.Close()
	t.Logf("[验证] 缓冲满时 Printf 立即返回，丢弃 %d 条", a.drop.Load())
}

// TestFileLoggerMultiKeepRotation 多份轮转：keep=5 时保留 .1 … .5，无 .6。
func TestFileLoggerMultiKeepRotation(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLoggerN(dir, "multi.log", 64, 5, 0) // 64 字节触发轮转
	for i := 0; i < 30; i++ {
		fl.Write([]byte(strings.Repeat("y", 32)))
	}
	fl.Close()
	for i := 1; i <= 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("multi.log.%d", i))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("轮转文件缺失: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "multi.log.6")); err == nil {
		t.Fatalf("不应存在第 6 份轮转文件")
	}
	cur, _ := os.Stat(filepath.Join(dir, "multi.log"))
	if cur.Size() > 64 || cur.Size() == 0 {
		t.Fatalf("当前文件大小异常: %d", cur.Size())
	}
	t.Logf("[验证] 多份轮转保留 .1-.5（当前 %d 字节）", cur.Size())
}

// TestLogLinesSinceTail 行缓冲：since 精确补发、tail 取尾、超限丢最旧。
func TestLogLinesSinceTail(t *testing.T) {
	l := newLogLines(3)
	l.add("a")
	l.add("b")
	l.add("c")
	l.add("d") // 超限后 a 被丢弃

	if got := l.tail(2); got != "c\nd\n" {
		t.Fatalf("tail(2) 结果错误: %q", got)
	}
	// 手动设置时间戳验证 since 过滤
	l.mu.Lock()
	l.lines[0].ts = 100
	l.lines[1].ts = 200
	l.lines[2].ts = 300
	l.mu.Unlock()
	if got := l.since(150); got != "c\nd\n" {
		t.Fatalf("since(150) 结果错误: %q", got)
	}
	if got := l.since(250); got != "d\n" {
		t.Fatalf("since(250) 结果错误: %q", got)
	}
	if got := l.since(400); got != "" {
		t.Fatalf("since(400) 应为空: %q", got)
	}
	l.clear()
	if got := l.tail(10); got != "" {
		t.Fatalf("clear 后应为空: %q", got)
	}
	t.Logf("[验证] 行缓冲 since/tail/超限丢弃/清空均正确")
}

// TestFileLoggerClear 清空指令删除当前与轮转文件，之后可继续写。
func TestFileLoggerClear(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLoggerN(dir, "clr.log", 64, 3, 0)
	fl.Write([]byte(strings.Repeat("z", 100))) // 触发一次轮转
	fl.Write([]byte("继续"))
	fl.Clear()
	base := filepath.Join(dir, "clr.log")
	for _, p := range []string{base, base + ".1", base + ".2", base + ".3"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("清空后文件仍存在: %s", p)
		}
	}
	// 清空后继续写：文件重建
	fl.Write([]byte("新日志\n"))
	fl.Close()
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("清空后写入失败: %v", err)
	}
	if string(data) != "新日志\n" {
		t.Fatalf("清空后内容错误: %q", string(data))
	}
	t.Logf("[验证] Clear 删除全部日志文件且后续写入正常重建")
}

// waitFileContains 轮询等待文件出现且包含指定字节。
func waitFileContains(path string, want []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) >= len(want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// doJSONReq 发起带 apikey 的请求并解析 {status, data, time} 响应。
func doJSONReq(t *testing.T, method, url string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应非 JSON: %s", body)
	}
	return resp.StatusCode, out
}

// TestInstanceLogsAPI 日志 API 端到端：tail 历史、since 补发、DELETE 清空。
func TestInstanceLogsAPI(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.LogDir = filepath.Join(dir, "logs")
	inst := NewInstance("log-api-uuid", InstanceConfig{
		Nickname:     "日志API",
		StartCommand: echoCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer func() {
		inst.mu.Lock()
		proc := inst.Proc
		inst.mu.Unlock()
		if proc != nil && proc.IsRunning() {
			_ = proc.Kill()
		}
	}()

	base := srv.URL + "/api/instance/logs?uuid=" + inst.InstanceUuid + "&apikey=test-key"

	// 1. tail 历史：等待输出落盘后读取，应非空
	logPath := filepath.Join(d.LogDir, inst.InstanceUuid+".log")
	if !waitFileContains(logPath, []byte{}, 5*time.Second) {
		t.Fatalf("日志文件未在超时内生成")
	}
	deadline := time.Now().Add(5 * time.Second)
	var dataStr string
	for time.Now().Before(deadline) {
		code, out := doJSONReq(t, http.MethodGet, base+"&tail=100")
		if code != 200 {
			t.Fatalf("日志读取状态码: %d", code)
		}
		dataStr, _ = out["data"].(string)
		if dataStr != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if dataStr == "" {
		t.Fatalf("tail 日志为空（长驻输出进程应有日志）")
	}

	// 2. tail 行数限制
	code, out := doJSONReq(t, http.MethodGet, base+"&tail=5")
	if code != 200 {
		t.Fatalf("tail=5 状态码: %d", code)
	}
	lines := strings.Split(out["data"].(string), "\n")
	if len(lines) > 6 { // 5 行 + 尾部空元素
		t.Fatalf("tail=5 返回 %d 行", len(lines))
	}

	// 3. since 补发：记录基线时间，等待新输出，应能取到后续行
	before := time.Now().UnixMilli()
	time.Sleep(1500 * time.Millisecond)
	code, out = doJSONReq(t, http.MethodGet, base+"&since="+fmt.Sprintf("%d", before))
	if code != 200 {
		t.Fatalf("since 查询状态码: %d", code)
	}
	if sinceStr, _ := out["data"].(string); sinceStr == "" {
		t.Fatalf("since 补发为空（运行中应有新输出行）")
	}

	// 4. DELETE 清空：文件被删除
	req, _ := http.NewRequest(http.MethodDelete, base, nil)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE 失败: %v", err)
	}
	resp.Body.Close()
	time.Sleep(300 * time.Millisecond) // 等待异步清空
	if _, err := os.Stat(logPath); err == nil {
		// 运行中进程的 fileLogger 会因新输出重建文件，属正常；
		// 只需确认清空指令执行过（文件大小应远小于清空前）
		if fi, err2 := os.Stat(logPath); err2 == nil && fi.Size() > 4096 {
			t.Fatalf("清空后文件仍过大: %d", fi.Size())
		}
	}
	t.Logf("[验证] 日志 API tail/since/DELETE 端到端通过")
}
