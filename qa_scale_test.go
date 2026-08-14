// 第二轮：扩展性与并行性能。
// 关注单节点在实例数、日志缓冲、并行请求下的资源与延迟表现。

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 并行基准：模拟多客户端同时轮询（面板行为）
// ---------------------------------------------------------------------------

// benchClient 构造复用连接的客户端。
// Windows 上动态端口范围有限，若每次请求新建连接会因 TIME_WAIT 堆积而端口耗尽，
// 因此基准统一复用 keep-alive 连接池。
func benchClient(conns int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        conns,
			MaxIdleConnsPerHost: conns,
			MaxConnsPerHost:     conns,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// BenchmarkParallelOverview 并行打概览接口。
func BenchmarkParallelOverview(b *testing.B) {
	_, srv := benchServer(b, 100)
	url := srv.URL + "/api/overview?apikey=test-key"
	client := benchClient(runtime.GOMAXPROCS(0) * 2)
	defer client.CloseIdleConnections()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// BenchmarkParallelInstanceListDeepPage 并行深分页（1000 实例取第 10 页）。
func BenchmarkParallelInstanceListDeepPage(b *testing.B) {
	_, srv := benchServer(b, 1000)
	url := srv.URL + "/api/service/remote_service_instances?apikey=test-key&page=10&page_size=20"
	client := benchClient(runtime.GOMAXPROCS(0) * 2)
	defer client.CloseIdleConnections()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// benchDaemon 构造带 n 个实例的守护进程（不起 HTTP 服务）。
func benchDaemon(b *testing.B, n int) *Daemon {
	b.Helper()
	dir := b.TempDir()
	d := NewDaemon(dir, "test-key")
	for i := 0; i < n; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}
	return d
}

// BenchmarkInstanceListPageSizes 同一实例总量下不同页大小的成本（验证惰性分页收益）。
// 直接调 handler：不经 TCP，测的是服务端纯成本（也避免占用临时端口）。
func BenchmarkInstanceListPageSizes(b *testing.B) {
	for _, pageSize := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("instances=1000_pageSize=%d", pageSize), func(b *testing.B) {
			d := benchDaemon(b, 1000)
			url := fmt.Sprintf("/api/service/remote_service_instances?apikey=test-key&page=1&page_size=%d", pageSize)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodGet, url, nil)
				rec := httptest.NewRecorder()
				d.handleInstanceList(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("HTTP %d", rec.Code)
				}
			}
		})
	}
}

// BenchmarkOverviewWithManyInstances 概览接口随实例数的成本（overview 仍走全量 List）。
func BenchmarkOverviewWithManyInstances(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("instances=%d", n), func(b *testing.B) {
			d := benchDaemon(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodGet, "/api/overview?apikey=test-key", nil)
				rec := httptest.NewRecorder()
				d.handleOverview(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("HTTP %d", rec.Code)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 扩展性上限
// ---------------------------------------------------------------------------

// TestScaleHundredThousandInstances 10 万实例下的加载/保存/查找/列表成本。
func TestScaleHundredThousandInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	if testing.Short() {
		t.Skip("跳过大规模测试")
	}
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	const n = 100000

	build := time.Now()
	for i := 0; i < n; i++ {
		d.Instances = append(d.Instances, sampleInst(i, dir))
	}
	t.Logf("[扩展] 构造 %d 实例内存耗时 %v", n, time.Since(build))

	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	t.Logf("[扩展] %d 实例常驻堆约 %.1f MB（约 %.2f KB/实例）",
		n, float64(ms.HeapAlloc)/1024/1024, float64(ms.HeapAlloc)/float64(n)/1024)

	save := time.Now()
	if err := d.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	saveDur := time.Since(save)
	fi, _ := os.Stat(d.instanceFile())
	t.Logf("[扩展] 全量 Save 耗时 %v，instances.json %.1f MB —— 每次增删改都要付这个代价",
		saveDur, float64(fi.Size())/1024/1024)

	load := time.Now()
	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	t.Logf("[扩展] 启动加载 %d 实例耗时 %v", n, time.Since(load))
	if len(d2.Instances) != n {
		t.Fatalf("加载数量不符: %d", len(d2.Instances))
	}

	find := time.Now()
	if d2.Find(sampleUUID(n-1)) == nil {
		t.Fatal("未找到末位实例")
	}
	t.Logf("[扩展] 最坏情况线性查找（末位）耗时 %v", time.Since(find))

	list := time.Now()
	items := d2.List()
	t.Logf("[扩展] 全量 List（overview 走此路径）耗时 %v，%d 项", time.Since(list), len(items))

	if saveDur > 3*time.Second {
		t.Logf("[发现] 10 万实例时单次 Save 超过 3 秒，任何一次实例增删改都会阻塞该实例操作")
	}
}

// TestLogBufferMemoryFootprint 多实例日志缓冲的内存上限（每实例 2MB 环形缓冲）。
func TestLogBufferMemoryFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	const instances = 100
	line := make([]byte, 4096)
	for i := range line {
		line[i] = 'x'
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	buffers := make([]*LogBuffer, 0, instances)
	for i := 0; i < instances; i++ {
		b := NewLogBuffer(0) // 默认 2MB
		// 填满缓冲
		for written := 0; written < 2*1024*1024+8192; written += len(line) {
			b.Write(line)
		}
		buffers = append(buffers, b)
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	grown := float64(after.HeapAlloc-before.HeapAlloc) / 1024 / 1024
	t.Logf("[扩展] %d 个实例日志缓冲填满后堆增长 %.1f MB（约 %.2f MB/实例，默认上限 2MB/实例）",
		instances, grown, grown/float64(instances))
	if grown > float64(instances)*3 {
		t.Errorf("日志缓冲内存超出预期上限: %.1f MB", grown)
	}

	// 确认环形缓冲确实截断
	for _, b := range buffers[:1] {
		if size := b.Len(); size > 2*1024*1024 {
			t.Errorf("环形缓冲未截断: %d 字节", size)
		}
	}
	runtime.KeepAlive(buffers)
}

// TestTailCostOnFullBuffer 满缓冲下 Tail 的拷贝成本（每次请求日志都会付）。
func TestTailCostOnFullBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	b := NewLogBuffer(0)
	line := make([]byte, 4096)
	for i := range line {
		line[i] = 'y'
	}
	for written := 0; written < 2*1024*1024+8192; written += len(line) {
		b.Write(line)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const calls = 50
	start := time.Now()
	for i := 0; i < calls; i++ {
		_ = b.Tail(2048) // 客户端允许的最大 size
	}
	dur := time.Since(start)
	runtime.ReadMemStats(&after)
	perCall := dur / calls
	allocPerCall := float64(after.TotalAlloc-before.TotalAlloc) / calls / 1024 / 1024
	t.Logf("[性能] 满 2MB 缓冲上 Tail(2048KB)：单次 %v、分配约 %.2f MB/次；"+
		"面板按秒轮询 N 个实例时为 N×%.2f MB/s 的分配压力", perCall, allocPerCall, allocPerCall)
}
