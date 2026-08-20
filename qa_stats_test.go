// 实例级指标测试：输出解析（玩家/TPS）、stats API 端到端、CPU 采样状态机。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestParseServerLine 从服务器输出行解析玩家数/TPS。
func TestParseServerLine(t *testing.T) {
	p := &Process{}
	cases := []struct {
		line string
		pl   int // 期望 players（-1 不检查）
		max  int // 期望 maxPlayers（-1 不检查）
		tps  float64
	}{
		{"[12:00:01 INFO]: There are 3 of a max of 20 players online:", 3, 20, -1},
		{"[12:00:02 INFO]: There are 0 of a max of 20 players online", 0, 20, -1},
		{"[12:00:03 INFO]: TPS from last 1m, 5m, 15m: 19.9, 19.8, 19.7", -1, -1, 19.9},
		{"[12:00:04 INFO]: TPS: 20.0", -1, -1, 20.0},
		{"[12:00:05 INFO]: 5 players online:", 5, -1, -1},
		{"plain startup line without stats", -1, -1, -1},
	}
	for i, c := range cases {
		p.parseServerLine(c.line)
		pl, max, tps := p.statsSnapshot()
		if c.pl >= 0 && pl != c.pl {
			t.Fatalf("case %d: players=%d 期望 %d", i, pl, c.pl)
		}
		if c.max >= 0 && max != c.max {
			t.Fatalf("case %d: maxPlayers=%d 期望 %d", i, max, c.max)
		}
		if c.tps >= 0 && tps != c.tps {
			t.Fatalf("case %d: tps=%v 期望 %v", i, tps, c.tps)
		}
	}
	t.Logf("[验证] 玩家数/TPS 输出解析正确（vanilla/spigot/paper 模式）")
}

// TestInstanceStatsAPI stats API 端到端：运行中实例返回 pid/uptime，未运行返回 0。
func TestInstanceStatsAPI(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("stats-uuid", InstanceConfig{
		Nickname: "指标", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	getStats := func() map[string]any {
		req, _ := http.NewRequest(http.MethodGet,
			srv.URL+"/api/instance/stats?uuid="+inst.InstanceUuid+"&apikey=test-key", nil)
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Status int            `json:"status"`
			Data   map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("响应解析失败: %v", err)
		}
		if out.Status != 200 {
			t.Fatalf("状态码: %d", out.Status)
		}
		return out.Data
	}

	// 未运行：pid=0
	data := getStats()
	if pid, _ := data["pid"].(float64); pid != 0 {
		t.Fatalf("未运行实例 pid 应为 0: %v", data)
	}

	// 启动后：pid>0；uptimeSec 随时间增长（等待至少 1 秒）
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer stopTestProc(t, inst)

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		data = getStats()
		if up, _ := data["uptimeSec"].(float64); up >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	pid, _ := data["pid"].(float64)
	if pid <= 0 {
		t.Fatalf("运行中实例 pid 应 > 0: %v", data)
	}
	if up, _ := data["uptimeSec"].(float64); up < 1 {
		t.Fatalf("uptimeSec 应 ≥ 1: %v", data)
	}
	// 未解析到玩家/TPS 时省略字段
	if _, ok := data["players"]; ok {
		t.Fatalf("ping 输出不应解析出 players: %v", data)
	}
	// memoryMb/cpuPercent 字段存在（值可为 0，首次采样无基线）
	for _, k := range []string{"cpuPercent", "memoryMb", "networkDownloadBps", "networkUploadBps"} {
		if _, ok := data[k]; !ok {
			t.Fatalf("缺少字段 %s: %v", k, data)
		}
	}
	t.Logf("[验证] stats API 端到端正常（pid=%v, uptime=%v, memoryMb=%v）",
		data["pid"], data["uptimeSec"], data["memoryMb"])
}

// TestSampleCPUPercentState 无进程时返回 0 不崩溃；运行中由 stats API 覆盖。
func TestSampleCPUPercentState(t *testing.T) {
	p := &Process{}
	if v := p.sampleCPUPercent(); v != 0 {
		t.Fatalf("无进程时应返回 0: %v", v)
	}
	t.Logf("[验证] CPU 采样状态机安全（无进程不崩溃）")
}

// TestRound2 两位小数舍入。
func TestRound2(t *testing.T) {
	if round2(12.345) != 12.35 || round2(0.004) != 0 {
		t.Fatalf("round2 错误: %v %v", round2(12.345), round2(0.004))
	}
	t.Logf("[验证] round2 舍入正确: %v", round2(12.345))
}
