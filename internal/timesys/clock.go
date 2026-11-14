// clock.go — 受纪律约束的节点时钟（漂移补偿 + 时钟伺服）。
//
// 设计要点：
//
//  1. 虚拟时钟锚定在**单调时钟**上：Now() = 锚点真值 + 单调流逝 ×(1+速率校正)
//     + 已消耗的相位校正。系统墙钟被 NTP 步进/手动改时不会让节点时间倒退；
//  2. 速率校正（ppm）由 Roughtime 观测样本对 (单调时刻, 真值) 做最小二乘拟合
//     得到，补偿本机晶振相对真实时间的频率误差；
//  3. 相位校正（offset）按 NTP 风格伺服：
//     - 小误差 → slew：按最大 500ppm 慢慢吃掉误差，绝不回拨业务时间；
//     - 大误差（首次锁定 / 超过步进阈值）→ step：直接对准真值，并记录事件；
//  4. 不确定度（uncertainty）随「上次同步距今时长」按晶振漂移上界增长，
//     供上层（TLS、会话、API）判断当前时间是否可信；
//  5. 全部状态由一把 RWMutex 保护，Now() 为读路径，热路径无分配。
package timesys

import (
	"math"
	"sync"
	"time"
)

// Quality 时间质量。
type Quality string

const (
	// QualityUnlocked 从未成功同步，当前直接用系统时钟（无纪律）。
	QualityUnlocked Quality = "unlocked"
	// QualityHoldover 曾同步成功（含重启后从状态文件恢复），但当前处于保持状态。
	QualityHoldover Quality = "holdover"
	// QualitySynced 最近一次同步在保持窗口内且不确定度在阈值内。
	QualitySynced Quality = "synced"
)

// clockMode 时钟工作模式。
type clockMode int

const (
	modeSystem      clockMode = iota // 直接使用系统时钟（尚未锁定）
	modeDisciplined                  // 受纪律约束的虚拟时钟
	modeFixed                        // 测试用：固定起点 + 单调流逝 + 手动偏移
)

// Limits 时钟纪律参数。零值由 DefaultLimits 补齐。
type Limits struct {
	MaxStep          time.Duration // 误差超过该值直接步进（默认 2s）
	MaxSlewPPM       float64       // 相位校正最大速率（默认 500ppm，约 1.8s/小时）
	MaxRatePPM       float64       // 频率校正钳制（默认 500ppm）
	MaxSlewOffset    time.Duration // 待校正量上限，超过改为步进（默认 10s）
	MinRateSpan      time.Duration // 频率拟合的最小样本跨度（默认 30 分钟）
	HoldoverLimit    time.Duration // 超过该时长未同步视为不可信（默认 24 小时）
	MaxUncertainty   time.Duration // 可信判定的不确定度上限（默认 5s）
	HoldoverDriftPPM float64       // 保持模式下漂移上界（默认 100ppm，用于不确定度增长）
}

// DefaultLimits 返回默认纪律参数（负值/零值视为未设置）。
func DefaultLimits() Limits {
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

// normalize 补齐零值并钳制非法值。
func (l Limits) normalize() Limits {
	d := DefaultLimits()
	if l.MaxStep <= 0 {
		l.MaxStep = d.MaxStep
	}
	if l.MaxSlewPPM <= 0 || l.MaxSlewPPM > 10000 {
		l.MaxSlewPPM = d.MaxSlewPPM
	}
	if l.MaxRatePPM <= 0 || l.MaxRatePPM > 10000 {
		l.MaxRatePPM = d.MaxRatePPM
	}
	if l.MaxSlewOffset <= 0 {
		l.MaxSlewOffset = d.MaxSlewOffset
	}
	if l.MinRateSpan <= 0 {
		l.MinRateSpan = d.MinRateSpan
	}
	if l.HoldoverLimit <= 0 {
		l.HoldoverLimit = d.HoldoverLimit
	}
	if l.MaxUncertainty <= 0 {
		l.MaxUncertainty = d.MaxUncertainty
	}
	if l.HoldoverDriftPPM <= 0 || l.HoldoverDriftPPM > 10000 {
		l.HoldoverDriftPPM = d.HoldoverDriftPPM
	}
	return l
}

// rateSample 一次成功观测：单调时刻（相对启动锚点）、对应真值与测量权重。
type rateSample struct {
	mono   int64   // 相对 clock.bootMono 的纳秒
	truth  int64   // Unix 纳秒
	weight float64 // 1/σ²（σ 为服务器半径 + RTT/2，单位秒）
}

// Observation 一次时钟观测的结果（用于日志/状态）。
type Observation struct {
	Offset      time.Duration // 观测误差：真值 - 当时的虚拟时间
	StepSize    time.Duration // 实际步进量（未步进为 0）
	Stepped     bool          // 是否发生步进
	RatePPM     float64       // 观测后的频率校正
	Uncertainty time.Duration // 观测后的不确定度
}

// Clock 受纪律约束的时钟。零值不可用，请用 NewClock / NewFakeClock。
type Clock struct {
	mu sync.RWMutex

	lim      Limits
	mode     clockMode
	bootMono time.Time

	// 虚拟时钟模型：Now = anchorTrue + (mono-anchorMono)*(1+ratePPM*1e-6)
	//                      + 已消耗的相位校正量
	anchorMono    time.Time
	anchorTrue    int64 // Unix 纳秒
	ratePPM       float64
	slewStart     time.Time
	slewRemaining int64 // 正 = 需要快进，负 = 需要放慢
	slewRatePPM   float64

	// 观测与统计
	samples      []rateSample
	locked       bool // 是否有可信锚点（含重启后从状态文件恢复）
	verified     bool // 本次进程内是否成功同步过
	lastSyncMono time.Time
	lastSyncTrue int64
	ageBias      time.Duration // 保持模式：进入本进程前已经过去的时长
	lastOffset   int64         // 上次观测到的「真值 - 系统墙钟」
	lastRadius   int64
	steps        int
	slews        int
	lastStepAt   time.Time
	lastStepSize int64

	// 测试模式
	fixedBase time.Time
	fixedAdd  int64
}

// NewClock 创建尚未锁定的系统时钟（Now() 直接返回系统时间）。
func NewClock(limits Limits) *Clock {
	now := time.Now()
	return &Clock{
		lim:      limits.normalize(),
		mode:     modeSystem,
		bootMono: now,
	}
}

// NewFakeClock 创建测试用时钟：从 start 开始按真实单调时钟流逝，
// 可用 Advance/Set 手动拨动。它不做纪律，仅用于让上层测试控制墙上时间。
func NewFakeClock(start time.Time) *Clock {
	now := time.Now()
	return &Clock{
		lim:       DefaultLimits(),
		mode:      modeFixed,
		bootMono:  now,
		fixedBase: start,
	}
}

// Now 返回当前受纪律约束的时间（本地时区）。
func (c *Clock) Now() time.Time {
	if c == nil {
		return time.Now()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nowAtLocked(time.Now())
}

// NowAt 返回单调时刻 mono 对应的虚拟时间（测试/诊断用）。
func (c *Clock) NowAt(mono time.Time) time.Time {
	if c == nil {
		return mono
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nowAtLocked(mono)
}

// nowAtLocked 计算 mono 时刻对应的虚拟时间（调用方需持锁）。
func (c *Clock) nowAtLocked(mono time.Time) time.Time {
	switch c.mode {
	case modeSystem:
		return mono
	case modeFixed:
		return c.fixedBase.Add(mono.Sub(c.bootMono) + time.Duration(c.fixedAdd))
	}
	elapsed := mono.Sub(c.anchorMono)
	if elapsed < 0 {
		elapsed = 0
	}
	adv := elapsed + time.Duration(float64(elapsed)*c.ratePPM*1e-6)
	return time.Unix(0, c.anchorTrue+int64(adv)+c.slewConsumedLocked(mono))
}

// slewConsumedLocked 返回到 mono 时刻为止已消耗的相位校正量（调用方需持锁）。
func (c *Clock) slewConsumedLocked(mono time.Time) int64 {
	if c.slewRemaining == 0 {
		return 0
	}
	elapsed := mono.Sub(c.slewStart)
	if elapsed <= 0 {
		return 0
	}
	limit := int64(float64(elapsed) * c.slewRatePPM * 1e-6)
	rem := c.slewRemaining
	if rem > 0 {
		if limit > rem {
			return rem
		}
		return limit
	}
	if limit > -rem {
		return rem
	}
	return -limit
}

// slewRemainingAtLocked 返回到 mono 时刻还剩下的待校正量（调用方需持锁）。
func (c *Clock) slewRemainingAtLocked(mono time.Time) int64 {
	if c.slewRemaining == 0 {
		return 0
	}
	return c.slewRemaining - c.slewConsumedLocked(mono)
}

// Observe 提交一次外部观测：truth 为 mono 时刻的可信时间，radius 为其误差半宽。
// 返回本次观测如何被采纳（步进/slew、速率校正）。
func (c *Clock) Observe(mono time.Time, truth time.Time, radius time.Duration) Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == modeFixed {
		// 测试时钟不接受外部纪律，避免测试语义被网络时间污染。
		return Observation{}
	}
	if radius < 0 {
		radius = 0
	}
	var obs Observation

	firstLock := !c.verified
	if firstLock {
		// 首次锁定：直接对准真值（此前系统时钟的偏差未知）。
		before := c.nowAtLocked(mono)
		obs.Offset = truth.Sub(before)
		c.mode = modeDisciplined
		c.anchorMono = mono
		c.anchorTrue = truth.UnixNano()
		c.slewStart = mono
		c.slewRemaining = 0
		c.locked = true
		c.verified = true
		obs.Stepped = true
		obs.StepSize = obs.Offset
		c.steps++
		c.lastStepSize = int64(obs.Offset)
		c.lastStepAt = mono
	} else {
		before := c.nowAtLocked(mono)
		obs.Offset = truth.Sub(before)
		remaining := c.slewRemainingAtLocked(mono)
		total := remaining + int64(obs.Offset)
		if absDuration(obs.Offset) > c.lim.MaxStep || absInt64(total) > int64(c.lim.MaxSlewOffset) {
			// 大误差：步进（记录事件，供上层日志/审计）。
			c.anchorMono = mono
			c.anchorTrue = truth.UnixNano()
			c.slewRemaining = 0
			c.slewStart = mono
			c.steps++
			c.lastStepSize = int64(obs.Offset)
			c.lastStepAt = mono
			obs.Stepped = true
			obs.StepSize = obs.Offset
		} else {
			// 小误差：重新锚定当前值，把误差挂到 slew 队列按速率吃掉。
			current := c.nowAtLocked(mono)
			c.anchorMono = mono
			c.anchorTrue = current.UnixNano()
			c.slewStart = mono
			c.slewRemaining = total
			c.slewRatePPM = c.lim.MaxSlewPPM
			if total != 0 {
				c.slews++
			}
		}
	}

	// 记录样本并更新频率校正（速率变化只影响未来推进）。
	c.addSampleLocked(mono, truth, radius)
	c.applyRateLocked(mono)

	c.lastSyncMono = mono
	c.lastSyncTrue = truth.UnixNano()
	c.ageBias = 0 // 在线同步成功：重启前的保持时长不再计入年龄
	c.lastOffset = truth.UnixNano() - mono.UnixNano()
	c.lastRadius = int64(radius)
	obs.RatePPM = c.ratePPM
	obs.Uncertainty = c.uncertaintyLocked(mono)
	return obs
}

// addSampleLocked 记录频率拟合样本并裁剪。
func (c *Clock) addSampleLocked(mono, truth time.Time, radius time.Duration) {
	// 权重取 1/σ²：legacy 服务器半径约毫秒级，几十个样本即可把漂移估计到
	// 亚 ppm；RFC 10049 服务器半径至少 3 秒，短窗口内拟合结果会被噪声主导，
	// 因此权重小、显著性检验不通过，不会乱改速率。
	sigma := radius.Seconds()
	if sigma < 0.001 {
		sigma = 0.001
	}
	w := 1 / (sigma * sigma)
	if w > 1e8 {
		w = 1e8
	}
	c.samples = append(c.samples, rateSample{
		mono:   mono.Sub(c.bootMono).Nanoseconds(),
		truth:  truth.UnixNano(),
		weight: w,
	})
	const maxSamples = 128
	if len(c.samples) > maxSamples {
		c.samples = append([]rateSample(nil), c.samples[len(c.samples)-maxSamples:]...)
	}
}

// applyRateLocked 用样本拟合频率误差；只有统计显著时才平滑更新并重锚
// （调用方需持锁）。
func (c *Clock) applyRateLocked(mono time.Time) {
	ppm, sePPM, ok := c.fitRateLocked()
	if !ok || sePPM <= 0 {
		return
	}
	// 显著性门槛：拟合值必须偏离当前值 3 个标准误以上，否则视为噪声。
	// RFC 10049 服务器（半径 ≥3s）在短窗口内几乎不会通过该门槛，
	// 这正是「宁可不做速率校正，也不要把噪声当成漂移」。
	delta := ppm - c.ratePPM
	if math.Abs(delta) < 3*sePPM {
		return
	}
	// 单次修正幅度限制，进一步抑制测量噪声。
	const maxDelta = 50.0
	if delta > maxDelta {
		delta = maxDelta
	} else if delta < -maxDelta {
		delta = -maxDelta
	}
	next := c.ratePPM + delta*0.5
	if math.Abs(next*1e-6) > c.lim.MaxRatePPM*1e-6 {
		next = math.Copysign(c.lim.MaxRatePPM, next)
	}
	if math.Abs(next-c.ratePPM) < 1e-6 {
		return
	}
	// 重锚：速率变化只影响未来推进，不改变当前读数。
	current := c.nowAtLocked(mono)
	remaining := c.slewRemainingAtLocked(mono)
	c.anchorMono = mono
	c.anchorTrue = current.UnixNano()
	c.ratePPM = next
	c.slewStart = mono
	c.slewRemaining = remaining
}

// fitRateLocked 对 (单调流逝, 真值流逝) 做加权最小二乘拟合，
// 返回频率误差 ppm 与斜率标准误（ppm）。
func (c *Clock) fitRateLocked() (float64, float64, bool) {
	n := len(c.samples)
	if n < 3 {
		return 0, 0, false
	}
	t0 := c.samples[0].mono
	y0 := c.samples[0].truth
	span := float64(c.samples[n-1].mono-t0) / 1e9
	if span < c.lim.MinRateSpan.Seconds() {
		return 0, 0, false
	}
	var sw, sx, sy, sxx, sxy float64
	for _, s := range c.samples {
		w := s.weight
		if w <= 0 {
			w = 1
		}
		x := float64(s.mono-t0) / 1e9
		y := float64(s.truth-y0) / 1e9
		sw += w
		sx += w * x
		sy += w * y
		sxx += w * x * x
		sxy += w * x * y
	}
	if sw <= 0 {
		return 0, 0, false
	}
	xbar := sx / sw
	ybar := sy / sw
	sxxC := sxx - sw*xbar*xbar
	sxyC := sxy - sw*xbar*ybar
	if sxxC < 1e-9 {
		return 0, 0, false
	}
	slope := sxyC / sxxC
	if math.IsNaN(slope) || math.IsInf(slope, 0) {
		return 0, 0, false
	}
	// 斜率标准误：σ_b = sqrt(1/Σw(x-x̄)²)（权重已是 1/σ²）。
	se := math.Sqrt(1/sxxC) * 1e6
	ppm := (slope - 1) * 1e6
	if math.Abs(ppm) > c.lim.MaxRatePPM {
		ppm = math.Copysign(c.lim.MaxRatePPM, ppm)
	}
	return ppm, se, true
}

// uncertaintyLocked 计算 mono 时刻的时间不确定度（调用方需持锁）。
func (c *Clock) uncertaintyLocked(mono time.Time) time.Duration {
	if !c.locked {
		return 0
	}
	base := time.Duration(c.lastRadius) + absInt64Duration(c.slewRemainingAtLocked(mono))
	age := mono.Sub(c.lastSyncMono) + c.ageBias
	if age < 0 {
		age = 0
	}
	drift := time.Duration(float64(age) * c.lim.HoldoverDriftPPM * 1e-6)
	return base + drift
}

// qualityLocked 计算当前质量（调用方需持锁）。
func (c *Clock) qualityLocked(mono time.Time) Quality {
	if !c.locked {
		return QualityUnlocked
	}
	if !c.verified || mono.Sub(c.lastSyncMono)+c.ageBias > c.lim.HoldoverLimit {
		return QualityHoldover
	}
	if c.uncertaintyLocked(mono) > c.lim.MaxUncertainty {
		return QualityHoldover
	}
	return QualitySynced
}

// Snapshot 时钟状态快照。
type Snapshot struct {
	UnixNano     int64         `json:"unixNano"`            // 当前时间（Unix 纳秒）
	Time         time.Time     `json:"time"`                // 当前时间
	Quality      Quality       `json:"quality"`             // synced / holdover / unlocked
	Locked       bool          `json:"locked"`              // 是否有可信锚点（含持久化恢复）
	Verified     bool          `json:"verified"`            // 本次进程内是否同步成功过
	Trustworthy  bool          `json:"trustworthy"`         // 当前时间是否可用于安全判定
	LastSync     time.Time     `json:"lastSync,omitzero"`   // 上次成功同步的（可信）时间
	SyncAge      time.Duration `json:"syncAge"`             // 距上次同步的时长
	Offset       time.Duration `json:"offset"`              // 上次观测到的「真值 - 系统墙钟」
	LastRadius   time.Duration `json:"lastRadius"`          // 上次测量的服务器半径
	Uncertainty  time.Duration `json:"uncertainty"`         // 当前时间误差上界
	RatePPM      float64       `json:"ratePpm"`             // 频率校正（ppm）
	Steps        int           `json:"steps"`               // 步进次数
	Slews        int           `json:"slews"`               // slew 次数
	LastStepSize time.Duration `json:"lastStepSize"`        // 最近一次步进量
	LastStepAt   time.Time     `json:"lastStepAt,omitzero"` // 最近一次步进时刻
	PendingSlew  time.Duration `json:"pendingSlew"`         // 尚未消化的相位校正量
	Holdover     time.Duration `json:"holdover"`            // 允许保持时长
}

// Snapshot 返回当前时钟状态。
func (c *Clock) Snapshot() Snapshot {
	return c.SnapshotAt(time.Now())
}

// SnapshotAt 返回单调时刻 mono 对应的时钟状态（测试/诊断用）。
func (c *Clock) SnapshotAt(mono time.Time) Snapshot {
	if c == nil {
		return Snapshot{
			UnixNano: mono.UnixNano(), Time: mono, Quality: QualityUnlocked,
			Holdover: DefaultLimits().HoldoverLimit,
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.nowAtLocked(mono)
	s := Snapshot{
		UnixNano:     now.UnixNano(),
		Time:         now,
		Quality:      c.qualityLocked(mono),
		Locked:       c.locked,
		Verified:     c.verified,
		LastSync:     time.Unix(0, c.lastSyncTrue),
		SyncAge:      mono.Sub(c.lastSyncMono) + c.ageBias,
		Offset:       time.Duration(c.lastOffset),
		LastRadius:   time.Duration(c.lastRadius),
		Uncertainty:  c.uncertaintyLocked(mono),
		RatePPM:      c.ratePPM,
		Steps:        c.steps,
		Slews:        c.slews,
		LastStepSize: time.Duration(c.lastStepSize),
		LastStepAt:   c.lastStepAt,
		PendingSlew:  time.Duration(c.slewRemainingAtLocked(mono)),
		Holdover:     c.lim.HoldoverLimit,
	}
	if !c.locked {
		s.LastSync = time.Time{}
		s.SyncAge = 0
		s.Uncertainty = 0
	}
	if !c.verified {
		// 只从状态文件恢复的锚点未经在线验证：允许显示，但不可信。
		s.LastStepAt = time.Time{}
	}
	s.Trustworthy = c.verified && c.locked &&
		s.SyncAge <= c.lim.HoldoverLimit &&
		s.Uncertainty <= c.lim.MaxUncertainty
	return s
}

// RatePPM 返回当前频率校正。
func (c *Clock) RatePPM() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ratePPM
}

// Trustworthy 判断当前时间是否可信（已在线同步且在保持窗口/不确定度内）。
func (c *Clock) Trustworthy() bool {
	return c.Snapshot().Trustworthy
}

// SeedHoldover 用持久化状态恢复一个未经在线验证的锚点：
// 以系统墙钟为基准施加 offset（来自上次同步的推算），并恢复频率校正。
// age 为上次同步到本次启动之间已经过去的时长（用于不确定度增长与展示）。
// 该状态 quality=holdover、trustworthy=false，直到下一次在线同步。
func (c *Clock) SeedHoldover(offset time.Duration, ratePPM float64, lastSync time.Time, radius, age time.Duration) {
	if math.IsNaN(ratePPM) || math.Abs(ratePPM) > 10000 {
		ratePPM = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locked || c.mode == modeFixed {
		return
	}
	now := time.Now()
	c.mode = modeDisciplined
	c.anchorMono = now
	c.anchorTrue = now.Add(offset).UnixNano()
	c.ratePPM = ratePPM
	c.slewStart = now
	c.slewRemaining = 0
	c.locked = true
	c.lastSyncMono = now
	c.lastSyncTrue = lastSync.UnixNano()
	c.lastOffset = int64(offset)
	if radius < 0 {
		radius = 0
	}
	c.lastRadius = int64(radius)
	c.ageBias = age
	if c.ageBias < 0 {
		c.ageBias = 0
	}
	c.steps = 0
	c.slews = 0
}

// SeedRate 用持久化的频率校正给尚未锁定的时钟一个初值。
// 系统时钟模式下不影响读数；首次在线同步后会以此为基础继续拟合。
func (c *Clock) SeedRate(ppm float64) {
	if math.IsNaN(ppm) || math.Abs(ppm) > 10000 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locked {
		return
	}
	c.ratePPM = ppm
}

// MarkTrustedForTest 把测试时钟标记为已完成在线同步（仅供测试）。
// 生产路径只能通过 Observe 获得锁定状态。
func (c *Clock) MarkTrustedForTest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != modeFixed && !c.locked {
		c.mode = modeDisciplined
		c.anchorMono = time.Now()
		c.anchorTrue = time.Now().UnixNano()
	}
	c.locked = true
	c.verified = true
	c.lastSyncMono = time.Now()
	c.lastSyncTrue = c.nowAtLocked(time.Now()).UnixNano()
	c.lastRadius = int64(10 * time.Millisecond)
	c.ageBias = 0
}

// Advance 手动拨动测试时钟。
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.fixedAdd += int64(d)
	c.mu.Unlock()
}

// Set 把测试时钟设为绝对时刻。
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.fixedBase = t
	c.fixedAdd = 0
	c.bootMono = time.Now()
	c.mu.Unlock()
}

// absDuration 返回时长绝对值。
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// absInt64Duration 把纳秒转成非负时长。
func absInt64Duration(v int64) time.Duration {
	if v < 0 {
		v = -v
	}
	return time.Duration(v)
}

// absInt64 返回 int64 绝对值。
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
