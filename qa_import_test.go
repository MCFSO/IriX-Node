// 导入目录创建实例测试：特征判定、目录占用、系统目录拒绝、端到端导入。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// importBody 构造导入请求。
func importBody(t *testing.T, srvURL, path, nickname string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"daemonId": "local", "path": path}
	if nickname != "" {
		body["nickname"] = nickname
	}
	raw, _ := json.Marshal(body)
	resp, err := testClient.Post(srvURL+"/api/instance/import?apikey=test-key",
		"application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return resp.StatusCode, out
}

// TestImportDirectory 端到端：含 server.jar 的目录导入成功，实例就位。
func TestImportDirectory(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	serverDir := filepath.Join(dir, "survival")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "server.jar"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	code, out := importBody(t, srv.URL, serverDir, "生存服")
	if code != 200 {
		t.Fatalf("导入应成功，实际 %d: %v", code, out)
	}
	uuid, _ := out["data"].(map[string]any)["instanceUuid"].(string)
	if uuid == "" {
		t.Fatalf("未返回 instanceUuid: %v", out)
	}
	inst := d.Find(uuid)
	if inst == nil {
		t.Fatalf("实例不存在: %s", uuid)
	}
	inst.mu.Lock()
	cfg := inst.Config
	inst.mu.Unlock()
	if cfg.Nickname != "生存服" || cfg.Cwd != serverDir {
		t.Fatalf("实例配置错误: %+v", cfg)
	}
	t.Logf("[验证] 导入目录成功创建实例 %s（nickname=%s）", uuid, cfg.Nickname)
}

// TestImportEulaFeature eula.txt 特征也可导入，昵称缺省用目录名。
func TestImportEulaFeature(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	serverDir := filepath.Join(dir, "my-server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "eula.txt"), []byte("eula=true"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	code, out := importBody(t, srv.URL, serverDir, "")
	if code != 200 {
		t.Fatalf("eula.txt 特征应可导入，实际 %d: %v", code, out)
	}
	uuid, _ := out["data"].(map[string]any)["instanceUuid"].(string)
	inst := d.Find(uuid)
	inst.mu.Lock()
	nickname := inst.Config.Nickname
	inst.mu.Unlock()
	if nickname != "my-server" {
		t.Fatalf("缺省昵称应为目录名: %s", nickname)
	}
	t.Logf("[验证] eula.txt 特征导入成功，缺省昵称=目录名")
}

// TestImportNotServer 无服务端特征的目录拒绝导入。
func TestImportNotServer(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	plainDir := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plainDir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	code, out := importBody(t, srv.URL, plainDir, "x")
	if code != 400 {
		t.Fatalf("无特征目录应拒绝，实际 %d: %v", code, out)
	}
	t.Logf("[验证] 无服务端特征目录拒绝导入: %v", out["data"])
}

// TestImportMissingDir 目录不存在拒绝。
func TestImportMissingDir(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	code, _ := importBody(t, srv.URL, filepath.Join(t.TempDir(), "no-such-dir"), "")
	if code != 400 {
		t.Fatalf("不存在目录应 400，实际 %d", code)
	}
	t.Logf("[验证] 目录不存在拒绝导入")
}

// TestImportDuplicateCwd 同一目录重复导入拒绝。
func TestImportDuplicateCwd(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	serverDir := filepath.Join(dir, "dup")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "server.jar"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	code, out := importBody(t, srv.URL, serverDir, "")
	if code != 200 {
		t.Fatalf("首次导入应成功: %d %v", code, out)
	}
	code2, out2 := importBody(t, srv.URL, serverDir, "")
	if code2 != 400 {
		t.Fatalf("重复导入应拒绝，实际 %d: %v", code2, out2)
	}
	t.Logf("[验证] 同一目录重复导入被拒绝")
}

// TestImportSystemDir 系统目录拒绝（validateCwd 黑名单）。
func TestImportSystemDir(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	sysDir := "/etc"
	if runtime.GOOS == "windows" {
		sysDir = `C:\Windows`
	}
	code, _ := importBody(t, srv.URL, sysDir, "")
	if code != 400 {
		t.Fatalf("系统目录应拒绝，实际 %d", code)
	}
	t.Logf("[验证] 系统目录拒绝导入")
}
