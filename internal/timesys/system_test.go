// system_test.go — 时间系统管理器测试：多源一致性、虚假源剔除、持久化恢复。
package timesys

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestSystem 用给定的模拟服务器构造时间系统。
func newTestSystem(t *testing.T, minAgreement int, servers ...*mockServer) *System {
	t.Helper()
	cfg := Config{
		Enabled:      true,
		Interval:     time.Minute,
		Timeout:      400 * time.Millisecond,
		Samples:      len(servers),
		MinAgreement: minAgreement,
		Limits:       testLimits(),
		StateFile:    filepath.Join(t.TempDir(), "timesys.json"),
	}
	for _, m := range servers {
		cfg.Servers = append(cfg.Servers, Server{
			Name:      "mock-" + m.Address(),
			Address:   m.Address(),
			PublicKey: m.PublicKeyBase64(),
			Profile:   m.opts.Profile,
		})
	}
	sys, err := New(cfg)
	if err != nil {
		t.Fatalf("创建时间系统失败: %v", err)
	}
	t.Cleanup(sys.Stop)
	return sys
}

// TestSystemSyncAndStatus 两台一致服务器 → 时钟锁定、状态正确。
func TestSystemSyncAndStatus(t *testing.T) {
	a := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	b := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	sys := newTestSystem(t, 2, a, b)
	if err := sys.SyncNow(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	st := sys.Status()
	if !st.Enabled || st.TotalSuccess != 1 || st.TotalRounds != 1 {
		t.Fatalf("状态异常: %+v", st)
	}
	if st.Quality != QualitySynced || !st.Trustworthy {
		t.Fatalf("质量 = %q trustworthy=%v", st.Quality, st.Trustworthy)
	}
	if d := st.Now.Sub(time.Now()); d < -300*time.Millisecond || d > 300*time.Millisecond {
		t.Fatalf("同步后时间偏差 = %v", d)
	}
	if st.LastSync.IsZero() || st.UncertaintyMillis <= 0 || st.UncertaintyMillis > 1000 {
		t.Fatalf("同步元数据异常: lastSync=%v unc=%v", st.LastSync, st.UncertaintyMillis)
	}
	if len(st.Servers) != 2 {
		t.Fatalf("服务器状态条目 = %d", len(st.Servers))
	}
	for _, srv := range st.Servers {
		if srv.Successes != 1 || srv.LastError != "" {
			t.Fatalf("服务器状态异常: %+v", srv)
		}
	}
	if st.Steps != 1 {
		t.Fatalf("首次锁定应记一次步进: %d", st.Steps)
	}
}

// TestSystemRejectsFalseticker 多数一致时，孤立的说谎服务器不得影响时间。
func TestSystemRejectsFalseticker(t *testing.T) {
	a := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	b := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	liar := startMockServer(t, mockOptions{Profile: ProfileLegacy, Offset: time.Hour})
	sys := newTestSystem(t, 2, a, b, liar)
	if err := sys.SyncNow(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	st := sys.Status()
	if d := st.Now.Sub(time.Now()); d < -300*time.Millisecond || d > 300*time.Millisecond {
		t.Fatalf("虚假源影响了时间: 偏差 %v", d)
	}
	if st.Quality != QualitySynced {
		t.Fatalf("质量 = %q", st.Quality)
	}
	var liarStatus *ServerStatus
	for i := range st.Servers {
		if st.Servers[i].Address == liar.Address() {
			liarStatus = &st.Servers[i]
		}
	}
	if liarStatus == nil {
		t.Fatal("未找到说谎服务器的状态")
	}
	if liarStatus.Successes != 1 {
		t.Fatalf("说谎服务器应查询成功: %+v", liarStatus)
	}
	if d := liarStatus.OffsetMillis; d < 3_000_000 || d > 3_700_000 {
		t.Fatalf("说谎服务器观测偏差 = %.0fms，期望约 3600000ms", d)
	}
}

// TestSystemNoConsensus 全部互相矛盾时拒绝调整时钟并报告错误。
func TestSystemNoConsensus(t *testing.T) {
	a := startMockServer(t, mockOptions{Profile: ProfileLegacy, Offset: -time.Hour})
	b := startMockServer(t, mockOptions{Profile: ProfileLegacy, Offset: time.Hour})
	c := startMockServer(t, mockOptions{Profile: ProfileLegacy, Offset: 2 * time.Hour})
	sys := newTestSystem(t, 2, a, b, c)
	err := sys.SyncNow(context.Background())
	if err == nil {
		t.Fatal("无一致结果时应返回错误")
	}
	st := sys.Status()
	if st.Quality != QualityUnlocked {
		t.Fatalf("无一致结果时质量 = %q，期望 unlocked", st.Quality)
	}
	if st.Trustworthy {
		t.Fatal("无一致结果时不应标记可信")
	}
	if st.TotalSuccess != 0 || st.ConsecutiveFailures != 1 {
		t.Fatalf("失败计数异常: %+v", st)
	}
	if st.LastError == "" {
		t.Fatal("缺少失败原因")
	}
}

// TestSystemSingleSource 只配置一台服务器时允许单源同步。
func TestSystemSingleSource(t *testing.T) {
	a := startMockServer(t, mockOptions{Profile: ProfileLegacy, Offset: 250 * time.Millisecond})
	sys := newTestSystem(t, 2, a)
	if err := sys.SyncNow(context.Background()); err != nil {
		t.Fatalf("单源同步失败: %v", err)
	}
	st := sys.Status()
	if st.Quality != QualitySynced {
		t.Fatalf("质量 = %q", st.Quality)
	}
	if d := st.Now.Sub(time.Now()); d < 100*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("单源偏移未被采纳: %v", d)
	}
}

// TestSystemPersistAndSeededHoldover 状态落盘、重启后进入保持模式。
func TestSystemPersistAndSeededHoldover(t *testing.T) {
	a := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	dir := t.TempDir()
	state := filepath.Join(dir, "timesys.json")
	cfg := Config{
		Enabled:      true,
		Servers:      []Server{{Name: "mock", Address: a.Address(), PublicKey: a.PublicKeyBase64(), Profile: ProfileLegacy}},
		Interval:     time.Minute,
		Timeout:      400 * time.Millisecond,
		Samples:      1,
		MinAgreement: 1,
		Limits:       testLimits(),
		StateFile:    state,
	}
	sys, err := New(cfg)
	if err != nil {
		t.Fatalf("创建时间系统失败: %v", err)
	}
	if err := sys.SyncNow(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	sys.Stop()
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("状态文件未生成: %v", err)
	}
	a.Close()

	// 重启：服务器不可达，但状态文件应恢复出保持锚点。
	sys2, err := New(cfg)
	if err != nil {
		t.Fatalf("重新创建时间系统失败: %v", err)
	}
	t.Cleanup(sys2.Stop)
	st := sys2.Status()
	if st.Quality != QualityHoldover {
		t.Fatalf("重启后质量 = %q，期望 holdover", st.Quality)
	}
	if st.Trustworthy {
		t.Fatal("重启后未在线验证不应标记可信")
	}
	if st.LastSync.IsZero() {
		t.Fatal("重启后丢失上次同步时间")
	}
	if d := st.Now.Sub(time.Now()); d < -time.Second || d > time.Second {
		t.Fatalf("重启后时间偏差 = %v", d)
	}
	if st.UncertaintyMillis <= 0 {
		t.Fatalf("重启后不确定度 = %.1fms，应大于 0", st.UncertaintyMillis)
	}
	// 保持模式下不确定度随离线时长增长：1 小时后约 +360ms（100ppm）。
	later := sys2.Clock().SnapshotAt(time.Now().Add(time.Hour))
	if later.Uncertainty < 300*time.Millisecond {
		t.Fatalf("保持 1 小时后不确定度 = %v，应包含漂移", later.Uncertainty)
	}
}

// TestSystemDisabled 未启用时不查询、不锁定，但状态可用。
func TestSystemDisabled(t *testing.T) {
	sys, err := New(Config{Enabled: false, Limits: testLimits()})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	sys.Start()
	sys.WaitFirstSync(context.Background())
	if err := sys.SyncNow(context.Background()); err == nil {
		t.Fatal("未启用时 SyncNow 应返回错误")
	}
	st := sys.Status()
	if st.Enabled {
		t.Fatal("状态应为未启用")
	}
	if st.Quality != QualityUnlocked {
		t.Fatalf("质量 = %q", st.Quality)
	}
	sys.Stop()
}

// TestSystemFallbackBatch 首批服务器不可用时自动追加查询剩余服务器。
func TestSystemFallbackBatch(t *testing.T) {
	dead := startMockServer(t, mockOptions{Profile: ProfileLegacy, Silent: true})
	alive := startMockServer(t, mockOptions{Profile: ProfileLegacy})
	cfg := Config{
		Enabled:      true,
		Interval:     time.Minute,
		Timeout:      150 * time.Millisecond,
		Samples:      1,
		MinAgreement: 1,
		Limits:       testLimits(),
		StateFile:    filepath.Join(t.TempDir(), "timesys.json"),
	}
	// dead 在前：首批只会查询它，失败后应追加 alive。
	cfg.Servers = []Server{
		{Name: "dead", Address: dead.Address(), PublicKey: dead.PublicKeyBase64(), Profile: ProfileLegacy},
		{Name: "alive", Address: alive.Address(), PublicKey: alive.PublicKeyBase64(), Profile: ProfileLegacy},
	}
	sys, err := New(cfg)
	if err != nil {
		t.Fatalf("创建时间系统失败: %v", err)
	}
	t.Cleanup(sys.Stop)
	if err := sys.SyncNow(context.Background()); err != nil {
		t.Fatalf("追加批次后仍失败: %v", err)
	}
	st := sys.Status()
	if st.Quality != QualitySynced || !st.Trustworthy {
		t.Fatalf("质量 = %q trustworthy=%v", st.Quality, st.Trustworthy)
	}
	var deadStatus, aliveStatus *ServerStatus
	for i := range st.Servers {
		switch st.Servers[i].Name {
		case "dead":
			deadStatus = &st.Servers[i]
		case "alive":
			aliveStatus = &st.Servers[i]
		}
	}
	if deadStatus == nil || deadStatus.Failures == 0 {
		t.Fatalf("不可用服务器应记录失败: %+v", deadStatus)
	}
	if aliveStatus == nil || aliveStatus.Successes == 0 {
		t.Fatalf("追加批次的服务器应记录成功: %+v", aliveStatus)
	}
}

// TestPickConsensus 一致组选择与加权合成。
func TestPickConsensus(t *testing.T) {
	ref := time.Unix(1_700_000_000, 0)
	mk := func(offset, radius time.Duration) *Result {
		return &Result{
			Midpoint: ref.Add(offset),
			Radius:   radius,
			MonoSent: ref, MonoRecv: ref,
		}
	}
	// 两真一假：假源远在一小时外。
	truth, unc, group, ok := pickConsensus([]*Result{
		mk(0, 10*time.Millisecond),
		mk(20*time.Millisecond, 10*time.Millisecond),
		mk(time.Hour, 10*time.Millisecond),
	}, 2)
	if !ok || len(group) != 2 {
		t.Fatalf("一致组 = %d ok=%v", len(group), ok)
	}
	if d := truth.Sub(ref.Add(10 * time.Millisecond)); d > 5*time.Millisecond || d < -5*time.Millisecond {
		t.Fatalf("合成真值偏差 = %v", d)
	}
	if unc < 10*time.Millisecond {
		t.Fatalf("合成不确定度 = %v", unc)
	}
	// 全部互不相交：没有一致组。
	if _, _, _, ok := pickConsensus([]*Result{
		mk(-time.Hour, time.Millisecond),
		mk(0, time.Millisecond),
		mk(time.Hour, time.Millisecond),
	}, 2); ok {
		t.Fatal("互不相交的响应不应形成一致组")
	}
	// 单源：required=1 时采纳。
	if _, _, group, ok := pickConsensus([]*Result{mk(0, time.Millisecond)}, 1); !ok || len(group) != 1 {
		t.Fatal("单源在 required=1 时应被采纳")
	}
}
