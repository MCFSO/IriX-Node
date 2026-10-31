// DDoS / CC 防御：进程内始终开启的 HTTP 层防护，**没有关闭开关**。
//
// 中间件在路由之前依次执行三道闸门，命中任意一道即拒绝（403/429/503），
// 被拒绝的请求不会进入审计、保险库门禁与业务处理器：
//
//  1. 封禁表：来源 IP / User-Agent 的手动或自动封禁（自动封禁到期自动解除，
//     手动封禁持久化到 {data}/defense.json，重启后仍生效）；
//  2. HTTP 头部规范校验（defense_headers.go）：请求走私形态、重复单例头、
//     Via / Forwarded / X-Forwarded-* 等代理链标记、方法覆盖头、非法 Upgrade、
//     超量/超大请求头；
//  3. 频率与并发限制：
//     - 单 IP 令牌桶（请求/秒）与在途并发上限——DDoS/CC 最直接的来源维度；
//     - 单 User-Agent 令牌桶与在途并发上限——同一 User-Agent 同时访问过多时
//       直接封禁该 UA（跨 IP 聚合，对应需求「同时间访问过多即屏蔽该 UA」）；
//     - 全局在途上限——进程自我保护，避免请求洪峰把内存/回调栈打满。
//
// 环回地址（127.0.0.0/8、::1）视为本机面板/CLI/同机反向代理：不受频率限制与
// 自动封禁影响（否则节点自身的面板会被自己的防御锁死），但请求走私/重复单例头等
// 硬违规仍然拒绝；人工/配置的 User-Agent 封禁对本机来源同样生效。
// 同机反代追加的 X-Forwarded-* 只计数不拦截。
//
// 自动封禁时长随「短时间内重复触发」翻倍（上限 24 小时）；
// 手动/配置封禁优先于自动封禁，不会被自动策略缩短或覆盖。
//
// 防御阈值可以调整（config.json defense 块），但**不存在关闭开关**：
// newHTTPServer 无条件挂载本中间件，配置里写 defense.enabled=false 只会被
// 忽略并在启动日志中提示。

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 防御默认阈值。全部可通过 config.json defense 块调整；没有关闭开关。
const (
	defenseDefaultIPRate         = 60.0   // 单 IP 每秒补充令牌
	defenseDefaultIPBurst        = 120    // 单 IP 突发容量
	defenseDefaultIPConcurrent   = 64     // 单 IP 在途请求上限
	defenseDefaultUARate         = 40.0   // 单 User-Agent 每秒补充令牌（跨 IP 聚合）
	defenseDefaultUABurst        = 80     // 单 User-Agent 突发容量
	defenseDefaultUAConcurrent   = 32     // 单 User-Agent 在途请求上限（超过即封禁该 UA）
	defenseDefaultGlobalRate     = 1000.0 // 全节点匿名请求每秒上限（跨 IP 聚合，防海量 IP 洪峰）
	defenseDefaultGlobalBurst    = 2000   // 全节点匿名请求突发容量
	defenseDefaultGlobalInflight = 4096   // 全节点在途请求上限
	defenseDefaultBan            = 10 * time.Minute
	defenseBanMax                = 24 * time.Hour
	defenseViolationWindow       = 10 * time.Minute
	defenseViolationLimit        = 3
	defenseThrottleWindow        = time.Minute
	defenseThrottleBanLimit      = 30 // 单 IP 每分钟被限流次数达到即封禁
	defenseMaxIPEntries          = 50000
	defenseMaxUAEntries          = 20000
	defenseMaxBans               = 20000 // 封禁表上限（防攻击者用随机 UA/大量 IP 撑爆内存）
	defenseEntryIdleTTL          = 15 * time.Minute
	defenseSweepInterval         = 30 * time.Second
	defenseEventKeep             = 100
	defenseEventShow             = 50
	defenseTopUALimit            = 20
	defenseUAValueMax            = 256
	defenseBanReasonMax          = 200
)

// defenseConfig 防御阈值（无 enabled 字段：防御恒开）。
type defenseConfig struct {
	IPRate           float64
	IPBurst          int
	IPConcurrent     int
	UARate           float64
	UABurst          int
	UAConcurrent     int
	GlobalRate       float64
	GlobalBurst      int
	GlobalConcurrent int
	BanDuration      time.Duration
	ViolationWindow  time.Duration
	ViolationLimit   int
	MaxIPs           int
	MaxUAs           int
	IdleTTL          time.Duration
}

// defaultDefenseConfig 返回内置默认阈值。
func defaultDefenseConfig() defenseConfig {
	return defenseConfig{
		IPRate:           defenseDefaultIPRate,
		IPBurst:          defenseDefaultIPBurst,
		IPConcurrent:     defenseDefaultIPConcurrent,
		UARate:           defenseDefaultUARate,
		UABurst:          defenseDefaultUABurst,
		UAConcurrent:     defenseDefaultUAConcurrent,
		GlobalRate:       defenseDefaultGlobalRate,
		GlobalBurst:      defenseDefaultGlobalBurst,
		GlobalConcurrent: defenseDefaultGlobalInflight,
		BanDuration:      defenseDefaultBan,
		ViolationWindow:  defenseViolationWindow,
		ViolationLimit:   defenseViolationLimit,
		MaxIPs:           defenseMaxIPEntries,
		MaxUAs:           defenseMaxUAEntries,
		IdleTTL:          defenseEntryIdleTTL,
	}
}

// defenseOptions 启动时的调优项（0/空 = 保持默认值；没有关闭项）。
type defenseOptions struct {
	IPConcurrent      int
	UAConcurrent      int
	IPRate            int
	UARate            int
	GlobalRate        int
	GlobalBurst       int
	BanMinutes        int
	BlockedUserAgents []string
}

// defenseIP 单来源 IP 的限速/并发/违规计数。
type defenseIP struct {
	tokens     float64   // 令牌桶余量
	last       time.Time // 上次补充令牌时刻
	inflight   int       // 在途请求数
	lastSeen   time.Time // 最近访问时刻（空闲回收）
	violations int       // 头部违规计分
	violStart  time.Time // 计分窗口起点
	throttles  int       // 限流次数
	throttleAt time.Time // 限流窗口起点
	banStrikes int       // 被自动封禁次数（用于时长升级）
}

// defenseUA 单 User-Agent 的跨 IP 聚合计数。
type defenseUA struct {
	tokens     float64
	last       time.Time
	inflight   int
	lastSeen   time.Time
	requests   int64
	banStrikes int
}

// defenseBan 封禁记录（手动/配置/自动共用）。
type defenseBan struct {
	Kind      string `json:"kind"`      // ip | ua
	Value     string `json:"value"`     // IP 或 User-Agent 原文
	Until     int64  `json:"until"`     // 到期 Unix 毫秒；0 = 永久
	Reason    string `json:"reason"`    // 中文原因
	Source    string `json:"source"`    // manual | config | auto
	CreatedAt int64  `json:"createdAt"` // 创建 Unix 毫秒
}

// defenseEvent 防御事件（状态接口展示最近若干条）。
type defenseEvent struct {
	At     int64  `json:"at"`     // Unix 毫秒
	Kind   string `json:"kind"`   // ban-ip | ban-ua | manual-ban | unban
	Value  string `json:"value"`  // 被处置对象
	Reason string `json:"reason"` // 原因
	Until  int64  `json:"until"`  // 到期 Unix 毫秒；0 = 永久
}

// defenseVerdict 一次拒绝的处置结果。
type defenseVerdict struct {
	status  int           // HTTP 状态码
	message string        // 中文错误消息（写回客户端）
	retry   time.Duration // >0 时写 Retry-After
	rule    string        // 规则标识（写 X-Defense 响应头）
}

// defenseState 恒开防御的运行时状态。所有字段由 mu 保护。
type defenseState struct {
	d   *Daemon
	cfg defenseConfig

	startOnce   sync.Once
	mu          sync.Mutex
	ips         map[string]*defenseIP
	uas         map[string]*defenseUA
	bans        map[string]*defenseBan
	headerRules map[string]int64 // 头部规则命中计数（固定规则集，内存有界）
	events      []defenseEvent
	inflight    int

	// 全节点匿名请求令牌桶：海量 IP 轮换时由聚合预算兜底，
	// 避免逐个 IP 的限速被“换 IP”绕过。
	globalTokens float64
	globalLast   time.Time

	requests   atomic.Int64 // 进入防御的请求总数
	rejected   atomic.Int64 // 被拒绝的请求总数
	rateLimits atomic.Int64 // 限流/并发拒绝数
	headerHits atomic.Int64 // 头部违规命中数
	bannedHits atomic.Int64 // 命中封禁表的拒绝数
	ipBans     atomic.Int64 // 自动封禁 IP 次数
	uaBans     atomic.Int64 // 自动封禁 UA 次数
	globalShed atomic.Int64 // 全节点预算拒绝数（匿名洪峰）
}

// newDefenseState 创建防御状态（加载手动封禁持久化文件）。
func newDefenseState(d *Daemon) *defenseState {
	s := &defenseState{
		d:           d,
		cfg:         defaultDefenseConfig(),
		ips:         map[string]*defenseIP{},
		uas:         map[string]*defenseUA{},
		bans:        map[string]*defenseBan{},
		headerRules: map[string]int64{},
	}
	s.mu.Lock()
	s.loadLocked()
	s.mu.Unlock()
	return s
}

// applyOptions 应用 config.json defense 块的调优项（防御本身不可关闭）。
func (s *defenseState) applyOptions(o defenseOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.IPConcurrent > 0 {
		s.cfg.IPConcurrent = o.IPConcurrent
	}
	if o.UAConcurrent > 0 {
		s.cfg.UAConcurrent = o.UAConcurrent
	}
	if o.IPRate > 0 {
		s.cfg.IPRate = float64(o.IPRate)
		s.cfg.IPBurst = o.IPRate * 2
	}
	if o.UARate > 0 {
		s.cfg.UARate = float64(o.UARate)
		s.cfg.UABurst = o.UARate * 2
	}
	if o.GlobalRate > 0 {
		s.cfg.GlobalRate = float64(o.GlobalRate)
		s.cfg.GlobalBurst = o.GlobalRate * 2
	}
	if o.GlobalBurst > 0 {
		s.cfg.GlobalBurst = o.GlobalBurst
	}
	if o.BanMinutes > 0 {
		s.cfg.BanDuration = time.Duration(o.BanMinutes) * time.Minute
	}
	for _, raw := range o.BlockedUserAgents {
		ua := strings.TrimSpace(raw)
		if ua == "" || len(ua) > defenseUAValueMax {
			continue
		}
		if old, ok := s.bans[defenseBanKey("ua", ua)]; ok && old.Source == "manual" {
			continue // 人工封禁优先
		}
		s.bans[defenseBanKey("ua", ua)] = &defenseBan{
			Kind: "ua", Value: ua, Until: 0,
			Reason: "配置文件 defense.blockedUserAgents 静态封禁",
			Source: "config", CreatedAt: time.Now().UnixMilli(),
		}
	}
}

// reconfigure 仅供测试/运行期调优：整体替换阈值（不改变恒开语义）。
func (s *defenseState) reconfigure(cfg defenseConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// cfgSnapshot 返回当前阈值副本（启动日志/测试使用）。
func (s *defenseState) cfgSnapshot() defenseConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// startCleanup 惰性启动空闲/过期条目清理任务（首次请求时登记）。
func (s *defenseState) startCleanup() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if s.d == nil || s.d.workers == nil {
			return
		}
		s.d.workers.start(func(ctx context.Context) {
			ticker := time.NewTicker(defenseSweepInterval)
			defer ticker.Stop()
			for {
				if !waitNextTick(ctx, ticker.C) {
					return
				}
				s.sweep(time.Now())
			}
		})
	})
}

// sweep 清理过期封禁与空闲跟踪条目（防内存无界增长）。
func (s *defenseState) sweep(now time.Time) {
	s.mu.Lock()
	s.sweepLocked(now)
	s.mu.Unlock()
}

func (s *defenseState) sweepLocked(now time.Time) {
	for key, b := range s.bans {
		if b.Until > 0 && b.Until <= now.UnixMilli() {
			delete(s.bans, key)
		}
	}
	for ip, e := range s.ips {
		if e.inflight != 0 || now.Sub(e.lastSeen) <= s.cfg.IdleTTL {
			continue
		}
		if _, banned := s.bans[defenseBanKey("ip", ip)]; !banned {
			delete(s.ips, ip)
		}
	}
	for ua, e := range s.uas {
		if e.inflight != 0 || now.Sub(e.lastSeen) <= s.cfg.IdleTTL {
			continue
		}
		if _, banned := s.bans[defenseBanKey("ua", ua)]; !banned {
			delete(s.uas, ua)
		}
	}
}

// defenseMiddleware 恒开防御中间件：封禁表 → 头部规范 → 频率/并发。
// 位置必须在业务路由与审计之前，攻击流量不产生审计落盘开销。
func (d *Daemon) defenseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := d.defense
		if st == nil {
			// 防御对象缺失属于装配错误：fail-closed，绝不放行。
			writeError(w, http.StatusServiceUnavailable, "防御子系统未就绪，请求被拒绝")
			return
		}
		st.startCleanup()
		st.requests.Add(1)

		ip := clientIP(r)
		ua := requestUserAgent(r)
		loopback := isLoopbackSource(ip)
		now := time.Now()

		// 1) 封禁表（手动/配置/自动）
		// 环回来源的「解除封禁」请求豁免 UA 封禁：管理员误封自己的浏览器 UA 后，
		// 仍可通过本机面板/curl 调用 DELETE /api/defense/ban 恢复（认证照常校验）。
		if !defenseRecoveryRequest(r, loopback) {
			if b, ok := st.activeBan(ip, ua, loopback); ok {
				st.rejected.Add(1)
				st.bannedHits.Add(1)
				d.defenseReject(w, r, &defenseVerdict{
					status:  http.StatusForbidden,
					message: defenseBanMessage(b),
					retry:   defenseBanRemaining(b),
					rule:    "ban",
				})
				return
			}
		}

		// 2) HTTP 头部规范（代理链标记 / 请求走私 / 重复单例头 ...）
		if v := st.inspectHeaders(ip, r, loopback, now); v != nil {
			st.rejected.Add(1)
			d.defenseReject(w, r, v)
			return
		}

		// 3) 频率与并发
		release, v := st.admit(ip, ua, loopback, now)
		if v != nil {
			st.rejected.Add(1)
			d.defenseReject(w, r, v)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// defenseReject 输出防御拒绝响应：保留 CORS 头（浏览器前端能读到原因），
// 附带 X-Defense 规则标识与 Retry-After（封禁/限流时）。
func (d *Daemon) defenseReject(w http.ResponseWriter, r *http.Request, v *defenseVerdict) {
	d.applyCORS(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Defense", v.rule)
	if v.retry > 0 {
		w.Header().Set("Retry-After", defenseRetrySeconds(v.retry))
	}
	writeError(w, v.status, v.message)
}

// defenseRecoveryRequest 判断是否为环回来源的「解除封禁」请求。
// 仅豁免封禁表检查，后续仍要经过头部规范、频率限制、认证与管理员校验。
func defenseRecoveryRequest(r *http.Request, loopback bool) bool {
	return loopback && r.Method == http.MethodDelete && r.URL.Path == "/api/defense/ban"
}

// inspectHeaders 头部规范校验：硬违规立即封禁来源；其他违规拒绝本次请求，
// 同一来源累计达到阈值后封禁。环回来源信任同机反代，只计数不拦截软违规。
func (s *defenseState) inspectHeaders(ip string, r *http.Request, loopback bool, now time.Time) *defenseVerdict {
	findings := inspectDefenseHeaders(r)
	if len(findings) == 0 {
		return nil
	}
	s.headerHits.Add(1)
	var hard, soft []defenseHeaderFinding
	for _, f := range findings {
		if f.Severity == defenseHeaderBan {
			hard = append(hard, f)
		} else {
			soft = append(soft, f)
		}
	}

	s.mu.Lock()
	s.noteHeaderRulesLocked(findings)
	if len(hard) > 0 {
		if !loopback {
			e := s.ipLocked(ip, now)
			s.banIPLocked(e, ip, "HTTP 头部硬违规: "+hard[0].Rule)
		}
		s.mu.Unlock()
		return &defenseVerdict{
			status:  http.StatusForbidden,
			message: "请求头不符合 HTTP 规范（请求走私/重复单例头等），已被 DDoS/CC 防御拦截",
			rule:    "header",
		}
	}
	if loopback {
		// 同机反向代理可能追加 X-Forwarded-*；信任环回来源，仅计数。
		s.mu.Unlock()
		return nil
	}
	e := s.ipLocked(ip, now)
	if e.violStart.IsZero() || now.Sub(e.violStart) > s.cfg.ViolationWindow {
		e.violStart, e.violations = now, 0
	}
	e.violations += len(soft)
	if e.violations >= s.cfg.ViolationLimit {
		e.violations = 0
		s.banIPLocked(e, ip, "HTTP 头部规范违规累计达到阈值")
	}
	s.mu.Unlock()
	return &defenseVerdict{
		status:  http.StatusForbidden,
		message: "请求头包含代理链/伪造标记（Via、Forwarded、X-Forwarded-* 等），已被 DDoS/CC 防御拦截",
		rule:    "header",
	}
}

// noteHeaderRulesLocked 累计头部规则命中次数（规则集固定，内存有界）。
func (s *defenseState) noteHeaderRulesLocked(findings []defenseHeaderFinding) {
	if s.headerRules == nil {
		s.headerRules = map[string]int64{}
	}
	for _, f := range findings {
		s.headerRules[f.Rule]++
	}
}

// activeBan 查询来源 IP / User-Agent 是否处于封禁期。
//
// 环回来源豁免自动封禁与 IP 封禁（避免节点把自己锁死），但**人工/配置的
// User-Agent 封禁仍然生效**——那是管理员的显式意图，理应对本机面板同样适用。
func (s *defenseState) activeBan(ip, ua string, loopback bool) (defenseBan, bool) {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	if ua != "" {
		key := defenseBanKey("ua", ua)
		if b, ok := s.bans[key]; ok {
			if b.Until == 0 || b.Until > now {
				if !loopback || b.Source == "manual" || b.Source == "config" {
					return *b, true
				}
			} else {
				delete(s.bans, key)
			}
		}
	}
	if loopback {
		return defenseBan{}, false
	}
	key := defenseBanKey("ip", ip)
	if b, ok := s.bans[key]; ok {
		if b.Until == 0 || b.Until > now {
			return *b, true
		}
		delete(s.bans, key)
	}
	return defenseBan{}, false
}

// admit 频率与并发准入。放行时返回在途计数释放函数；拒绝时返回处置结果。
func (s *defenseState) admit(ip, ua string, loopback bool, now time.Time) (func(), *defenseVerdict) {
	if loopback {
		// 本机面板/CLI：不做频率与并发限制，避免节点把自己锁死。
		return func() {}, nil
	}
	s.mu.Lock()

	// 全局在途上限：进程自我保护。
	if s.inflight >= s.cfg.GlobalConcurrent {
		s.mu.Unlock()
		return nil, &defenseVerdict{
			status:  http.StatusServiceUnavailable,
			message: "节点请求过载，已被 DDoS 防御限流，请稍后重试",
			retry:   time.Second,
			rule:    "overload",
		}
	}

	// 全节点匿名预算：跨 IP 聚合的请求洪峰兜底。海量 IP 轮换时，
	// 单 IP 令牌桶会被绕开，这里给出总量上限；环回面板不受影响。
	if !takeDefenseToken(&s.globalTokens, &s.globalLast, s.cfg.GlobalRate, s.cfg.GlobalBurst, now) {
		s.mu.Unlock()
		s.globalShed.Add(1)
		return nil, &defenseVerdict{
			status:  http.StatusServiceUnavailable,
			message: "节点正在承受请求洪峰，已被 DDoS/CC 防御限流，请稍后重试",
			retry:   time.Second,
			rule:    "overload",
		}
	}

	// User-Agent 维度（跨 IP 聚合）：并发/频率超限即封禁该 UA。
	var u *defenseUA
	if ua != "" {
		u = s.uaLocked(ua, now)
		if u != nil {
			if u.inflight >= s.cfg.UAConcurrent {
				u.banStrikes++
				s.banLocked("ua", ua, "同一 User-Agent 并发请求过多", "auto", u.banStrikes)
				s.mu.Unlock()
				s.rateLimits.Add(1)
				return nil, &defenseVerdict{
					status:  http.StatusTooManyRequests,
					message: "该 User-Agent 并发请求过多，已被 DDoS/CC 防御临时封禁",
					retry:   time.Second,
					rule:    "ua-concurrency",
				}
			}
			if !takeDefenseToken(&u.tokens, &u.last, s.cfg.UARate, s.cfg.UABurst, now) {
				u.banStrikes++
				s.banLocked("ua", ua, "同一 User-Agent 请求频率过高", "auto", u.banStrikes)
				s.mu.Unlock()
				s.rateLimits.Add(1)
				return nil, &defenseVerdict{
					status:  http.StatusTooManyRequests,
					message: "该 User-Agent 请求过于频繁，已被 DDoS/CC 防御临时封禁",
					retry:   time.Second,
					rule:    "ua-rate",
				}
			}
		}
	}

	// 来源 IP 维度：令牌桶 + 在途并发。
	e := s.ipLocked(ip, now)
	if !takeDefenseToken(&e.tokens, &e.last, s.cfg.IPRate, s.cfg.IPBurst, now) {
		s.noteThrottleLocked(ip, e, now)
		s.mu.Unlock()
		s.rateLimits.Add(1)
		return nil, &defenseVerdict{
			status:  http.StatusTooManyRequests,
			message: "请求过于频繁，已被 DDoS/CC 防御限流，请稍后重试",
			retry:   time.Second,
			rule:    "ip-rate",
		}
	}
	if e.inflight >= s.cfg.IPConcurrent {
		s.noteThrottleLocked(ip, e, now)
		s.mu.Unlock()
		s.rateLimits.Add(1)
		return nil, &defenseVerdict{
			status:  http.StatusTooManyRequests,
			message: "同一来源并发请求过多，已被 DDoS/CC 防御限流，请稍后重试",
			retry:   time.Second,
			rule:    "ip-concurrency",
		}
	}

	if u != nil {
		u.inflight++
		u.requests++
	}
	e.inflight++
	s.inflight++
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		if e.inflight > 0 {
			e.inflight--
		}
		if u != nil && u.inflight > 0 {
			u.inflight--
		}
		if s.inflight > 0 {
			s.inflight--
		}
		s.mu.Unlock()
	}, nil
}

// noteThrottleLocked 记录一次限流；同一 IP 每分钟被限流达到阈值即自动封禁。
func (s *defenseState) noteThrottleLocked(ip string, e *defenseIP, now time.Time) {
	if e.throttleAt.IsZero() || now.Sub(e.throttleAt) > defenseThrottleWindow {
		e.throttleAt, e.throttles = now, 0
	}
	e.throttles++
	if e.throttles >= defenseThrottleBanLimit {
		e.throttles = 0
		s.banIPLocked(e, ip, "请求频率持续超限")
	}
}

// banIPLocked 自动封禁来源 IP（调用方必须持有 s.mu 且传入对应条目）。
func (s *defenseState) banIPLocked(e *defenseIP, ip, reason string) defenseBan {
	e.banStrikes++
	return s.banLocked("ip", ip, reason, "auto", e.banStrikes)
}

// banLocked 写入封禁记录并按触发次数升级时长。
// 手动/配置封禁优先：已存在时直接返回，不被自动策略覆盖或缩短。
func (s *defenseState) banLocked(kind, value, reason, source string, strikes int) defenseBan {
	key := defenseBanKey(kind, value)
	now := time.Now()
	if old, ok := s.bans[key]; ok {
		if old.Source == "manual" || old.Source == "config" {
			return *old
		}
		if old.Until == 0 {
			return *old
		}
	}
	if strikes < 1 {
		strikes = 1
	}
	dur := s.cfg.BanDuration
	for i := 1; i < strikes; i++ {
		dur *= 2
		if dur >= defenseBanMax {
			dur = defenseBanMax
			break
		}
	}
	b := defenseBan{
		Kind: kind, Value: value, Until: now.Add(dur).UnixMilli(),
		Reason: reason, Source: source, CreatedAt: now.UnixMilli(),
	}
	// 封禁表上限：攻击者可用大量随机 UA/IP 触发自动封禁撑爆内存。
	// 表满时随机淘汰一条自动封禁（期望 O(1)），手动/配置封禁永不淘汰；
	// 不在此处做全表过期扫描，避免攻击者用封禁请求制造 O(n) CPU。
	if len(s.bans) >= defenseMaxBans {
		victim := ""
		for key, old := range s.bans {
			if old.Source == "auto" {
				victim = key
				break
			}
		}
		if victim == "" {
			return defenseBan{}
		}
		delete(s.bans, victim)
	}
	s.bans[key] = &b
	s.eventLocked("ban-"+kind, value, reason, b.Until)
	if kind == "ua" {
		s.uaBans.Add(1)
	} else {
		s.ipBans.Add(1)
	}
	if s.d != nil {
		label := "IP"
		if kind == "ua" {
			label = "User-Agent"
		}
		s.d.auditLogf("防御 | 封禁 %s %q | 原因: %s | 时长: %s | 触发: %s",
			label, value, reason, defenseDurationText(dur), defenseSourceLabel(source))
	}
	return b
}

// ipLocked 取/建来源 IP 条目。表满时淘汰一个随机条目继续服务：
// 海量 IP 轮换攻击下绝不能“表满就拒绝所有新来源”（那等于攻击者用 5 万个 IP
// 把全部正常用户挡在门外）；总量约束由单 IP 限速/全局在途上限与上游清洗承担。
func (s *defenseState) ipLocked(ip string, now time.Time) *defenseIP {
	if e, ok := s.ips[ip]; ok {
		e.lastSeen = now
		return e
	}
	if len(s.ips) >= s.cfg.MaxIPs {
		for key := range s.ips {
			delete(s.ips, key)
			break
		}
	}
	e := &defenseIP{lastSeen: now}
	s.ips[ip] = e
	return e
}

// uaLocked 取/建 User-Agent 条目；表满时随机淘汰一条（封禁记录独立存放，
// 淘汰跟踪条目不解除封禁）。随机淘汰是 O(1)，避免随机 UA 攻击制造 O(n) 扫描。
func (s *defenseState) uaLocked(ua string, now time.Time) *defenseUA {
	if e, ok := s.uas[ua]; ok {
		e.lastSeen = now
		return e
	}
	if len(s.uas) >= s.cfg.MaxUAs {
		for key := range s.uas {
			delete(s.uas, key)
			break
		}
	}
	e := &defenseUA{lastSeen: now}
	s.uas[ua] = e
	return e
}

// takeDefenseToken 令牌桶：按经过时间补充，最多补到 burst；不足 1 个即拒绝。
func takeDefenseToken(tokens *float64, last *time.Time, rate float64, burst int, now time.Time) bool {
	if last.IsZero() {
		*tokens = float64(burst)
	} else if rate > 0 {
		*tokens += now.Sub(*last).Seconds() * rate
	}
	*last = now
	if *tokens > float64(burst) {
		*tokens = float64(burst)
	}
	if *tokens < 1 {
		return false
	}
	*tokens--
	return true
}

// eventLocked 追加防御事件（只保留最近 defenseEventKeep 条）。
func (s *defenseState) eventLocked(kind, value, reason string, until int64) {
	s.events = append(s.events, defenseEvent{
		At: time.Now().UnixMilli(), Kind: kind, Value: value, Reason: reason, Until: until,
	})
	if len(s.events) > defenseEventKeep {
		s.events = append([]defenseEvent(nil), s.events[len(s.events)-defenseEventKeep:]...)
	}
}

// ---------- 手动封禁（持久化） ----------

// defenseFile 手动封禁持久化文件路径。
func (s *defenseState) defenseFile() string {
	if s.d == nil || strings.TrimSpace(s.d.DataDir) == "" {
		return ""
	}
	return filepath.Join(s.d.DataDir, "defense.json")
}

// defensePersist 持久化文件结构（只保存手动封禁：自动封禁随进程重启失效，
// 配置封禁由 config.json 重新注入）。
type defensePersist struct {
	ManualBans []defenseBan `json:"manualBans"`
}

// loadLocked 从磁盘加载手动封禁（文件不存在/损坏时按空表处理）。
func (s *defenseState) loadLocked() {
	file := s.defenseFile()
	if file == "" {
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if !os.IsNotExist(err) {
			alog.Printf("警告: 读取防御封禁文件 %s 失败: %v（按空表处理）", file, err)
		}
		return
	}
	var doc defensePersist
	if err := json.Unmarshal(data, &doc); err != nil {
		alog.Printf("警告: 防御封禁文件 %s 损坏（%v），按空表处理", file, err)
		return
	}
	now := time.Now().UnixMilli()
	for _, b := range doc.ManualBans {
		kind := strings.ToLower(strings.TrimSpace(b.Kind))
		value := strings.TrimSpace(b.Value)
		if (kind != "ip" && kind != "ua") || value == "" {
			continue
		}
		if b.Until > 0 && b.Until <= now {
			continue
		}
		b.Kind, b.Value, b.Source = kind, value, "manual"
		if b.CreatedAt == 0 {
			b.CreatedAt = now
		}
		s.bans[defenseBanKey(kind, value)] = &b
	}
}

// persistLocked 原子写入手动封禁列表（调用方持有 s.mu）。
func (s *defenseState) persistLocked() {
	file := s.defenseFile()
	if file == "" {
		return
	}
	var doc defensePersist
	for _, b := range s.bans {
		if b.Source == "manual" {
			doc.ManualBans = append(doc.ManualBans, *b)
		}
	}
	sort.Slice(doc.ManualBans, func(i, j int) bool {
		if doc.ManualBans[i].Kind != doc.ManualBans[j].Kind {
			return doc.ManualBans[i].Kind < doc.ManualBans[j].Kind
		}
		return doc.ManualBans[i].Value < doc.ManualBans[j].Value
	})
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		alog.Printf("警告: 序列化防御封禁失败: %v", err)
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		alog.Printf("警告: 写入防御封禁文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, file); err != nil {
		alog.Printf("警告: 替换防御封禁文件失败: %v", err)
	}
}

// addManualBan 人工封禁（ttl=0 表示永久）。返回最终生效的封禁记录。
func (s *defenseState) addManualBan(kind, value string, ttl time.Duration, reason string) (defenseBan, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.TrimSpace(value)
	if kind != "ip" && kind != "ua" {
		return defenseBan{}, fmt.Errorf("封禁类型无效: %q（须为 ip 或 ua）", kind)
	}
	if kind == "ip" {
		if ip := net.ParseIP(stripIPv6Zone(value)); ip == nil {
			return defenseBan{}, fmt.Errorf("IP 地址无效: %q", value)
		}
		value = stripIPv6Zone(value)
	}
	if kind == "ua" {
		if value == "" {
			return defenseBan{}, fmt.Errorf("User-Agent 不能为空")
		}
		if len(value) > defenseUAValueMax {
			return defenseBan{}, fmt.Errorf("User-Agent 过长（上限 %d 字节）", defenseUAValueMax)
		}
	}
	reason = sanitizeLog(strings.TrimSpace(reason))
	if reason == "" {
		reason = "管理员手动封禁"
	}
	if len(reason) > defenseBanReasonMax {
		reason = reason[:defenseBanReasonMax]
	}
	now := time.Now()
	var until int64
	if ttl > 0 {
		until = now.Add(ttl).UnixMilli()
	}
	b := defenseBan{
		Kind: kind, Value: value, Until: until,
		Reason: reason, Source: "manual", CreatedAt: now.UnixMilli(),
	}
	s.mu.Lock()
	s.bans[defenseBanKey(kind, value)] = &b
	s.eventLocked("manual-ban", value, reason, until)
	s.persistLocked()
	s.mu.Unlock()

	if s.d != nil {
		s.d.auditLogf("防御 | 人工封禁 %s %q | 原因: %s | 时长: %s",
			defenseKindLabel(kind), value, reason, defenseTTLText(ttl))
	}
	return b, nil
}

// removeBan 解除封禁（自动/配置/手动均可解除）。返回是否存在该记录。
func (s *defenseState) removeBan(kind, value string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.TrimSpace(value)
	key := defenseBanKey(kind, value)
	s.mu.Lock()
	_, ok := s.bans[key]
	if ok {
		delete(s.bans, key)
		s.eventLocked("unban", value, "管理员解除封禁", 0)
		s.persistLocked()
	}
	s.mu.Unlock()
	if ok && s.d != nil {
		s.d.auditLogf("防御 | 解除封禁 %s %q", defenseKindLabel(kind), value)
	}
	return ok
}

// ---------- 状态快照 ----------

// snapshot 生成状态接口响应（阈值、统计、封禁表、UA 排行、头部规则命中、最近事件）。
func (s *defenseState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cfg := s.cfg

	var uaBans, ipBans []map[string]any
	for _, b := range s.bans {
		if b.Until > 0 && b.Until <= now.UnixMilli() {
			continue
		}
		item := map[string]any{
			"value":            b.Value,
			"reason":           b.Reason,
			"source":           b.Source,
			"until":            b.Until,
			"permanent":        b.Until == 0,
			"remainingSeconds": defenseBanRemainingSeconds(*b, now),
		}
		if b.Kind == "ua" {
			uaBans = append(uaBans, item)
		} else {
			ipBans = append(ipBans, item)
		}
	}
	sortBans := func(list []map[string]any) {
		sort.Slice(list, func(i, j int) bool {
			return list[i]["value"].(string) < list[j]["value"].(string)
		})
	}
	sortBans(uaBans)
	sortBans(ipBans)

	top := make([]map[string]any, 0, len(s.uas))
	for ua, e := range s.uas {
		banned := false
		if b, ok := s.bans[defenseBanKey("ua", ua)]; ok {
			banned = b.Until == 0 || b.Until > now.UnixMilli()
		}
		top = append(top, map[string]any{
			"value":    ua,
			"inFlight": e.inflight,
			"requests": e.requests,
			"banned":   banned,
		})
	}
	sort.Slice(top, func(i, j int) bool {
		ri, rj := top[i]["requests"].(int64), top[j]["requests"].(int64)
		if ri != rj {
			return ri > rj
		}
		return top[i]["value"].(string) < top[j]["value"].(string)
	})
	if len(top) > defenseTopUALimit {
		top = top[:defenseTopUALimit]
	}

	events := make([]defenseEvent, 0, defenseEventShow)
	for i := len(s.events) - 1; i >= 0 && len(events) < defenseEventShow; i-- {
		events = append(events, s.events[i])
	}

	// 头部规则命中排行：让管理员看到「被识别出什么违规」，而不只是总数。
	headerRules := make([]map[string]any, 0, len(s.headerRules))
	for rule, hits := range s.headerRules {
		headerRules = append(headerRules, map[string]any{"rule": rule, "hits": hits})
	}
	sort.Slice(headerRules, func(i, j int) bool {
		hi, hj := headerRules[i]["hits"].(int64), headerRules[j]["hits"].(int64)
		if hi != hj {
			return hi > hj
		}
		return headerRules[i]["rule"].(string) < headerRules[j]["rule"].(string)
	})
	if len(headerRules) > 30 {
		headerRules = headerRules[:30]
	}

	return map[string]any{
		// enabled 恒为 true：本功能没有关闭路径，字段仅用于前端展示与契约。
		"enabled":   true,
		"immutable": true,
		"thresholds": map[string]any{
			"ipRatePerSecond":        cfg.IPRate,
			"ipBurst":                cfg.IPBurst,
			"ipConcurrent":           cfg.IPConcurrent,
			"userAgentRatePerSecond": cfg.UARate,
			"userAgentBurst":         cfg.UABurst,
			"userAgentConcurrent":    cfg.UAConcurrent,
			"globalRatePerSecond":    cfg.GlobalRate,
			"globalBurst":            cfg.GlobalBurst,
			"globalConcurrent":       cfg.GlobalConcurrent,
			"banMinutes":             int(cfg.BanDuration / time.Minute),
			"violationsBeforeBan":    cfg.ViolationLimit,
		},
		"stats": map[string]any{
			"requests":          s.requests.Load(),
			"rejected":          s.rejected.Load(),
			"rateLimited":       s.rateLimits.Load(),
			"headerViolations":  s.headerHits.Load(),
			"bannedHits":        s.bannedHits.Load(),
			"autoIPBans":        s.ipBans.Load(),
			"autoUABans":        s.uaBans.Load(),
			"globalShed":        s.globalShed.Load(),
			"inFlight":          s.inflight,
			"trackedIPs":        len(s.ips),
			"trackedUserAgents": len(s.uas),
		},
		"blockedUserAgents": uaBans,
		"blockedIPs":        ipBans,
		"topUserAgents":     top,
		"headerRules":       headerRules,
		"recentEvents":      events,
	}
}

// ---------- 工具函数 ----------

// defenseBanKey 封禁表键（kind 与 value 分隔，避免不同类别的同名值互相覆盖）。
func defenseBanKey(kind, value string) string { return kind + "\x00" + value }

// requestUserAgent 取请求 User-Agent（去空白、限长；重复/超长已由头部校验拒绝）。
func requestUserAgent(r *http.Request) string {
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	if len(ua) > defenseUAValueMax {
		ua = ua[:defenseUAValueMax]
	}
	return ua
}

// isLoopbackSource 判断来源是否是环回地址（IPv6 允许 %zone 形式）。
func isLoopbackSource(ip string) bool {
	if i := strings.IndexByte(ip, '%'); i >= 0 {
		ip = ip[:i]
	}
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.IsLoopback()
}

// defenseBanMessage 封禁拒绝的中文消息。
func defenseBanMessage(b defenseBan) string {
	switch b.Source {
	case "manual":
		return "该来源已被管理员封禁"
	case "config":
		return "该 User-Agent 已被节点配置封禁"
	}
	if b.Kind == "ua" {
		return "该 User-Agent 已被 DDoS/CC 防御临时封禁，请稍后重试"
	}
	return "请求来源已被 DDoS/CC 防御临时封禁，请稍后重试"
}

// defenseBanRemaining 距解封剩余时长（永久封禁返回 0）。
func defenseBanRemaining(b defenseBan) time.Duration {
	if b.Until == 0 {
		return 0
	}
	remain := time.Until(time.UnixMilli(b.Until))
	if remain < 0 {
		return 0
	}
	return remain
}

// defenseBanRemainingSeconds 状态接口用的剩余秒数（永久封禁返回 0）。
func defenseBanRemainingSeconds(b defenseBan, now time.Time) int64 {
	if b.Until == 0 {
		return 0
	}
	remain := b.Until - now.UnixMilli()
	if remain <= 0 {
		return 0
	}
	return (remain + 999) / 1000
}

// defenseRetrySeconds Retry-After 值（向上取整，至少 1 秒）。
func defenseRetrySeconds(d time.Duration) string {
	seconds := int64((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

// defenseDurationText 人类可读时长（日志展示）。
func defenseDurationText(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.0f小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f分钟", d.Minutes())
	default:
		return fmt.Sprintf("%.0f秒", d.Seconds())
	}
}

// defenseTTLText 人工封禁时长（0 = 永久）。
func defenseTTLText(ttl time.Duration) string {
	if ttl <= 0 {
		return "永久"
	}
	return defenseDurationText(ttl)
}

// defenseKindLabel 封禁类别中文名。
func defenseKindLabel(kind string) string {
	if kind == "ua" {
		return "User-Agent"
	}
	return "IP"
}

// defenseSourceLabel 封禁来源中文名。
func defenseSourceLabel(source string) string {
	switch source {
	case "manual":
		return "人工"
	case "config":
		return "配置"
	default:
		return "自动"
	}
}
