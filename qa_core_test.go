// 核心下载测试：mock 源端到端下载、sha512 校验（成功/失败）、
// 无哈希跳过、文件名穿越拒绝、HTTP 错误。

package main

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corePayload 测试用核心 jar 内容。
var corePayload = []byte("META-INF/MANIFEST.MF\nMain-Class: org.bukkit.craftbukkit.Main\nfake server jar content\n")

// mockCoreServer 提供核心下载的模拟源。
func mockCoreServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write(corePayload)
		}
	}))
}

// startCoreDownload 发起下载任务并返回 jobId。
func startCoreDownload(t *testing.T, srvURL, uuid, dlURL, fileName, sha string) string {
	t.Helper()
	body := map[string]any{
		"uuid": uuid, "daemonId": "local", "url": dlURL, "fileName": fileName,
	}
	if sha != "" {
		body["sha512"] = sha
	}
	raw, _ := json.Marshal(body)
	resp, err := testClient.Post(srvURL+"/api/instance/download-core?apikey=test-key",
		"application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("发起下载失败: %v", err)
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
	return out.Data.JobID
}

// waitCoreDone 轮询进度直到 done/failed，返回最终状态 map。
func waitCoreDone(t *testing.T, srvURL, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, res := doJSONReq(t, http.MethodGet,
			srvURL+"/api/instance/download-core-progress?jobId="+jobID+"&apikey=test-key")
		if code != 200 {
			t.Fatalf("进度查询状态码: %d", code)
		}
		last, _ = res["data"].(map[string]any)
		if status, _ := last["status"].(string); status == "done" || status == "failed" {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("任务超时未结束: %v", last)
	return nil
}

// TestDownloadCoreEndToEnd 下载 + sha512 校验成功并就位。
func TestDownloadCoreEndToEnd(t *testing.T) {
	sum := sha512.Sum512(corePayload)
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	src := mockCoreServer(t, http.StatusOK)
	defer src.Close()

	jobID := startCoreDownload(t, srv.URL, inst.InstanceUuid, src.URL, "server.jar",
		hex.EncodeToString(sum[:]))
	last := waitCoreDone(t, srv.URL, jobID)
	if last["status"] != "done" {
		t.Fatalf("下载未完成: %v", last)
	}
	got, err := os.ReadFile(filepath.Join(dir, "server.jar"))
	if err != nil {
		t.Fatalf("核心文件缺失: %v", err)
	}
	if string(got) != string(corePayload) {
		t.Fatalf("核心内容不一致")
	}
	// 无 .part 临时文件残留
	if entries, _ := os.ReadDir(dir); len(entries) != 2 { // server.jar + instances.json
		t.Fatalf("目录内容异常（应只有 server.jar 与 instances.json）: %v", entries)
	}
	t.Logf("[验证] 核心下载 + sha512 校验成功并就位")
}

// TestDownloadCoreHashMismatch sha512 不匹配 → failed 且不留文件。
func TestDownloadCoreHashMismatch(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	src := mockCoreServer(t, http.StatusOK)
	defer src.Close()

	jobID := startCoreDownload(t, srv.URL, inst.InstanceUuid, src.URL, "server.jar",
		strings.Repeat("0", 128))
	last := waitCoreDone(t, srv.URL, jobID)
	if last["status"] != "failed" {
		t.Fatalf("哈希不匹配应失败: %v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.jar")); err == nil {
		t.Fatalf("校验失败不应留下核心文件")
	}
	// 无 .part 临时文件残留
	for _, name := range []string{"server.jar.part-", "server.jar"} {
		if entries, _ := filepath.Glob(filepath.Join(dir, name)); len(entries) > 0 {
			t.Fatalf("校验失败应清理残留: %v", entries)
		}
	}
	t.Logf("[验证] sha512 不匹配任务失败且无残留")
}

// TestDownloadCoreNoHash 未提供 sha512 时跳过校验并完成。
func TestDownloadCoreNoHash(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	src := mockCoreServer(t, http.StatusOK)
	defer src.Close()

	jobID := startCoreDownload(t, srv.URL, inst.InstanceUuid, src.URL, "server.jar", "")
	last := waitCoreDone(t, srv.URL, jobID)
	if last["status"] != "done" {
		t.Fatalf("无哈希下载应完成: %v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.jar")); err != nil {
		t.Fatalf("核心文件缺失: %v", err)
	}
	t.Logf("[验证] 无 sha512 时跳过校验并完成")
}

// TestDownloadCorePathTraversal fileName 带路径穿越被 Base 净化后落在 cwd 内。
func TestDownloadCorePathTraversal(t *testing.T) {
	d, dir := newTestDaemon(t)
	parent := filepath.Dir(dir)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	src := mockCoreServer(t, http.StatusOK)
	defer src.Close()

	body := fmt.Sprintf(`{"uuid":"%s","url":"%s","fileName":"../evil.jar"}`, inst.InstanceUuid, src.URL)
	resp, err := testClient.Post(srv.URL+"/api/instance/download-core?apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Base 净化后的文件名应接受，实际 %d", resp.StatusCode)
	}
	// 等待任务结束（异步）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "evil.jar")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 文件落在 cwd 内，父目录无穿越产物
	if _, err := os.Stat(filepath.Join(dir, "evil.jar")); err != nil {
		t.Fatalf("净化后文件应落在 cwd 内: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.jar")); err == nil {
		t.Fatalf("穿越文件不得落在 cwd 外")
	}
	t.Logf("[验证] fileName 经 Base 净化，穿越名落在 cwd 内")
}

// TestDownloadCoreHTTPError 源返回错误时任务失败。
func TestDownloadCoreHTTPError(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	src := mockCoreServer(t, http.StatusNotFound)
	defer src.Close()

	jobID := startCoreDownload(t, srv.URL, inst.InstanceUuid, src.URL, "server.jar", "")
	last := waitCoreDone(t, srv.URL, jobID)
	if last["status"] != "failed" {
		t.Fatalf("404 源应失败: %v", last)
	}
	t.Logf("[验证] 源 HTTP 错误任务失败: %v", last["message"])
}

// TestValidateCoreURL 链接协议校验。
func TestValidateCoreURL(t *testing.T) {
	for _, good := range []string{"https://example.com/core.jar", "http://192.168.1.5/core.jar"} {
		if err := validateCoreURL(good); err != nil {
			t.Fatalf("%s 应通过: %v", good, err)
		}
	}
	for _, bad := range []string{"ftp://x/core.jar", "file:///etc/passwd", "javascript:alert(1)", ""} {
		if err := validateCoreURL(bad); err == nil {
			t.Fatalf("%q 应拒绝", bad)
		}
	}
	t.Logf("[验证] 核心链接仅允许 http/https")
}
