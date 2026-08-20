// JDK 安装/卸载测试：mock Adoptium API 的端到端安装、参数校验、
// 卸载、解压安全（zip slip）、tar.gz 权限保留。

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeJDKZip 构造包含假 JDK 根目录的 zip 归档。
func fakeJDKZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	bin := "bin/java"
	if runtime.GOOS == "windows" {
		bin = "bin/java.exe"
	}
	for _, name := range []string{"jdk-21-fake/", "jdk-21-fake/bin/", "jdk-21-fake/" + bin, "jdk-21-fake/release"} {
		if strings.HasSuffix(name, "/") {
			if _, err := zw.Create(name); err != nil {
				t.Fatalf("创建目录条目失败: %v", err)
			}
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建文件条目失败: %v", err)
		}
		if _, err := w.Write([]byte("fake java\n")); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

// mockAdoptium 启动模拟 Adoptium API 服务：资产接口 + 归档下载接口。
// link 延迟求值（测试中先建服务、再引用 server.URL 拼下载地址）。
func mockAdoptium(t *testing.T, link func() string, apiStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/assets/latest/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(apiStatus)
		if apiStatus == http.StatusOK {
			_ = json.NewEncoder(w).Encode([]adoptiumAsset{{
				Binary: struct {
					Package struct {
						Link     string `json:"link"`
						Name     string `json:"name"`
						Size     int64  `json:"size"`
						Checksum string `json:"checksum"`
					} `json:"package"`
					JavaVersion string `json:"java_version"`
				}{
					Package: struct {
						Link     string `json:"link"`
						Name     string `json:"name"`
						Size     int64  `json:"size"`
						Checksum string `json:"checksum"`
					}{Link: link(), Name: "jdk.zip", Size: 1},
					JavaVersion: "21.0.4+7",
				},
			}})
		}
	})
	mux.HandleFunc("/pkg/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fakeJDKZip(t))
	})
	return httptest.NewServer(mux)
}

// TestInstallJDKEndToEnd mock API 端到端安装：任务化、进度、就位、验证。
func TestInstallJDKEndToEnd(t *testing.T) {
	bin := "bin/java"
	if runtime.GOOS == "windows" {
		bin = "bin/java.exe"
	}
	var downloadURL string
	server := mockAdoptium(t, func() string { return downloadURL }, http.StatusOK)
	defer server.Close()
	downloadURL = server.URL + "/pkg/jdk.zip"
	adoptiumAPI = server.URL

	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 发起安装
	body := `{"major":21}`
	resp, err := testClient.Post(srv.URL+"/api/runtime/java/install?apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("发起安装失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status int `json:"status"`
		Data   struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if out.Status != 200 || out.Data.JobID == "" {
		t.Fatalf("未返回 jobId: %+v", out)
	}
	jobID := out.Data.JobID

	// 轮询进度直至完成
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, res := doJSONReq(t, http.MethodGet,
			srv.URL+"/api/runtime/java/install-progress?jobId="+jobID+"&apikey=test-key")
		if code != 200 {
			t.Fatalf("进度查询状态码: %d", code)
		}
		data, _ := res["data"].(map[string]any)
		last = data
		if status, _ := data["status"].(string); status == "done" || status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last == nil || last["status"] != "done" {
		t.Fatalf("安装未完成: %v", last)
	}
	javaPath, _ := last["path"].(string)
	want := filepath.Join(dir, "jdk", "jdk-21", bin)
	if javaPath != want {
		t.Fatalf("产物路径错误: %s（期望 %s）", javaPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("安装后 java 不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "jdk", "jdk-21", "release")); err != nil {
		t.Fatalf("JDK 根目录内容不完整: %v", err)
	}
	t.Logf("[验证] 端到端安装完成: %s", javaPath)
}

// TestInstallJDKInvalidMajor 非法大版本直接拒绝。
func TestInstallJDKInvalidMajor(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	resp, err := testClient.Post(srv.URL+"/api/runtime/java/install?apikey=test-key",
		"application/json", strings.NewReader(`{"major":99}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法版本应返回 400，实际 %d", resp.StatusCode)
	}
	t.Logf("[验证] 非法 major 拒绝（400）")
}

// TestInstallJDKAPIError Adoptium 返回错误时任务标记 failed。
func TestInstallJDKAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	adoptiumAPI = server.URL

	d, _ := newTestDaemon(t)
	_, task := d.newTask()
	go d.runInstallJDK("t-fail", task, 21)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if task.jobStatusNow() == taskStatusFailed {
			t.Logf("[验证] API 错误时任务标记 failed: %v", task.messageNow())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务未在超时内失败")
}

// jobStatusNow / messageNow 测试辅助（读取任务状态）。
func (t *task) jobStatusNow() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *task) messageNow() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.message
}

// TestUninstallJava 卸载：不存在 404，存在删除成功。
func TestUninstallJava(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 未安装 → 404
	req404, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/runtime/java?apikey=test-key&major=21", nil)
	resp, err := testClient.Do(req404)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未安装应 404，实际 %d", resp.StatusCode)
	}

	// 安装目录 → 卸载成功
	target := filepath.Join(dir, "jdk", "jdk-21")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/runtime/java?apikey=test-key&major=21", nil)
	resp2, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("卸载状态码: %d", resp2.StatusCode)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("卸载后目录仍存在")
	}
	t.Logf("[验证] 卸载流程正确（404/删除）")
}

// TestExtractZipSlip zip slip 解压越界被拒绝。
func TestExtractZipSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../../evil.txt")
	_, _ = w.Write([]byte("evil"))
	zw.Close()
	dir := t.TempDir()
	arch := filepath.Join(dir, "bad.zip")
	if err := os.WriteFile(arch, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := extractZip(arch, filepath.Join(dir, "out"), nil); err == nil {
		t.Fatalf("zip slip 应被拒绝")
	}
	t.Logf("[验证] zip slip 解压被拒绝")
}

// TestExtractTarGzPerm tar.gz 解压保留可执行权限。
func TestExtractTarGzPerm(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "jdk.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "jdk-x/bin/java", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("fake"))
	tw.Close()
	gz.Close()
	if err := os.WriteFile(arch, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out := filepath.Join(dir, "out")
	if err := extractTarGz(arch, out, nil); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	info, err := os.Stat(filepath.Join(out, "jdk-x", "bin", "java"))
	if err != nil {
		t.Fatalf("解压产物缺失: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("可执行权限未保留: %v", info.Mode())
	}
	t.Logf("[验证] tar.gz 解压保留可执行权限（%v）", info.Mode())
}

// TestValidateDownloadURL 下载链接协议校验。
func TestValidateDownloadURL(t *testing.T) {
	if err := validateDownloadURL("https://api.adoptium.net/v3/binary/1.zip"); err != nil {
		t.Fatalf("https 应通过: %v", err)
	}
	if err := validateDownloadURL("http://127.0.0.1:8080/x.zip"); err != nil {
		t.Fatalf("环回地址应通过: %v", err)
	}
	if err := validateDownloadURL("http://evil.example.com/x.zip"); err == nil {
		t.Fatalf("非 https 非环回应拒绝")
	}
	if err := validateDownloadURL("file:///etc/passwd"); err == nil {
		t.Fatalf("file 协议应拒绝")
	}
	t.Logf("[验证] 下载链接协议校验正确")
}

// TestDownloadFileProgress 下载进度按字节推进。
func TestDownloadFileProgress(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	dir := t.TempDir()
	_, task := newTaskStore().create()
	if err := downloadFile(context.Background(), server.Client(), server.URL, filepath.Join(dir, "f.bin"), task, 0, 1); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if p, _ := task.snapshot()["percent"].(float64); p < 0.99 {
		t.Fatalf("下载进度未推进到完成: %v", p)
	}
	data, err := os.ReadFile(filepath.Join(dir, "f.bin"))
	if err != nil || len(data) != len(payload) {
		t.Fatalf("下载内容不完整: %v", err)
	}
	t.Logf("[验证] 下载进度按字节推进并落盘")
}

// TestDownloadFileHTTPError 非 200 响应报错。
func TestDownloadFileHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, task := newTaskStore().create()
	err := downloadFile(context.Background(), server.Client(), server.URL, filepath.Join(t.TempDir(), "f.bin"), task, 0, 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("应报 HTTP 403 错误: %v", err)
	}
	t.Logf("[验证] 非 200 下载报错")
}
