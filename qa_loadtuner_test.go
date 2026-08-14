// 负载自适应调谐器测试：状态判定（纯函数）与防抖状态机。
// 不触碰真实 GOMAXPROCS/GOGC（applyFn 注入假函数，避免影响测试套件全局状态）。

package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestEvaluateLoad 负载状态判定纯函数。
func TestEvaluateLoad(t *testing.T) {
	cases := []struct {
		cpu  float64
		gos  int
		want loadState
	}{
		{0.90, 10, loadBusy},   // CPU 触发
		{0.10, 5000, loadBusy}, // goroutine 触发
		{0.30, 100, loadNormal},
		{0.00, 5, loadIdle},
		{0.05, 20, loadIdle},   // 边界（含）
		{0.06, 20, loadNormal}, // 刚过 idle 阈值
		{0.60, 10, loadBusy},   // busy 边界（含）
		{0.59, 10, loadNormal},
	}
	for _, c := range cases {
		if got := evaluateLoad(c.cpu, c.gos); got != c.want {
			t.Fatalf("evaluateLoad(%v, %d) = %v, 期望 %v", c.cpu, c.gos, got, c.want)
		}
	}
}

// TestLoadTunerDebounce 状态切换防抖：连续 loadTuneStable 次同候选才切换。
func TestLoadTunerDebounce(t *testing.T) {
	tuner := &loadTuner{state: loadNormal, candidate: loadNormal, maxProcs: 8}
	applied := []loadState{}
	tuner.applyFn = func(s loadState) { applied = append(applied, s) }

	// 手动注入采样：绕过真实采样（进程 CPU 不稳定）
	tickWith := func(cpu float64, gos int) {
		tuner.mu.Lock()
		tuner.cpuBusy, tuner.goroutines = cpu, gos
		cand := evaluateLoad(cpu, gos)
		if cand == tuner.state {
			tuner.candidate = tuner.state
			tuner.stable = 0
		} else if cand == tuner.candidate {
			tuner.stable++
		} else {
			tuner.candidate = cand
			tuner.stable = 1
		}
		if tuner.stable >= loadTuneStable {
			tuner.applyFn(cand)
		}
		tuner.mu.Unlock()
	}

	// 偶发一次高负载：不应切换
	tickWith(0.9, 10)
	tickWith(0.01, 3)
	if len(applied) != 0 {
		t.Fatalf("抖动采样不应触发切换: %v", applied)
	}
	// 连续 3 次高负载：切换 busy
	tickWith(0.9, 10)
	tickWith(0.9, 10)
	tickWith(0.9, 10)
	if len(applied) != 1 || applied[0] != loadBusy {
		t.Fatalf("应切换 busy: %v", applied)
	}
	// 连续 3 次低负载：切换 idle
	tickWith(0.01, 3)
	tickWith(0.01, 3)
	tickWith(0.01, 3)
	if len(applied) != 2 || applied[1] != loadIdle {
		t.Fatalf("应切换 idle: %v", applied)
	}
}

// TestLoadEndpoint /api/load 端点返回状态与调谐参数。
func TestLoadEndpoint(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/load?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("/api/load 失败: %d %s", code, body)
	}
	text := string(body)
	for _, want := range []string{
		`"state"`,
		`"gomaxprocs"`,
		`"gcPercent"`,
		`"cpuBusy"`,
		`"goroutines"`,
		`"numCPU"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/api/load 缺少 %q: %s", want, text)
		}
	}
}
