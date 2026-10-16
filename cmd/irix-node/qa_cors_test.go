// CORS 测试：跨源响应头、预检终结、错误响应同样带 CORS 头、
// 保险库锁定态下预检不被门禁拦截（浏览器跨源调用的完整链路）。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newCORSServer 用与生产一致的中间件链启动测试服务器
// （audit → cors → vaultGate → limitAPIBody → 路由）。
func newCORSServer(t testing.TB, d *Daemon) *httptest.Server {
	t.Helper()
	ensureTestAuth(t, d)
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(d.auditMiddleware(d.corsMiddleware(d.vaultGate(limitAPIBody(mux)))))
}

// TestCORSPreflight CORS 预检：204 终结，来源与请求头回显，预检结果缓存 24 小时。
func TestCORSPreflight(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(t, d)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:3080")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type, X-Auth-Token")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:3080" {
		t.Errorf("Allow-Origin 应回显来源，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Allow-Methods 应包含 POST，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Auth-Token") {
		t.Errorf("Allow-Headers 应回显 X-Auth-Token，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "86400" {
		t.Errorf("Max-Age 错误: %q", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary 应包含 Origin，实际 %q", got)
	}
}

// TestCORSActualAndError 实际请求与错误响应：
// 200/403 均带 CORS 头（错误不带头前端读不到失败原因）；无 Origin 时放行 *。
func TestCORSActualAndError(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(t, d)
	defer srv.Close()

	// 正常跨源请求：200 + 回显来源 + 暴露 Content-Disposition
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/overview?token=test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:3080")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3080" {
		t.Errorf("Allow-Origin 应回显来源，实际 %q", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary 应包含 Origin，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Disposition") {
		t.Errorf("Expose-Headers 应包含 Content-Disposition，实际 %q", got)
	}

	// 认证失败（403）：错误响应同样必须带 CORS 头
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/overview?token=wrong-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:3080")
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("错误 token 应 403，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3080" {
		t.Errorf("403 响应应回显来源，实际 %q", got)
	}

	// 无 Origin（curl / 服务间调用）：不下发任何 CORS 头
	// （CORS 头只对浏览器跨源有意义；对无 Origin 的请求下发 * 是无意义的
	// 过度声明，红队审计 中-1 修复一并去掉）。
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/overview?token=test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("无 Origin 时不应下发 Allow-Origin，实际 %q", got)
	}
}

// TestCORSPreflightBypassesVaultGate 保险库锁定态：
// CORS 在门禁外层，预检必须 204（否则浏览器根本无法发起解锁请求），
// 门禁 403 响应同样带 CORS 头。
func TestCORSPreflightBypassesVaultGate(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.vault = &vaultState{enabled: true, initialized: true} // 已初始化但锁定
	srv := newCORSServer(t, d)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:3080")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("锁定态预检应 204（不被门禁拦截），实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:3080" {
		t.Errorf("锁定态预检应回显来源，实际 %q", got)
	}

	// 锁定态实际请求：403 但带 CORS 头
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/instance?token=test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:3080")
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("锁定态数据面应 403，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:3080" {
		t.Errorf("锁定态 403 应回显来源，实际 %q", got)
	}
}

// TestCORSOptionsWithoutPreflight 无 Access-Control-Request-Method 的普通
// OPTIONS 不属于 CORS 预检：透传到路由，由 ServeMux 返回 405（与原行为一致）。
func TestCORSOptionsWithoutPreflight(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(t, d)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("普通 OPTIONS 应透传路由返回 405，实际 %d", resp.StatusCode)
	}
}

// TestCORSUnknownOriginRejected 回归 [中-1]：未在放行范围内的来源
// 不得拿到任何 CORS 头（修复前任意来源都会被回显，且带 Allow-Credentials）。
func TestCORSUnknownOriginRejected(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(t, d)
	defer srv.Close()

	// 实际请求：外部来源拿不到 Allow-Origin / Allow-Credentials
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/overview?token=test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("未放行来源不应回显 Allow-Origin，实际 %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("未放行来源不应下发 Allow-Credentials，实际 %q", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("无论是否放行都应带 Vary: Origin（避免缓存串号），实际 %q", got)
	}

	// 预检：204 终结但不给出任何 Allow-* 头（浏览器判定预检失败）
	pre, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	pre.Header.Set("Origin", "https://evil.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "X-Auth-Token, X-Evil")
	resp2, err := testClient.Do(pre)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d", resp2.StatusCode)
	}
	for _, h := range []string{
		"Access-Control-Allow-Origin", "Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers", "Access-Control-Allow-Credentials",
	} {
		if got := resp2.Header.Get(h); got != "" {
			t.Errorf("未放行来源的预检不应下发 %s，实际 %q", h, got)
		}
	}
	t.Logf("[验证] 未放行来源不下发任何 CORS 头（实际/预检）")
}

// TestCORSAllowHeadersFixed 放行来源的 Allow-Headers 使用固定清单，
// 不回显 Access-Control-Request-Headers（避免把任意头声明为服务端接受）。
func TestCORSAllowHeadersFixed(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(t, d)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:3080")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-Evil-Header")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	resp.Body.Close()
	got := resp.Header.Get("Access-Control-Allow-Headers")
	if strings.Contains(got, "X-Evil-Header") {
		t.Errorf("不应回显客户端声明的任意请求头，实际 %q", got)
	}
	if !strings.Contains(got, "X-Auth-Token") || !strings.Contains(got, "X-Vault-Token") {
		t.Errorf("Allow-Headers 应包含节点实际读取的头，实际 %q", got)
	}
	t.Logf("[验证] Allow-Headers 为固定清单: %s", got)
}

// TestCORSOriginAllowList 白名单判定：显式配置、同主机名、环回放行；
// 其他来源与 "null" 拒绝。
func TestCORSOriginAllowList(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.setCorsOrigins("https://panel.example.com, http://192.168.1.9:23333/")

	cases := []struct {
		origin string
		host   string
		want   bool
		desc   string
	}{
		{"https://panel.example.com", "node.local:12346", true, "显式配置的来源"},
		{"http://192.168.1.9:23333", "node.local:12346", true, "显式配置（容忍结尾斜杠/大小写）"},
		{"HTTPS://PANEL.EXAMPLE.COM", "node.local:12346", true, "显式配置（大小写不敏感）"},
		{"http://node.local:23333", "node.local:12346", true, "与请求 Host 同主机名（端口不限）"},
		{"https://evil.example", "node.local:12346", false, "未配置且主机名不同"},
		{"null", "node.local:12346", false, "沙箱 iframe / file:// 的 null 来源"},
		{"", "node.local:12346", false, "无 Origin"},
		{"http://127.0.0.1:3080", "node.local:12346", true, "IPv4 环回"},
		{"http://localhost:3080", "node.local:12346", true, "localhost"},
		{"http://[::1]:3080", "node.local:12346", true, "IPv6 环回"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://"+c.host+"/api/overview", nil)
		req.Host = c.host
		if got := d.corsOriginAllowed(req, c.origin); got != c.want {
			t.Errorf("corsOriginAllowed(%q, host=%q) = %v，期望 %v（%s）", c.origin, c.host, got, c.want, c.desc)
		}
	}
	t.Logf("[验证] 白名单判定 %d 例全部符合预期", len(cases))
}

// TestSetCorsOriginsParsing -cors-origins 解析：逗号分隔、去空白、去尾斜杠、
// 大小写归一。
func TestSetCorsOriginsParsing(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.setCorsOrigins(" https://A.example.com , http://b.example.com:8080/ ,,")
	for _, want := range []string{"https://a.example.com", "http://b.example.com:8080"} {
		if !d.corsOrigins[want] {
			t.Errorf("应解析出 %q，实际 %v", want, d.corsOrigins)
		}
	}
	if len(d.corsOrigins) != 2 {
		t.Errorf("空项应被忽略，实际 %d 项: %v", len(d.corsOrigins), d.corsOrigins)
	}
	// 空配置 = 仅环回 + 同主机名
	d.setCorsOrigins("")
	if len(d.corsOrigins) != 0 {
		t.Errorf("空配置应得到空白名单，实际 %v", d.corsOrigins)
	}
	t.Logf("[验证] corsOrigins 解析正确")
}
