// qa_time_test.go — 时间系统与 TLS/API/会话的集成测试。
//
// 覆盖：时间源全局接管、API 时间字段与 Date 头、会话过期、证书签发/续签/
// 拒绝、配置合并。协议与时钟纪律本身在 internal/timesys 内测试。
package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"irix/node/internal/timesys"
	"irix/node/internal/vaultcrypto"
)

// installFakeClock 把进程时间源换成假时钟，测试结束自动恢复。
func installFakeClock(t *testing.T, fake *timesys.Clock) {
	t.Helper()
	restore := timesys.SetCurrent(fake)
	t.Cleanup(restore)
}

// timeEnvelope 解析 MCSM 风格响应体（data 为泛型）。
type timeEnvelope[T any] struct {
	Status int   `json:"status"`
	Data   T     `json:"data"`
	Time   int64 `json:"time"`
}

// decodeTimeEnvelope 解码统一响应体。
func decodeTimeEnvelope[T any](t *testing.T, body []byte) timeEnvelope[T] {
	t.Helper()
	var out timeEnvelope[T]
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, string(body))
	}
	return out
}

// TestTimeStatusEndpoint 未启用时间系统时 /api/time 仍可用且如实报告。
func TestTimeStatusEndpoint(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	code, body := doReq(t, srv.URL+"/api/time?token="+qaTestToken())
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", code, body)
	}
	resp := decodeTimeEnvelope[struct {
		Enabled     bool   `json:"enabled"`
		Quality     string `json:"quality"`
		Trustworthy bool   `json:"trustworthy"`
		Now         string `json:"now"`
	}](t, body)
	if resp.Data.Enabled {
		t.Fatal("测试守护进程的时间系统应处于未启用状态")
	}
	if resp.Data.Quality != string(timesys.QualityUnlocked) {
		t.Fatalf("质量 = %q，期望 unlocked", resp.Data.Quality)
	}
	if resp.Data.Trustworthy {
		t.Fatal("未同步时不应标记可信")
	}
	if resp.Time <= 0 {
		t.Fatalf("响应体 time 字段无效: %d", resp.Time)
	}
}

// TestTimeSyncDisabled 未启用时手动同步返回明确错误。
func TestTimeSyncDisabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/time/sync?token="+qaTestToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
	var body struct {
		Data string `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if !strings.Contains(body.Data, "未启用") {
		t.Fatalf("错误消息未说明未启用: %q", body.Data)
	}
}

// TestAPITimeFieldUsesClock 统一响应体的 time 字段取节点时间。
func TestAPITimeFieldUsesClock(t *testing.T) {
	fake := timesys.NewFakeClock(time.Now().Add(time.Hour))
	installFakeClock(t, fake)
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	code, body := doReq(t, srv.URL+"/api/overview?token="+qaTestToken())
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", code, body)
	}
	resp := decodeTimeEnvelope[map[string]any](t, body)
	want := fake.Now().UnixMilli()
	if d := resp.Time - want; d < -5000 || d > 5000 {
		t.Fatalf("响应 time = %d，期望约 %d（节点时间）", resp.Time, want)
	}
}

// TestHTTPDateHeaderUsesClock Date 响应头取节点时间。
func TestHTTPDateHeaderUsesClock(t *testing.T) {
	fake := timesys.NewFakeClock(time.Date(2041, 3, 4, 5, 6, 7, 0, time.UTC))
	installFakeClock(t, fake)
	h := clockDateMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	raw := rec.Header().Get("Date")
	if raw == "" {
		t.Fatal("缺少 Date 响应头")
	}
	got, err := http.ParseTime(raw)
	if err != nil {
		t.Fatalf("Date 头无法解析: %v", err)
	}
	if d := got.Sub(fake.Now()); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("Date = %s，期望约 %s", got, fake.Now())
	}
}

// TestSessionExpiryUsesClock 会话过期跟随节点时间（而非系统墙钟）。
func TestSessionExpiryUsesClock(t *testing.T) {
	base := time.Now()
	fake := timesys.NewFakeClock(base)
	installFakeClock(t, fake)
	d, _ := newTestDaemon(t)
	const token = "expire-test-token"
	d.accounts.putSession(token, accountSession{
		Username:  qaTestAdminUser,
		IsAdmin:   true,
		ExpiresAt: fake.Now().Add(accountSessionTTL).UnixMilli(),
	}, accountSessionTTL)
	srv := newTestServer(t, d)
	defer srv.Close()
	url := srv.URL + "/api/time?token=" + token
	if code, body := doReq(t, url); code != http.StatusOK {
		t.Fatalf("到期前请求失败: %d %s", code, body)
	}
	fake.Advance(accountSessionTTL + time.Hour)
	if code, _ := doReq(t, url); code != http.StatusForbidden {
		t.Fatalf("到期后请求状态码 = %d，期望 403（会话失效）", code)
	}
}

// TestTLSCertificateUsesClock 自签证书日期取节点时间，过期后自动重签。
func TestTLSCertificateUsesClock(t *testing.T) {
	base := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := timesys.NewFakeClock(base)
	fake.MarkTrustedForTest()
	installFakeClock(t, fake)
	d, _ := newTestDaemon(t)
	t.Cleanup(d.stopTimeSystem)

	dir := filepath.Join(t.TempDir(), "tls")
	cfg, fp, err := d.setupTLS("auto", "", "", dir)
	if err != nil {
		t.Fatalf("初始化自签 TLS 失败: %v", err)
	}
	if cfg.Time == nil {
		t.Fatal("tls.Config.Time 未设置（证书校验不会使用节点时间）")
	}
	if d := cfg.Time().Sub(fake.Now()); d < -time.Second || d > time.Second {
		t.Fatalf("tls.Config.Time 偏差 = %v", d)
	}
	if fp == "" {
		t.Fatal("缺少证书指纹")
	}
	cert, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("取证书失败: %v", err)
	}
	notBefore, notAfter, err := vaultcrypto.CertificateValidity(cert)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if notBefore.After(base) {
		t.Fatalf("NotBefore = %s 晚于节点时间 %s（未按节点时间回拨）", notBefore, base)
	}
	if d := notAfter.Sub(base) - 10*365*24*time.Hour; d < -24*time.Hour || d > 24*time.Hour {
		t.Fatalf("NotAfter = %s，与节点时间相差 %v（期望约 10 年）", notAfter, notAfter.Sub(base))
	}

	// 跨过证书有效期：握手取证书应触发自动重签。
	fake.Set(time.Date(2046, 5, 6, 7, 8, 9, 0, time.UTC))
	fake.MarkTrustedForTest()
	cert2, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("证书过期后取证书失败（应自动重签）: %v", err)
	}
	_, notAfter2, err := vaultcrypto.CertificateValidity(cert2)
	if err != nil {
		t.Fatalf("解析新证书失败: %v", err)
	}
	if !notAfter2.After(time.Date(2055, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("重签后 NotAfter = %s，期望 2055 年之后", notAfter2)
	}
	if d.tlsFingerprintValue() == fp {
		t.Fatal("重签后证书指纹未更新")
	}
}

// TestManualCertExpiredRejected manual 模式下证书在节点时间过期应拒绝握手。
func TestManualCertExpiredRejected(t *testing.T) {
	base := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := timesys.NewFakeClock(base)
	fake.MarkTrustedForTest()
	installFakeClock(t, fake)
	dir := t.TempDir()
	if _, _, err := vaultcrypto.EnsureSelfSignedCert(dir); err != nil {
		t.Fatalf("生成测试证书失败: %v", err)
	}
	d, _ := newTestDaemon(t)
	t.Cleanup(d.stopTimeSystem)

	fake.Set(time.Date(2050, 1, 2, 3, 4, 5, 0, time.UTC))
	fake.MarkTrustedForTest()
	cfg, _, err := d.setupTLS("manual", filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "")
	if err != nil {
		t.Fatalf("加载 manual 证书失败: %v", err)
	}
	_, err = cfg.GetCertificate(nil)
	if err == nil {
		t.Fatal("节点时间已过期，manual 证书仍被接受")
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("错误消息未说明过期: %v", err)
	}
}

// TestTimeConfigMerge 配置文件 time 块 → 启动选项 → 时间系统配置。
func TestTimeConfigMerge(t *testing.T) {
	no := false
	interval := 120
	minAgreement := 1
	cfg := &Config{Time: &TimeConfig{
		Enabled:         &no,
		IntervalSeconds: &interval,
		MinAgreement:    &minAgreement,
		Servers: []TimeServerConfig{{
			Name: "internal", Address: "10.0.0.5:5319",
			PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Protocol: "legacy",
		}},
	}}
	o := baseOptions()
	o.applyConfig(cfg, map[string]bool{})
	if o.TimeEnabled {
		t.Fatal("配置 time.enabled=false 未生效")
	}
	if o.TimeIntervalSeconds != 120 || o.TimeMinAgreement != 1 {
		t.Fatalf("配置标量未生效: %+v", o)
	}
	if o.TimeServersConfig == "" {
		t.Fatal("配置服务器列表未编码进选项")
	}
	tc, err := buildTimeConfig(o, t.TempDir())
	if err != nil {
		t.Fatalf("构造时间系统配置失败: %v", err)
	}
	if tc.Enabled {
		t.Fatal("时间系统配置仍为启用")
	}
	if len(tc.Servers) != 1 || tc.Servers[0].Address != "10.0.0.5:5319" || tc.Servers[0].Profile != "legacy" {
		t.Fatalf("服务器配置错误: %+v", tc.Servers)
	}
	if tc.Interval != 120*time.Second {
		t.Fatalf("同步间隔 = %v", tc.Interval)
	}
}

// TestParseTimeServerList 命令行服务器列表解析。
func TestParseTimeServerList(t *testing.T) {
	list, err := parseTimeServerList("127.0.0.1:5319@AAAA,rt.example.com:2002@BBBB")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(list) != 2 || list[0].Address != "127.0.0.1:5319" || list[1].PublicKey != "BBBB" {
		t.Fatalf("解析结果错误: %+v", list)
	}
	if _, err := parseTimeServerList("bad-entry"); err == nil {
		t.Fatal("非法条目未被拒绝")
	}
}

// TestInitTimeSystemDisabled 初始化未启用的时间系统后状态可查、可停止。
func TestInitTimeSystemDisabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := timesys.Config{
		Enabled:   false,
		StateFile: filepath.Join(t.TempDir(), "timesys.json"),
		Logf:      nil,
	}
	if err := d.initTimeSystem(cfg, 0, false); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	if d.timeRestore != nil {
		t.Cleanup(d.timeRestore)
	}
	d.stopTimeSystem()
	st := d.timeStatus()
	if st.Enabled {
		t.Fatal("状态应为未启用")
	}
}

// TestTLSTrustGate 开启 tlsRequireTrusted 后，时间不可信时拒绝握手。
func TestTLSTrustGate(t *testing.T) {
	fake := timesys.NewFakeClock(time.Now()) // 未标记可信 = unlocked
	installFakeClock(t, fake)
	d, _ := newTestDaemon(t)
	t.Cleanup(d.stopTimeSystem)
	d.tlsRequireTrusted = true
	cfg, _, err := d.setupTLS("auto", "", "", filepath.Join(t.TempDir(), "tls"))
	if err != nil {
		t.Fatalf("初始化 TLS 失败: %v", err)
	}
	if _, err := cfg.GetCertificate(nil); err == nil {
		t.Fatal("时间不可信且 tlsRequireTrusted=true 时应拒绝握手")
	} else if !strings.Contains(err.Error(), "不可信") {
		t.Fatalf("错误消息未说明时间不可信: %v", err)
	}
}

// TestTLSConfigTimeUsesClock 手工构造的 TLS 配置必须把证书校验接到节点时间。
func TestTLSConfigTimeUsesClock(t *testing.T) {
	fake := timesys.NewFakeClock(time.Date(2044, 6, 7, 8, 9, 10, 0, time.UTC))
	installFakeClock(t, fake)
	cert := tls.Certificate{}
	cfg := vaultcrypto.ServerTLSConfig(cert)
	if cfg.Time == nil {
		t.Fatal("ServerTLSConfig 未设置 Time")
	}
	if d := cfg.Time().Sub(fake.Now()); d < -time.Second || d > time.Second {
		t.Fatalf("Time() 偏差 = %v", d)
	}
}
