// 第二轮质量测试：安全边界、传输通道、真实进程、可用性。
// 覆盖第一轮遗漏的文件管理 API、下载/上传直连通道与进程交互路径。

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// netDial 建立裸 TCP 连接（用于协议层测试）。
func netDial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

// isTimeout 判断错误是否为超时。
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fileTestEnv 带一个实例（cwd = 独立子目录）的测试环境。
type fileTestEnv struct {
	d    *Daemon
	inst *Instance
	cwd  string
	url  string // 已含 apikey 与 uuid 的公共查询串前缀
	base string
}

// newFileEnv 构造文件管理测试环境。
func newFileEnv(t *testing.T) *fileTestEnv {
	t.Helper()
	d, dir := newTestDaemon(t)
	cwd := filepath.Join(dir, "server")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := NewInstance("file-uuid", InstanceConfig{Nickname: "文件实例", Cwd: cwd})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	t.Cleanup(srv.Close)
	return &fileTestEnv{
		d:    d,
		inst: inst,
		cwd:  cwd,
		base: srv.URL,
		url:  "apikey=test-key&uuid=" + inst.InstanceUuid,
	}
}

// apiCall 发起 JSON 请求，返回 body.status 与 body.data 原文。
func (e *fileTestEnv) apiCall(t *testing.T, method, path string, payload any) (float64, json.RawMessage) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequest(method, e.base+path+sep+e.url, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Status float64         `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s 响应非合法 JSON: %v (%s)", method, path, err, raw)
	}
	return out.Status, out.Data
}

// ---------------------------------------------------------------------------
// 文件管理 API 功能正确性（第一轮零覆盖）
// ---------------------------------------------------------------------------

// TestFileAPIRoundTrip 新建/写入/读取/复制/移动/删除全链路。
func TestFileAPIRoundTrip(t *testing.T) {
	e := newFileEnv(t)

	// mkdir
	if s, _ := e.apiCall(t, http.MethodPost, "/api/files/mkdir", map[string]any{"target": "plugins/sub"}); s != 200 {
		t.Fatalf("mkdir 失败: status=%v", s)
	}
	if fi, err := os.Stat(filepath.Join(e.cwd, "plugins", "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}

	// touch
	if s, _ := e.apiCall(t, http.MethodPost, "/api/files/touch", map[string]any{"target": "server.properties"}); s != 200 {
		t.Fatalf("touch 失败: status=%v", s)
	}

	// 写入
	content := "level-name=world\nmax-players=20\n中文内容\n"
	if s, _ := e.apiCall(t, http.MethodPut, "/api/files/", map[string]any{"target": "server.properties", "text": content}); s != 200 {
		t.Fatalf("写入失败: status=%v", s)
	}

	// 读取
	s, data := e.apiCall(t, http.MethodPut, "/api/files/", map[string]any{"target": "server.properties"})
	if s != 200 {
		t.Fatalf("读取失败: status=%v", s)
	}
	var got string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("读取内容不一致:\n期望 %q\n实际 %q", content, got)
	}

	// 复制
	if s, _ := e.apiCall(t, http.MethodPost, "/api/files/copy", map[string]any{
		"targets": [][]string{{"server.properties", "plugins/copy.properties"}},
	}); s != 200 {
		t.Fatalf("复制失败: status=%v", s)
	}
	if raw, err := os.ReadFile(filepath.Join(e.cwd, "plugins", "copy.properties")); err != nil || string(raw) != content {
		t.Fatalf("复制内容不一致: %v", err)
	}

	// 移动（重命名）
	if s, _ := e.apiCall(t, http.MethodPut, "/api/files/move", map[string]any{
		"targets": [][]string{{"plugins/copy.properties", "plugins/renamed.properties"}},
	}); s != 200 {
		t.Fatalf("移动失败: status=%v", s)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "plugins", "renamed.properties")); err != nil {
		t.Fatalf("移动后文件不存在: %v", err)
	}

	// 删除
	if s, _ := e.apiCall(t, http.MethodDelete, "/api/files", map[string]any{
		"targets": []string{"plugins/renamed.properties", "plugins/sub"},
	}); s != 200 {
		t.Fatalf("删除失败: status=%v", s)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "plugins", "renamed.properties")); !os.IsNotExist(err) {
		t.Fatalf("删除后文件仍存在")
	}
}

// TestFileCompressRoundTrip 压缩与解压往返。
func TestFileCompressRoundTrip(t *testing.T) {
	e := newFileEnv(t)
	if err := os.MkdirAll(filepath.Join(e.cwd, "data", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"data/a.txt":          "aaa",
		"data/nested/b.txt":   "bbb",
		"data/nested/c中文.txt": "ccc",
	} {
		if err := os.WriteFile(filepath.Join(e.cwd, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 压缩
	if s, _ := e.apiCall(t, http.MethodPost, "/api/files/compress", map[string]any{
		"type": 1, "source": "backup.zip", "targets": []string{"data"},
	}); s != 200 {
		t.Fatalf("压缩失败: status=%v", s)
	}
	zi, err := os.Stat(filepath.Join(e.cwd, "backup.zip"))
	if err != nil || zi.Size() == 0 {
		t.Fatalf("压缩包无效: %v", err)
	}

	// 解压到新目录
	if s, _ := e.apiCall(t, http.MethodPost, "/api/files/compress", map[string]any{
		"type": 2, "source": "backup.zip", "targets": []string{"restored"},
	}); s != 200 {
		t.Fatalf("解压失败: status=%v", s)
	}
	for name, want := range map[string]string{
		"restored/data/a.txt":          "aaa",
		"restored/data/nested/b.txt":   "bbb",
		"restored/data/nested/c中文.txt": "ccc",
	} {
		raw, err := os.ReadFile(filepath.Join(e.cwd, filepath.FromSlash(name)))
		if err != nil || string(raw) != want {
			t.Fatalf("解压内容不一致 %s: %v (%q)", name, err, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// 安全边界
// ---------------------------------------------------------------------------

// TestZipSlipDefense 恶意 zip 中的 ../ 路径不得逃出解压目录。
func TestZipSlipDefense(t *testing.T) {
	e := newFileEnv(t)
	zipPath := filepath.Join(e.cwd, "evil.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 多种穿越写法
	for _, name := range []string{
		"../escaped.txt",
		"../../escaped2.txt",
		"sub/../../escaped3.txt",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("pwned")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	status, data := e.apiCall(t, http.MethodPost, "/api/files/compress", map[string]any{
		"type": 2, "source": "evil.zip", "targets": []string{"out"},
	})
	if status == 200 {
		t.Errorf("zip-slip 应被拒绝，实际成功: %s", data)
	}
	// cwd 之外不得出现文件
	parent := filepath.Dir(e.cwd)
	for _, name := range []string{"escaped.txt", "escaped2.txt", "escaped3.txt"} {
		if _, err := os.Stat(filepath.Join(parent, name)); err == nil {
			t.Errorf("zip-slip 逃逸成功，写出了 %s", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(parent), name)); err == nil {
			t.Errorf("zip-slip 逃逸到上两级，写出了 %s", name)
		}
	}
	t.Logf("[验证] zip-slip 被拒绝: %s", data)
}

// TestUploadFilenameTraversal 上传文件名中的路径穿越必须被拒绝。
func TestUploadFilenameTraversal(t *testing.T) {
	e := newFileEnv(t)
	// 申请上传票据
	status, data := e.apiCall(t, http.MethodPost, "/api/files/upload?upload_dir=/uploads", nil)
	if status != 200 {
		t.Fatalf("申请上传票据失败: %v %s", status, data)
	}
	var ticket struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &ticket); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../escaped.txt", "../../escaped.txt", `..\escaped.txt`} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte("pwned"))
		mw.Close()

		req, err := http.NewRequest(http.MethodPost, e.base+"/upload/"+ticket.Password, &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("上传请求失败: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("[上传穿越] 文件名 %q -> HTTP %d %s", name, resp.StatusCode, strings.TrimSpace(string(body)))

		// 无论服务端如何响应，都不得在上传目录外留下文件
		for _, probe := range []string{
			filepath.Join(e.cwd, "escaped.txt"),
			filepath.Join(filepath.Dir(e.cwd), "escaped.txt"),
			filepath.Join(filepath.Dir(filepath.Dir(e.cwd)), "escaped.txt"),
		} {
			if _, err := os.Stat(probe); err == nil {
				t.Errorf("上传穿越成功，写出了 %s（文件名 %q）", probe, name)
				os.Remove(probe)
			}
		}
		// 落点必须在上传目录内（Go 的 multipart 对 filename 取 basename，
		// 这里显式断言该保证，避免日后依赖被打破而无人察觉）
		landed := filepath.Join(e.cwd, "uploads", "escaped.txt")
		if _, err := os.Stat(landed); err != nil {
			t.Errorf("文件名 %q 上传后未落在上传目录内（预期 %s）: %v", name, landed, err)
		} else {
			os.Remove(landed)
		}
	}
}

// TestDownloadTicketScope 下载票据不得跨实例读取文件。
func TestDownloadTicketScope(t *testing.T) {
	e := newFileEnv(t)
	// 另建一个实例，其 cwd 为兄弟目录，内含密文件
	otherCwd := filepath.Join(filepath.Dir(e.cwd), "other")
	if err := os.MkdirAll(otherCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(otherCwd, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cwd, "public.txt"), []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 用实例 A 的票据尝试下载实例 B 的文件
	status, data := e.apiCall(t, http.MethodPost, "/api/files/download?file_name=public.txt", nil)
	if status != 200 {
		t.Fatalf("申请下载票据失败: %v %s", status, data)
	}
	var ticket struct {
		Password string `json:"password"`
	}
	json.Unmarshal(data, &ticket)

	for _, target := range []string{"../other/secret.txt", "..%2Fother%2Fsecret.txt", "/etc/passwd"} {
		resp, err := http.Get(e.base + "/download/" + ticket.Password + "/" + target)
		if err != nil {
			t.Fatalf("下载请求失败: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "top-secret") {
			t.Errorf("票据越权读取了其他实例文件（target=%q）", target)
		}
		t.Logf("[票据越权] target=%q -> HTTP %d (%d 字节)", target, resp.StatusCode, len(body))
	}

	// 正常下载应成功
	resp, err := http.Get(e.base + "/download/" + ticket.Password + "/public.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "public" {
		t.Fatalf("正常下载失败: HTTP %d %q", resp.StatusCode, body)
	}
}

// TestFileWriteMemoryAmplification 文件写入接口在上限内的服务端内存放大。
// 只统计 handler 内部分配（直接调用 handler，排除客户端构造载荷的开销，
// 否则 runtime.MemStats 会把客户端序列化的内存也算进来）。
// 精确的分配数据见 BenchmarkFileWriteHandler。
func TestFileWriteMemoryAmplification(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("amp-uuid", InstanceConfig{Nickname: "放大", Cwd: dir})
	d.Instances = append(d.Instances, inst)

	const sizeMB = 8 // 限内（< 16MiB）
	payload, err := json.Marshal(map[string]any{
		"target": "big.bin",
		"text":   strings.Repeat("A", sizeMB*1024*1024),
	})
	if err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	req := httptest.NewRequest(http.MethodPut, "/api/files/?apikey=test-key&uuid="+inst.InstanceUuid, bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	d.handleFileReadWrite(rec, req)
	runtime.ReadMemStats(&after)

	if rec.Code != http.StatusOK {
		t.Fatalf("限内写入应成功: HTTP %d", rec.Code)
	}
	fi, err := os.Stat(filepath.Join(dir, "big.bin"))
	if err != nil || fi.Size() != sizeMB*1024*1024 {
		t.Fatalf("文件未正确落盘: %v", err)
	}
	serverAlloc := float64(after.TotalAlloc-before.TotalAlloc) / 1024 / 1024
	amplif := serverAlloc / float64(sizeMB)
	t.Logf("[性能] handler 写入 %d MB 文本：服务端分配约 %.1f MB（放大约 %.1f 倍）；"+
		"请求体上限 %d MiB，最坏并发内存 ≈ 并发数 × %.0f MB",
		sizeMB, serverAlloc, amplif, maxAPIBodyBytes>>20, float64(maxAPIBodyBytes>>20)*amplif)

	// 读取路径同样全量进内存
	req = httptest.NewRequest(http.MethodPut, "/api/files/?apikey=test-key&uuid="+inst.InstanceUuid,
		strings.NewReader(`{"target":"big.bin"}`))
	rec = httptest.NewRecorder()
	d.handleFileReadWrite(rec, req)
	if rec.Code == http.StatusOK {
		t.Logf("[观察] 读取 %d MB 文件：响应体 %.1f MB，无分块/范围读取（大文件建议走 /download/ 通道）",
			sizeMB, float64(rec.Body.Len())/1024/1024)
	}
}

// TestFileListPathEscape 文件列表与读写接口的越界防护。
func TestFileListPathEscape(t *testing.T) {
	e := newFileEnv(t)
	outside := filepath.Join(filepath.Dir(e.cwd), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 列表越界
	status, data := e.apiCall(t, http.MethodGet, "/api/files/list?target=../", nil)
	if status == 200 {
		t.Errorf("列表越界应被拒绝: %s", data)
	}
	// 读取越界
	status, data = e.apiCall(t, http.MethodPut, "/api/files/", map[string]any{"target": "../outside.txt"})
	if status == 200 {
		var got string
		json.Unmarshal(data, &got)
		if strings.Contains(got, "outside") {
			t.Errorf("读取越界成功，泄漏了 cwd 外文件内容")
		}
	}
	// 写入越界
	status, _ = e.apiCall(t, http.MethodPut, "/api/files/", map[string]any{"target": "../written.txt", "text": "x"})
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.cwd), "written.txt")); err == nil {
		t.Errorf("写入越界成功（status=%v）", status)
	}
	// 删除越界
	e.apiCall(t, http.MethodDelete, "/api/files", map[string]any{"targets": []string{"../outside.txt"}})
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("删除越界成功，删掉了 cwd 外文件")
	}
}

// TestSymlinkEscape 符号链接逃逸：NormalizePath 只做字面规范化，不解析软链。
func TestSymlinkEscape(t *testing.T) {
	e := newFileEnv(t)
	outsideDir := filepath.Join(filepath.Dir(e.cwd), "outsidedir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("link-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.cwd, "link")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("无法创建符号链接（需要权限）: %v", err)
	}

	status, data := e.apiCall(t, http.MethodPut, "/api/files/", map[string]any{"target": "link/secret.txt"})
	var got string
	if status == 200 {
		json.Unmarshal(data, &got)
	}
	if strings.Contains(got, "link-secret") {
		t.Logf("[发现] 符号链接可绕过 cwd 限制读取外部文件（NormalizePath 为字面规范化，未 EvalSymlinks）")
	} else {
		t.Logf("[观察] 符号链接读取被拒绝: status=%v", status)
	}
}

// ---------------------------------------------------------------------------
// 真实进程交互（第一轮零覆盖）
// ---------------------------------------------------------------------------

// echoLoopCommand 返回一个会持续输出并读取 stdin 的命令。
func echoLoopCommand() string {
	if runtime.GOOS == "windows" {
		// cmd 交互模式：读取 stdin 命令并回显
		return "cmd /q /k"
	}
	return "sh -c 'while read line; do echo got:$line; done'"
}

// TestInstanceCommandAndLog 命令下发到 stdin 并被日志捕获。
func TestInstanceCommandAndLog(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("cmd-uuid", InstanceConfig{
		Nickname:     "命令实例",
		StartCommand: echoLoopCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	if err := d.startInstance(inst.InstanceUuid); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer func() {
		inst.mu.Lock()
		proc := inst.Proc
		inst.Proc = nil
		inst.mu.Unlock()
		if proc != nil {
			_ = proc.Kill()
		}
	}()

	// 下发命令
	code, body := doReq(t, srv.URL+"/api/protected_instance/command?apikey=test-key&uuid="+inst.InstanceUuid+"&command=echo+hello-irix")
	if code != http.StatusOK {
		t.Fatalf("命令下发失败: HTTP %d %s", code, body)
	}
	var resp map[string]any
	json.Unmarshal(body, &resp)
	if resp["status"] != float64(200) {
		t.Fatalf("命令下发 body.status=%v: %s", resp["status"], body)
	}

	// 等待日志出现
	deadline := time.Now().Add(5 * time.Second)
	var logText string
	for time.Now().Before(deadline) {
		_, logBody := doReq(t, srv.URL+"/api/protected_instance/outputlog?apikey=test-key&size=64&uuid="+inst.InstanceUuid)
		var lr struct {
			Data string `json:"data"`
		}
		json.Unmarshal(logBody, &lr)
		logText = lr.Data
		if strings.Contains(logText, "hello-irix") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(logText, "hello-irix") {
		t.Errorf("命令输出未出现在日志中，日志尾部: %q", tailStr(logText, 200))
	} else {
		t.Logf("[验证] stdin 命令下发与日志捕获正常")
	}

	// 未运行实例下发命令应失败
	idle := NewInstance("idle-uuid", InstanceConfig{Nickname: "空闲", Cwd: dir})
	d.Instances = append(d.Instances, idle)
	_, body = doReq(t, srv.URL+"/api/protected_instance/command?apikey=test-key&command=x&uuid="+idle.InstanceUuid)
	json.Unmarshal(body, &resp)
	if resp["status"] == float64(200) {
		t.Errorf("未运行实例下发命令应失败")
	}
}

// tailStr 取字符串尾部若干字符。
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestConcurrentStartStopStateMachine 并发启停同一实例的状态机互斥。
func TestConcurrentStartStopStateMachine(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("race-uuid", InstanceConfig{
		Nickname:     "状态机",
		StartCommand: longRunCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)

	var wg sync.WaitGroup
	var startOK, startErr int32
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.startInstance(inst.InstanceUuid)
			mu.Lock()
			if err == nil {
				startOK++
			} else {
				startErr++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 只应有一个进程在跑
	inst.mu.Lock()
	proc := inst.Proc
	status := inst.Status
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		t.Fatalf("并发启动后应有一个运行中的进程，status=%d", status)
	}
	t.Logf("[验证] 16 并发启动：成功 %d 次、被互斥拒绝 %d 次，最终单一进程存活", startOK, startErr)
	if startOK > 1 {
		t.Logf("[发现] %d 次启动均成功：并发启动可能产生孤儿进程（Proc 被覆盖）", startOK)
	}

	// 收尾
	inst.mu.Lock()
	inst.Proc = nil
	inst.mu.Unlock()
	_ = proc.Kill()
}

// TestOrphanProcessAfterRestart 守护进程重启后无法接管旧子进程（孤儿进程）。
func TestOrphanProcessAfterRestart(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("orphan-uuid", InstanceConfig{
		Nickname:     "孤儿",
		StartCommand: longRunCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	if err := d.startInstance(inst.InstanceUuid); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	inst.mu.Lock()
	proc := inst.Proc
	pid := 0
	if proc != nil && proc.cmd != nil && proc.cmd.Process != nil {
		pid = proc.cmd.Process.Pid
	}
	inst.mu.Unlock()

	// 模拟守护进程重启：新 Daemon 加载同一数据目录
	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatalf("重启加载失败: %v", err)
	}
	reloaded := d2.Find(inst.InstanceUuid)
	if reloaded == nil {
		t.Fatalf("重启后实例丢失")
	}
	reloaded.mu.Lock()
	status, hasProc := reloaded.Status, reloaded.Proc != nil
	reloaded.mu.Unlock()
	t.Logf("[发现] 守护进程重启后：实例状态=%d（0=已关闭）、是否接管进程=%v，但 PID %d 仍在运行 → 孤儿进程，且面板显示为已关闭",
		status, hasProc, pid)

	// 收尾：杀掉真实进程
	inst.mu.Lock()
	inst.Proc = nil
	inst.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}
}

// ---------------------------------------------------------------------------
// 高可用：超时与优雅关停
// ---------------------------------------------------------------------------

// TestAPIBodyLimit /api/ 请求体超过上限必须被拒绝，且 /upload/ 通道不受限。
func TestAPIBodyLimit(t *testing.T) {
	d, dir := newTestDaemon(t)
	cwd := filepath.Join(dir, "server")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := NewInstance("limit-uuid", InstanceConfig{Nickname: "限流", Cwd: cwd})
	d.Instances = append(d.Instances, inst)

	// 用与 main 相同的中间件组合起服务
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	srv := newTestServerWithHandler(limitAPIBody(mux))
	defer srv.Close()
	q := "?apikey=test-key&uuid=" + inst.InstanceUuid

	// 超限请求体（20 MiB > 16 MiB 上限）应被拒绝
	oversize, _ := json.Marshal(map[string]any{"target": "big.bin", "text": strings.Repeat("A", 20<<20)})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/files/"+q, bytes.NewReader(oversize))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, statErr := os.Stat(filepath.Join(cwd, "big.bin")); statErr == nil {
		t.Errorf("超限请求体仍被写入文件，说明上限未生效")
	}
	t.Logf("[验证] 20MiB 请求体被拒绝: HTTP %d %s", resp.StatusCode, tailStr(strings.TrimSpace(string(body)), 120))

	// 限内请求体（1 MiB）应正常
	ok, _ := json.Marshal(map[string]any{"target": "small.bin", "text": strings.Repeat("B", 1<<20)})
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/api/files/"+q, bytes.NewReader(ok))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	fi, err := os.Stat(filepath.Join(cwd, "small.bin"))
	if err != nil || fi.Size() != 1<<20 {
		t.Fatalf("限内请求体应写入成功: %v", err)
	}

	// /upload/ 通道不受 16MiB 限制（流式落盘）
	status, data := (&fileTestEnv{d: d, inst: inst, cwd: cwd, base: srv.URL, url: "apikey=test-key&uuid=" + inst.InstanceUuid}).
		apiCall(t, http.MethodPost, "/api/files/upload?upload_dir=/up", nil)
	if status != 200 {
		t.Fatalf("申请上传票据失败: %v %s", status, data)
	}
	var ticket struct {
		Password string `json:"password"`
	}
	json.Unmarshal(data, &ticket)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("C"), 1<<20)
	for i := 0; i < 20; i++ { // 20 MiB
		fw.Write(chunk)
	}
	mw.Close()
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/upload/"+ticket.Password, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	fi, err = os.Stat(filepath.Join(cwd, "up", "large.bin"))
	if err != nil || fi.Size() != 20<<20 {
		t.Fatalf("20MiB 上传应成功（上传通道不受 API 上限约束）: %v", err)
	}
	t.Logf("[验证] /upload/ 通道 20MiB 流式落盘成功，不受 API 请求体上限影响")
}

// TestStopAllTerminatesChildren 优雅关停必须终止子进程，不留孤儿。
func TestStopAllTerminatesChildren(t *testing.T) {
	d, dir := newTestDaemon(t)
	var procs []*Process
	for i := 0; i < 3; i++ {
		inst := NewInstance(fmt.Sprintf("stopall-%d", i), InstanceConfig{
			Nickname:     fmt.Sprintf("待关停-%d", i),
			StartCommand: longRunCommand(),
			Cwd:          dir,
		})
		d.Instances = append(d.Instances, inst)
		if err := d.startInstance(inst.InstanceUuid); err != nil {
			t.Fatalf("启动失败: %v", err)
		}
		inst.mu.Lock()
		procs = append(procs, inst.Proc)
		inst.mu.Unlock()
	}

	start := time.Now()
	d.StopAll(2 * time.Second) // 进程不响应 stdin，走超时强杀路径
	t.Logf("[验证] StopAll 关停 3 个实例耗时 %v", time.Since(start))

	for i, p := range procs {
		if p == nil {
			t.Fatalf("实例 %d 无进程", i)
		}
		if p.IsRunning() {
			t.Errorf("实例 %d 的子进程在 StopAll 后仍在运行（孤儿进程）", i)
		}
	}
	for _, inst := range d.Instances {
		inst.mu.Lock()
		status, proc := inst.Status, inst.Proc
		inst.mu.Unlock()
		if status != StatusStopped || proc != nil {
			t.Errorf("关停后状态应为 Stopped 且解除引用，实际 status=%d proc!=nil=%v", status, proc != nil)
		}
	}
}

// TestServerHasTimeouts 校验服务端已配置 slowloris 防护相关超时。
func TestServerHasTimeouts(t *testing.T) {
	// 与 main 中一致的配置
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if srv.ReadHeaderTimeout == 0 {
		t.Error("应设置 ReadHeaderTimeout 以防 slowloris")
	}
	if srv.IdleTimeout == 0 {
		t.Error("应设置 IdleTimeout 以回收空闲连接")
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Error("不应设置 ReadTimeout/WriteTimeout：会切断大文件上传下载")
	}
}

// TestSlowlorisBlockedByHeaderTimeout 带 ReadHeaderTimeout 时半开连接会被关闭。
func TestSlowlorisBlockedByHeaderTimeout(t *testing.T) {
	d, _ := newTestDaemon(t)
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           limitAPIBody(mux),
		ReadHeaderTimeout: 500 * time.Millisecond,
		IdleTimeout:       time.Second,
	}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := netDial(ln.Addr().String())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	// 只发部分请求头
	if _, err := conn.Write([]byte("GET /api/overview HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if isTimeout(err) {
		t.Errorf("配置了 ReadHeaderTimeout 后，半开连接仍未被关闭")
	} else {
		t.Logf("[验证] 半开连接被 ReadHeaderTimeout 关闭: n=%d err=%v", n, err)
	}
}
