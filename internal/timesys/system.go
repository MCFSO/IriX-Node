// system.go — 时间系统管理器：多源 Roughtime 采样、一致性判定、后台同步、
// 状态持久化与对外状态快照。
//
// 一轮同步的语义：
//  1. 从服务器列表轮转挑选至多 Samples 台，并发查询（各自独立随机 nonce）；
//  2. 每台服务器给出一个带半径的时间区间；区间两两相交视为互相印证；
//  3. 取最大的相交组，组内数量达到 MinAgreement 才采纳（否则本轮不调整时钟，
//     保持 holdover 并记录错误）——孤立的「时间跳变」服务器不会被采信；
//  4. 组内按不确定度加权合成真值，交给受纪律时钟（clock.go）做步进/缓变调整；
//  5. 结果落盘（{data}/timesys.json），供重启后保持模式与状态展示。
package timesys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 默认参数。
const (
	DefaultInterval        = 5 * time.Minute
	DefaultTimeout         = 1500 * time.Millisecond
	DefaultSamples         = 3
	DefaultMinAgreement    = 2
	stateFileVersion       = 1
	minRoundInterval       = 5 * time.Second
	initialRetryDelay      = 5 * time.Second
	maxHoldoverSeedElapsed = 7 * 24 * time.Hour
	maxSeedOffset          = time.Hour
)

// Config 时间系统配置。
type Config struct {
	Enabled      bool          `json:"enabled"`
	Servers      []Server      `json:"servers"`
	Interval     time.Duration `json:"interval"`
	Timeout      time.Duration `json:"timeout"`
	Samples      int           `json:"samples"`
	MinAgreement int           `json:"minAgreement"`
	Limits       Limits        `json:"limits"`
	StateFile    string        `json:"stateFile"`
	// Logf 日志输出（由 main 注入 alog.Printf；nil = 不输出）。
	Logf func(format string, args ...any) `json:"-"`
}

// normalize 补齐默认值。
func (c *Config) normalize() {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Interval < minRoundInterval {
		c.Interval = minRoundInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Samples <= 0 {
		c.Samples = DefaultSamples
	}
	if c.MinAgreement <= 0 {
		c.MinAgreement = DefaultMinAgreement
	}
	c.Limits = c.Limits.normalize()
	if len(c.Servers) == 0 {
		c.Servers = DefaultServers()
	}
}

// serverState 单台服务器的运行状态。
type serverState struct {
	Server
	key        []byte
	profile    Profile
	lastOK     time.Time
	lastError  string
	lastOffset time.Duration
	lastRadius time.Duration
	lastRTT    time.Duration
	successes  int64
	failures   int64
	// 连续失败与冷却轮次：连续 3 次失败后暂停参与轮转 12 轮，
	// 避免死服务器/不兼容服务器反复占用每轮的超时预算。
	consecutive int
	skipUntil   int64
}

// System 时间系统（持有受纪律时钟与同步循环）。
type System struct {
	cfg    Config
	clock  *Clock
	client *Client
	logf   func(format string, args ...any)

	roundMu sync.Mutex // 串行化同步轮次（后台循环与手动触发共用）

	mu           sync.Mutex
	servers      []*serverState
	cursor       int
	round        int64
	started      bool
	stopped      bool
	stop         chan struct{}
	done         chan struct{}
	firstDone    chan struct{}
	firstOnce    sync.Once
	nextSyncAt   time.Time
	lastRoundAt  time.Time
	lastRoundDur time.Duration
	lastError    string
	lastApplyAt  time.Time
	consecutive  int
	totalRounds  int64
	totalSuccess int64

	statePath string
	profiles  map[string]Profile
}

// New 创建时间系统：校验服务器公钥、恢复持久化状态、构造受纪律时钟。
// 未启用时返回的系统仍可提供系统时钟与状态查询（Enabled=false）。
func New(cfg Config) (*System, error) {
	cfg.normalize()
	s := &System{
		cfg:       cfg,
		clock:     NewClock(cfg.Limits),
		client:    &Client{Timeout: cfg.Timeout},
		logf:      cfg.Logf,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		firstDone: make(chan struct{}),
		statePath: cfg.StateFile,
		profiles:  map[string]Profile{},
	}
	for _, srv := range cfg.Servers {
		srv.Name = strings.TrimSpace(srv.Name)
		srv.Address = strings.TrimSpace(srv.Address)
		if srv.Address == "" {
			return nil, errors.New("Roughtime 服务器地址为空")
		}
		if _, _, err := net.SplitHostPort(srv.Address); err != nil {
			return nil, fmt.Errorf("服务器 %s 地址无效（需 host:port）: %w", srv.DisplayName(), err)
		}
		key, err := decodeServerKey(srv.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("服务器 %s: %w", srv.DisplayName(), err)
		}
		profile, err := ParseProfile(string(srv.Profile))
		if err != nil {
			return nil, fmt.Errorf("服务器 %s: %w", srv.DisplayName(), err)
		}
		srv.Profile = profile
		s.servers = append(s.servers, &serverState{Server: srv, key: key, profile: profile})
	}
	s.statePath = strings.TrimSpace(s.statePath)
	if cfg.Enabled {
		s.loadState()
	}
	return s, nil
}

// Clock 返回受纪律时钟。
func (s *System) Clock() *Clock {
	if s == nil {
		return Current()
	}
	return s.clock
}

// Enabled 返回是否启用了 Roughtime 同步。
func (s *System) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

// log 输出日志（nil 安全）。
func (s *System) log(format string, args ...any) {
	if s != nil && s.logf != nil {
		s.logf(format, args...)
	}
}

// Start 启动后台同步循环（幂等）。未启用时只关闭 firstDone 以便等待方立即返回。
func (s *System) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	enabled := s.cfg.Enabled
	s.mu.Unlock()
	if !enabled {
		s.firstOnce.Do(func() { close(s.firstDone) })
		s.mu.Lock()
		close(s.done)
		s.mu.Unlock()
		return
	}
	go s.loop()
}

// loop 后台同步循环：首轮立即执行，之后按间隔/退避重试。
func (s *System) loop() {
	defer close(s.done)
	s.runRound(context.Background())
	s.firstOnce.Do(func() { close(s.firstDone) })
	for {
		delay := s.nextDelay()
		timer := time.NewTimer(delay)
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.runRound(context.Background())
		s.firstOnce.Do(func() { close(s.firstDone) })
	}
}

// Stop 停止后台循环（幂等）。
func (s *System) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	started := s.started
	if started {
		close(s.stop)
	}
	s.mu.Unlock()
	if started {
		<-s.done
	}
	// 等待可能仍在进行的手动同步轮次，避免关停后继续写状态文件。
	s.roundMu.Lock()
	s.roundMu.Unlock()
	s.persist()
}

// WaitFirstSync 等待首轮同步完成或 ctx 结束（未启用/未启动时立即返回）。
func (s *System) WaitFirstSync(ctx context.Context) {
	if s == nil {
		return
	}
	select {
	case <-s.firstDone:
	case <-ctx.Done():
	}
}

// nextDelay 计算下一轮同步延迟：成功用固定间隔，失败按 1.5 倍指数退避。
func (s *System) nextDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextDelayLocked()
}

// nextDelayLocked 与 nextDelay 相同，但调用方需持 s.mu。
func (s *System) nextDelayLocked() time.Duration {
	if s.consecutive <= 0 {
		return s.cfg.Interval
	}
	delay := initialRetryDelay
	for i := 1; i < s.consecutive && delay < s.cfg.Interval; i++ {
		delay = time.Duration(float64(delay) * 1.5)
	}
	if delay > s.cfg.Interval {
		delay = s.cfg.Interval
	}
	return delay
}

// queryOutcome 单台服务器一轮查询结果。
type queryOutcome struct {
	idx int
	res *Result
	err error
}

// runRound 执行一轮同步。
func (s *System) runRound(ctx context.Context) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	s.roundMu.Lock()
	defer s.roundMu.Unlock()

	start := time.Now()
	s.mu.Lock()
	s.round++
	picks := s.pickServersLocked(s.cfg.Samples)
	s.mu.Unlock()
	if len(picks) == 0 {
		return
	}

	results := s.queryBatch(ctx, picks)
	success := len(results)

	required := s.cfg.MinAgreement
	if required > len(s.servers) {
		required = len(s.servers)
	}
	if required < 1 {
		required = 1
	}
	// 首批响应不足以判定一致性时，追加一批尚未查询的服务器（最多一次），
	// 避免「本轮恰好轮转到不可用服务器」导致整轮失败。
	if len(results) < required {
		s.mu.Lock()
		extra := s.pickRemainingLocked(picks, s.cfg.Samples)
		s.mu.Unlock()
		if len(extra) > 0 {
			results = append(results, s.queryBatch(ctx, extra)...)
			success = len(results)
		}
	}

	s.mu.Lock()
	s.lastRoundAt = s.clock.Now()
	s.lastRoundDur = time.Since(start)
	s.totalRounds++
	s.mu.Unlock()

	if truth, unc, group, ok := pickConsensus(results, required); ok {
		ref := group[0]
		for _, r := range group[1:] {
			if r.Uncertainty() < ref.Uncertainty() {
				ref = r
			}
		}
		obs := s.clock.Observe(ref.MonoMid(), truth, unc)
		s.mu.Lock()
		s.lastError = ""
		s.consecutive = 0
		s.lastApplyAt = time.Now()
		s.totalSuccess++
		s.nextSyncAt = time.Now().Add(s.cfg.Interval)
		s.mu.Unlock()
		s.persist()

		rate := s.clock.RatePPM()
		if obs.Stepped && absDuration(obs.StepSize) > 0 {
			s.log("时间系统：已锁定 Roughtime 时间（来源 %s，本地偏差 %+.3fs，不确定度 ±%s）",
				ref.Server, obs.StepSize.Seconds(), unc.Round(time.Millisecond))
		} else {
			s.log("时间系统：同步完成（来源 %s，偏差 %+.3fs，不确定度 ±%s，频率校正 %+.1fppm）",
				ref.Server, obs.Offset.Seconds(), unc.Round(time.Millisecond), rate)
		}
		return
	}

	// 没有达到一致性门槛：保持当前时间（holdover），记录原因。
	reason := "没有可用的 Roughtime 响应"
	switch {
	case success == 0:
		reason = "所有 Roughtime 服务器均不可达"
	case len(results) > 0:
		reason = fmt.Sprintf("Roughtime 服务器时间不一致（%d 个响应，需要 %d 个一致）", len(results), required)
	}
	locked := s.clock.Snapshot().Locked
	s.mu.Lock()
	s.consecutive++
	fails := s.consecutive
	changed := s.lastError != reason
	s.lastError = reason
	s.nextSyncAt = time.Now().Add(s.nextDelayLocked())
	s.mu.Unlock()
	// 只在错误变化或第 1/5/20 次失败时输出，避免离线环境刷屏。
	if changed || fails == 1 || fails == 5 || fails%20 == 0 {
		state := "继续按上次同步结果保持（holdover）"
		if !locked {
			state = "继续使用系统时钟"
		}
		s.log("时间系统：同步失败（第 %d 次）：%s；%s", fails, reason, state)
	}
}

// queryBatch 并发查询一批服务器，更新各自状态并返回成功响应。
func (s *System) queryBatch(ctx context.Context, picks []int) []*Result {
	if len(picks) == 0 {
		return nil
	}
	outcomes := make(chan queryOutcome, len(picks))
	for _, idx := range picks {
		go func(idx int) {
			s.mu.Lock()
			srv := s.servers[idx].Server
			if cached := s.profiles[srv.Address]; cached != "" && srv.Profile == ProfileAuto {
				srv.Profile = cached
			}
			s.mu.Unlock()
			queryCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
			defer cancel()
			res, err := s.client.Query(queryCtx, srv)
			outcomes <- queryOutcome{idx: idx, res: res, err: err}
		}(idx)
	}

	results := make([]*Result, 0, len(picks))
	for range picks {
		out := <-outcomes
		s.mu.Lock()
		st := s.servers[out.idx]
		if out.err != nil {
			st.failures++
			st.consecutive++
			st.lastError = out.err.Error()
			if st.consecutive >= 3 {
				st.skipUntil = s.round + 12
			}
			s.mu.Unlock()
			continue
		}
		st.successes++
		st.consecutive = 0
		st.skipUntil = 0
		st.lastError = ""
		st.lastOK = s.clock.Now()
		st.lastRadius = out.res.Radius
		st.lastRTT = out.res.RoundTrip
		st.lastOffset = out.res.Midpoint.Sub(out.res.MonoMid())
		if out.res.Profile != ProfileAuto {
			st.profile = out.res.Profile
			s.profiles[st.Address] = out.res.Profile
		}
		s.mu.Unlock()
		results = append(results, out.res)
	}
	return results
}

// pickServersLocked 轮转挑选本轮要查询的服务器下标（调用方需持 s.mu）。
// 处于冷却期的服务器优先跳过；若全部都在冷却则退回全量轮转，保证仍会重试。
func (s *System) pickServersLocked(count int) []int {
	n := len(s.servers)
	if n == 0 {
		return nil
	}
	if count <= 0 {
		count = s.cfg.Samples
	}
	if count > n {
		count = n
	}
	var healthy []int
	for i := 0; i < n; i++ {
		idx := (s.cursor + i) % n
		if s.servers[idx].skipUntil <= s.round {
			healthy = append(healthy, idx)
		}
	}
	if len(healthy) < count {
		healthy = healthy[:0]
		for i := 0; i < n; i++ {
			healthy = append(healthy, (s.cursor+i)%n)
		}
	}
	out := append([]int(nil), healthy[:count]...)
	s.cursor = (s.cursor + count) % n
	return out
}

// pickRemainingLocked 返回尚未查询、且不在冷却期的服务器下标（最多 count 个）。
func (s *System) pickRemainingLocked(queried []int, count int) []int {
	n := len(s.servers)
	if n == 0 || count <= 0 {
		return nil
	}
	seen := make(map[int]bool, len(queried))
	for _, idx := range queried {
		seen[idx] = true
	}
	var out []int
	for i := 0; i < n && len(out) < count; i++ {
		idx := (s.cursor + i) % n
		if seen[idx] || s.servers[idx].skipUntil > s.round {
			continue
		}
		out = append(out, idx)
	}
	if len(out) == 0 {
		// 全部被跳过时，至少给轮转起点的服务器一次机会。
		for i := 0; i < n; i++ {
			idx := (s.cursor + i) % n
			if !seen[idx] {
				out = append(out, idx)
				break
			}
		}
	}
	return out
}

// pickConsensus 从多个响应中选出一致组，合成真值。
// 区间相交视为互相印证；取最大相交组，数量达到 required 才采纳。
func pickConsensus(results []*Result, required int) (time.Time, time.Duration, []*Result, bool) {
	if len(results) == 0 || required < 1 {
		return time.Time{}, 0, nil, false
	}
	best := make([]*Result, 0, len(results))
	for _, r := range results {
		group := make([]*Result, 0, len(results))
		for _, o := range results {
			if intervalsOverlap(r, o) {
				group = append(group, o)
			}
		}
		if len(group) > len(best) {
			best = group
		}
	}
	if len(best) < required {
		return time.Time{}, 0, nil, false
	}
	// 以第一台中点为参考，避免大 int64 直接做浮点加权损失精度。
	refNano := best[0].Midpoint.UnixNano()
	var sumW, sumDelta float64
	for _, r := range best {
		unc := r.Uncertainty()
		if unc < time.Millisecond {
			unc = time.Millisecond
		}
		w := 1.0 / float64(unc)
		d := float64(r.Midpoint.UnixNano() - refNano)
		sumW += w
		sumDelta += d * w
	}
	if sumW <= 0 {
		return time.Time{}, 0, nil, false
	}
	truth := time.Unix(0, refNano+int64(sumDelta/sumW))
	var unc time.Duration
	for _, r := range best {
		d := absDuration(truth.Sub(r.Midpoint)) + r.Uncertainty()
		if d > unc {
			unc = d
		}
	}
	if unc <= 0 {
		unc = time.Millisecond
	}
	return truth, unc, best, true
}

// intervalsOverlap 判断两个带半径的时间区间是否相交。
func intervalsOverlap(a, b *Result) bool {
	return !a.Midpoint.Add(-a.Uncertainty()).After(b.Midpoint.Add(b.Uncertainty())) &&
		!b.Midpoint.Add(-b.Uncertainty()).After(a.Midpoint.Add(a.Uncertainty()))
}

// SyncNow 立即执行一轮同步（串行化，遵守 ctx）。未启用时返回错误。
func (s *System) SyncNow(ctx context.Context) error {
	if s == nil || !s.cfg.Enabled {
		return errors.New("时间系统未启用，无法同步")
	}
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return errors.New("时间系统已停止，无法同步")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		s.runRound(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	lastErr := s.lastError
	s.mu.Unlock()
	if lastErr != "" {
		return errors.New(lastErr)
	}
	return nil
}

// ServerStatus 单台服务器状态。
type ServerStatus struct {
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	Profile      string    `json:"protocol"`
	LastSuccess  time.Time `json:"lastSuccess,omitzero"`
	LastError    string    `json:"lastError,omitempty"`
	OffsetMillis float64   `json:"offsetMillis"`
	RadiusMillis float64   `json:"radiusMillis"`
	RTTMillis    float64   `json:"rttMillis"`
	Successes    int64     `json:"successes"`
	Failures     int64     `json:"failures"`
}

// Status 时间系统完整状态（GET /api/time 的数据面）。
type Status struct {
	Enabled             bool           `json:"enabled"`
	Quality             Quality        `json:"quality"`
	Trustworthy         bool           `json:"trustworthy"`
	Now                 time.Time      `json:"now"`
	UnixMilli           int64          `json:"unixMilli"`
	LastSync            time.Time      `json:"lastSync,omitzero"`
	SyncAgeSeconds      float64        `json:"syncAgeSeconds"`
	OffsetMillis        float64        `json:"offsetMillis"`
	UncertaintyMillis   float64        `json:"uncertaintyMillis"`
	RatePPM             float64        `json:"ratePpm"`
	Steps               int            `json:"steps"`
	Slews               int            `json:"slews"`
	PendingSlewMillis   float64        `json:"pendingSlewMillis"`
	HoldoverSeconds     float64        `json:"holdoverSeconds"`
	IntervalSeconds     float64        `json:"intervalSeconds"`
	NextSyncInSeconds   float64        `json:"nextSyncInSeconds"`
	LastRound           time.Time      `json:"lastRound,omitzero"`
	LastRoundMillis     int64          `json:"lastRoundMillis"`
	TotalRounds         int64          `json:"totalRounds"`
	TotalSuccess        int64          `json:"totalSuccess"`
	ConsecutiveFailures int            `json:"consecutiveFailures"`
	LastError           string         `json:"lastError,omitempty"`
	Servers             []ServerStatus `json:"servers"`
}

// Status 返回当前状态快照。
func (s *System) Status() Status {
	if s == nil {
		snap := Current().Snapshot()
		return Status{
			Enabled:           false,
			Quality:           snap.Quality,
			Trustworthy:       snap.Trustworthy,
			Now:               snap.Time,
			UnixMilli:         snap.Time.UnixMilli(),
			UncertaintyMillis: float64(snap.Uncertainty) / float64(time.Millisecond),
			LastSync:          snap.LastSync,
			RatePPM:           snap.RatePPM,
		}
	}
	snap := s.clock.Snapshot()
	now := time.Now()
	s.mu.Lock()
	st := Status{
		Enabled:             s.cfg.Enabled,
		Quality:             snap.Quality,
		Trustworthy:         snap.Trustworthy,
		Now:                 snap.Time,
		UnixMilli:           snap.Time.UnixMilli(),
		LastSync:            snap.LastSync,
		SyncAgeSeconds:      snap.SyncAge.Seconds(),
		OffsetMillis:        float64(snap.Offset) / float64(time.Millisecond),
		UncertaintyMillis:   float64(snap.Uncertainty) / float64(time.Millisecond),
		RatePPM:             snap.RatePPM,
		Steps:               snap.Steps,
		Slews:               snap.Slews,
		PendingSlewMillis:   float64(snap.PendingSlew) / float64(time.Millisecond),
		HoldoverSeconds:     snap.Holdover.Seconds(),
		IntervalSeconds:     s.cfg.Interval.Seconds(),
		LastRound:           s.lastRoundAt,
		LastRoundMillis:     s.lastRoundDur.Milliseconds(),
		TotalRounds:         s.totalRounds,
		TotalSuccess:        s.totalSuccess,
		ConsecutiveFailures: s.consecutive,
		LastError:           s.lastError,
	}
	if !s.nextSyncAt.IsZero() {
		st.NextSyncInSeconds = s.nextSyncAt.Sub(now).Seconds()
	}
	for _, srv := range s.servers {
		st.Servers = append(st.Servers, ServerStatus{
			Name:         srv.DisplayName(),
			Address:      srv.Address,
			Profile:      string(srv.profile),
			LastSuccess:  srv.lastOK,
			LastError:    srv.lastError,
			OffsetMillis: float64(srv.lastOffset) / float64(time.Millisecond),
			RadiusMillis: float64(srv.lastRadius) / float64(time.Millisecond),
			RTTMillis:    float64(srv.lastRTT) / float64(time.Millisecond),
			Successes:    srv.successes,
			Failures:     srv.failures,
		})
	}
	s.mu.Unlock()
	return st
}

// persistState 持久化到磁盘的结构。
type persistState struct {
	Version              int               `json:"version"`
	RatePPM              float64           `json:"ratePpm"`
	LastSyncUnixNano     int64             `json:"lastSyncUnixNano"`
	LastSyncWallUnixNano int64             `json:"lastSyncWallUnixNano"`
	LastOffsetNanos      int64             `json:"lastOffsetNanos"`
	LastRadiusNanos      int64             `json:"lastRadiusNanos"`
	LastUncertaintyNanos int64             `json:"lastUncertaintyNanos"`
	Verified             bool              `json:"verified"`
	Profiles             map[string]string `json:"profiles,omitempty"`
}

// persist 原子写入状态文件（失败仅告警，不影响时间系统运行）。
// 只有「本次进程内在线同步成功」才写盘：离线启动的 holdover 锚点来自
// 旧状态文件，若用未验证数据覆盖它，会让后续重启丢失真实的上次同步时间。
func (s *System) persist() {
	if s == nil || !s.cfg.Enabled || s.statePath == "" {
		return
	}
	snap := s.clock.Snapshot()
	if !snap.Verified || !snap.Locked || snap.LastSync.IsZero() {
		return
	}
	s.mu.Lock()
	profiles := make(map[string]string, len(s.profiles))
	for k, v := range s.profiles {
		profiles[k] = string(v)
	}
	s.mu.Unlock()
	st := persistState{
		Version:              stateFileVersion,
		RatePPM:              snap.RatePPM,
		LastSyncUnixNano:     snap.LastSync.UnixNano(),
		LastSyncWallUnixNano: time.Now().UnixNano(),
		LastOffsetNanos:      int64(snap.Offset),
		LastRadiusNanos:      int64(snap.LastRadius),
		LastUncertaintyNanos: int64(snap.Uncertainty),
		Verified:             snap.Verified,
		Profiles:             profiles,
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o755); err != nil {
		s.log("时间系统：状态文件目录创建失败: %v", err)
		return
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.log("时间系统：状态文件写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.statePath); err != nil {
		s.log("时间系统：状态文件落盘失败: %v", err)
	}
}

// loadState 读取持久化状态：恢复频率校正与保持锚点、协议探测缓存。
// 文件不存在/损坏/过期都不阻断启动（仅告警）。
func (s *System) loadState() {
	if s.statePath == "" {
		return
	}
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log("时间系统：读取状态文件 %s 失败: %v", s.statePath, err)
		}
		return
	}
	if len(data) > 64<<10 {
		s.log("时间系统：状态文件 %s 异常过大（%d 字节），忽略", s.statePath, len(data))
		return
	}
	var st persistState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateFileVersion {
		s.log("时间系统：状态文件 %s 无效，忽略（将重新同步）", s.statePath)
		return
	}
	for k, v := range st.Profiles {
		if p, err := ParseProfile(v); err == nil {
			s.profiles[k] = p
		}
	}
	rate := st.RatePPM
	if rate > 1000 || rate < -1000 {
		rate = 0
	}
	s.clock.SeedRate(rate)
	if !st.Verified || st.LastSyncUnixNano <= 0 || st.LastSyncWallUnixNano <= 0 {
		return
	}
	lastSync := time.Unix(0, st.LastSyncUnixNano)
	elapsedWall := time.Duration(time.Now().UnixNano() - st.LastSyncWallUnixNano)
	if elapsedWall < 0 || elapsedWall > maxHoldoverSeedElapsed {
		return
	}
	// 以系统墙钟为基准，用持久化的「真值-墙钟」偏差与频率校正推算当前时间。
	offset := time.Duration(st.LastOffsetNanos)
	if elapsedWall > 0 && (rate > 1 || rate < -1) {
		offset += time.Duration(float64(elapsedWall) * rate * 1e-6)
	}
	if absDuration(offset) > maxSeedOffset {
		return
	}
	radius := time.Duration(st.LastRadiusNanos)
	driftPPM := s.cfg.Limits.HoldoverDriftPPM
	age := elapsedWall
	// 上次落盘时若已处于保持状态，其不确定度里已包含漂移；按漂移速率折算成等效时长。
	if extra := time.Duration(st.LastUncertaintyNanos) - radius; extra > 0 && driftPPM > 0 {
		age += time.Duration(float64(extra) / (driftPPM * 1e-6))
	}
	s.clock.SeedHoldover(offset, rate, lastSync, radius, age)
	s.log("时间系统：已从状态文件恢复保持锚点（上次同步 %s 前，推算偏差 %+.3fs，频率校正 %+.1fppm）",
		age.Round(time.Second), offset.Seconds(), rate)
}
