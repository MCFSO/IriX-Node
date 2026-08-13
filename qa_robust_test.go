// 健壮性回归测试：针对六维度审查中发现的缺陷。
//
// 覆盖：
//   - 高可用：实例被并发删除时处理器不得 nil 解引用崩溃（TOCTOU）
//   - 高可靠：Save 崩溃一致性、读文件内存上限
//   - 高扩展：publicAddr 需反映实际监听地址
//   - 高可维护：重启语义、上传错误不被吞掉

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 高可用：并发删除下的 TOCTOU nil 解引用
// ---------------------------------------------------------------------------

// TestNoPanicWhenInstanceRemovedConcurrently 实例在「校验存在」与「再次查找」
// 之间被删除时，处理器不得 panic（nil 解引用会杀掉整个守护进程）。
func TestNoPanicWhenInstanceRemovedConcurrently(t *testing.T) {
	d, dir := newTestDaemon(t)
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)

	paths := []string{
		"/api/instance?uuid=%s&apikey=test-key",
		"/api/protected_instance/kill?uuid=%s&apikey=test-key",
		"/api/protected_instance/command?uuid=%s&command=ls&apikey=test-key",
		"/api/protected_instance/outputlog?uuid=%s&size=1&apikey=test-key",
		"/api/protected_instance/open?uuid=%s&apikey=test-key",
		"/api/protected_instance/stop?uuid=%s&apikey=test-key",
		"/api/protected_instance/restart?uuid=%s&apikey=test-key",
	}

	const rounds = 300
	var panics sync.Map

	for round := 0; round < rounds; round++ {
		uuid := sampleUUID(round)
		inst := sampleInst(round, dir)
		// 用一个必定启动失败的命令：本测试只关心 nil 解引用，
		// 不能留下真实子进程占住临时目录导致清理失败
		inst.Config.StartCommand = "irix-node-no-such-binary-xyz"
		if err := d.Add(inst); err != nil {
			t.Fatalf("Add 失败: %v", err)
		}

		var wg sync.WaitGroup
		for _, p := range paths {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				defer func() {
					if rec := recover(); rec != nil {
						panics.Store(p, fmt.Sprint(rec))
					}
				}()
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(p, uuid), nil)
				if strings.HasPrefix(p, "/api/instance?") {
					req.Method = http.MethodGet
				}
				mux.ServeHTTP(httptest.NewRecorder(), req)
			}(p)
		}
		// 与请求并发删除，制造 TOCTOU 窗口
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Remove(uuid, false)
		}()
		wg.Wait()
	}

	failed := false
	panics.Range(func(k, v any) bool {
		failed = true
		t.Errorf("处理器 %v 在实例被并发删除时 panic: %v", k, v)
		return true
	})
	if failed {
		t.Fatal("存在 nil 解引用崩溃：实例查找结果未判空")
	}
}

// ---------------------------------------------------------------------------
// 高可维护：重启语义
// ---------------------------------------------------------------------------

// TestRestartStoppedInstanceSucceeds 对已停止的实例调用 restart 应直接启动，
// 而不是因「实例未在运行」返回 500。
func TestRestartStoppedInstanceSucceeds(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	inst := sampleInst(1, dir)
	inst.Config.StartCommand = longRunCommand()
	if err := d.Add(inst); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	defer d.StopAll(2 * time.Second)

	code, body := doReq(t, srv.URL+"/api/protected_instance/restart?uuid="+inst.InstanceUuid+"&apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("restart 已停止实例应成功，得到 %d: %s", code, body)
	}
	inst.mu.Lock()
	running := inst.Proc != nil
	inst.mu.Unlock()
	if !running {
		t.Fatal("restart 后实例进程应存在")
	}
}

// ---------------------------------------------------------------------------
// 高扩展：publicAddr 必须反映真实监听地址
// ---------------------------------------------------------------------------

// TestPublicAddrHonorsBindAll 绑定 0.0.0.0 时下载/上传地址不能写死 127.0.0.1，
// 否则远端客户端拿到的直连地址不可用。
func TestPublicAddrHonorsBindAll(t *testing.T) {
	d := NewDaemon(t.TempDir(), "test-key")
	d.Port = 12346

	if got := d.publicAddr(); got != "127.0.0.1:12346" {
		t.Fatalf("默认应为回环地址，得到 %s", got)
	}

	d.BindHost = "0.0.0.0"
	got := d.publicAddr()
	if strings.HasPrefix(got, "127.0.0.1") {
		t.Fatalf("绑定 0.0.0.0 时 publicAddr 不应写死回环地址，得到 %s", got)
	}
	if !strings.HasSuffix(got, ":12346") {
		t.Fatalf("publicAddr 应包含端口，得到 %s", got)
	}
}

// ---------------------------------------------------------------------------
// 高可靠：读文件内存上限
// ---------------------------------------------------------------------------

// TestFileReadRejectsOversizeFile 文本读取接口把整个文件读进内存并转成字符串，
// 无上限时一个大文件即可打爆内存；应拒绝超限文件而不是尝试读取。
func TestFileReadRejectsOversizeFile(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	if err := f.Truncate(maxTextReadBytes + 1); err != nil {
		f.Close()
		t.Fatalf("扩展文件失败: %v", err)
	}
	f.Close()

	req, _ := http.NewRequest(http.MethodPut,
		srv.URL+"/api/files/?uuid="+inst.InstanceUuid+"&apikey=test-key",
		strings.NewReader(`{"target":"/big.bin"}`))
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Status int `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, raw)
	}
	if out.Status == 200 {
		t.Fatalf("超限文件读取应被拒绝，却返回成功")
	}
}

// ---------------------------------------------------------------------------
// 高可靠：上传解析错误不得被吞掉
// ---------------------------------------------------------------------------

// TestUploadRejectsNonMultipart 非 multipart 请求体应返回 4xx 并带明确错误，
// 而不是忽略 ParseMultipartForm 的错误后报「缺少 file 字段」。
func TestUploadRejectsNonMultipart(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	password := tickets.Create(inst.InstanceUuid, dir, dir)
	if password == "" {
		t.Fatal("创建票据失败")
	}

	resp, err := testClient.Post(srv.URL+"/upload/"+password,
		"application/json", strings.NewReader(`{"not":"multipart"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("非 multipart 上传应返回 4xx，得到 %d", resp.StatusCode)
	}
}

// TestUploadWritesFile 正常 multipart 上传应落盘成功（回归保护）。
func TestUploadWritesFile(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	password := tickets.Create(inst.InstanceUuid, dir, dir)
	if password == "" {
		t.Fatal("创建票据失败")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatalf("构造表单失败: %v", err)
	}
	if _, err := fw.Write([]byte("你好")); err != nil {
		t.Fatalf("写表单失败: %v", err)
	}
	mw.Close()

	resp, err := testClient.Post(srv.URL+"/upload/"+password, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("上传应成功，得到 %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("读取上传文件失败: %v", err)
	}
	if string(got) != "你好" {
		t.Fatalf("上传内容不符: %q", got)
	}
}
