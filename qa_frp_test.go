// 节点端 FRP 测试：配置生成（self/openfrp/sakura）、假 frpc 二进制的
// 创建/启停/日志/删除/状态、二进制上传。

package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBuildFRPConfig self 透传 / openfrp / sakura 配置生成与校验。
func TestBuildFRPConfig(t *testing.T) {
	// self：完整 toml 透传
	self := &frpTunnel{Provider: "self", Config: map[string]any{
		"toml": "serverAddr = \"1.2.3.4\"\nserverPort = 7000\n",
	}}
	cfg, err := buildFRPConfig(self)
	if err != nil || !strings.Contains(cfg, "1.2.3.4") {
		t.Fatalf("self 透传失败: %v %q", err, cfg)
	}
	if _, err := buildFRPConfig(&frpTunnel{Provider: "self", Config: map[string]any{}}); err == nil {
		t.Fatalf("self 缺 toml 应报错")
	}

	// openfrp
	of := &frpTunnel{Provider: "openfrp", Name: "生存服", Config: map[string]any{
		"node": "frp-n.openfrp.net", "token": "secret", "localPort": float64(25565),
		"remotePort": float64(20001), "type": "tcp",
	}}
	cfg, err = buildFRPConfig(of)
	if err != nil {
		t.Fatalf("openfrp 生成失败: %v", err)
	}
	for _, want := range []string{`serverAddr = "frp-n.openfrp.net"`, `auth.token = "secret"`,
		`localPort = 25565`, `remotePort = 20001`, `name = "生存服"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("openfrp 配置缺少 %s:\n%s", want, cfg)
		}
	}

	// 缺 node / localPort 报错
	if _, err := buildFRPConfig(&frpTunnel{Provider: "openfrp", Config: map[string]any{}}); err == nil {
		t.Fatalf("缺 node 应报错")
	}
	if _, err := buildFRPConfig(&frpTunnel{Provider: "openfrp", Config: map[string]any{"node": "x"}}); err == nil {
		t.Fatalf("缺 localPort 应报错")
	}
	// sakura 同构
	sak := &frpTunnel{Provider: "sakura", Config: map[string]any{
		"node": "frp-xx.sakurafrp.com", "user": "u1", "token": "k1",
		"localPort": float64(25565),
	}}
	cfg, err = buildFRPConfig(sak)
	if err != nil || !strings.Contains(cfg, `auth.user = "u1"`) {
		t.Fatalf("sakura 生成失败: %v %q", err, cfg)
	}
	// 非法 provider
	if _, err := buildFRPConfig(&frpTunnel{Provider: "bogus", Config: map[string]any{}}); err == nil {
		t.Fatalf("非法 provider 应报错")
	}
	t.Logf("[验证] FRP 配置生成正确（self 透传/openfrp/sakura/校验）")
}

// fakeFRPC 生成假 frpc 二进制（sh 脚本 / bat 脚本）：
// -v 打印版本即退；其余参数模拟隧道进程持续输出日志行。
func fakeFRPC(t *testing.T, dir string) string {
	t.Helper()
	var name, content string
	if runtime.GOOS == "windows" {
		name = "frpc.bat"
		content = "@echo off\r\nif \"%1\"==\"-v\" (echo fake-frpc 1.0.0 & exit /b 0)\r\n" +
			":loop\r\necho fake-frpc-log line\r\nping -n 2 127.0.0.1 >nul\r\ngoto loop\r\n"
	} else {
		name = "frpc"
		content = "#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then echo fake-frpc 1.0.0; exit 0; fi\n" +
			"while true; do echo fake-frpc-log line; sleep 1; done\n"
	}
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("写假 frpc 失败: %v", err)
	}
	return p
}

// TestFRPTunnelLifecycle 假 frpc 的创建/启动/日志/停止/删除端到端。
func TestFRPTunnelLifecycle(t *testing.T) {
	d, dir := newTestDaemon(t)
	fakeFRPC(t, filepath.Join(dir, frpDirName))
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/frp"
	auth := "?apikey=test-key"

	// 状态：二进制存在
	code, out := doJSONReq(t, http.MethodGet, base+"/status"+auth)
	if code != 200 {
		t.Fatalf("status 失败: %d", code)
	}
	bin := out["data"].(map[string]any)["binary"].(map[string]any)
	if bin["present"] != true {
		t.Fatalf("假 frpc 应被识别: %v", bin)
	}

	// 创建（自动启动）
	body := map[string]any{
		"name": "生存服", "provider": "openfrp",
		"config": map[string]any{
			"node": "frp-n.openfrp.net", "token": "t", "localPort": float64(25565),
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := testClient.Post(base+"/tunnels"+auth, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer resp.Body.Close()
	var created struct {
		Status int `json:"status"`
		Data   struct {
			TunnelID string `json:"tunnelId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if created.Status != 200 || created.Data.TunnelID == "" {
		t.Fatalf("创建失败: %+v", created)
	}
	id := created.Data.TunnelID

	// 等日志出现
	deadline := time.Now().Add(10 * time.Second)
	var logs string
	for time.Now().Before(deadline) {
		code, out = doJSONReq(t, http.MethodGet, base+"/tunnels/"+id+"/logs?tail=50&apikey=test-key")
		if code != 200 {
			t.Fatalf("日志失败: %d", code)
		}
		logs, _ = out["data"].(string)
		if strings.Contains(logs, "fake-frpc-log") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(logs, "fake-frpc-log") {
		t.Fatalf("未收到隧道日志: %q", logs)
	}

	// 状态 running
	code, out = doJSONReq(t, http.MethodGet, base+"/status"+auth)
	tunnels := out["data"].(map[string]any)["tunnels"].([]any)
	if len(tunnels) != 1 || tunnels[0].(map[string]any)["status"] != "running" {
		t.Fatalf("隧道应 running: %v", tunnels)
	}

	// 停止
	doJSONReq(t, http.MethodPost, base+"/tunnels/"+id+"/stop"+auth)
	code, out = doJSONReq(t, http.MethodGet, base+"/status"+auth)
	tunnels = out["data"].(map[string]any)["tunnels"].([]any)
	if tunnels[0].(map[string]any)["status"] != "stopped" {
		t.Fatalf("隧道应 stopped: %v", tunnels)
	}

	// 删除
	req, _ := http.NewRequest(http.MethodDelete, base+"/tunnels/"+id+auth, nil)
	resp2, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("删除状态码: %d", resp2.StatusCode)
	}
	code, out = doJSONReq(t, http.MethodGet, base+"/status"+auth)
	tunnels = out["data"].(map[string]any)["tunnels"].([]any)
	if len(tunnels) != 0 {
		t.Fatalf("删除后应无隧道: %v", tunnels)
	}
	if _, err := os.Stat(filepath.Join(dir, frpDirName, "tunnels", id+".toml")); err == nil {
		t.Fatalf("配置文件未删除")
	}
	t.Logf("[验证] FRP 隧道创建/日志/停止/删除端到端通过")
}

// TestFRPUploadBinary 二进制上传。
func TestFRPUploadBinary(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "frpc")
	if err != nil {
		t.Fatalf("创建表单失败: %v", err)
	}
	_, _ = fw.Write([]byte("#!/bin/sh\necho fake\n"))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/frp/binary?apikey=test-key", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("上传状态码: %d", resp.StatusCode)
	}
	name := "frpc"
	if runtime.GOOS == "windows" {
		name = "frpc.exe"
	}
	if _, err := os.Stat(filepath.Join(dir, frpDirName, name)); err != nil {
		t.Fatalf("二进制未就位: %v", err)
	}
	t.Logf("[验证] frpc 二进制上传就位")
}

// TestFRPConfigValidation 创建时配置校验失败返回 400。
func TestFRPConfigValidation(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	body := `{"name":"x","provider":"openfrp","config":{}}`
	resp, err := testClient.Post(srv.URL+"/api/frp/tunnels?apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("配置缺失应 400: %d", resp.StatusCode)
	}
	t.Logf("[验证] 创建隧道配置校验（400）")
}
