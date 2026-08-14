// 零侵入遥测（OpenTelemetry 生态兼容，零第三方依赖）：
// - Prometheus 文本格式：GET /api/metrics（OTel Collector 的 prometheus receiver 可直接抓取）
// - OTLP/JSON：GET /api/metrics/otlp（ExportMetricsServiceRequest 的 proto JSON 编码，
//   可喂给 OTel Collector 的 otlp receiver / otlpjsonfile 等消费方）
// - OTLP/JSON traces：GET /api/metrics/traces（ExportTraceServiceRequest，
//   最近请求的 server span 环形缓冲）
// - W3C TraceContext：解析入站 traceparent 头、无则生成新 trace，
//   响应头回传 traceparent 供上游/下游关联（零依赖的 trace 传播）
//
// 业务 handler 零改动：计数与 span 记录都在 telemetryMiddleware 中间件层完成。

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/pprof"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
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

// spanRecord 单次请求的 server span（对齐 OTel span 的核心字段）。
type spanRecord struct {
	TraceID      string // 32 位十六进制
	SpanID       string // 16 位十六进制
	ParentSpanID string // 空 = 根 span
	Name         string // 路由模式（r.Pattern）或路径
	Method       string
	Path         string
	StatusCode   int
	StartTime    time.Time
	DurationMs   float64
	Bytes        int64
}

// spanRing server span 环形缓冲（最近 1024 条）。
type spanRing struct {
	mu    sync.Mutex
	spans []spanRecord
	next  int
}

// spanBuf 全局 span 缓冲。
var spanBuf = &spanRing{spans: make([]spanRecord, 1024)}

func (r *spanRing) add(s spanRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans[r.next] = s
	r.next = (r.next + 1) % len(r.spans)
}

// list 按时间顺序返回已记录的 span（最旧在前）。
func (r *spanRing) list() []spanRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]spanRecord, 0, len(r.spans))
	for i := 0; i < len(r.spans); i++ {
		idx := (r.next + i) % len(r.spans)
		if r.spans[idx].SpanID != "" {
			out = append(out, r.spans[idx])
		}
	}
	return out
}

// randomHex 生成 n 字节的随机十六进制串（crypto/rand；失败回退时间戳）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < n; i++ {
			b[i] = byte(now >> (8 * (i % 8)))
		}
	}
	return hex.EncodeToString(b)
}

// parseTraceparent 解析 W3C TraceContext traceparent 头（00-<32hex>-<16hex>-<2hex>）。
// 非法或缺失时生成全新 trace（span 视为根）。
func parseTraceparent(header string) (traceID, parentSpanID string) {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 {
		if _, err := hex.DecodeString(parts[1]); err == nil {
			if _, err := hex.DecodeString(parts[2]); err == nil {
				return parts[1], parts[2]
			}
		}
	}
	return randomHex(16), ""
}

// telemetryMiddleware 统计每个请求的状态码/响应字节/耗时，并记录 server span。
// W3C TraceContext：解析入站 traceparent；响应头回传本 span 的 traceparent。
func telemetryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID, parentSpanID := parseTraceparent(r.Header.Get("traceparent"))
		spanID := randomHex(8)
		w.Header().Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, spanID))

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
				goto done
			}
		}
		tel.buckets[len(latencyBucketBounds)-1].Add(1)
	done:

		name := r.Pattern
		if name == "" {
			name = r.URL.Path
		}
		spanBuf.add(spanRecord{
			TraceID:      traceID,
			SpanID:       spanID,
			ParentSpanID: parentSpanID,
			Name:         name,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   cw.status,
			StartTime:    start,
			DurationMs:   float64(dur.Microseconds()) / 1000,
			Bytes:        cw.bytes,
		})
	})
}

// registerTelemetryRoutes 注册遥测与调试路由（认证与其余 API 一致）。
func (d *Daemon) registerTelemetryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/metrics", d.auth(d.handleMetrics))
	mux.HandleFunc("GET /api/metrics/otlp", d.auth(d.handleMetricsOTLP))
	mux.HandleFunc("GET /api/metrics/traces", d.auth(d.handleTraces))
	// pprof：零侵入调试遥测（CPU/内存剖析、goroutine、trace）
	mux.HandleFunc("GET /debug/pprof/", d.auth(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", d.auth(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", d.auth(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", d.auth(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", d.auth(pprof.Trace))
}

// handleMetrics 输出 Prometheus 文本格式遥测（含 OTel 语义约定命名）。
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

	// 请求延迟直方图（OTel 语义约定命名：http.server.duration → http_server_request_duration_seconds）
	b.WriteString("# TYPE http_server_request_duration_seconds histogram\n")
	b.WriteString("# UNIT http_server_request_duration_seconds seconds\n")
	fmt.Fprintf(&b, "http_server_request_duration_seconds_sum %.6f\n",
		float64(tel.durSumUs.Load())/1e6)
	fmt.Fprintf(&b, "http_server_request_duration_seconds_count %d\n", tel.durCount.Load())
	cum := int64(0)
	for i := 0; i < len(latencyBucketBounds); i++ {
		cum += tel.buckets[i].Load()
		le := "+Inf"
		if i < len(latencyBucketBounds)-1 {
			le = strconv.FormatFloat(latencyBucketBounds[i], 'f', -1, 64)
		}
		fmt.Fprintf(&b, "http_server_request_duration_seconds_bucket{le=\"%s\"} %d\n", le, cum)
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

// sanitizeFloat 把 NaN/Inf 归一为 0（部分平台的 CPU 类 runtime 指标未支持时为 NaN，
// JSON/Prometheus 文本均不允许 NaN/Inf，会导致整个响应编码失败）。
func sanitizeFloat(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
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
			fmt.Fprintf(b, "# TYPE %s gauge\n%s %g\n", name, name, sanitizeFloat(s.Value.Float64()))
		case metrics.KindFloat64Histogram:
			h := s.Value.Float64Histogram()
			// 防御式解析：不同 Go 版本 Counts 与 Buckets 的长度关系存在差异。
			fmt.Fprintf(b, "# TYPE %s histogram\n", name)
			n := len(h.Buckets)
			if len(h.Counts) < n {
				n = len(h.Counts)
			}
			cum := uint64(0)
			for bi := 0; bi < n; bi++ {
				cum += h.Counts[bi]
				fmt.Fprintf(b, "%s_bucket{le=\"%g\"} %d\n", name, sanitizeFloat(h.Buckets[bi]), cum)
			}
			if len(h.Counts) > len(h.Buckets) {
				cum += h.Counts[len(h.Counts)-1]
			}
			fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, cum)
			fmt.Fprintf(b, "%s_sum %g\n", name, 0.0)
			fmt.Fprintf(b, "%s_count %d\n", name, cum)
		}
	}
}

// otelResource 构造 OTLP 的 Resource 对象（proto JSON 编码）。
func otelResource() map[string]any {
	return map[string]any{
		"attributes": []map[string]any{
			{"key": "service.name", "value": map[string]any{"stringValue": "irix-node"}},
			{"key": "service.version", "value": map[string]any{"stringValue": Version}},
			{"key": "telemetry.sdk.name", "value": map[string]any{"stringValue": "irix-node"}},
			{"key": "telemetry.sdk.language", "value": map[string]any{"stringValue": "go"}},
		},
	}
}

// otelScope 构造 OTLP 的 InstrumentationScope 对象。
func otelScope() map[string]any {
	return map[string]any{"name": "irix.node", "version": Version}
}

// handleMetricsOTLP 输出 OTLP/JSON 编码的指标（ExportMetricsServiceRequest）。
// GET /api/metrics/otlp
// proto JSON 约定：int64/uint64 为十进制字符串，枚举为字符串，bytes 为 base64。
func (d *Daemon) handleMetricsOTLP(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UnixNano()
	startNs := strconv.FormatInt(d.StartedAt.UnixNano(), 10)
	timeNs := strconv.FormatInt(now, 10)

	// 延迟直方图桶：explicitBounds（不含 +Inf）+ bucketCounts（长度 = bounds+1）
	bounds := make([]float64, 0, len(latencyBucketBounds)-1)
	bounds = append(bounds, latencyBucketBounds[:len(latencyBucketBounds)-1]...)
	bucketCounts := make([]string, len(latencyBucketBounds))
	for i := range tel.buckets {
		bucketCounts[i] = strconv.FormatInt(tel.buckets[i].Load(), 10)
	}

	d.mu.Lock()
	totalInstances := len(d.Instances)
	d.mu.Unlock()

	metricsList := []map[string]any{
		// http.server.duration 直方图（秒）
		{
			"name": "http.server.duration", "description": "HTTP 请求耗时", "unit": "s",
			"histogram": map[string]any{
				"dataPoints": []map[string]any{{
					"startTimeUnixNano": startNs,
					"timeUnixNano":      timeNs,
					"count":             strconv.FormatInt(tel.durCount.Load(), 10),
					"sum":               float64(tel.durSumUs.Load()) / 1e6,
					"bucketCounts":      bucketCounts,
					"explicitBounds":    bounds,
				}},
				"aggregationTemporality": "AGGREGATION_TEMPORALITY_CUMULATIVE",
			},
		},
		// http.server.requests 计数器
		{
			"name": "http.server.requests", "description": "HTTP 请求总数", "unit": "1",
			"sum": map[string]any{
				"dataPoints": []map[string]any{{
					"startTimeUnixNano": startNs,
					"timeUnixNano":      timeNs,
					"asInt":             strconv.FormatInt(tel.requests.Load(), 10),
				}},
				"aggregationTemporality": "AGGREGATION_TEMPORALITY_CUMULATIVE",
				"isMonotonic":            true,
			},
		},
		// 运行时间 / 实例数 gauge
		{
			"name": "irix.node.uptime", "unit": "s",
			"gauge": map[string]any{
				"dataPoints": []map[string]any{{
					"startTimeUnixNano": startNs, "timeUnixNano": timeNs,
					"asDouble": time.Since(d.StartedAt).Seconds(),
				}},
			},
		},
		{
			"name": "irix.node.instances", "unit": "1",
			"gauge": map[string]any{
				"dataPoints": []map[string]any{
					{"attributes": []map[string]any{{"key": "state", "value": map[string]any{"stringValue": "total"}}},
						"startTimeUnixNano": startNs, "timeUnixNano": timeNs,
						"asInt": strconv.Itoa(totalInstances)},
					{"attributes": []map[string]any{{"key": "state", "value": map[string]any{"stringValue": "running"}}},
						"startTimeUnixNano": startNs, "timeUnixNano": timeNs,
						"asInt": strconv.Itoa(d.CountRunning())},
				},
			},
		},
	}
	// Go 运行时指标（runtime/metrics 全量 → OTLP gauge/histogram）
	metricsList = append(metricsList, runtimeMetricsOTLP(timeNs)...)

	payload := map[string]any{
		"resourceMetrics": []map[string]any{{
			"resource": otelResource(),
			"scopeMetrics": []map[string]any{{
				"scope":   otelScope(),
				"metrics": metricsList,
			}},
		}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

// runtimeMetricsOTLP 把 runtime/metrics 全量转为 OTLP 指标（proto JSON 编码）。
func runtimeMetricsOTLP(timeNs string) []map[string]any {
	descs := metrics.All()
	samples := make([]metrics.Sample, len(descs))
	for i := range samples {
		samples[i].Name = descs[i].Name
	}
	metrics.Read(samples)
	out := make([]map[string]any, 0, len(samples))
	for i, s := range samples {
		name := promName(descs[i].Name)
		switch s.Value.Kind() {
		case metrics.KindUint64:
			out = append(out, map[string]any{
				"name": name, "unit": "1",
				"gauge": map[string]any{
					"dataPoints": []map[string]any{{
						"timeUnixNano": timeNs,
						"asInt":        strconv.FormatUint(s.Value.Uint64(), 10),
					}},
				},
			})
		case metrics.KindFloat64:
			out = append(out, map[string]any{
				"name": name, "unit": "1",
				"gauge": map[string]any{
					"dataPoints": []map[string]any{{
						"timeUnixNano": timeNs,
						"asDouble":     sanitizeFloat(s.Value.Float64()),
					}},
				},
			})
		case metrics.KindFloat64Histogram:
			h := s.Value.Float64Histogram()
			n := len(h.Buckets)
			if len(h.Counts) < n {
				n = len(h.Counts)
			}
			counts := make([]string, n+1)
			for bi := 0; bi < n; bi++ {
				counts[bi] = strconv.FormatUint(h.Counts[bi], 10)
			}
			if len(h.Counts) > len(h.Buckets) {
				counts[n] = strconv.FormatUint(h.Counts[len(h.Counts)-1], 10)
			} else {
				counts[n] = "0"
			}
			bounds := make([]float64, n)
			for bi := 0; bi < n; bi++ {
				bounds[bi] = sanitizeFloat(h.Buckets[bi])
			}
			out = append(out, map[string]any{
				"name": name, "unit": "1",
				"histogram": map[string]any{
					"dataPoints": []map[string]any{{
						"timeUnixNano":   timeNs,
						"count":          strconv.FormatUint(countOf(h.Counts, n), 10),
						"sum":            0.0,
						"bucketCounts":   counts,
						"explicitBounds": bounds,
					}},
					"aggregationTemporality": "AGGREGATION_TEMPORALITY_CUMULATIVE",
				},
			})
		}
	}
	return out
}

// countOf 汇总直方图计数（防御式：与 Counts/Buckets 对齐逻辑一致）。
func countOf(counts []uint64, n int) uint64 {
	var sum uint64
	limit := n
	if len(counts) < limit {
		limit = len(counts)
	}
	for i := 0; i < limit; i++ {
		sum += counts[i]
	}
	if len(counts) > n {
		sum += counts[len(counts)-1]
	}
	return sum
}

// handleTraces 输出 OTLP/JSON 编码的 server span（ExportTraceServiceRequest）。
// GET /api/metrics/traces
// traceId/spanId 为 bytes 字段 → base64 编码。
func (d *Daemon) handleTraces(w http.ResponseWriter, r *http.Request) {
	spans := make([]map[string]any, 0, len(spanBuf.list()))
	for _, s := range spanBuf.list() {
		traceBytes, _ := hex.DecodeString(s.TraceID)
		spanBytes, _ := hex.DecodeString(s.SpanID)
		parentBytes, _ := hex.DecodeString(s.ParentSpanID)
		spans = append(spans, map[string]any{
			"traceId":           base64Std(traceBytes),
			"spanId":            base64Std(spanBytes),
			"parentSpanId":      base64Std(parentBytes),
			"name":              s.Name,
			"kind":              "SPAN_KIND_SERVER",
			"startTimeUnixNano": strconv.FormatInt(s.StartTime.UnixNano(), 10),
			"endTimeUnixNano": strconv.FormatInt(
				s.StartTime.Add(time.Duration(s.DurationMs*1e6)).UnixNano(), 10),
			"attributes": []map[string]any{
				{"key": "http.request.method", "value": map[string]any{"stringValue": s.Method}},
				{"key": "url.path", "value": map[string]any{"stringValue": s.Path}},
				{"key": "http.response.status_code", "value": map[string]any{"intValue": strconv.Itoa(s.StatusCode)}},
				{"key": "http.response.body.size", "value": map[string]any{"intValue": strconv.FormatInt(s.Bytes, 10)}},
			},
			"status": map[string]any{"code": "STATUS_CODE_UNSET"},
		})
	}
	payload := map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": otelResource(),
			"scopeSpans": []map[string]any{{
				"scope": otelScope(),
				"spans": spans,
			}},
		}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

// base64Std base64 标准编码（空输入返回空串）。
func base64Std(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
