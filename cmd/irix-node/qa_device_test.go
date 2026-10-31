// 设备绑定（docs/device-binding.md）测试：
//   - Ed25519 / ECDSA P-256 设备注册、挑战登录、会话绑定；
//   - 已绑定会话的逐请求 HMAC 证明、时间戳、序号窗口与重放拒绝；
//   - required 策略（无设备不能登录）、策略切换撤销旧会话；
//   - 设备撤销、最后一台设备保护、管理员降级恢复；
//   - 旧 accounts.db 补列迁移与重复初始化幂等；
//   - 逐请求证明性能基准。

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deviceTestKey 测试用设备密钥（Ed25519 或 P-256）。
type deviceTestKey struct {
	keyType  string
	edPriv   ed25519.PrivateKey
	p256Priv *ecdsa.PrivateKey
	pubRaw   []byte
	pubB64   string
	deviceID string
}

// newEd25519TestKey 生成 Ed25519 设备密钥。
func newEd25519TestKey(t testing.TB) *deviceTestKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &deviceTestKey{
		keyType:  deviceKeyTypeEd25519,
		edPriv:   priv,
		pubRaw:   pub,
		pubB64:   base64.RawURLEncoding.EncodeToString(pub),
		deviceID: deviceIDFromPublicKey(pub),
	}
}

// newP256TestKey 生成 P-256 设备密钥（未压缩公钥点）。
func newP256TestKey(t testing.TB) *deviceTestKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := elliptic.Marshal(elliptic.P256(), priv.X, priv.Y)
	return &deviceTestKey{
		keyType:  deviceKeyTypeP256,
		p256Priv: priv,
		pubRaw:   raw,
		pubB64:   base64.RawURLEncoding.EncodeToString(raw),
		deviceID: deviceIDFromPublicKey(raw),
	}
}

// sign 用设备私钥签名规范串。
func (k *deviceTestKey) sign(t testing.TB, transcript []byte) string {
	t.Helper()
	if k.keyType == deviceKeyTypeEd25519 {
		return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.edPriv, transcript))
	}
	digest := sha256.Sum256(transcript)
	r, s, err := ecdsa.Sign(rand.Reader, k.p256Priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return base64.RawURLEncoding.EncodeToString(sig)
}

// deviceTestDescriptor 客户端签名的设备描述（OS/版本/安装随机数等风控字段）。
func deviceTestDescriptor(t testing.TB) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"os":"test","osVersion":"1.0","build":"26100","arch":"amd64","appVersion":"1.4.0","installId":%q,"hardwareClass":"desktop"}`,
		hex.EncodeToString(buf))
}

// deviceHTTP 发起 JSON 请求（可带 token 与额外请求头）。
func deviceHTTP(t testing.TB, method, url, token string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// deviceTestChallenge 申请一次性挑战。
func deviceTestChallenge(t testing.TB, srv *httptest.Server, username, deviceID, purpose string) deviceChallengeResponse {
	t.Helper()
	code, body := deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/device/challenge", "",
		map[string]string{"username": username, "deviceId": deviceID, "purpose": purpose}, nil)
	if code != http.StatusOK {
		t.Fatalf("申请挑战应 200，实际 %d: %s", code, body)
	}
	var resp struct {
		Data deviceChallengeResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析挑战失败: %v", err)
	}
	return resp.Data
}

// deviceTestRegister 完整走一次设备注册（挑战 + 密码 + 签名）。
func deviceTestRegister(t testing.TB, srv *httptest.Server, token, username, password string, key *deviceTestKey) (int, []byte) {
	t.Helper()
	ch := deviceTestChallenge(t, srv, username, key.deviceID, devicePurposeRegister)
	req := &deviceRegisterRequest{
		DeviceID:      key.deviceID,
		PublicKey:     key.pubB64,
		KeyType:       key.keyType,
		KeyProtection: "tpm2",
		Descriptor:    deviceTestDescriptor(t),
		Name:          "测试设备",
		Nonce:         ch.Nonce,
		Timestamp:     time.Now().Unix(),
		Password:      password,
	}
	descriptorHash := sha256.Sum256([]byte(req.Descriptor))
	req.Signature = key.sign(t, deviceRegisterTranscript(username, req, key.deviceID,
		hex.EncodeToString(descriptorHash[:]), ch.TLSFingerprint))
	return deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/devices", token, req, nil)
}

// deviceTestLogin 走密码 + 设备签名登录，返回 token / proofKey / 状态码。
func deviceTestLogin(t testing.TB, srv *httptest.Server, username, password string, key *deviceTestKey) (int, string, string, []byte) {
	t.Helper()
	ch := deviceTestChallenge(t, srv, username, key.deviceID, devicePurposeLogin)
	proofKeyBytes := make([]byte, deviceProofKeyBytes)
	if _, err := rand.Read(proofKeyBytes); err != nil {
		t.Fatal(err)
	}
	proofKey := base64.RawURLEncoding.EncodeToString(proofKeyBytes)
	proof := &deviceLoginProof{
		DeviceID:  key.deviceID,
		Nonce:     ch.Nonce,
		Timestamp: time.Now().Unix(),
		ProofKey:  proofKey,
	}
	proof.Signature = key.sign(t, deviceLoginTranscript(username, proof, ch.TLSFingerprint))
	code, body := deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/login", "",
		accountLoginRequest{Username: username, Password: password, Device: proof}, nil)
	if code != http.StatusOK {
		return code, "", "", body
	}
	var resp struct {
		Data accountLoginResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	return code, resp.Data.Token, proofKey, body
}

// passwordLogin 普通密码登录（用于验证 required 策略/降级恢复）。
func passwordLogin(t testing.TB, srv *httptest.Server, username, password string) (int, []byte) {
	t.Helper()
	return deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/login", "",
		map[string]string{"username": username, "password": password}, nil)
}

// deviceProofHeaders 生成逐请求证明头（timestamp 由调用方指定）。
func deviceProofHeaders(t testing.TB, key *deviceTestKey, proofKey, method, requestURI string, seq uint64, ts int64) map[string]string {
	t.Helper()
	keyBytes, err := base64.RawURLEncoding.DecodeString(proofKey)
	if err != nil {
		t.Fatal(err)
	}
	tsText := strconv.FormatInt(ts, 10)
	seqText := strconv.FormatUint(seq, 10)
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write(deviceProofTranscript(method, requestURI, tsText, seqText))
	return map[string]string{
		"X-Device-Id":         key.deviceID,
		"X-Session-Timestamp": tsText,
		"X-Session-Seq":       seqText,
		"X-Session-Proof":     base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// TestDeviceEd25519RegisterLoginAndProof 主链路：注册 → 登录 → 逐请求证明 → 防重放。
func TestDeviceEd25519RegisterLoginAndProof(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.TLSFingerprint = "test-fingerprint" // 设备登录/绑定会话要求 TLS
	srv := newTestServer(t, d)
	defer srv.Close()
	key := newEd25519TestKey(t)

	// 注册
	code, body := deviceTestRegister(t, srv, qaTestToken(), qaTestAdminUser, qaTestAdminPass, key)
	if code != http.StatusOK {
		t.Fatalf("设备注册应 200，实际 %d: %s", code, body)
	}
	// 设备列表
	code, body = deviceHTTP(t, http.MethodGet, srv.URL+"/api/auth/devices", qaTestToken(), nil, nil)
	if code != http.StatusOK {
		t.Fatalf("设备列表应 200，实际 %d: %s", code, body)
	}
	var list struct {
		Data struct {
			Devices      []map[string]any `json:"devices"`
			DevicePolicy string           `json:"devicePolicy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data.Devices) != 1 || list.Data.DevicePolicy != accountDevicePolicyOptional {
		t.Fatalf("设备列表异常: %s", body)
	}

	// 策略 optional：纯密码登录仍可用（旧客户端兼容）
	code, body = passwordLogin(t, srv, qaTestAdminUser, qaTestAdminPass)
	if code != http.StatusOK {
		t.Fatalf("optional 策略下密码登录应 200，实际 %d: %s", code, body)
	}

	// 设备登录：会话应绑定设备
	code, token, proofKey, body := deviceTestLogin(t, srv, qaTestAdminUser, qaTestAdminPass, key)
	if code != http.StatusOK {
		t.Fatalf("设备登录应 200，实际 %d: %s", code, body)
	}
	if token == "" || proofKey == "" {
		t.Fatalf("设备登录缺少 token/proofKey: %s", body)
	}

	// 逐请求证明：seq=1 成功
	headers := deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 1, time.Now().Unix())
	code, body = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, headers)
	if code != http.StatusOK {
		t.Fatalf("带证明的请求应 200，实际 %d: %s", code, body)
	}
	var me struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatal(err)
	}
	if me.Data["deviceBound"] != true {
		t.Fatalf("me 应显示 deviceBound=true: %s", body)
	}

	// 重放同一 seq：拒绝
	code, _ = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, headers)
	if code != http.StatusForbidden {
		t.Fatalf("重放请求应 403，实际 %d", code)
	}
	// seq+1：成功
	headers = deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 2, time.Now().Unix())
	code, body = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, headers)
	if code != http.StatusOK {
		t.Fatalf("递增 seq 应 200，实际 %d: %s", code, body)
	}
	// 缺少证明：拒绝
	code, _ = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, nil)
	if code != http.StatusForbidden {
		t.Fatalf("缺少证明应 403，实际 %d", code)
	}
	// 设备 ID 不匹配：拒绝
	badHeaders := deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 3, time.Now().Unix())
	badHeaders["X-Device-Id"] = newEd25519TestKey(t).deviceID
	code, _ = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, badHeaders)
	if code != http.StatusForbidden {
		t.Fatalf("设备 ID 不匹配应 403，实际 %d", code)
	}
	// 过期时间戳：拒绝
	staleHeaders := deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 4, time.Now().Add(-time.Hour).Unix())
	code, _ = deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, staleHeaders)
	if code != http.StatusForbidden {
		t.Fatalf("过期时间戳应 403，实际 %d", code)
	}
	t.Logf("[验证] Ed25519 设备注册/登录/证明/防重放链路通过")
}

// TestDeviceP256RegisterLogin P-256（WebCrypto/TPM 常见曲线）完整链路。
func TestDeviceP256RegisterLogin(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.TLSFingerprint = "test-fingerprint"
	srv := newTestServer(t, d)
	defer srv.Close()
	key := newP256TestKey(t)

	if code, body := deviceTestRegister(t, srv, qaTestToken(), qaTestAdminUser, qaTestAdminPass, key); code != http.StatusOK {
		t.Fatalf("P-256 注册应 200，实际 %d: %s", code, body)
	}
	code, token, proofKey, body := deviceTestLogin(t, srv, qaTestAdminUser, qaTestAdminPass, key)
	if code != http.StatusOK {
		t.Fatalf("P-256 登录应 200，实际 %d: %s", code, body)
	}
	headers := deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 1, time.Now().Unix())
	if code, body := deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, headers); code != http.StatusOK {
		t.Fatalf("P-256 带证明请求应 200，实际 %d: %s", code, body)
	}
	t.Logf("[验证] P-256 设备注册/登录/证明通过")
}

// TestDevicePolicyRequired required 策略：无设备不能登录；切换策略撤销旧会话。
func TestDevicePolicyRequired(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.TLSFingerprint = "test-fingerprint" // required 要求 TLS；测试用固定指纹
	srv := newTestServer(t, d)
	defer srv.Close()

	const (
		username = "device-required"
		password = "DevicePass123"
	)
	if err := d.accounts.createAccount(username, password, false); err != nil {
		t.Fatal(err)
	}
	// 先登录拿未绑定 token 并注册设备
	code, body := passwordLogin(t, srv, username, password)
	if code != http.StatusOK {
		t.Fatalf("初始密码登录应 200，实际 %d: %s", code, body)
	}
	var login struct {
		Data accountLoginResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatal(err)
	}
	key := newEd25519TestKey(t)
	if code, body := deviceTestRegister(t, srv, login.Data.Token, username, password, key); code != http.StatusOK {
		t.Fatalf("注册设备应 200，实际 %d: %s", code, body)
	}

	// 管理员开启 required
	code, body = deviceHTTP(t, http.MethodPut, srv.URL+"/api/accounts/device-policy", qaTestToken(),
		map[string]string{"username": username, "policy": accountDevicePolicyRequired}, nil)
	if code != http.StatusOK {
		t.Fatalf("设置 required 应 200，实际 %d: %s", code, body)
	}
	// 旧会话被撤销
	if code, _ := deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", login.Data.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("策略变更后旧会话应 403，实际 %d", code)
	}
	// 纯密码登录：403
	if code, body := passwordLogin(t, srv, username, password); code != http.StatusForbidden {
		t.Fatalf("required 下密码登录应 403，实际 %d: %s", code, body)
	}
	// 设备登录：200
	code, token, proofKey, body := deviceTestLogin(t, srv, username, password, key)
	if code != http.StatusOK {
		t.Fatalf("required 下设备登录应 200，实际 %d: %s", code, body)
	}
	headers := deviceProofHeaders(t, key, proofKey, http.MethodGet, "/api/accounts/me", 1, time.Now().Unix())
	if code, body := deviceHTTP(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil, headers); code != http.StatusOK {
		t.Fatalf("required 下带证明请求应 200，实际 %d: %s", code, body)
	}

	// 无设备账户不能开启 required（避免立即锁死）
	if err := d.accounts.createAccount("device-empty", "EmptyPass123", false); err != nil {
		t.Fatal(err)
	}
	code, _ = deviceHTTP(t, http.MethodPut, srv.URL+"/api/accounts/device-policy", qaTestToken(),
		map[string]string{"username": "device-empty", "policy": accountDevicePolicyRequired}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("无设备账户开启 required 应 400，实际 %d", code)
	}
	t.Logf("[验证] required 策略阻止无设备登录，策略切换撤销旧会话")
}

// TestDeviceRevokeAndAdminRecovery 设备撤销、最后一台设备保护、管理员降级恢复。
func TestDeviceRevokeAndAdminRecovery(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.TLSFingerprint = "test-fingerprint"
	srv := newTestServer(t, d)
	defer srv.Close()

	const (
		username = "device-revoke"
		password = "RevokePass123"
	)
	if err := d.accounts.createAccount(username, password, false); err != nil {
		t.Fatal(err)
	}
	code, body := passwordLogin(t, srv, username, password)
	if code != http.StatusOK {
		t.Fatalf("初始登录应 200: %s", body)
	}
	var login struct {
		Data accountLoginResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatal(err)
	}
	keyA := newEd25519TestKey(t)
	keyB := newEd25519TestKey(t)
	if code, body := deviceTestRegister(t, srv, login.Data.Token, username, password, keyA); code != http.StatusOK {
		t.Fatalf("注册设备 A 应 200: %s", body)
	}
	if code, body := deviceTestRegister(t, srv, login.Data.Token, username, password, keyB); code != http.StatusOK {
		t.Fatalf("注册设备 B 应 200: %s", body)
	}
	// 开启 required
	if code, body := deviceHTTP(t, http.MethodPut, srv.URL+"/api/accounts/device-policy", qaTestToken(),
		map[string]string{"username": username, "policy": accountDevicePolicyRequired}, nil); code != http.StatusOK {
		t.Fatalf("设置 required 应 200: %s", body)
	}
	code, tokenA, proofA, body := deviceTestLogin(t, srv, username, password, keyA)
	if code != http.StatusOK {
		t.Fatalf("设备 A 登录应 200: %s", body)
	}
	// 撤销设备 A（带会话证明）
	headers := deviceProofHeaders(t, keyA, proofA, http.MethodPost, "/api/auth/devices/revoke", 1, time.Now().Unix())
	code, body = deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/devices/revoke", tokenA,
		map[string]string{"deviceId": keyA.deviceID, "password": password}, headers)
	if code != http.StatusOK {
		t.Fatalf("撤销设备 A 应 200，实际 %d: %s", code, body)
	}
	// 设备 A 不能再次登录
	if code, _, _, body := deviceTestLogin(t, srv, username, password, keyA); code != http.StatusForbidden {
		t.Fatalf("已撤销设备登录应 403，实际 %d: %s", code, body)
	}
	// 设备 B 仍可登录
	code, tokenB, proofB, body := deviceTestLogin(t, srv, username, password, keyB)
	if code != http.StatusOK {
		t.Fatalf("设备 B 登录应 200: %s", body)
	}
	// required 下不能撤销最后一台设备
	headers = deviceProofHeaders(t, keyB, proofB, http.MethodPost, "/api/auth/devices/revoke", 1, time.Now().Unix())
	code, _ = deviceHTTP(t, http.MethodPost, srv.URL+"/api/auth/devices/revoke", tokenB,
		map[string]string{"deviceId": keyB.deviceID, "password": password}, headers)
	if code != http.StatusForbidden {
		t.Fatalf("撤销最后一台设备应 403，实际 %d", code)
	}
	// 管理员删除最后一台设备：自动降级 optional 并撤销会话
	code, body = deviceHTTP(t, http.MethodDelete,
		srv.URL+"/api/accounts/devices?username="+username+"&deviceId="+keyB.deviceID, qaTestToken(), nil, nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"policyReset":true`)) {
		t.Fatalf("管理员删除最后一台设备应 200 且 policyReset=true，实际 %d: %s", code, body)
	}
	// 降级后密码登录恢复
	if code, body := passwordLogin(t, srv, username, password); code != http.StatusOK {
		t.Fatalf("降级 optional 后密码登录应 200，实际 %d: %s", code, body)
	}
	t.Logf("[验证] 设备撤销/最后一台保护/管理员降级恢复通过")
}

// TestDeviceChallengeSingleUse 挑战一次性：过期/重复使用都拒绝。
func TestDeviceChallengeSingleUse(t *testing.T) {
	d, _ := newTestDaemon(t)
	key := newEd25519TestKey(t)
	nonce, _, err := d.accounts.issueDeviceChallenge(qaTestAdminUser, key.deviceID, devicePurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	if !d.accounts.consumeDeviceChallenge(nonce, qaTestAdminUser, key.deviceID, devicePurposeLogin) {
		t.Fatal("首次消费挑战应成功")
	}
	if d.accounts.consumeDeviceChallenge(nonce, qaTestAdminUser, key.deviceID, devicePurposeLogin) {
		t.Fatal("挑战被重复消费")
	}
	// 过期挑战
	nonce2, _, err := d.accounts.issueDeviceChallenge(qaTestAdminUser, key.deviceID, devicePurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	d.accounts.deviceMu.Lock()
	c := d.accounts.deviceChallenges[nonce2]
	c.Expires = time.Now().Add(-time.Second)
	d.accounts.deviceChallenges[nonce2] = c
	d.accounts.deviceMu.Unlock()
	if d.accounts.consumeDeviceChallenge(nonce2, qaTestAdminUser, key.deviceID, devicePurposeLogin) {
		t.Fatal("过期挑战不应通过")
	}
	t.Logf("[验证] 挑战一次性与过期判定通过")
}

// TestDeviceAccountSchemaMigration 旧库补列与重复初始化幂等。
func TestDeviceAccountSchemaMigration(t *testing.T) {
	dir := t.TempDir()
	dsn, err := openSqlite(dir, "")
	if err != nil {
		t.Skipf("当前平台无 SQLite 驱动: %v", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 v1.3.x 旧库：accounts/sessions 无设备字段。
	old := []string{
		`CREATE TABLE accounts (username VARCHAR(64) PRIMARY KEY, password_hash VARCHAR(255) NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0, permissions TEXT NOT NULL DEFAULT '{}',
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE sessions (token VARCHAR(64) PRIMARY KEY, username VARCHAR(64) NOT NULL,
			created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, last_used BIGINT NOT NULL)`,
	}
	for _, st := range old {
		if _, err := db.Exec(st); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	d := NewDaemon(dir)
	if err := d.initAccounts(accountsConfig{Driver: "sqlite"}); err != nil {
		t.Fatalf("旧库升级失败: %v", err)
	}
	t.Cleanup(func() { _ = d.closeAccounts() })
	if err := d.accounts.createAccount("migrated", "MigratedPass123", false); err != nil {
		t.Fatal(err)
	}
	a, err := d.accounts.getAccount("migrated")
	if err != nil || a == nil {
		t.Fatalf("升级后读取账户失败: %v", err)
	}
	if a.DevicePolicy != accountDevicePolicyOptional {
		t.Fatalf("升级后默认策略应为 optional，实际 %q", a.DevicePolicy)
	}
	// 设备表可用
	key := newEd25519TestKey(t)
	dev := &accountDevice{
		DeviceID: key.deviceID, Username: "migrated", PublicKey: key.pubB64,
		KeyType: key.keyType, KeyProtection: "file", Descriptor: `{}`, Name: "旧库设备",
	}
	if err := d.accounts.createDevice(dev); err != nil {
		t.Fatalf("升级后创建设备失败: %v", err)
	}
	// 重复初始化（补列已存在）必须幂等：先关闭旧连接池，避免文件句柄泄漏。
	if err := d.closeAccounts(); err != nil {
		t.Fatalf("关闭旧连接池失败: %v", err)
	}
	if err := d.initAccounts(accountsConfig{Driver: "sqlite"}); err != nil {
		t.Fatalf("重复初始化失败: %v", err)
	}
	t.Logf("[验证] 旧库补列迁移与重复初始化幂等通过")
}

// TestDeviceProofWindow 序号窗口：乱序、重放、过旧、跨窗口重置。
func TestDeviceProofWindow(t *testing.T) {
	var w proofWindow
	for _, seq := range []uint64{1, 3, 2} {
		if !w.accept(seq) {
			t.Fatalf("乱序序号 %d 应接受", seq)
		}
	}
	if w.accept(2) {
		t.Fatal("重复序号应拒绝（重放）")
	}
	if !w.accept(2000) {
		t.Fatal("大幅前进的序号应接受并重置窗口")
	}
	if !w.accept(1999) {
		t.Fatal("窗口内旧序号（乱序）应接受")
	}
	if w.accept(1) {
		t.Fatal("超出窗口的过旧序号应拒绝")
	}
	t.Logf("[验证] 序号窗口乱序/重放/过旧判定正确")
}

// TestDevicePolicyRequiredNeedsTLS required 必须以 TLS 指纹为前提。
func TestDevicePolicyRequiredNeedsTLS(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	const (
		username = "device-tls"
		password = "DeviceTlsPass123"
	)
	if err := d.accounts.createAccount(username, password, false); err != nil {
		t.Fatal(err)
	}
	code, body := passwordLogin(t, srv, username, password)
	if code != http.StatusOK {
		t.Fatalf("登录失败: %s", body)
	}
	var login struct {
		Data accountLoginResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatal(err)
	}
	key := newEd25519TestKey(t)
	if code, body := deviceTestRegister(t, srv, login.Data.Token, username, password, key); code != http.StatusOK {
		t.Fatalf("注册设备失败: %s", body)
	}
	// TLS 关闭（指纹为空）时不能开启 required
	if code, body := deviceHTTP(t, http.MethodPut, srv.URL+"/api/accounts/device-policy", qaTestToken(),
		map[string]string{"username": username, "policy": accountDevicePolicyRequired}, nil); code != http.StatusBadRequest {
		t.Fatalf("TLS 关闭时开启 required 应 400，实际 %d: %s", code, body)
	}
	d.TLSFingerprint = "test-fingerprint"
	if code, body := deviceHTTP(t, http.MethodPut, srv.URL+"/api/accounts/device-policy", qaTestToken(),
		map[string]string{"username": username, "policy": accountDevicePolicyRequired}, nil); code != http.StatusOK {
		t.Fatalf("TLS 开启后 required 应 200，实际 %d: %s", code, body)
	}
	t.Logf("[验证] required 策略要求 TLS 指纹")
}

// TestDeviceAuditRedaction 设备登录的 proofKey 不得明文进入审计日志。
func TestDeviceAuditRedaction(t *testing.T) {
	body := redactBody(`{"proofKey":"very-secret-proof-key","password":"pw","device":{"deviceId":"abc"}}`, false)
	if strings.Contains(body, "very-secret-proof-key") {
		t.Fatalf("proofKey 未打码: %s", body)
	}
	if !strings.Contains(body, `"proofKey":"***"`) {
		t.Fatalf("proofKey 打码格式异常: %s", body)
	}
	t.Logf("[验证] 审计日志对 proofKey 打码: %s", body)
}

// BenchmarkDeviceSessionProof 逐请求 HMAC 证明的端到端校验性能（客户端 HMAC +
// 服务端 HMAC + 序号窗口）。用于确认高并发路径没有非对称签名/DB I/O。
func BenchmarkDeviceSessionProof(b *testing.B) {
	d := NewDaemon(b.TempDir())
	if err := d.initAccounts(accountsConfig{Driver: "sqlite"}); err != nil {
		b.Fatal(err)
	}
	defer d.closeAccounts()
	key := newEd25519TestKey(b)
	proofKeyBytes := make([]byte, deviceProofKeyBytes)
	if _, err := rand.Read(proofKeyBytes); err != nil {
		b.Fatal(err)
	}
	proofKey := base64.RawURLEncoding.EncodeToString(proofKeyBytes)
	const token = "bench-token"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seqText := strconv.FormatInt(int64(i+1), 10)
		mac := hmac.New(sha256.New, proofKeyBytes)
		mac.Write(deviceProofTranscript(http.MethodGet, "/api/overview", ts, seqText))
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		req.Header.Set("X-Device-Id", key.deviceID)
		req.Header.Set("X-Session-Timestamp", ts)
		req.Header.Set("X-Session-Seq", seqText)
		req.Header.Set("X-Session-Proof", base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
		if !d.accounts.verifySessionProof(token, "bench", key.deviceID, proofKey, req) {
			b.Fatal("证明校验失败")
		}
	}
}
