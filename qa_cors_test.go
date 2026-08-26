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
func newCORSServer(d *Daemon) *httptest.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(d.auditMiddleware(corsMiddleware(d.vaultGate(limitAPIBody(mux)))))
}

// TestCORSPreflight CORS 预检：204 终结，来源与请求头回显，预检结果缓存 24 小时。
func TestCORSPreflight(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newCORSServer(d)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://127.0.0.1:3080")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type, X-Api-Key")
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
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Api-Key") {
		t.Errorf("Allow-Headers 应回显 X-Api-Key，实际 %q", got)
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
	srv := newCORSServer(d)
	defer srv.Close()

	// 正常跨源请求：200 + 回显来源 + 暴露 Content-Disposition
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/overview?apikey=test-key", nil)
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
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/overview?apikey=wrong-key", nil)
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
		t.Fatalf("错误 apikey 应 403，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3080" {
		t.Errorf("403 响应应回显来源，实际 %q", got)
	}

	// 无 Origin（curl / 服务间调用）：放行全部来源
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/overview?apikey=test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("无 Origin 时应放行 *，实际 %q", got)
	}
}

// TestCORSPreflightBypassesVaultGate 保险库锁定态：
// CORS 在门禁外层，预检必须 204（否则浏览器根本无法发起解锁请求），
// 门禁 403 响应同样带 CORS 头。
func TestCORSPreflightBypassesVaultGate(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.vault = &vaultState{enabled: true, initialized: true} // 已初始化但锁定
	srv := newCORSServer(d)
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
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/instance?apikey=test-key", nil)
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
	srv := newCORSServer(d)
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
