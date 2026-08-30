// 性能基准（benchmark）与扩展性测试：万级实例、大目录、大日志。
// 运行：go test -bench . -benchmem -run '^$' -v

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 高层基准：HTTP 处理器
// ---------------------------------------------------------------------------

// benchServer 构造带 n 个实例的 httptest 服务器。
func benchServer(b *testing.B, n int) (*Daemon, *httptest.Server) {
	b.Helper()
	dir := b.TempDir()
	d := NewDaemon(dir, "test-key")
	for i := 0; i < n; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}
	srv := newTestServer(d)
	b.Cleanup(srv.Close)
	return d, srv
}

// BenchmarkHTTPOverview 概览接口吞吐（单连接压）。
func BenchmarkHTTPOverview(b *testing.B) {
	_, srv := benchServer(b, 100)
	url := srv.URL + "/api/overview?apikey=test-key"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkHTTPInstanceList 实例列表接口吞吐（100 实例）。
func BenchmarkHTTPInstanceList(b *testing.B) {
	_, srv := benchServer(b, 100)
	url := srv.URL + "/api/service/remote_service_instances?apikey=test-key&page=1&page_size=100"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkHTTPInstanceDetail 实例详情接口吞吐。
func BenchmarkHTTPInstanceDetail(b *testing.B) {
	d, srv := benchServer(b, 100)
	url := srv.URL + "/api/instance?apikey=test-key&uuid=" + d.Instances[0].InstanceUuid
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkHTTPAuth 认证开销（配对码 SHA-256 + 恒定时间比较路径）。
func BenchmarkHTTPAuth(b *testing.B) {
	d, _ := benchServer(b, 0)
	d.APIKey = ""
	d.PairingHash = pairingHash("12345678901234567890")
	_ = newTestServer(d)
	req := &http.Request{Header: http.Header{}, URL: &url.URL{}}
	q := req.URL.Query()
	q.Set("apikey", "12345678901234567890")
	req.URL.RawQuery = q.Encode()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !d.authOK(req) {
			b.Fatal("认证失败")
		}
	}
}

// ---------------------------------------------------------------------------
// 中层基准：数据模型
// ---------------------------------------------------------------------------

// BenchmarkFind 实例查找（O(n) 线性扫描！）。
func BenchmarkFind(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			d := NewDaemon(dir, "k")
			for i := 0; i < n; i++ {
				d.Instances = append(d.Instances, sampleInst(i, dir))
			}
			target := d.Instances[n/2].InstanceUuid
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if d.Find(target) == nil {
					b.Fatal("未找到")
				}
			}
		})
	}
}

// BenchmarkList 实例列表（全量 Detail）。
func BenchmarkList(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			d := NewDaemon(dir, "k")
			for i := 0; i < n; i++ {
				d.Instances = append(d.Instances, sampleInst(i, dir))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = d.List()
			}
		})
	}
}

// BenchmarkSave 全量持久化（每次操作都写整个文件）。
func BenchmarkSave(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			d := NewDaemon(dir, "k")
			for i := 0; i < n; i++ {
				d.Instances = append(d.Instances, sampleInst(i, dir))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := d.Save(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLoad 启动加载（读取 + 解析 instances.json）。
func BenchmarkLoad(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			d := NewDaemon(dir, "k")
			for i := 0; i < n; i++ {
				d.Instances = append(d.Instances, sampleInst(i, dir))
			}
			if err := d.Save(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d2 := NewDaemon(dir, "k")
				if err := d2.Load(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLogTail 环形日志缓冲读取（64KB 缓冲）。
func BenchmarkLogTail(b *testing.B) {
	buf := NewLogBuffer(2 * 1024 * 1024)
	line := []byte("2026-01-01 12:00:00 [INFO] server started successfully\n")
	for i := 0; i < 10000; i++ {
		buf.Write(line)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buf.Tail(256)
	}
}

// BenchmarkLogWrite 环形日志缓冲写入。
func BenchmarkLogWrite(b *testing.B) {
	buf := NewLogBuffer(2 * 1024 * 1024)
	line := []byte("2026-01-01 12:00:00 [INFO] tick\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = buf.Write(line)
	}
}

// BenchmarkJSONEncodeResponse 单个响应 JSON 编码开销。
func BenchmarkJSONEncodeResponse(b *testing.B) {
	payload := map[string]any{
		"status": 200, "time": int64(1700000000000),
		"data": map[string]any{"maxPage": 1, "page": 0, "total": 100, "data": []any{}},
	}
	var sb strings.Builder
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sb.Reset()
		_ = json.NewEncoder(&sb).Encode(payload)
	}
}

// ---------------------------------------------------------------------------
// 低层基准：工具函数
// ---------------------------------------------------------------------------

// BenchmarkFileWriteHandler 只测服务端写文件 handler 的分配（排除客户端构造开销）。
// 用于验证流式解码相对 ReadAll+map 的内存收益。
func BenchmarkFileWriteHandler(b *testing.B) {
	for _, sizeKB := range []int{64, 1024, 8192} {
		b.Run(fmt.Sprintf("payload=%dKB", sizeKB), func(b *testing.B) {
			dir := b.TempDir()
			d := NewDaemon(dir, "test-key")
			inst := NewInstance("bench-uuid", InstanceConfig{Nickname: "bench", Cwd: dir})
			d.Instances = append(d.Instances, inst)

			payload, err := json.Marshal(map[string]any{
				"target": "bench.bin",
				"text":   strings.Repeat("A", sizeKB*1024),
			})
			if err != nil {
				b.Fatal(err)
			}
			url := "/api/files/?apikey=test-key&uuid=" + inst.InstanceUuid

			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
				rec := httptest.NewRecorder()
				d.handleFileReadWrite(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("HTTP %d", rec.Code)
				}
			}
		})
	}
}

// BenchmarkSplitCommand 命令行解析。
func BenchmarkSplitCommand(b *testing.B) {
	cmd := `java -Xmx2G -jar "server.jar" --nogui --named 'arg with space'`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SplitCommand(cmd)
	}
}

// BenchmarkNormalizePath 路径规范化（每次请求都会执行）。
func BenchmarkNormalizePath(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NormalizePath(`C:\server`, "plugins/config.yml"); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// 扩展性：大规模数据下的行为正确性
// ---------------------------------------------------------------------------

// TestLoadTenThousandInstances 一万实例：加载、查找、列表、分页正确性。
func TestLoadTenThousandInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	dir := t.TempDir()
	d := NewDaemon(dir, "k")
	const n = 10000
	for i := 0; i < n; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}
	saveStart := time.Now()
	if err := d.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	t.Logf("[扩展] 内存 %d 个实例，全量 Save 耗时: %v", n, time.Since(saveStart))

	// 重启加载
	loadStart := time.Now()
	d2 := NewDaemon(dir, "k")
	if err := d2.Load(); err != nil {
		t.Fatalf("加载 1 万实例失败: %v", err)
	}
	t.Logf("[扩展] 重启加载 1 万实例耗时: %v, 实例数=%d", time.Since(loadStart), len(d2.Instances))
	if len(d2.Instances) != n {
		t.Fatalf("加载数量不对: %d", len(d2.Instances))
	}

	// 排序正确：按 CreateDatetime 升序
	listStart := time.Now()
	insts := d.List()
	t.Logf("[扩展] List 1 万实例耗时: %v", time.Since(listStart))
	if len(insts) != n {
		t.Fatalf("List 数量不对: %d", len(insts))
	}
	for i := 1; i < len(insts); i++ {
		a := insts[i-1]["config"].(InstanceConfig).CreateDatetime
		bb := insts[i]["config"].(InstanceConfig).CreateDatetime
		if a > bb {
			t.Fatalf("List 排序错误 at %d", i)
		}
	}
}

// TestLargeDirectoryList 大目录（1 万文件）文件列表分页正确性。
func TestLargeDirectoryList(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	d.Instances = append(d.Instances, inst)
	const files = 10000
	for i := 0; i < files; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.dat", i)), []byte{1}, 0o644)
	}
	srv := newTestServer(d)
	defer srv.Close()

	start := time.Now()
	url := srv.URL + "/api/files/list?apikey=test-key&uuid=" + inst.InstanceUuid + "&page=1&page_size=100"
	code, body := doReq(t, url)
	if code != http.StatusOK {
		t.Fatalf("文件列表失败: %d %s", code, body)
	}
	var resp struct {
		Data struct {
			Total int              `json:"total"`
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Data.Total != files {
		t.Fatalf("total 应为 %d, 实际 %d", files, resp.Data.Total)
	}
	if len(resp.Data.Items) != 100 {
		t.Fatalf("首页应 100 条, 实际 %d", len(resp.Data.Items))
	}
	t.Logf("[扩展] 1 万文件目录列表（第 1 页 100 条）耗时: %v", time.Since(start))
}

// TestLargeLogTail 2MB 环形缓冲 + 大 size 查询。
func TestLargeLogTail(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	buf := NewLogBuffer(2 * 1024 * 1024)
	line := []byte("2026-01-01 12:00:00 [INFO] hello world, this is a log line with some padding content\n")
	for i := 0; i < 50000; i++ {
		_, _ = buf.Write(line)
	}
	if buf.Len() > 2*1024*1024 {
		t.Fatalf("缓冲超限: %d", buf.Len())
	}
	s := buf.Tail(2048)
	if len(s) > 2048*1024 {
		t.Fatalf("Tail(2048KB) 返回 %d 字节", len(s))
	}
	start := time.Now()
	buf.Tail(2048)
	t.Logf("[扩展] LogBuffer 上限 2MB，Tail(2048KB) 耗时: %v(单次)", time.Since(start))
}
