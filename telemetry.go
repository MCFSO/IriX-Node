// 零侵入遥测：
// - 业务 handler 零改动：HTTP 计数在中间件层完成（telemetryMiddleware 仅包装一层）；
// - Go 运行时指标直接读标准库 runtime/metrics（无需任何埋点）；
// - 暴露 GET /api/metrics（Prometheus 文本格式）与 /debug/pprof/*（标准库 pprof）。

package main

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// telemetryStats 全局遥测计数器（原子操作，无锁）。
type telemetryStats struct {
	requests      atomic.Int64 // 请求总数
	code2xx       atomic.Int64
	code3xx       atomic.Int64
	code4xx       atomic.Int64
	code5xx       atomic.Int64
	responseBytes atomic.Int64 // 响应总字节
	durSumUs      atomic.Int64 // 总耗时（微秒）
	durCount      atomic.Int64
	buckets       [12]atomic.Int64 // 延迟直方图桶（秒）
}

// tel 遥测计数器实例。
var tel = &telemetryStats{}

// latencyBucketBounds 延迟直方图桶上界（秒），末尾元素恒为 +Inf。
var latencyBucketBounds = [12]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 0}

// countingWriter 包装 ResponseWriter 统计状态码与写入字节。
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

// telemetryMiddleware 统计每个请求的状态码/响应字节/耗时（低侵入：仅包装一层）。
func telemetryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cw := &countingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		dur := time.Since(start)

		tel.requests.Add(1)
		switch {
		case cw.status >= 500:
			tel.code5xx.Add(1)
		case cw.status >= 400:
			tel.code4xx.Add(1)
		case cw.status >= 300:
			tel.code3xx.Add(1)
		default:
			tel.code2xx.Add(1)
		}
		tel.responseBytes.Add(cw.bytes)
		tel.durSumUs.Add(dur.Microseconds())
		tel.durCount.Add(1)
		sec := dur.Seconds()
		for i := 0; i < len(latencyBucketBounds)-1; i++ {
			if sec <= latencyBucketBounds[i] {
				tel.buckets[i].Add(1)
				return
			}
		}
		tel.buckets[len(latencyBucketBounds)-1].Add(1)
	})
}

// registerTelemetryRoutes 注册遥测与调试路由（认证与其余 API 一致）。
func (d *Daemon) registerTelemetryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/metrics", d.auth(d.handleMetrics))
	// pprof：零侵入调试遥测（CPU/内存剖析、goroutine、trace）
	mux.HandleFunc("GET /debug/pprof/", d.auth(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", d.auth(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", d.auth(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", d.auth(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", d.auth(pprof.Trace))
}

// handleMetrics 输出 Prometheus 文本格式遥测。
// GET /api/metrics
func (d *Daemon) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	// HTTP 请求计数
	b.WriteString("# TYPE irix_node_http_requests_total counter\n")
	fmt.Fprintf(&b, "irix_node_http_requests_total %d\n", tel.requests.Load())
	b.WriteString("# TYPE irix_node_http_responses_total counter\n")
	fmt.Fprintf(&b, "irix_node_http_responses_total{code=\"2xx\"} %d\n", tel.code2xx.Load())
	fmt.Fprintf(&b, "irix_node_http_responses_total{code=\"3xx\"} %d\n", tel.code3xx.Load())
	fmt.Fprintf(&b, "irix_node_http_responses_total{code=\"4xx\"} %d\n", tel.code4xx.Load())
	fmt.Fprintf(&b, "irix_node_http_responses_total{code=\"5xx\"} %d\n", tel.code5xx.Load())

	// 请求延迟直方图
	b.WriteString("# TYPE irix_node_http_request_duration_seconds histogram\n")
	fmt.Fprintf(&b, "irix_node_http_request_duration_seconds_sum %.6f\n",
		float64(tel.durSumUs.Load())/1e6)
	fmt.Fprintf(&b, "irix_node_http_request_duration_seconds_count %d\n", tel.durCount.Load())
	cum := int64(0)
	for i := 0; i < len(latencyBucketBounds); i++ {
		cum += tel.buckets[i].Load()
		le := "+Inf"
		if i < len(latencyBucketBounds)-1 {
			le = strconv.FormatFloat(latencyBucketBounds[i], 'f', -1, 64)
		}
		fmt.Fprintf(&b, "irix_node_http_request_duration_seconds_bucket{le=\"%s\"} %d\n", le, cum)
	}

	// 响应字节 / 进程存活时间
	fmt.Fprintf(&b, "irix_node_http_response_bytes_total %d\n", tel.responseBytes.Load())
	fmt.Fprintf(&b, "irix_node_uptime_seconds %.0f\n", time.Since(d.StartedAt).Seconds())

	// 实例统计（读已有状态，零侵入）
	d.mu.Lock()
	totalInstances := len(d.Instances)
	d.mu.Unlock()
	fmt.Fprintf(&b, "# TYPE irix_node_instances gauge\n")
	fmt.Fprintf(&b, "irix_node_instances{state=\"total\"} %d\n", totalInstances)
	fmt.Fprintf(&b, "irix_node_instances{state=\"running\"} %d\n", d.CountRunning())

	// Go 运行时指标（runtime/metrics 全量）
	writeRuntimeMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// promName 把 runtime/metrics 的 key 转为 Prometheus 指标名。
// "/gc/heap/allocs:bytes" → "go_gc_heap_allocs_bytes"
func promName(key string) string {
	s := strings.TrimPrefix(key, "/")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return "go_" + s
}

// writeRuntimeMetrics 输出全部 Go 运行时指标（gauge 与 histogram 两类）。
func writeRuntimeMetrics(b *strings.Builder) {
	descs := metrics.All()
	samples := make([]metrics.Sample, len(descs))
	for i := range samples {
		samples[i].Name = descs[i].Name
	}
	metrics.Read(samples)
	for i, s := range samples {
		name := promName(descs[i].Name)
		switch s.Value.Kind() {
		case metrics.KindUint64:
			fmt.Fprintf(b, "# TYPE %s gauge\n%s %d\n", name, name, s.Value.Uint64())
		case metrics.KindFloat64:
			fmt.Fprintf(b, "# TYPE %s gauge\n%s %g\n", name, name, s.Value.Float64())
		case metrics.KindFloat64Histogram:
			h := s.Value.Float64Histogram()
			// 防御式解析：不同 Go 版本 Counts 与 Buckets 的长度关系存在差异
			//（文档语义为 Counts = Buckets + 1，但实测存在相等/短一的情况）。
			// 桶边界按 min(len) 对齐，+Inf 计数取累计值，保证不越界。
			fmt.Fprintf(b, "# TYPE %s histogram\n", name)
			n := len(h.Buckets)
			if len(h.Counts) < n {
				n = len(h.Counts)
			}
			cum := uint64(0)
			for bi := 0; bi < n; bi++ {
				cum += h.Counts[bi]
				fmt.Fprintf(b, "%s_bucket{le=\"%g\"} %d\n", name, h.Buckets[bi], cum)
			}
			if len(h.Counts) > len(h.Buckets) {
				cum += h.Counts[len(h.Counts)-1]
			}
			fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, cum)
			fmt.Fprintf(b, "%s_sum %g\n", name, 0.0) // runtime/metrics 直方图不含 sum，置 0 占位
			fmt.Fprintf(b, "%s_count %d\n", name, cum)
		}
	}
}
