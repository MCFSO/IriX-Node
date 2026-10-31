// DDoS/CC 防御与管理员内网限制测试：
//   - 头部规范判定（Via/Forwarded/走私形态/重复单例头/超量头）；
//   - 代理链头拦截与累计违规封禁；
//   - 同一 User-Agent 并发过多 → 封禁该 UA（跨 IP）；
//   - UA/来源 IP 频率限流、环回豁免；
//   - 人工封禁持久化、状态/封禁接口；
//   - 管理员账户仅允许内网来源登录与访问（admin_access.go）；
//   - newHTTPServer 无条件挂载防御（不可关闭）。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// defenseTestHandler 只挂载防御中间件的测试链（业务处理器恒 200）。
func defenseTestHandler(d *Daemon) http.Handler {
	return d.defenseMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// defenseTestRequest 构造指定来源地址与 User-Agent 的请求。
func defenseTestRequest(remote, ua string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://node.local/api/overview", nil)
	req.RemoteAddr = remote
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	return req
}

// defenseServe 直接执行 handler 并返回响应记录。
func defenseServe(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// defenseTestConfig 放宽频率阈值、只保留被测维度的测试配置。
func defenseTestConfig() defenseConfig {
	cfg := defaultDefenseConfig()
	cfg.IPRate = 10000
	cfg.IPBurst = 20000
	cfg.IPConcurrent = 256
	cfg.UARate = 10000
	cfg.UABurst = 20000
	cfg.UAConcurrent = 256
	cfg.GlobalRate = 1e9
	cfg.GlobalBurst = 1e9
	cfg.ViolationLimit = 3
	cfg.BanDuration = time.Minute
	return cfg
}

// TestDefenseHeaderInspection 头部规范判定：每一类异常都能被识别并分级。
func TestDefenseHeaderInspection(t *testing.T) {
	base := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "http://node.local/api/overview", nil)
	}
	cases := []struct {
		name  string
		build func(*http.Request)
		rule  string
		sev   defenseHeaderSeverity
	}{
		{"Via", func(r *http.Request) { r.Header.Set("Via", "1.1 proxy") }, "proxy-header", defenseHeaderBlock},
		{"Forwarded", func(r *http.Request) { r.Header.Set("Forwarded", "for=1.2.3.4") }, "proxy-header", defenseHeaderBlock},
		{"X-Forwarded-For", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "1.2.3.4") }, "proxy-header", defenseHeaderBlock},
		{"X-Real-IP", func(r *http.Request) { r.Header.Set("X-Real-IP", "1.2.3.4") }, "proxy-header", defenseHeaderBlock},
		{"CF-Connecting-IP", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "1.2.3.4") }, "proxy-header", defenseHeaderBlock},
		{"Proxy-Connection", func(r *http.Request) { r.Header.Set("Proxy-Connection", "keep-alive") }, "proxy-header", defenseHeaderBlock},
		{"方法覆盖", func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "DELETE") }, "method-override", defenseHeaderBlock},
		{"TRACE 方法", func(r *http.Request) { r.Method = http.MethodTrace }, "method-blocked", defenseHeaderBan},
		{"CL 与 TE 并存", func(r *http.Request) {
			r.Header.Set("Content-Length", "6")
			r.Header.Set("Transfer-Encoding", "chunked")
		}, "cl-te-conflict", defenseHeaderBan},
		{"重复 Content-Length", func(r *http.Request) {
			r.Header["Content-Length"] = []string{"6", "6"}
		}, "duplicate-content-length", defenseHeaderBan},
		{"非法 Transfer-Encoding", func(r *http.Request) {
			r.Header.Set("Transfer-Encoding", "gzip")
		}, "invalid-transfer-encoding", defenseHeaderBan},
		{"重复认证头", func(r *http.Request) {
			r.Header["X-Auth-Token"] = []string{"a", "b"}
		}, "duplicate-x-auth-token", defenseHeaderBan},
		{"重复 User-Agent", func(r *http.Request) {
			r.Header["User-Agent"] = []string{"a", "b"}
		}, "duplicate-user-agent", defenseHeaderBan},
		{"超长 User-Agent", func(r *http.Request) {
			r.Header.Set("User-Agent", strings.Repeat("a", defenseMaxUserAgent+1))
		}, "user-agent-too-long", defenseHeaderBan},
		{"逐跳头注入", func(r *http.Request) {
			r.Header.Set("Connection", "keep-alive, Content-Length")
		}, "hop-by-hop-injection", defenseHeaderBan},
		{"非法 Upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "h2c") }, "invalid-upgrade", defenseHeaderBlock},
		{"升级缺 Connection", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }, "upgrade-without-connection", defenseHeaderBlock},
		{"非法 Expect", func(r *http.Request) { r.Header.Set("Expect", "200-ok") }, "invalid-expect", defenseHeaderBlock},
		{"请求头过多", func(r *http.Request) {
			for i := 0; i < defenseMaxHeaderCount+5; i++ {
				r.Header.Set(fmt.Sprintf("X-Test-%d", i), "1")
			}
		}, "too-many-headers", defenseHeaderBan},
	}
	for _, c := range cases {
		req := base()
		c.build(req)
		found := false
		for _, f := range inspectDefenseHeaders(req) {
			if f.Rule == c.rule && f.Severity == c.sev {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: 未识别出规则 %s（等级 %d），实际 %+v", c.name, c.rule, c.sev, inspectDefenseHeaders(base()))
		}
	}

	// 正常请求与合法 WebSocket 握手不应误报
	if got := inspectDefenseHeaders(base()); len(got) != 0 {
		t.Errorf("正常请求不应命中头部规则: %+v", got)
	}
	ws := base()
	ws.Header.Set("Connection", "Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	if got := inspectDefenseHeaders(ws); len(got) != 0 {
		t.Errorf("合法 WebSocket 握手不应命中头部规则: %+v", got)
	}
	t.Logf("[验证] 头部规范判定覆盖 %d 类异常，正常请求/WS 握手无误报", len(cases))
}

// TestDefenseProxyHeaderBlockedAndBanned 代理链头（Via 等）：
// 每次携带都被拒绝；同一来源累计 3 次违规后封禁该 IP（干净请求同样被拒）。
func TestDefenseProxyHeaderBlockedAndBanned(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.defense.reconfigure(defenseTestConfig())
	h := defenseTestHandler(d)
	const remote = "203.0.113.10:5000"

	for i := 0; i < 3; i++ {
		req := defenseTestRequest(remote, "scan-bot/1.0")
		req.Header.Set("Via", "1.1 evil-proxy")
		rec := defenseServe(h, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("第 %d 次携带 Via 应 403，实际 %d", i+1, rec.Code)
		}
		if got := rec.Header().Get("X-Defense"); got != "header" {
			t.Fatalf("应标记 X-Defense=header，实际 %q", got)
		}
	}

	// 累计违规达到阈值：即使不再携带 Via，来源 IP 也已进入封禁表
	clean := defenseServe(h, defenseTestRequest(remote, "scan-bot/1.0"))
	if clean.Code != http.StatusForbidden || clean.Header().Get("X-Defense") != "ban" {
		t.Fatalf("累计违规后应封禁来源 IP，实际 code=%d X-Defense=%q", clean.Code, clean.Header().Get("X-Defense"))
	}
	// 其他来源不受影响（UA 未被封禁，只有 IP 维度）
	other := defenseServe(h, defenseTestRequest("198.51.100.20:5000", "scan-bot/1.0"))
	if other.Code != http.StatusOK {
		t.Fatalf("其他来源不应被连坐，实际 %d", other.Code)
	}
	// 状态快照记录具体规则命中次数（可辨析「被识别出什么违规」）
	rules, _ := d.defense.snapshot()["headerRules"].([]map[string]any)
	seen := false
	for _, rule := range rules {
		if rule["rule"] == "proxy-header" && rule["hits"].(int64) >= 3 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("状态快照应记录 proxy-header 命中次数，实际 %v", rules)
	}
	t.Logf("[验证] Via 每次被拒，累计 3 次后来源 IP 封禁，其他来源正常")
}

// TestDefenseHardHeaderViolationBans 请求走私类硬违规：首次即封禁来源。
func TestDefenseHardHeaderViolationBans(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.defense.reconfigure(defenseTestConfig())
	h := defenseTestHandler(d)
	const remote = "203.0.113.11:5000"

	req := defenseTestRequest(remote, "smuggler/1.0")
	req.Header.Set("Content-Length", "5")
	req.Header.Set("Transfer-Encoding", "chunked")
	rec := defenseServe(h, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Defense") != "header" {
		t.Fatalf("走私形态应 403/header，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	// 同一来源的干净请求也应被立即封禁
	rec = defenseServe(h, defenseTestRequest(remote, "smuggler/1.0"))
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Defense") != "ban" {
		t.Fatalf("硬违规应立即使来源进入封禁，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	t.Logf("[验证] 请求走私形态首次命中即封禁来源")
}

// TestDefenseBansUserAgentOnConcurrency 同一 User-Agent 并发过多 → 封禁该 UA
// （跨 IP 聚合；封禁后该 UA 从任何来源都被拒绝，其他 UA 不受影响）。
func TestDefenseBansUserAgentOnConcurrency(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.UAConcurrent = 2
	d.defense.reconfigure(cfg)

	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	h := d.defenseMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := defenseServe(h, defenseTestRequest(fmt.Sprintf("203.0.113.%d:5000", 20+i), "shared-ua/1.0"))
			if rec.Code != http.StatusOK {
				t.Errorf("在途请求 %d 应放行，实际 %d", i, rec.Code)
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("在途请求未按预期进入业务处理器")
		}
	}

	// 第 3 个并发请求：同一 UA、不同 IP → 429，同时该 UA 进入封禁表
	rec := defenseServe(h, defenseTestRequest("203.0.113.99:5000", "shared-ua/1.0"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("UA 并发超限应 429，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("X-Defense"); got != "ua-concurrency" {
		t.Fatalf("应标记 X-Defense=ua-concurrency，实际 %q", got)
	}

	close(release)
	wg.Wait()

	// 封禁生效：同一 UA 从任意来源都被拒绝
	banned := defenseServe(h, defenseTestRequest("198.51.100.30:5000", "shared-ua/1.0"))
	if banned.Code != http.StatusForbidden || banned.Header().Get("X-Defense") != "ban" {
		t.Fatalf("UA 封禁后其他来源也应 403/ban，实际 code=%d X-Defense=%q",
			banned.Code, banned.Header().Get("X-Defense"))
	}
	// 其他 User-Agent 正常放行
	other := defenseServe(h, defenseTestRequest("198.51.100.31:5000", "other-ua/1.0"))
	if other.Code != http.StatusOK {
		t.Fatalf("其他 UA 不应被连坐，实际 %d", other.Code)
	}
	// 环回来源豁免自动封禁（本机面板不会被自动策略锁死）
	local := defenseServe(h, defenseTestRequest("127.0.0.1:5000", "shared-ua/1.0"))
	if local.Code != http.StatusOK {
		t.Fatalf("环回来源应豁免自动封禁，实际 %d", local.Code)
	}
	t.Logf("[验证] UA 并发超限 → 该 UA 全局封禁，其他 UA/环回来源不受影响")
}

// TestDefenseBansUserAgentOnRate 同一 User-Agent 请求频率超限 → 封禁该 UA。
func TestDefenseBansUserAgentOnRate(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.UARate = 0.000001 // 几乎不补充令牌
	cfg.UABurst = 2
	cfg.UAConcurrent = 256
	d.defense.reconfigure(cfg)
	h := defenseTestHandler(d)

	for i := 0; i < 2; i++ {
		rec := defenseServe(h, defenseTestRequest("203.0.113.40:5000", "flood-ua/1.0"))
		if rec.Code != http.StatusOK {
			t.Fatalf("突发额度内第 %d 次应放行，实际 %d", i+1, rec.Code)
		}
	}
	rec := defenseServe(h, defenseTestRequest("203.0.113.41:5000", "flood-ua/1.0"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-Defense") != "ua-rate" {
		t.Fatalf("UA 频率超限应 429/ua-rate，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	if rec := defenseServe(h, defenseTestRequest("203.0.113.42:5000", "flood-ua/1.0")); rec.Code != http.StatusForbidden {
		t.Fatalf("UA 封禁后应 403，实际 %d", rec.Code)
	}
	t.Logf("[验证] UA 频率超限 → 该 UA 封禁")
}

// TestDefenseGlobalBudget 全节点匿名预算：海量 IP 轮换时在进入跟踪表之前
// 就按聚合预算丢弃，避免“换 IP 就重新获得突发额度”把节点打满。
func TestDefenseGlobalBudget(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.GlobalRate = 0.000001
	cfg.GlobalBurst = 2
	cfg.MaxIPs = 100
	d.defense.reconfigure(cfg)
	h := defenseTestHandler(d)

	for i := 0; i < 2; i++ {
		remote := fmt.Sprintf("203.0.113.%d:5000", 150+i)
		if rec := defenseServe(h, defenseTestRequest(remote, fmt.Sprintf("budget-ua-%d/1.0", i))); rec.Code != http.StatusOK {
			t.Fatalf("预算内第 %d 次应放行，实际 %d", i+1, rec.Code)
		}
	}
	// 第 3 个全新 IP：聚合预算耗尽，直接 503，不创建新的跟踪条目
	rec := defenseServe(h, defenseTestRequest("203.0.113.200:5000", "budget-ua-3/1.0"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("X-Defense") != "overload" {
		t.Fatalf("超出全局预算应 503/overload，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	stats := d.defense.snapshot()["stats"].(map[string]any)
	if stats["trackedIPs"].(int) != 2 {
		t.Fatalf("超预算请求不应进入跟踪表，实际 trackedIPs=%v", stats["trackedIPs"])
	}
	if stats["globalShed"].(int64) < 1 {
		t.Fatalf("应记录 globalShed，实际 %v", stats["globalShed"])
	}
	// 环回来源不受全局预算影响
	if rec := defenseServe(h, defenseTestRequest("127.0.0.1:5000", "local/1.0")); rec.Code != http.StatusOK {
		t.Fatalf("环回来源应豁免全局预算，实际 %d", rec.Code)
	}
	t.Logf("[验证] 全节点匿名预算在跟踪表之前生效，环回豁免")
}

// TestDefenseIPRateLimitAndLoopbackExempt 单 IP 限流；环回来源完全豁免。
func TestDefenseIPRateLimitAndLoopbackExempt(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.IPRate = 0.000001
	cfg.IPBurst = 2
	d.defense.reconfigure(cfg)
	h := defenseTestHandler(d)

	for i := 0; i < 2; i++ {
		rec := defenseServe(h, defenseTestRequest("203.0.113.50:5000", fmt.Sprintf("ua-%d/1.0", i)))
		if rec.Code != http.StatusOK {
			t.Fatalf("突发额度内第 %d 次应放行，实际 %d", i+1, rec.Code)
		}
	}
	rec := defenseServe(h, defenseTestRequest("203.0.113.50:5000", "ua-x/1.0"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-Defense") != "ip-rate" {
		t.Fatalf("IP 频率超限应 429/ip-rate，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("限流响应应携带 Retry-After")
	}

	// 环回来源（本机面板/CLI）不受频率限制
	for i := 0; i < 20; i++ {
		rec := defenseServe(h, defenseTestRequest("127.0.0.1:5000", "local-panel/1.0"))
		if rec.Code != http.StatusOK {
			t.Fatalf("环回来源不应被限流（第 %d 次），实际 %d", i+1, rec.Code)
		}
	}
	t.Logf("[验证] 单 IP 令牌桶限流生效，环回来源豁免")
}

// TestDefenseTrackingTableEviction 跟踪表有界但不会因表满而拒绝所有新来源：
// 海量 IP/UA 轮换时应淘汰旧条目继续服务，而不是让攻击者用表满拖垮正常用户。
func TestDefenseTrackingTableEviction(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.MaxIPs = 2
	cfg.MaxUAs = 2
	d.defense.reconfigure(cfg)
	h := defenseTestHandler(d)

	for i := 0; i < 20; i++ {
		remote := fmt.Sprintf("203.0.113.%d:5000", 100+i%50)
		ua := fmt.Sprintf("ua-evict-%d/1.0", i%5)
		if rec := defenseServe(h, defenseTestRequest(remote, ua)); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行（表满应淘汰而非拒绝），实际 %d", i+1, rec.Code)
		}
	}
	stats := d.defense.snapshot()["stats"].(map[string]any)
	if stats["trackedIPs"].(int) > cfg.MaxIPs || stats["trackedUserAgents"].(int) > cfg.MaxUAs {
		t.Fatalf("跟踪表超出上限: %v", stats)
	}
	t.Logf("[验证] 跟踪表满时淘汰旧条目继续服务（IP/UA 各 ≤ %d/%d）", cfg.MaxIPs, cfg.MaxUAs)
}

// TestDefenseManualBanPersists 人工封禁持久化：重启后仍生效，解封后清除。
func TestDefenseManualBanPersists(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.defense.reconfigure(defenseTestConfig())
	h := defenseTestHandler(d)

	if _, err := d.defense.addManualBan("ua", "BadBot/1.0", 0, "测试封禁"); err != nil {
		t.Fatalf("人工封禁失败: %v", err)
	}
	file := filepath.Join(dir, "defense.json")
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "BadBot/1.0") {
		t.Fatalf("人工封禁未持久化到 %s: err=%v", file, err)
	}
	rec := defenseServe(h, defenseTestRequest("203.0.113.60:5000", "BadBot/1.0"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "管理员") {
		t.Fatalf("人工封禁应 403 且提示管理员封禁，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 人工封禁对本机面板同样生效（环回只豁免自动封禁与频率限制）
	if rec := defenseServe(h, defenseTestRequest("127.0.0.1:5000", "BadBot/1.0")); rec.Code != http.StatusForbidden {
		t.Fatalf("人工封禁应对环回来源同样生效，实际 %d", rec.Code)
	}
	// 环回来源的「解除封禁」请求豁免 UA 封禁（误封后可恢复）
	recovery := httptest.NewRequest(http.MethodDelete, "http://node.local/api/defense/ban", nil)
	recovery.RemoteAddr = "127.0.0.1:5000"
	recovery.Header.Set("User-Agent", "BadBot/1.0")
	if rec := defenseServe(h, recovery); rec.Code != http.StatusOK {
		t.Fatalf("环回解封请求应豁免 UA 封禁，实际 %d", rec.Code)
	}

	// 重启（同一数据目录）后仍生效
	d2 := NewDaemon(dir)
	defer d2.workers.stop()
	h2 := defenseTestHandler(d2)
	rec = defenseServe(h2, defenseTestRequest("203.0.113.61:5000", "BadBot/1.0"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("重启后人工封禁应仍生效，实际 %d", rec.Code)
	}

	// 解封后恢复访问，持久化文件中不再包含该记录
	if !d2.defense.removeBan("ua", "BadBot/1.0") {
		t.Fatal("解除封禁应返回 true")
	}
	rec = defenseServe(h2, defenseTestRequest("203.0.113.61:5000", "BadBot/1.0"))
	if rec.Code != http.StatusOK {
		t.Fatalf("解封后应恢复访问，实际 %d", rec.Code)
	}
	if data, err := os.ReadFile(file); err == nil && strings.Contains(string(data), "BadBot/1.0") {
		t.Fatalf("解封后持久化文件仍包含被封禁 UA: %s", data)
	}
	t.Logf("[验证] 人工封禁持久化、重启生效、解封清理")
}

// TestDefenseStatusAndBanAPI 防御状态/人工封禁接口（认证 + 管理员）。
func TestDefenseStatusAndBanAPI(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 未认证：403
	resp, err := testClient.Get(srv.URL + "/api/defense/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未认证访问防御状态应 403，实际 %d", resp.StatusCode)
	}

	// 认证：enabled/immutable 恒为 true（没有关闭路径）
	code, body := doReq(t, srv.URL+"/api/defense/status?token="+qaTestToken())
	if code != http.StatusOK {
		t.Fatalf("防御状态应 200，实际 %d: %s", code, body)
	}
	var statusResp struct {
		Status int            `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &statusResp); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}
	if statusResp.Data["enabled"] != true || statusResp.Data["immutable"] != true {
		t.Fatalf("防御必须恒开: %v", statusResp.Data)
	}
	if _, ok := statusResp.Data["thresholds"].(map[string]any); !ok {
		t.Fatalf("状态应包含 thresholds: %v", statusResp.Data)
	}

	// 人工封禁（管理员）
	bodyJSON := `{"type":"ua","value":"APIBot/1.0","minutes":1,"reason":"接口测试"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/defense/ban", strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", qaTestToken())
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	banBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("人工封禁应 200，实际 %d: %s", resp.StatusCode, banBody)
	}

	// 状态中可见
	_, body = doReq(t, srv.URL+"/api/defense/status?token="+qaTestToken())
	if !strings.Contains(string(body), "APIBot/1.0") {
		t.Fatalf("状态响应应包含人工封禁的 UA: %s", body)
	}

	// 非法参数：400
	bad, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/defense/ban", strings.NewReader(`{"type":"ua","value":""}`))
	bad.Header.Set("X-Auth-Token", qaTestToken())
	resp, err = testClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 UA 应 400，实际 %d", resp.StatusCode)
	}

	// 解封
	del, _ := http.NewRequest(http.MethodDelete,
		srv.URL+"/api/defense/ban?type=ua&value="+url.QueryEscape("APIBot/1.0"), nil)
	del.Header.Set("X-Auth-Token", qaTestToken())
	resp, err = testClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("解封应 200，实际 %d", resp.StatusCode)
	}
	// 再次解封：404
	del, _ = http.NewRequest(http.MethodDelete,
		srv.URL+"/api/defense/ban?type=ua&value="+url.QueryEscape("APIBot/1.0"), nil)
	del.Header.Set("X-Auth-Token", qaTestToken())
	resp, err = testClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复解封应 404，实际 %d", resp.StatusCode)
	}
	t.Logf("[验证] 防御状态接口恒开可见，人工封禁/解封接口可用")
}

// TestDefenseAlwaysOn 生产服务器链无条件挂载防御：携带 Via 的请求在认证之前
// 即被拒绝（即使带合法管理员 token、即使配置写了 enabled=false）。
func TestDefenseAlwaysOn(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := d.newHTTPServer("")
	req := httptest.NewRequest(http.MethodGet, "/api/overview?token="+qaTestToken(), nil)
	req.RemoteAddr = "203.0.113.70:5000"
	req.Header.Set("X-Auth-Token", qaTestToken())
	req.Header.Set("Via", "1.1 evil")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Defense") != "header" {
		t.Fatalf("生产链应优先拒绝代理链头，实际 code=%d X-Defense=%q", rec.Code, rec.Header().Get("X-Defense"))
	}
	t.Logf("[验证] newHTTPServer 无条件挂载防御（认证前拦截）")
}

// TestDefenseConfigCannotDisable 配置写 defense.enabled=false 只会得到告警，
// 阈值调优正常生效；不存在任何关闭路径。
func TestDefenseConfigCannotDisable(t *testing.T) {
	cfg := &Config{}
	if err := json.Unmarshal([]byte(`{"defense":{"enabled":false,"maxUserAgentConcurrent":8,"blockedUserAgents":["StaticBot/1.0"]}}`), cfg); err != nil {
		t.Fatal(err)
	}
	o := baseOptions()
	o.applyConfig(cfg, map[string]bool{})
	if !o.DefenseDisableWarn {
		t.Fatal("应识别 defense.enabled=false 为需告警的误配")
	}
	if o.DefenseUAConcurrent != 8 {
		t.Fatalf("阈值调优未生效: %d", o.DefenseUAConcurrent)
	}
	if !strings.Contains(o.DefenseBlockedUAs, "StaticBot/1.0") {
		t.Fatalf("静态封禁列表未生效: %q", o.DefenseBlockedUAs)
	}

	// 应用后：配置封禁立即生效，且没有 enabled 字段可用于关闭防御
	d, _ := newTestDaemon(t)
	d.defense.applyOptions(defenseOptions{BlockedUserAgents: []string{"StaticBot/1.0"}})
	h := defenseTestHandler(d)
	rec := defenseServe(h, defenseTestRequest("203.0.113.80:5000", "StaticBot/1.0"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("配置静态封禁应 403，实际 %d", rec.Code)
	}
	t.Logf("[验证] defense.enabled=false 被忽略并告警，静态封禁生效")
}

// TestAdminIntranetToggle 管理员内网限制可通过 AdminIntranetOnly 开关：
// 关闭后公网来源可登录/访问，重新开启后立即恢复 403（登录与会话两层）。
func TestAdminIntranetToggle(t *testing.T) {
	d, _ := newTestDaemon(t)
	login := func() int {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, qaTestAdminUser, qaTestAdminPass)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
		req.RemoteAddr = "203.0.113.220:5000"
		rec := httptest.NewRecorder()
		d.handleAccountLogin(rec, req)
		return rec.Code
	}
	sessionRequest := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		req.RemoteAddr = "203.0.113.220:5000"
		req.Header.Set("X-Auth-Token", token)
		rec := httptest.NewRecorder()
		d.auth(d.handleOverview).ServeHTTP(rec, req)
		return rec.Code
	}

	// 默认开启：公网管理员登录/会话均 403
	if code := login(); code != http.StatusForbidden {
		t.Fatalf("默认内网限制下公网管理员登录应 403，实际 %d", code)
	}
	if code := sessionRequest(qaTestToken()); code != http.StatusForbidden {
		t.Fatalf("默认内网限制下公网管理员会话应 403，实际 %d", code)
	}
	// 关闭开关：公网登录/会话放行
	d.AdminIntranetOnly = false
	if code := login(); code != http.StatusOK {
		t.Fatalf("关闭内网限制后公网管理员登录应 200，实际 %d", code)
	}
	if code := sessionRequest(qaTestToken()); code != http.StatusOK {
		t.Fatalf("关闭内网限制后公网管理员会话应 200，实际 %d", code)
	}
	// 重新开启：立即恢复
	d.AdminIntranetOnly = true
	if code := sessionRequest(qaTestToken()); code != http.StatusForbidden {
		t.Fatalf("重新开启内网限制后公网管理员会话应 403，实际 %d", code)
	}
	t.Logf("[验证] 管理员内网限制可开关（登录与会话两层）")
}

// TestAdminIntranetConfig 配置/命令行开关合并：配置 false 生效，
// 命令行显式 true 优先于配置文件 false。
func TestAdminIntranetConfig(t *testing.T) {
	no := false
	cfg := &Config{AdminIntranetOnly: &no}
	o := baseOptions()
	o.applyConfig(cfg, map[string]bool{})
	if o.AdminIntranetOnly {
		t.Fatal("配置 adminIntranetOnly=false 未生效")
	}
	yes := true
	o = baseOptions()
	o.AdminIntranetOnly = yes // 模拟命令行 -admin-intranet-only=true
	o.applyConfig(cfg, map[string]bool{"admin-intranet-only": true})
	if !o.AdminIntranetOnly {
		t.Fatal("命令行显式 true 应优先于配置文件 false")
	}
	t.Logf("[验证] adminIntranetOnly 配置合并与命令行优先级正确")
}

// TestAdminIntranetAllowed 管理员来源网段判定（RFC 1918 / 环回 / IPv6 内网段）。
func TestAdminIntranetAllowed(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		desc string
	}{
		{"127.0.0.1", true, "IPv4 环回"},
		{"127.10.20.30", true, "127/8 环回"},
		{"::1", true, "IPv6 环回"},
		{"10.0.0.1", true, "RFC1918 10/8"},
		{"10.255.255.254", true, "RFC1918 10/8 边界"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"172.31.255.254", true, "RFC1918 172.16/12 边界"},
		{"192.168.1.9", true, "RFC1918 192.168/16"},
		{"fd00::1", true, "IPv6 ULA"},
		{"fe80::1%eth0", true, "IPv6 链路本地（带 zone）"},
		{"172.32.0.1", false, "172.32 已出 RFC1918 范围"},
		{"11.0.0.1", false, "非私网"},
		{"8.8.8.8", false, "公网"},
		{"100.64.0.1", false, "CGNAT 不属于 RFC1918"},
		{"192.0.2.1", false, "文档测试网段"},
		{"203.0.113.9", false, "文档测试网段"},
		{"2001:db8::1", false, "IPv6 文档网段"},
		{"", false, "空地址 fail-closed"},
		{"not-an-ip", false, "非法地址 fail-closed"},
	}
	for _, c := range cases {
		if got := adminIntranetAllowed(c.ip); got != c.want {
			t.Errorf("adminIntranetAllowed(%q) = %v，期望 %v（%s）", c.ip, got, c.want, c.desc)
		}
	}
	t.Logf("[验证] 管理员来源判定 %d 例全部符合预期", len(cases))
}

// TestAdminLoginRequiresIntranet 管理员登录：公网来源即使密码正确也拒绝；
// 内网与环回来源正常签发会话。
func TestAdminLoginRequiresIntranet(t *testing.T) {
	d, _ := newTestDaemon(t)
	login := func(remote string) (int, string) {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, qaTestAdminUser, qaTestAdminPass)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		d.handleAccountLogin(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, body := login("203.0.113.90:5555"); code != http.StatusForbidden || !strings.Contains(body, "内网") {
		t.Fatalf("公网来源管理员登录应 403 且提示内网，实际 code=%d body=%s", code, body)
	}
	if code, body := login("[2001:db8::9]:5555"); code != http.StatusForbidden {
		t.Fatalf("IPv6 公网来源管理员登录应 403，实际 code=%d body=%s", code, body)
	}
	for _, remote := range []string{"127.0.0.1:5555", "10.1.2.3:5555", "172.20.0.9:5555", "192.168.1.50:5555"} {
		if code, body := login(remote); code != http.StatusOK {
			t.Fatalf("内网来源 %s 管理员登录应 200，实际 code=%d body=%s", remote, code, body)
		}
	}
	// 非管理员账户不受来源限制（公网可登录）
	d.accounts.createAccount("public-user", "PublicPass123", false)
	body := `{"username":"public-user","password":"PublicPass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.91:5555"
	rec := httptest.NewRecorder()
	d.handleAccountLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("非管理员公网登录应放行，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	t.Logf("[验证] 管理员登录仅限内网，非管理员不受限")
}

// TestAdminSessionRequiresIntranet 已签发的管理员会话同样按请求来源校验：
// 公网携带管理员 token 也无法访问；非管理员不受限。
func TestAdminSessionRequiresIntranet(t *testing.T) {
	d, _ := newTestDaemon(t)
	handler := d.auth(d.handleOverview)
	call := func(h http.HandlerFunc, remote, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Auth-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(handler, "203.0.113.92:5000", qaTestToken()); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "内网") {
		t.Fatalf("公网使用管理员会话应 403/内网提示，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call(handler, "[2001:db8::5]:5000", qaTestToken()); rec.Code != http.StatusForbidden {
		t.Fatalf("IPv6 公网使用管理员会话应 403，实际 %d", rec.Code)
	}
	for _, remote := range []string{"127.0.0.1:5000", "10.9.8.7:5000", "172.18.0.2:5000", "192.168.0.8:5000", "[fd00::7]:5000"} {
		if rec := call(handler, remote, qaTestToken()); rec.Code != http.StatusOK {
			t.Fatalf("内网来源 %s 使用管理员会话应 200，实际 %d: %s", remote, rec.Code, rec.Body.String())
		}
	}
	// 非管理员账户从公网访问自身端点不受管理员来源限制影响
	if err := d.accounts.createAccount("public-user", "PublicPass123", false); err != nil {
		t.Fatal(err)
	}
	d.accounts.putSession("public-user-token", accountSession{
		Username:  "public-user",
		IsAdmin:   false,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}, time.Hour)
	me := d.auth(d.handleAccountMe)
	meReq := httptest.NewRequest(http.MethodGet, "/api/accounts/me", nil)
	meReq.Pattern = "GET /api/accounts/me" // 直接调用 handler 时补齐 ServeMux 会写入的路由模式
	meReq.RemoteAddr = "203.0.113.93:5000"
	meReq.Header.Set("X-Auth-Token", "public-user-token")
	meRec := httptest.NewRecorder()
	me.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("非管理员公网访问自身端点应 200，实际 %d: %s", meRec.Code, meRec.Body.String())
	}
	t.Logf("[验证] 管理员会话逐请求校验来源，非管理员不受限")
}

// TestDefenseConcurrentSafety 并发压测防御状态机（配合 -race 检查数据竞争）。
func TestDefenseConcurrentSafety(t *testing.T) {
	d, _ := newTestDaemon(t)
	cfg := defenseTestConfig()
	cfg.UAConcurrent = 1000
	cfg.IPConcurrent = 1000
	cfg.MaxIPs = 500
	cfg.MaxUAs = 500
	d.defense.reconfigure(cfg)
	h := defenseTestHandler(d)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				remote := fmt.Sprintf("198.51.100.%d:%d", (i%200)+1, 10000+j)
				ua := fmt.Sprintf("ua-%d/1.0", i%8)
				rec := defenseServe(h, defenseTestRequest(remote, ua))
				if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests && rec.Code != http.StatusForbidden {
					t.Errorf("异常状态码: %d", rec.Code)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	snapshot := d.defense.snapshot()
	if snapshot["enabled"] != true {
		t.Fatal("状态快照异常")
	}
	t.Logf("[验证] 800 次并发防御判定无竞争/无死锁")
}
