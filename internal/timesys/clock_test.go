// clock_test.go — 时钟纪律（漂移估计、slew/step、保持不确定度）测试。
//
// 时钟模型只依赖传入的单调时刻，因此测试可以用「合成时间轴」精确验证
// 伺服行为，不需要真实等待。
package timesys

import (
	"math"
	"testing"
	"time"
)

// testLimits 返回测试用纪律参数（缩短频率拟合窗口）。
func testLimits() Limits {
	return Limits{
		MaxStep:          2 * time.Second,
		MaxSlewPPM:       500,
		MaxRatePPM:       500,
		MaxSlewOffset:    10 * time.Second,
		MinRateSpan:      30 * time.Minute,
		HoldoverLimit:    24 * time.Hour,
		MaxUncertainty:   5 * time.Second,
		HoldoverDriftPPM: 100,
	}
}

// TestClockFirstLockSteps 首次同步必须直接对准真值。
func TestClockFirstLockSteps(t *testing.T) {
	c := NewClock(testLimits())
	base := time.Unix(1_700_000_000, 0)
	obs := c.Observe(base, base.Add(5*time.Second), 10*time.Millisecond)
	if !obs.Stepped {
		t.Fatal("首次同步应步进")
	}
	if obs.StepSize != 5*time.Second {
		t.Fatalf("步进量 = %v，期望 5s", obs.StepSize)
	}
	if got := c.NowAt(base); !got.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("锁定后时间 = %v，期望 %v", got, base.Add(5*time.Second))
	}
	snap := c.SnapshotAt(base)
	if !snap.Locked || !snap.Verified {
		t.Fatalf("快照应显示已锁定/已验证: %+v", snap)
	}
	if snap.Quality != QualitySynced || !snap.Trustworthy {
		t.Fatalf("质量 = %q trustworthy=%v", snap.Quality, snap.Trustworthy)
	}
	if snap.Uncertainty != 10*time.Millisecond {
		t.Fatalf("不确定度 = %v，期望 10ms", snap.Uncertainty)
	}
}

// TestClockSlewKeepsContinuity 小误差按固定速率缓慢吃掉，读数不跳变。
func TestClockSlewKeepsContinuity(t *testing.T) {
	c := NewClock(testLimits())
	base := time.Unix(1_700_000_000, 0)
	c.Observe(base, base.Add(5*time.Second), time.Millisecond)

	mono := base.Add(10 * time.Second)
	truth := c.NowAt(mono).Add(100 * time.Millisecond) // 本机慢了 100ms
	obs := c.Observe(mono, truth, time.Millisecond)
	if obs.Stepped {
		t.Fatal("100ms 误差不应步进")
	}
	if obs.Offset != 100*time.Millisecond {
		t.Fatalf("观测误差 = %v，期望 100ms", obs.Offset)
	}
	if got := c.NowAt(mono); !got.Equal(truth.Add(-100 * time.Millisecond)) {
		t.Fatalf("slew 起点不应跳变: got %v", got)
	}
	// 500ppm × 100s = 50ms。
	got := c.NowAt(mono.Add(100 * time.Second))
	want := truth.Add(100*time.Second - 50*time.Millisecond)
	if diff := got.Sub(want); diff > 2*time.Millisecond || diff < -2*time.Millisecond {
		t.Fatalf("slew 100s 后偏差 = %v（got=%v want=%v）", diff, got, want)
	}
	// 200s 后误差应完全吃掉。
	got = c.NowAt(mono.Add(200 * time.Second))
	if d := got.Sub(truth.Add(200 * time.Second)); d > 2*time.Millisecond {
		t.Fatalf("slew 完成后仍有偏差: %v", d)
	}
	if c.RatePPM() != 0 {
		t.Fatalf("样本不足时不应产生速率校正: %v", c.RatePPM())
	}
	snap := c.SnapshotAt(mono.Add(200 * time.Second))
	if snap.Slews != 1 || snap.Steps != 1 {
		t.Fatalf("步进/slew 次数 = %d/%d，期望 1/1", snap.Steps, snap.Slews)
	}
	if snap.PendingSlew != 0 {
		t.Fatalf("待校正量应为 0: %v", snap.PendingSlew)
	}
}

// TestClockStepsOnLargeError 超过阈值的误差必须步进。
func TestClockStepsOnLargeError(t *testing.T) {
	c := NewClock(testLimits())
	base := time.Unix(1_700_000_000, 0)
	c.Observe(base, base, time.Millisecond)
	mono := base.Add(time.Minute)
	truth := c.NowAt(mono).Add(-3 * time.Second)
	obs := c.Observe(mono, truth, time.Millisecond)
	if !obs.Stepped || obs.StepSize != -3*time.Second {
		t.Fatalf("应步进 -3s，实际 %+v", obs)
	}
	if got := c.NowAt(mono); !got.Equal(truth) {
		t.Fatalf("步进后时间 = %v，期望 %v", got, truth)
	}
	if snap := c.SnapshotAt(mono); snap.Steps != 2 {
		t.Fatalf("步进次数 = %d，期望 2", snap.Steps)
	}
}

// TestClockRateEstimate 频率误差应被拟合并在推进中补偿。
func TestClockRateEstimate(t *testing.T) {
	lim := testLimits()
	lim.MaxStep = 5 * time.Second
	c := NewClock(lim)
	base := time.Unix(1_700_000_000, 0)
	// 本机晶振快 100ppm：真实时间比单调时钟多走 100ppm。
	trueAt := func(elapsed time.Duration) time.Time {
		return base.Add(elapsed + time.Duration(float64(elapsed)*100e-6))
	}
	step := 5 * time.Minute
	var lastMono time.Time
	for i := 0; i <= 24; i++ {
		lastMono = base.Add(time.Duration(i) * step)
		c.Observe(lastMono, trueAt(time.Duration(i)*step), 5*time.Millisecond)
	}
	ppm := c.RatePPM()
	if ppm < 60 || ppm > 120 {
		t.Fatalf("频率校正 = %.1fppm，期望约 100ppm", ppm)
	}
	// 当前时刻应紧跟真实时间（伺服把相位误差吃掉）。
	if d := c.NowAt(lastMono).Sub(trueAt(24 * step)); math.Abs(d.Seconds()) > 0.05 {
		t.Fatalf("当前时间偏差 = %v，超过 50ms", d)
	}
	// 按拟合速率外推 1 小时，误差仍应远小于未补偿的 3.6ms/h（100ppm）。
	futureMono := lastMono.Add(time.Hour)
	futureTruth := trueAt(24*step + time.Hour)
	if d := c.NowAt(futureMono).Sub(futureTruth); math.Abs(d.Seconds()) > 0.01 {
		t.Fatalf("1 小时外推偏差 = %v，超过 10ms", d)
	}
}

// TestClockRateIgnoresNoisySamples RFC 10049 级别的 3 秒半径在短窗口内不应
// 把噪声当成漂移（速率校正保持接近 0）。
func TestClockRateIgnoresNoisySamples(t *testing.T) {
	lim := testLimits()
	lim.MaxStep = 10 * time.Second
	c := NewClock(lim)
	base := time.Unix(1_700_000_000, 0)
	trueAt := func(elapsed time.Duration) time.Time {
		return base.Add(elapsed + time.Duration(float64(elapsed)*100e-6))
	}
	for i := 0; i <= 6; i++ {
		mono := base.Add(time.Duration(i) * 5 * time.Minute)
		c.Observe(mono, trueAt(time.Duration(i)*5*time.Minute), 3*time.Second)
	}
	if ppm := c.RatePPM(); math.Abs(ppm) > 30 {
		t.Fatalf("噪声样本导致速率校正 = %.1fppm，应保持在 0 附近", ppm)
	}
}

// TestClockHoldoverUncertainty 长时间无同步时不确定度增长、可信度下降。
func TestClockHoldoverUncertainty(t *testing.T) {
	c := NewClock(testLimits())
	base := time.Unix(1_700_000_000, 0)
	c.Observe(base, base, 100*time.Millisecond)
	// 2 小时后仍是 synced（不确定度 100ms + 100ppm×2h ≈ 820ms < 5s）。
	snap := c.SnapshotAt(base.Add(2 * time.Hour))
	if snap.Quality != QualitySynced || !snap.Trustworthy {
		t.Fatalf("2h 后质量 = %q trustworthy=%v unc=%v", snap.Quality, snap.Trustworthy, snap.Uncertainty)
	}
	// 30 小时后超出保持窗口：holdover 且不可信。
	snap = c.SnapshotAt(base.Add(30 * time.Hour))
	if snap.Quality != QualityHoldover {
		t.Fatalf("30h 后质量 = %q，期望 holdover", snap.Quality)
	}
	if snap.Trustworthy {
		t.Fatal("超出保持窗口仍标记为可信")
	}
	if snap.Uncertainty < 10*time.Second {
		t.Fatalf("30h 后不确定度 = %v，应显著增长", snap.Uncertainty)
	}
}

// TestClockSeedHoldover 重启后从状态文件恢复的锚点不可信但可用。
func TestClockSeedHoldover(t *testing.T) {
	c := NewClock(testLimits())
	lastSync := time.Now().Add(-time.Hour)
	c.SeedHoldover(2*time.Second, 10, lastSync, 50*time.Millisecond, time.Hour)
	now := c.Now()
	if d := now.Sub(time.Now()); d < 1500*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("保持锚点偏差 = %v，期望约 2s", d)
	}
	snap := c.Snapshot()
	if snap.Quality != QualityHoldover {
		t.Fatalf("恢复后质量 = %q，期望 holdover", snap.Quality)
	}
	if snap.Trustworthy {
		t.Fatal("未在线验证的锚点不应标记可信")
	}
	// 不确定度 = 50ms + 100ppm×1h = 410ms。
	if d := snap.Uncertainty - 410*time.Millisecond; d > 20*time.Millisecond || d < -20*time.Millisecond {
		t.Fatalf("恢复不确定度 = %v，期望约 410ms", snap.Uncertainty)
	}
	// 频率校正被恢复。
	if snap.RatePPM != 10 {
		t.Fatalf("恢复频率校正 = %v，期望 10", snap.RatePPM)
	}
}

// TestClockSeedThenSyncClearsAgeBias 从状态文件恢复后再在线同步，年龄/不确定度
// 必须回到本次同步的测量值（不能永久保留重启前的离线时长）。
func TestClockSeedThenSyncClearsAgeBias(t *testing.T) {
	c := NewClock(testLimits())
	c.SeedHoldover(0, 0, time.Now().Add(-time.Hour), 10*time.Millisecond, time.Hour)
	before := c.Snapshot()
	if before.SyncAge < 50*time.Minute {
		t.Fatalf("恢复后年龄 = %v，期望约 1 小时", before.SyncAge)
	}
	now := time.Now()
	obs := c.Observe(now, now, 5*time.Millisecond)
	if !obs.Stepped {
		t.Fatal("恢复锚点后的首次同步应步进校准")
	}
	after := c.Snapshot()
	if after.SyncAge > time.Second {
		t.Fatalf("同步后年龄 = %v，应重置为 0", after.SyncAge)
	}
	if after.Uncertainty > 100*time.Millisecond {
		t.Fatalf("同步后不确定度 = %v，应只剩服务器半径", after.Uncertainty)
	}
	if after.Quality != QualitySynced || !after.Trustworthy {
		t.Fatalf("同步后质量 = %q trustworthy=%v", after.Quality, after.Trustworthy)
	}
}

// TestFakeClock 假时钟可被拨动，供上层测试控制墙上时间。
func TestFakeClock(t *testing.T) {
	start := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	c := NewFakeClock(start)
	if d := c.Now().Sub(start); d < 0 || d > time.Second {
		t.Fatalf("假时钟初始偏差 = %v", d)
	}
	c.Advance(2 * time.Hour)
	if d := c.Now().Sub(start.Add(2 * time.Hour)); d < 0 || d > time.Second {
		t.Fatalf("拨动后偏差 = %v", d)
	}
	// 假时钟不接受外部纪律。
	c.Observe(time.Now(), time.Now().Add(time.Hour), time.Millisecond)
	if c.RatePPM() != 0 {
		t.Fatal("假时钟不应接受外部纪律")
	}
}
