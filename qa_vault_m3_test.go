// qa_vault_m3_test.go — Vault M3 测试。
//
// 覆盖 docs/vault-design.md §6/§7/§11 的端到端语义：初始化与 onboarding
// （initToken/TOTP 绑定/证书绑定）、三重认证解锁、统一限速（S1/S2）、
// 锁定与数据面门禁、改密 rewrap（含 forceExpire 同请求改密 A3）、恢复令牌
// 流程、多用户管理、重启持久化、审计掩码（redactBody）。

package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vaultTestEnv vault 测试环境（vault 启用、PBKDF2 迭代加速、限速可调）。
type vaultTestEnv struct {
	d   *Daemon
	srv *httptest.Server
	dir string
}

// newVaultEnv 组装测试环境；maxAttempts 为限速阈值（测试默认 3）。
// 处理链与生产一致：RegisterRoutes → vaultGate（数据面门禁）。
func newVaultEnv(t *testing.T, maxAttempts int) *vaultTestEnv {
	t.Helper()
	d, dir := newTestDaemon(t)
	d.vault.enabled = true
	d.vault.file = filepath.Join(dir, "vault", "vault.json")
	d.vault.pbkdf2Iterations = 1000 // 测试加速（生产默认 600k）
	d.vault.maxAttempts = maxAttempts
	d.vault.lockoutDuration = time.Minute
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	srv := httptest.NewServer(d.vaultGate(mux))
	t.Cleanup(srv.Close)
	return &vaultTestEnv{d: d, srv: srv, dir: dir}
}

// vreq 发起带 apikey 的 JSON 请求，返回状态码与解析后的响应体。
func (e *vaultTestEnv) vreq(t *testing.T, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("JSON 编码失败: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Api-Key", "test-key")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// vdata 取响应 data 字段（map）。
func vdata(m map[string]any) map[string]any {
	if d, ok := m["data"].(map[string]any); ok {
		return d
	}
	return nil
}

// vstr 取 data 中的字符串字段。
func vstr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// onboardCreds 一次完整 onboarding 后的全部凭据。
type onboardCreds struct {
	user, password string
	totpSecret     string // base32
	recoveryToken  string
	priv           *rsa.PrivateKey // 证书私钥（客户端侧持有）
	certPEM        string
}

// decodeB32 解码 base32（无填充）。
func decodeB32(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		t.Fatalf("base32 解码失败: %v", err)
	}
	return b
}

// makeTestCertPEM 生成测试用 RSA-2048 自签证书 PEM 与私钥。
func makeTestCertPEM(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := makeSelfSignedCertDER(t, &priv.PublicKey, priv)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), priv
}

// signChallenge 用私钥对签名消息签名（与客户端实现一致：PKCS#1 v1.5 + SHA-256，
// base64 无填充）。
func signChallenge(t *testing.T, priv *rsa.PrivateKey, message []byte) string {
	t.Helper()
	digest := sha256.Sum256(message)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return base64.RawStdEncoding.EncodeToString(sig)
}

// onboard 完整 onboarding：init → totp/verify → challenge(cert-bind) → cert。
func (e *vaultTestEnv) onboard(t *testing.T, user, password string) *onboardCreds {
	t.Helper()
	code, resp := e.vreq(t, "POST", "/api/vault/init", map[string]any{"user": user, "password": password}, nil)
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	data := vdata(resp)
	creds := &onboardCreds{
		user:          user,
		password:      password,
		totpSecret:    vstr(data, "totpSecret"),
		recoveryToken: vstr(data, "recoveryToken"),
	}
	initToken := vstr(data, "initToken")
	if initToken == "" || creds.totpSecret == "" || creds.recoveryToken == "" {
		t.Fatalf("init 返回值不完整: %v", data)
	}
	// TOTP 绑定
	secret := decodeB32(t, creds.totpSecret)
	code2 := totpCode(secret, time.Now(), totpDigits, totpPeriod)
	code3, resp3 := e.vreq(t, "POST", "/api/vault/totp/verify", map[string]any{"code": code2},
		map[string]string{"X-Vault-Token": initToken})
	if code3 != http.StatusOK {
		t.Fatalf("totp/verify 失败: %d %v", code3, resp3)
	}
	// 证书绑定
	certPEM, priv := makeTestCertPEM(t)
	creds.certPEM = certPEM
	creds.priv = priv
	code4, resp4 := e.vreq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "cert-bind"}, nil)
	if code4 != http.StatusOK {
		t.Fatalf("challenge 失败: %d %v", code4, resp4)
	}
	chData := vdata(resp4)
	chID, chValue := vstr(chData, "challengeId"), vstr(chData, "challenge")
	sig := signChallenge(t, priv, []byte(signPrefixCertBind+chValue))
	code5, resp5 := e.vreq(t, "POST", "/api/vault/cert",
		map[string]any{"certPem": certPEM, "challengeId": chID, "signature": sig},
		map[string]string{"X-Vault-Token": initToken})
	if code5 != http.StatusOK {
		t.Fatalf("cert 绑定失败: %d %v", code5, resp5)
	}
	return creds
}

// unlock 执行完整解锁（每次取新挑战）。
// 解锁成功后把该用户防重放窗口清零，模拟「用户等到下一窗口再解锁」
// （30 秒窗口内真实场景同样会被防重放拒绝，测试不等待真实时间）。
func (e *vaultTestEnv) unlock(t *testing.T, creds *onboardCreds, extra map[string]any) (int, map[string]any) {
	t.Helper()
	code, resp := e.rawUnlock(t, creds, extra)
	if code == http.StatusOK {
		e.d.vault.mu.Lock()
		if u := e.d.vault.users[creds.user]; u != nil {
			u.lastTOTPWindow = -1 // 模拟跨窗口；防重放语义由 TestVaultOnboardAndUnlock 单独验证
		}
		e.d.vault.mu.Unlock()
	}
	return code, resp
}

// rawUnlock 执行完整解锁（不重置防重放窗口，供重放语义测试使用）。
func (e *vaultTestEnv) rawUnlock(t *testing.T, creds *onboardCreds, extra map[string]any) (int, map[string]any) {
	t.Helper()
	secret := decodeB32(t, creds.totpSecret)
	totp := totpCode(secret, time.Now(), totpDigits, totpPeriod)
	code, resp := e.vreq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "unlock"}, nil)
	if code != http.StatusOK {
		t.Fatalf("challenge 失败: %d %v", code, resp)
	}
	chData := vdata(resp)
	sig := signChallenge(t, creds.priv, []byte(signPrefixUnlock+vstr(chData, "challenge")))
	body := map[string]any{
		"user": creds.user, "password": creds.password, "totp": totp,
		"challengeId": vstr(chData, "challengeId"), "signature": sig,
	}
	for k, v := range extra {
		body[k] = v
	}
	return e.vreq(t, "POST", "/api/vault/unlock", body, nil)
}

// dataPlane 请求数据面端点（验证门禁）。
func (e *vaultTestEnv) dataPlane(t *testing.T, token string) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["X-Vault-Token"] = token
	}
	return e.vreq(t, "GET", "/api/service/remote_service_instances?daemonId=x&page=1&page_size=10", nil, headers)
}

// ---------------------------------------------------------------------------

// TestVaultInitBasics 初始化基础：弱密码拒绝、initToken 返回、重复 init 拒绝、
// 未初始化门禁 403。
func TestVaultInitBasics(t *testing.T) {
	e := newVaultEnv(t, 3)

	code, resp := e.vreq(t, "GET", "/api/vault/status", nil, nil)
	if code != http.StatusOK || vdata(resp)["enabled"] != true || vdata(resp)["initialized"] != false {
		t.Fatalf("未初始化状态异常: %d %v", code, resp)
	}
	// 未初始化：数据面 403 vault not initialized
	code, resp = e.dataPlane(t, "")
	if code != http.StatusForbidden || vstr(resp, "data") != "vault not initialized" {
		t.Fatalf("未初始化门禁异常: %d %v", code, resp)
	}
	// 弱密码拒绝
	code, resp = e.vreq(t, "POST", "/api/vault/init", map[string]any{"user": "admin", "password": "weak"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("弱密码应拒绝: %d %v", code, resp)
	}
	// 正常初始化
	code, resp = e.vreq(t, "POST", "/api/vault/init", map[string]any{"user": "admin", "password": "Passw0rd1234"}, nil)
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	data := vdata(resp)
	for _, k := range []string{"initToken", "totpSecret", "otpauthURI", "recoveryToken"} {
		if vstr(data, k) == "" {
			t.Fatalf("init 缺少字段 %s: %v", k, data)
		}
	}
	// 重复 init 拒绝
	code, resp = e.vreq(t, "POST", "/api/vault/init", map[string]any{"user": "admin2", "password": "Passw0rd1234"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("重复 init 应拒绝: %d %v", code, resp)
	}
	// 初始化后仍未解锁（onboarding 未完成）
	code, resp = e.vreq(t, "GET", "/api/vault/status", nil, nil)
	if vdata(resp)["locked"] != true {
		t.Fatalf("初始化后未解锁状态异常: %v", resp)
	}
}

// TestVaultOnboardAndUnlock 完整 onboarding 与三重认证解锁；各因素单独错误
// 统一 401；挑战首试即作废。（限速语义由 TestVaultRateLimit 单独覆盖，
// 此处放宽阈值避免 3 次刻意失败触发锁定。）
func TestVaultOnboardAndUnlock(t *testing.T) {
	e := newVaultEnv(t, 10)
	creds := e.onboard(t, "admin", "Passw0rd1234")

	// onboarding 完成后用户信息
	code, resp := e.vreq(t, "GET", "/api/vault/status", nil, nil)
	if vdata(resp)["locked"] != true {
		t.Fatalf("onboarding 后应仍锁定: %v", resp)
	}
	e.d.vault.mu.RLock()
	u := e.d.vault.users["admin"]
	e.d.vault.mu.RUnlock()
	if u == nil || !u.TOTPBound || u.CertFingerprint == "" {
		t.Fatalf("onboarding 未完成: %+v", u)
	}

	// 错误密码 → 统一 401
	code, resp = e.unlock(t, creds, map[string]any{"password": "WrongPass123"})
	if code != http.StatusUnauthorized || vstr(resp, "data") != "认证失败" {
		t.Fatalf("错误密码应统一 401: %d %v", code, resp)
	}
	// 错误 TOTP → 统一 401
	secret := decodeB32(t, creds.totpSecret)
	badCode := totpCode(secret, time.Now().Add(-90*time.Second), totpDigits, totpPeriod)
	code, resp = e.unlock(t, creds, map[string]any{"totp": badCode})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误 TOTP 应 401: %d %v", code, resp)
	}
	// 错误签名 → 统一 401
	code, resp = e.unlock(t, creds, map[string]any{"signature": base64.RawStdEncoding.EncodeToString([]byte("bad-sig"))})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误签名应 401: %d %v", code, resp)
	}
	// 正确解锁
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("正确解锁应 200: %d %v", code, resp)
	}
	token := vstr(vdata(resp), "sessionToken")
	if token == "" {
		t.Fatalf("解锁未返回 sessionToken: %v", resp)
	}
	// 数据面门禁：带令牌 200，无令牌 403
	code, resp = e.dataPlane(t, token)
	if code != http.StatusOK {
		t.Fatalf("解锁后数据面应放行: %d %v", code, resp)
	}
	code, resp = e.dataPlane(t, "")
	if code != http.StatusForbidden || vstr(resp, "data") != "vault locked" {
		t.Fatalf("无令牌应 403: %d %v", code, resp)
	}
	// TOTP 重放防护：同窗口第二次解锁被拒（raw 请求，不经 helper 的窗口重置）
	code, resp = e.rawUnlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("raw 解锁失败: %d %v", code, resp)
	}
	code, resp = e.rawUnlock(t, creds, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("同窗口 TOTP 重放应拒绝: %d %v", code, resp)
	}
	// 挑战首试即作废：拿一次挑战，先用错误密码消耗，再用同一挑战正确解锁 → 失败
	code, resp = e.vreq(t, "POST", "/api/vault/lock", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("lock 失败: %d %v", code, resp)
	}
	code, resp = e.vreq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "unlock"}, nil)
	chData := vdata(resp)
	chID, chValue := vstr(chData, "challengeId"), vstr(chData, "challenge")
	secret2 := decodeB32(t, creds.totpSecret)
	totpNow := totpCode(secret2, time.Now(), totpDigits, totpPeriod)
	sig1 := signChallenge(t, creds.priv, []byte(signPrefixUnlock+chValue))
	code, resp = e.vreq(t, "POST", "/api/vault/unlock",
		map[string]any{"user": creds.user, "password": "WrongPass123", "totp": totpNow, "challengeId": chID, "signature": sig1}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401: %d %v", code, resp)
	}
	sig2 := signChallenge(t, creds.priv, []byte(signPrefixUnlock+chValue))
	code, resp = e.vreq(t, "POST", "/api/vault/unlock",
		map[string]any{"user": creds.user, "password": creds.password, "totp": totpNow, "challengeId": chID, "signature": sig2}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("已消耗挑战复用应拒绝: %d %v", code, resp)
	}
}

// TestVaultRateLimit 统一限速（S1/S2）：失败达到阈值进入锁定，锁定期满恢复。
func TestVaultRateLimit(t *testing.T) {
	e := newVaultEnv(t, 3)
	e.d.vault.lockoutDuration = 300 * time.Millisecond
	creds := e.onboard(t, "admin", "Passw0rd1234")

	for i := 0; i < 3; i++ {
		code, resp := e.unlock(t, creds, map[string]any{"password": "WrongPass123"})
		if code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401: %d %v", i+1, code, resp)
		}
	}
	// 锁定中：即使凭据正确也被拒
	code, resp := e.unlock(t, creds, nil)
	if code != http.StatusUnauthorized || vstr(resp, "data") != "尝试次数过多，请稍后再试" {
		t.Fatalf("锁定中应提示次数过多: %d %v", code, resp)
	}
	// 锁定期满后恢复
	time.Sleep(400 * time.Millisecond)
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("锁定期满后应可解锁: %d %v", code, resp)
	}
}

// TestVaultLockAndGate 锁定语义：lock 清零密钥、会话失效、数据面重新 403。
func TestVaultLockAndGate(t *testing.T) {
	e := newVaultEnv(t, 3)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	code, resp := e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("解锁失败: %d %v", code, resp)
	}
	token := vstr(vdata(resp), "sessionToken")
	if code, _ = e.dataPlane(t, token); code != http.StatusOK {
		t.Fatalf("解锁后数据面应放行")
	}
	code, resp = e.vreq(t, "POST", "/api/vault/lock", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("lock 失败: %d %v", code, resp)
	}
	if e.d.vault.unlockedSafe() {
		t.Fatal("lock 后应处于锁定状态")
	}
	if code, _ = e.dataPlane(t, token); code != http.StatusForbidden {
		t.Fatal("lock 后旧令牌应 403")
	}
	// 锁定后 vault/users 需会话 → 401
	code, resp = e.vreq(t, "GET", "/api/vault/users", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusUnauthorized {
		t.Fatalf("锁定后 users 应 401: %d %v", code, resp)
	}
}

// TestVaultPasswordChange 改密 rewrap：旧密码验证、新密码生效、其他会话作废。
func TestVaultPasswordChange(t *testing.T) {
	e := newVaultEnv(t, 3)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	code, resp := e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("解锁失败: %d %v", code, resp)
	}
	token := vstr(vdata(resp), "sessionToken")

	// 旧密码错误 → 拒绝
	code, resp = e.vreq(t, "POST", "/api/vault/password",
		map[string]any{"oldPassword": "WrongOld123", "newPassword": "NewPassw0rd456"}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusUnauthorized {
		t.Fatalf("旧密码错误应 401: %d %v", code, resp)
	}
	// 正确改密
	code, resp = e.vreq(t, "POST", "/api/vault/password",
		map[string]any{"oldPassword": "Passw0rd1234", "newPassword": "NewPassw0rd456"}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("改密失败: %d %v", code, resp)
	}
	// 旧密码解锁失败、新密码成功
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d %v", code, resp)
	}
	creds.password = "NewPassw0rd456"
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("新密码应可解锁: %d %v", code, resp)
	}
}

// TestVaultForceExpire 密码过期 + forceExpire：解锁须同请求改密（A3/D15）。
func TestVaultForceExpire(t *testing.T) {
	e := newVaultEnv(t, 3)
	e.d.vault.forceExpire = true
	e.d.vault.passwordExpire = 90 * 24 * time.Hour
	creds := e.onboard(t, "admin", "Passw0rd1234")

	// 人为制造密码过期
	e.d.vault.mu.Lock()
	e.d.vault.users["admin"].PasswordChangedAt = time.Now().Add(-100 * 24 * time.Hour)
	e.d.vault.mu.Unlock()

	// 不带 newPassword → 拒绝
	code, resp := e.unlock(t, creds, nil)
	if code != http.StatusUnauthorized || vstr(resp, "data") == "认证失败" {
		t.Fatalf("过期未带新密码应拒绝且提示: %d %v", code, resp)
	}
	// 带 newPassword → 解锁 + 改密
	code, resp = e.unlock(t, creds, map[string]any{"newPassword": "FreshPassw0rd789"})
	if code != http.StatusOK {
		t.Fatalf("过期携带新密码应解锁成功: %d %v", code, resp)
	}
	// 旧密码失效、新密码生效
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d %v", code, resp)
	}
	creds.password = "FreshPassw0rd789"
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("新密码应可解锁: %d %v", code, resp)
	}
}

// TestVaultRecovery 恢复令牌：建立恢复会话（不开放数据面）、无需旧密码改密。
func TestVaultRecovery(t *testing.T) {
	e := newVaultEnv(t, 3)
	creds := e.onboard(t, "admin", "Passw0rd1234")

	// 错误令牌 → 401
	code, resp := e.vreq(t, "POST", "/api/vault/recovery", map[string]any{"recoveryToken": "wrong-token"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("错误恢复令牌应 401: %d %v", code, resp)
	}
	// 正确令牌 → 恢复会话
	code, resp = e.vreq(t, "POST", "/api/vault/recovery", map[string]any{"recoveryToken": creds.recoveryToken}, nil)
	if code != http.StatusOK {
		t.Fatalf("恢复令牌应建立恢复会话: %d %v", code, resp)
	}
	recToken := vstr(vdata(resp), "sessionToken")
	if vdata(resp)["recovery"] != true {
		t.Fatalf("应标记 recovery 会话: %v", resp)
	}
	// 恢复会话不开放数据面
	if code, _ = e.dataPlane(t, recToken); code != http.StatusForbidden {
		t.Fatal("恢复会话不应开放数据面")
	}
	// 恢复会话免旧密码改密
	code, resp = e.vreq(t, "POST", "/api/vault/password",
		map[string]any{"newPassword": "RecoveredPass1"}, map[string]string{"X-Vault-Token": recToken})
	if code != http.StatusOK {
		t.Fatalf("恢复会话改密失败: %d %v", code, resp)
	}
	// 旧密码失效、新密码生效
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d %v", code, resp)
	}
	creds.password = "RecoveredPass1"
	code, resp = e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("新密码应可解锁: %d %v", code, resp)
	}
}

// TestVaultUserManage 多用户：add（onboarding）、list、remove、禁删最后一个。
func TestVaultUserManage(t *testing.T) {
	e := newVaultEnv(t, 3)
	admin := e.onboard(t, "admin", "Passw0rd1234")
	code, resp := e.unlock(t, admin, nil)
	if code != http.StatusOK {
		t.Fatalf("管理员解锁失败: %d %v", code, resp)
	}
	token := vstr(vdata(resp), "sessionToken")

	// 新增用户
	code, resp = e.vreq(t, "POST", "/api/vault/user/add",
		map[string]any{"user": "op", "password": "OpPassw0rd456"}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("user/add 失败: %d %v", code, resp)
	}
	opData := vdata(resp)
	opInit := vstr(opData, "initToken")
	opSecret := decodeB32(t, vstr(opData, "totpSecret"))

	// 新用户 onboarding：TOTP 绑定 + 证书绑定
	opCode := totpCode(opSecret, time.Now(), totpDigits, totpPeriod)
	code, resp = e.vreq(t, "POST", "/api/vault/totp/verify", map[string]any{"code": opCode},
		map[string]string{"X-Vault-Token": opInit})
	if code != http.StatusOK {
		t.Fatalf("op TOTP 绑定失败: %d %v", code, resp)
	}
	opCert, opPriv := makeTestCertPEM(t)
	code, resp = e.vreq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "cert-bind"}, nil)
	chData := vdata(resp)
	sig := signChallenge(t, opPriv, []byte(signPrefixCertBind+vstr(chData, "challenge")))
	code, resp = e.vreq(t, "POST", "/api/vault/cert",
		map[string]any{"certPem": opCert, "challengeId": vstr(chData, "challengeId"), "signature": sig},
		map[string]string{"X-Vault-Token": opInit})
	if code != http.StatusOK {
		t.Fatalf("op 证书绑定失败: %d %v", code, resp)
	}

	// 用户列表：2 人
	code, resp = e.vreq(t, "GET", "/api/vault/users", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("users 失败: %d %v", code, resp)
	}
	if users, ok := vdata(resp)["users"].([]any); !ok || len(users) != 2 {
		t.Fatalf("用户数应为 2: %v", resp)
	}

	// 新用户可独立解锁
	opCreds := &onboardCreds{user: "op", password: "OpPassw0rd456", totpSecret: vstr(opData, "totpSecret"), priv: opPriv}
	code, resp = e.unlock(t, opCreds, nil)
	if code != http.StatusOK {
		t.Fatalf("op 解锁失败: %d %v", code, resp)
	}

	// 删除 op
	code, resp = e.vreq(t, "POST", "/api/vault/user/remove", map[string]any{"user": "op"},
		map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("user/remove 失败: %d %v", code, resp)
	}
	// 删除后 op 无法解锁
	code, resp = e.unlock(t, opCreds, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("删除后 op 应无法解锁: %d %v", code, resp)
	}
	// 禁删最后一个用户
	code, resp = e.vreq(t, "POST", "/api/vault/user/remove", map[string]any{"user": "admin"},
		map[string]string{"X-Vault-Token": token})
	if code != http.StatusBadRequest {
		t.Fatalf("禁删最后一个用户: %d %v", code, resp)
	}
}

// TestVaultTOTPBurn 初始化 TOTP 验证失败 maxTOTPFails 次 → initToken 作废。
func TestVaultTOTPBurn(t *testing.T) {
	e := newVaultEnv(t, 10) // 限速阈值放宽，让 initToken 的 5 次失败先触发
	code, resp := e.vreq(t, "POST", "/api/vault/init", map[string]any{"user": "admin", "password": "Passw0rd1234"}, nil)
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	initToken := vstr(vdata(resp), "initToken")
	for i := 0; i < maxTOTPFails; i++ {
		code, resp = e.vreq(t, "POST", "/api/vault/totp/verify", map[string]any{"code": "000000"},
			map[string]string{"X-Vault-Token": initToken})
		if code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误验证应 401: %d %v", i+1, code, resp)
		}
	}
	// 第 6 次即使正确也因 initToken 作废被拒
	code, resp = e.vreq(t, "POST", "/api/vault/totp/verify", map[string]any{"code": "123456"},
		map[string]string{"X-Vault-Token": initToken})
	if code != http.StatusUnauthorized || vstr(resp, "data") == "验证码错误" {
		t.Fatalf("initToken 应已作废: %d %v", code, resp)
	}
}

// TestVaultPersistence 重启持久化：vault.json 重新加载后用户/证书/恢复记录完整，
// 且可正常解锁。
func TestVaultPersistence(t *testing.T) {
	e := newVaultEnv(t, 3)
	creds := e.onboard(t, "admin", "Passw0rd1234")

	// 模拟重启：同一数据目录新建守护进程并加载 vault.json
	d2 := NewDaemon(e.dir, "test-key")
	d2.vault.enabled = true
	d2.vault.pbkdf2Iterations = 1000
	d2.vault.maxAttempts = 3
	d2.vault.lockoutDuration = time.Minute
	if err := d2.vault.load(filepath.Join(e.dir, "vault", "vault.json")); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if !d2.vault.initialized {
		t.Fatal("重启后应已初始化")
	}
	if d2.vault.unlockedSafe() {
		t.Fatal("重启后应处于锁定状态")
	}
	d2.vault.mu.RLock()
	u := d2.vault.users["admin"]
	d2.vault.mu.RUnlock()
	if u == nil || !u.TOTPBound || u.CertFingerprint == "" || u.MasterKeyWrapB64 == "" {
		t.Fatalf("重启后用户数据不完整: %+v", u)
	}
	// 重启后可正常解锁
	mux2 := http.NewServeMux()
	d2.RegisterRoutes(mux2)
	srv2 := httptest.NewServer(d2.vaultGate(mux2))
	defer srv2.Close()
	e2 := &vaultTestEnv{d: d2, srv: srv2, dir: e.dir}
	code, resp := e2.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("重启后解锁失败: %d %v", code, resp)
	}
}

// TestVaultChallengePurpose 挑战用途隔离（S4）：cert-bind 挑战不能用于 unlock。
func TestVaultChallengePurpose(t *testing.T) {
	e := newVaultEnv(t, 3)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	code, resp := e.vreq(t, "POST", "/api/vault/challenge", map[string]any{"purpose": "cert-bind"}, nil)
	if code != http.StatusOK {
		t.Fatalf("challenge 失败: %d %v", code, resp)
	}
	chData := vdata(resp)
	sig := signChallenge(t, creds.priv, []byte(signPrefixUnlock+vstr(chData, "challenge")))
	secret := decodeB32(t, creds.totpSecret)
	totp := totpCode(secret, time.Now(), totpDigits, totpPeriod)
	code, resp = e.vreq(t, "POST", "/api/vault/unlock",
		map[string]any{"user": creds.user, "password": creds.password, "totp": totp,
			"challengeId": vstr(chData, "challengeId"), "signature": sig}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("cert-bind 挑战用于 unlock 应拒绝: %d %v", code, resp)
	}
}

// TestRedactBody 审计掩码（S5/§11）：敏感字段值打码；code 字段仅 vault 路径打码。
func TestRedactBody(t *testing.T) {
	body := `{"user":"admin","password":"Secret123","newPassword":"NewSecret1","totp":"123456","code":"654321","signature":"abc","recoveryToken":"tok","sessionToken":"sess"}`
	masked := redactBody(body, true)
	for _, f := range []string{`"password":"***"`, `"newPassword":"***"`, `"totp":"***"`,
		`"code":"***"`, `"signature":"***"`, `"recoveryToken":"***"`, `"sessionToken":"***"`} {
		if !strings.Contains(masked, f) {
			t.Errorf("vault 路径掩码缺少 %s: %s", f, masked)
		}
	}
	if strings.Contains(masked, "Secret123") || strings.Contains(masked, "NewSecret1") {
		t.Errorf("密码明文不应残留: %s", masked)
	}
	// 非 vault 路径：code 不打码
	masked2 := redactBody(body, false)
	if !strings.Contains(masked2, `"code":"654321"`) {
		t.Errorf("非 vault 路径 code 不应打码: %s", masked2)
	}
	if !strings.Contains(masked2, `"password":"***"`) {
		t.Errorf("非 vault 路径 password 仍应打码: %s", masked2)
	}
}
