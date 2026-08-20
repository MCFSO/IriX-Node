// AI 日志查询与监控历史测试：关键词过滤、行数上限、指标采样（缩短间隔）、
// minutes 窗口过滤。

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// metricsGet 请求 metrics 接口并解析 samples。
func metricsGet(t *testing.T, srvURL, uuid, minutes string) []map[string]any {
	t.Helper()
	url := srvURL + "/api/instance/metrics?uuid=" + uuid + "&apikey=test-key"
	if minutes != "" {
		url += "&minutes=" + minutes
	}
	resp, err := testClient.Get(url)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Status int `json:"status"`
		Data   struct {
			Samples []map[string]any `json:"samples"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if out.Status != 200 {
		t.Fatalf("状态码: %d %s", out.Status, body)
	}
	return out.Data.Samples
}

// TestLogsQuery 日志查询：关键词过滤 + maxLines 上限 + 尾部优先。
func TestLogsQuery(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	// 直接写日志文件（含关键词与干扰行）
	logPath := filepath.Join(dir, "logs", inst.InstanceUuid+".log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	content := "line one\n[ERROR] 启动失败: java.io.IOException\nline three\n[ERROR] 端口被占用\n"
	_ = os.WriteFile(logPath, []byte(content), 0o644)
	d.LogDir = filepath.Join(dir, "logs")

	srv := newTestServer(d)
	defer srv.Close()

	// 关键词过滤
	resp, err := testClient.Get(srv.URL + "/api/instance/logs/query?uuid=" +
		inst.InstanceUuid + "&keyword=error&apikey=test-key")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Data struct {
			Items []string `json:"items"`
			Total int      `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if out.Data.Total != 2 || len(out.Data.Items) != 2 {
		t.Fatalf("关键词过滤应得 2 行: %+v", out.Data)
	}
	for _, l := range out.Data.Items {
		if !strings.Contains(strings.ToLower(l), "error") {
			t.Fatalf("结果行不含关键词: %q", l)
		}
	}

	// maxLines 上限
	resp2, _ := testClient.Get(srv.URL + "/api/instance/logs/query?uuid=" +
		inst.InstanceUuid + "&maxLines=1&apikey=test-key")
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err := json.Unmarshal(body2, &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if len(out.Data.Items) != 1 || out.Data.Items[0] != "[ERROR] 端口被占用" {
		t.Fatalf("maxLines=1 应取尾部一行: %+v", out.Data)
	}
	t.Logf("[验证] 日志查询关键词过滤与 maxLines 正确")
}

// TestMetricsSamples 缩短采样间隔后 metrics 返回采样点。
func TestMetricsSamples(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.metricsInterval = time.Second // 仅本守护进程缩短，避免全局竞争
	inst := NewInstance("metrics-uuid", InstanceConfig{
		Nickname: "监控", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer stopTestProc(t, inst)

	// 触发惰性启动采样循环并等待至少 2 次采样
	samples := metricsGet(t, srv.URL, inst.InstanceUuid, "")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		samples = metricsGet(t, srv.URL, inst.InstanceUuid, "")
		if len(samples) >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(samples) < 2 {
		t.Fatalf("应有至少 2 条采样: %d", len(samples))
	}
	s := samples[len(samples)-1]
	if _, ok := s["time"]; !ok {
		t.Fatalf("采样缺少 time: %v", s)
	}
	if _, ok := s["cpu"]; !ok {
		t.Fatalf("采样缺少 cpu: %v", s)
	}
	// minutes 窗口过滤
	recent := metricsGet(t, srv.URL, inst.InstanceUuid, "1")
	if len(recent) == 0 {
		t.Fatalf("minutes=1 应返回采样")
	}
	t.Logf("[验证] metrics 采样正常（%d 条，窗口过滤 %d 条）", len(samples), len(recent))
}

// TestMetricsNotRunning 未运行实例返回空采样。
func TestMetricsNotRunning(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	samples := metricsGet(t, srv.URL, inst.InstanceUuid, "")
	if len(samples) != 0 {
		t.Fatalf("未运行实例应无采样: %v", samples)
	}
	t.Logf("[验证] 未运行实例 metrics 为空")
}
