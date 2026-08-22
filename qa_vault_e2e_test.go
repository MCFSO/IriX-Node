// qa_vault_e2e_test.go — 真实进程 E2E（环境变量门控，CI 默认跳过）。
//
// 验证真实二进制 + 真实 TLS + 真实 vault 门禁处理链下的完整流程：
//   - VAULT_E2E=1（全新数据目录）：status → init → totp/verify → cert 绑定 →
//     unlock → 数据面放行 → lock → 数据面 403；
//   - VAULT_E2E=phase2（重启后同一数据目录）：已初始化且锁定、数据面 403、
//     错误凭据解锁被拒（认证路径存活）。
//
// 用法：
//   .\irix-node.exe -tls-mode auto -vault -apikey e2e-key -port 19997 -data <dir> ...
//   $env:VAULT_E2E=1; go test -run TestVaultRealServerE2E -count=1 -v .
//   重启进程后：$env:VAULT_E2E=phase2; go test -run TestVaultRealServerE2E -count=1 -v .

package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eURL 真实进程地址（与 E2E 启动参数一致）。
const e2eURL = "https://127.0.0.1:19997"

// e2eClient 忽略自签证书校验的客户端（TOFU 冒烟场景）。
var e2eClient = &http.Client{
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // E2E 冒烟专用
}

// e2eReq 向真实进程发起请求。
func e2eReq(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e2eURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "e2e-key")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e2eClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// e2eData 取响应 data。
func e2eData(m map[string]any) map[string]any {
	if d, ok := m["data"].(map[string]any); ok {
		return d
	}
	return nil
}

// e2eStr 取 data 字符串字段。
func e2eStr(m map[string]any, k string) string {
	if d := e2eData(m); d != nil {
		if s, ok := d[k].(string); ok {
			return s
		}
	}
	return ""
}

// TestVaultRealServerE2E 真实进程端到端（需先手动启动节点并设置 VAULT_E2E）。
func TestVaultRealServerE2E(t *testing.T) {
	phase := os.Getenv("VAULT_E2E")
	if phase == "" {
		t.Skip("未设置 VAULT_E2E（真实进程 E2E 需手动启动节点，见文件头注释）")
	}

	// 状态可达（真实 TLS + 路由 + vault 状态加载）
	code, resp := e2eReq(t, "GET", "/api/vault/status", nil, "")
	if code != http.StatusOK {
		t.Fatalf("status 失败: %d %v", code, resp)
	}
	if e2eData(resp)["enabled"] != true {
		t.Fatalf("vault 未启用: %v", resp)
	}
	// 数据面门禁（生产链：auditMiddleware → vaultGate → limitAPIBody）
	code, resp = e2eReq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, "")
	if e2eData(resp)["initialized"] == false && code != http.StatusForbidden {
		t.Fatalf("未初始化门禁异常: %d %v", code, resp)
	}

	if phase == "phase2" {
		// 重启后：status 显示已初始化且锁定；数据面 403；错误凭据解锁被拒
		code, resp = e2eReq(t, "GET", "/api/vault/status", nil, "")
		if code != http.StatusOK {
			t.Fatalf("status 失败: %d %v", code, resp)
		}
		if e2eData(resp)["initialized"] != true || e2eData(resp)["locked"] != true {
			t.Fatalf("重启后应已初始化且锁定: %v", resp)
		}
		code, resp = e2eReq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, "")
		if code != http.StatusForbidden {
			t.Fatalf("锁定态数据面应 403: %d %v", code, resp)
		}
		// 错误凭据（无证书签名）→ 401
		code, resp = e2eReq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "unlock"}, "")
		if code != http.StatusOK {
			t.Fatalf("challenge 失败: %d %v", code, resp)
		}
		code, resp = e2eReq(t, "POST", "/api/vault/unlock", map[string]any{
			"user": "admin", "password": "Passw0rd1234", "totp": "000000",
			"challengeId": e2eStr(resp, "challengeId"), "signature": "bad",
		}, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("错误凭据应 401: %d %v", code, resp)
		}
		t.Log("PHASE2 OK：重启后状态/门禁/认证路径全部正常")
		return
	}

	// 未初始化：init
	if e2eData(resp)["initialized"] == true {
		t.Fatalf("预期全新数据目录（未初始化），当前已初始化: %v", resp)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/init",
		map[string]any{"user": "admin", "password": "Passw0rd1234"}, "")
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	initToken := e2eStr(resp, "initToken")
	totpSecret := e2eStr(resp, "totpSecret")
	recoveryToken := e2eStr(resp, "recoveryToken")
	if initToken == "" || totpSecret == "" || recoveryToken == "" {
		t.Fatalf("init 返回不完整: %v", resp)
	}

	// TOTP 绑定（服务端同一套实现计算验证码，保证两端一致）
	secret, err := base32decode(t, totpSecret)
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/totp/verify",
		map[string]any{"code": totpCode(secret, time.Now(), totpDigits, totpPeriod)}, initToken)
	if code != http.StatusOK {
		t.Fatalf("totp/verify 失败: %d %v", code, resp)
	}

	// 证书绑定（.NET 侧签名路径已在脚本版验证过协议；此处用 Go 生成证书+签名）
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "irix-e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	code, resp = e2eReq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "cert-bind"}, "")
	if code != http.StatusOK {
		t.Fatalf("challenge 失败: %d %v", code, resp)
	}
	chValue := e2eStr(resp, "challenge")
	digest := sha256.Sum256([]byte(signPrefixCertBind + chValue))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/cert", map[string]any{
		"certPem": certPEM, "challengeId": e2eStr(resp, "challengeId"),
		"signature": base64.RawStdEncoding.EncodeToString(sig),
	}, initToken)
	if code != http.StatusOK {
		t.Fatalf("cert 绑定失败: %d %v", code, resp)
	}

	// 解锁（三重认证）
	code, resp = e2eReq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "unlock"}, "")
	if code != http.StatusOK {
		t.Fatalf("challenge 失败: %d %v", code, resp)
	}
	chValue = e2eStr(resp, "challenge")
	digest = sha256.Sum256([]byte(signPrefixUnlock + chValue))
	sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/unlock", map[string]any{
		"user": "admin", "password": "Passw0rd1234",
		"totp":        totpCode(secret, time.Now(), totpDigits, totpPeriod),
		"challengeId": e2eStr(resp, "challengeId"),
		"signature":   base64.RawStdEncoding.EncodeToString(sig),
	}, "")
	if code != http.StatusOK {
		t.Fatalf("unlock 失败: %d %v", code, resp)
	}
	sessionToken := e2eStr(resp, "sessionToken")
	if sessionToken == "" {
		t.Fatalf("未返回 sessionToken: %v", resp)
	}

	// 数据面：带令牌放行、无令牌 403
	code, resp = e2eReq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, sessionToken)
	if code != http.StatusOK {
		t.Fatalf("解锁后数据面应放行: %d %v", code, resp)
	}
	code, resp = e2eReq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, "")
	if code != http.StatusForbidden {
		t.Fatalf("无令牌应 403: %d %v", code, resp)
	}

	// 锁定
	code, resp = e2eReq(t, "POST", "/api/vault/lock", nil, sessionToken)
	if code != http.StatusOK {
		t.Fatalf("lock 失败: %d %v", code, resp)
	}
	code, resp = e2eReq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, sessionToken)
	if code != http.StatusForbidden {
		t.Fatalf("锁定后应 403: %d %v", code, resp)
	}
	t.Log("PHASE1 OK：init → onboarding → unlock → 数据面 → lock 全链路通过（真实进程 + 真实 TLS）")
}

// TestVaultRealServerE2EM4 真实进程 M4 E2E（vaultFiles 物化生命周期）。
// 用法：VAULT_E2E=m4（全新数据目录启动节点）：
//
//	.\irix-node.exe -tls-mode auto -vault -apikey e2e-key -port 19997 -data <dir> ...
//	$env:VAULT_E2E='m4'; go test -run TestVaultRealServerE2EM4 -count=1 -v .
func TestVaultRealServerE2EM4(t *testing.T) {
	if os.Getenv("VAULT_E2E") != "m4" {
		t.Skip("未设置 VAULT_E2E=m4（真实进程 M4 E2E 需手动启动节点）")
	}
	// 初始化 + onboarding + 解锁（复用 Phase 1 的流程）
	code, resp := e2eReq(t, "GET", "/api/vault/status", nil, "")
	if code != http.StatusOK || e2eData(resp)["initialized"] == true {
		t.Fatalf("预期全新数据目录: %d %v", code, resp)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/init",
		map[string]any{"user": "admin", "password": "Passw0rd1234"}, "")
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	initToken := e2eStr(resp, "initToken")
	totpSecret := e2eStr(resp, "totpSecret")
	secret, err := base32decode(t, totpSecret)
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/totp/verify",
		map[string]any{"code": totpCode(secret, time.Now(), totpDigits, totpPeriod)}, initToken)
	if code != http.StatusOK {
		t.Fatalf("totp/verify 失败: %d %v", code, resp)
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "irix-e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	code, resp = e2eReq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "cert-bind"}, "")
	chValue := e2eStr(resp, "challenge")
	digest := sha256.Sum256([]byte(signPrefixCertBind + chValue))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/cert", map[string]any{
		"certPem": certPEM, "challengeId": e2eStr(resp, "challengeId"),
		"signature": base64.RawStdEncoding.EncodeToString(sig),
	}, initToken)
	if code != http.StatusOK {
		t.Fatalf("cert 绑定失败: %d %v", code, resp)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "unlock"}, "")
	chValue = e2eStr(resp, "challenge")
	digest = sha256.Sum256([]byte(signPrefixUnlock + chValue))
	sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/vault/unlock", map[string]any{
		"user": "admin", "password": "Passw0rd1234",
		"totp":        totpCode(secret, time.Now(), totpDigits, totpPeriod),
		"challengeId": e2eStr(resp, "challengeId"),
		"signature":   base64.RawStdEncoding.EncodeToString(sig),
	}, "")
	if code != http.StatusOK {
		t.Fatalf("unlock 失败: %d %v", code, resp)
	}
	token := e2eStr(resp, "sessionToken")

	// 创建 vaultFiles 实例（cwd 含明文文件）
	cwd := t.TempDir()
	world := filepath.Join(cwd, "world")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "server.properties"), []byte("motd=e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "level.dat"), []byte(strings.Repeat("L", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, resp = e2eReq(t, "POST", "/api/instance", map[string]any{
		"nickname": "e2e-sv", "cwd": cwd, "startCommand": "ping 127.0.0.1 -n 3",
		"vaultFiles": true,
	}, token)
	if code != http.StatusOK {
		t.Fatalf("创建实例失败: %d %v", code, resp)
	}
	uuid := e2eStr(resp, "instanceUuid")
	if uuid == "" {
		t.Fatalf("未返回 instanceUuid: %v", resp)
	}

	// 启动实例：物化（崩溃残留→加密→物化）+ 进程运行
	code, resp = e2eReq(t, "GET", "/api/protected_instance/open?daemonId=x&uuid="+uuid, nil, token)
	if code != http.StatusOK {
		t.Fatalf("启动失败: %d %v", code, resp)
	}
	got, err := os.ReadFile(filepath.Join(cwd, "server.properties"))
	if err != nil || string(got) != "motd=e2e\n" {
		t.Fatalf("物化失败: %v %q", err, got)
	}
	t.Log("实例已启动，文件树已物化")

	// 停止实例：回收（整树加密入库 + 删除明文）
	code, resp = e2eReq(t, "GET", "/api/protected_instance/stop?daemonId=x&uuid="+uuid, nil, token)
	if code != http.StatusOK {
		t.Fatalf("停止失败: %d %v", code, resp)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cwd, "server.properties")); !os.IsNotExist(err) {
		t.Fatal("停止后明文文件应已删除（回收）")
	}
	// 加密层列表与读取
	code, resp = e2eReq(t, "GET", "/api/files/list?daemonId=x&uuid="+uuid+"&page=1&page_size=100", nil, token)
	if code != http.StatusOK {
		t.Fatalf("加密层列表失败: %d %v", code, resp)
	}
	items, ok := e2eData(resp)["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("加密层应有 2 个文件: %v", resp)
	}
	code, resp = e2eReq(t, "PUT", "/api/files/?daemonId=x&uuid="+uuid,
		map[string]any{"target": "/world/level.dat"}, token)
	if code != http.StatusOK || vstr(resp, "data") != strings.Repeat("L", 10000) {
		t.Fatalf("加密层读取失败: %d %v", code, resp)
	}
	t.Log("PHASE-M4 OK：创建 → 物化启动 → 回收停止 → 加密层读写 全链路通过")
}

// base32decode 解码 base32（无填充）。
func base32decode(t *testing.T, s string) ([]byte, error) {
	t.Helper()
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}
