// 零侵入遥测测试：/api/metrics 计数器与运行时指标、/debug/pprof 端点。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTelemetryServer 启动带遥测中间件的 httptest 服务器（与 main.go 的包装一致）。
func newTelemetryServer(d *Daemon) *httptest.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(telemetryMiddleware(mux))
}

// TestMetricsEndpoint 遥测端点输出 Prometheus 文本格式的计数器与运行时指标。
func TestMetricsEndpoint(t *testing.T) {
	d, _ := newTestDaemon(t)
	inst := sampleInst(1, t.TempDir())
	d.Instances = append(d.Instances, inst)
	srv := newTelemetryServer(d)
	defer srv.Close()

	// 发起几种状态的请求（计数由中间件完成，业务零改动）
	doReq(t, srv.URL+"/api/overview?apikey=test-key")  // 200
	doReq(t, srv.URL+"/api/instance?apikey=test-key")  // 400（缺 uuid）
	doReq(t, srv.URL+"/api/not-exist?apikey=test-key") // 404（路由不存在）
	code, body := doReq(t, srv.URL+"/api/metrics?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("metrics 失败: %d %s", code, body)
	}
	text := string(body)
	for _, want := range []string{
		"irix_node_http_requests_total",
		"irix_node_http_responses_total{code=\"2xx\"}",
		"irix_node_http_responses_total{code=\"4xx\"}",
		"irix_node_http_request_duration_seconds_bucket{le=\"+Inf\"}",
		"irix_node_http_response_bytes_total",
		"irix_node_uptime_seconds",
		"irix_node_instances{state=\"total\"} 1",
		"irix_node_instances{state=\"running\"} 0",
		"go_", // 运行时指标前缀
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics 缺少 %q:\n%s", want, text)
		}
	}
	// 请求总数至少 = 3 个业务请求（metrics 自身在响应写出后才计数，不计入本响应）
	if !strings.Contains(text, "irix_node_http_requests_total 3") {
		t.Fatalf("请求计数不符合预期（应至少 3）:\n%s", text)
	}
}

// TestPprofEndpoints 零侵入 pprof 调试端点可访问（带认证）。
func TestPprofEndpoints(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/debug/pprof/?apikey=test-key")
	if code != http.StatusOK || !strings.Contains(string(body), "goroutine") {
		t.Fatalf("pprof 首页失败: %d %s", code, body)
	}
	code, body = doReq(t, srv.URL+"/debug/pprof/goroutine?apikey=test-key&debug=1")
	if code != http.StatusOK || !strings.Contains(string(body), "goroutine profile") {
		t.Fatalf("pprof goroutine 失败: %d %s", code, body)
	}
	// 无认证应 403
	code, _ = doReq(t, srv.URL+"/debug/pprof/")
	if code != http.StatusOK {
		t.Fatalf("无认证请求 HTTP 应 200（业务状态在 body）: %d", code)
	}
}

// TestCountingWriter 状态码与字节计数包装。
func TestCountingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: rec}
	_, _ = cw.Write([]byte("hello"))
	if cw.status != http.StatusOK {
		t.Fatalf("默认状态应为 200: %d", cw.status)
	}
	if cw.bytes != 5 {
		t.Fatalf("字节数应为 5: %d", cw.bytes)
	}
	cw2 := &countingWriter{ResponseWriter: httptest.NewRecorder()}
	cw2.WriteHeader(http.StatusNotFound)
	if cw2.status != http.StatusNotFound {
		t.Fatalf("状态码应为 404: %d", cw2.status)
	}
}

// TestPromName 运行时指标名转换。
func TestPromName(t *testing.T) {
	cases := map[string]string{
		"/gc/heap/allocs:bytes":                   "go_gc_heap_allocs_bytes",
		"/cpu/classes/gc/mark/assist:cpu-seconds": "go_cpu_classes_gc_mark_assist_cpu_seconds",
		"/sched/goroutines:goroutines":            "go_sched_goroutines_goroutines",
	}
	for in, want := range cases {
		if got := promName(in); got != want {
			t.Fatalf("promName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
