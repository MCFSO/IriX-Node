// HTTP 头部规范校验（DDoS/CC 防御的第二道闸门，入口见 defense.go）。
//
// 目标：在进入业务路由之前识别「非正常客户端」的请求头形态：
//   - 请求走私/歧义：Transfer-Encoding 与 Content-Length 并存、重复
//     Content-Length/Host、逐跳头注入（Connection: content-length）、
//     Transfer-Encoding 非 chunked 等；
//   - 代理链标记：Via、Forwarded、X-Forwarded-*、X-Real-IP、CF-Connecting-IP、
//     Proxy-Connection、X-Original-URL 等——本节点从不信任任何转发头，
//     直连客户端不应携带；同机反代（环回来源）由 defense.go 豁免；
//   - 认证头歧义：重复 Authorization / X-Auth-Token / X-Vault-Token；
//   - 方法覆盖头：X-HTTP-Method-Override / X-Method-Override；
//   - 协议异常：TRACE/TRACK/CONNECT、非法 Upgrade、非 100-continue 的 Expect、
//     超量/超大请求头、超长 User-Agent。
//
// 两级处置（由 defense.go 执行）：
//   - defenseHeaderBan：拒绝本次请求并立即封禁来源——请求走私/重复单例头
//     属于正常客户端不可能产生的形态；
//   - defenseHeaderBlock：拒绝本次请求，同一来源累计违规达到阈值后封禁——
//     代理链/方法覆盖/协议异常可能来自被篡改的客户端或中间设备。

package main

import (
	"net/http"
	"strings"
)

// defenseHeaderSeverity 头部违规的处置级别。
type defenseHeaderSeverity int

const (
	// defenseHeaderBlock 拒绝本次请求；同一来源累计违规达到阈值后封禁。
	defenseHeaderBlock defenseHeaderSeverity = iota + 1
	// defenseHeaderBan 拒绝本次请求并立即封禁来源（硬违规）。
	defenseHeaderBan
)

// defenseHeaderFinding 一条头部规范问题。
type defenseHeaderFinding struct {
	Rule     string                // 规则标识（状态接口/日志展示，不含用户输入）
	Detail   string                // 中文说明
	Severity defenseHeaderSeverity // 处置级别
}

// 头部数量/体积上限：远低于 http.Server.MaxHeaderBytes（64KiB），
// 把「头很多/头很大」的 Slowloris 变体挡在业务之前。
const (
	defenseMaxHeaderCount = 64
	defenseMaxHeaderBytes = 32 << 10
	defenseMaxUserAgent   = 512
)

// defenseProxyHeaderNames 代理链/转发标记头（节点不信任任何转发头）。
var defenseProxyHeaderNames = []string{
	"Via",
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Forwarded-Port",
	"X-Forwarded-Server",
	"X-Real-IP",
	"X-Client-IP",
	"Client-IP",
	"CF-Connecting-IP",
	"True-Client-IP",
	"Fastly-Client-IP",
	"X-Original-URL",
	"X-Rewrite-URL",
	"Proxy-Connection",
	"X-Originating-IP",
	"X-ProxyUser-IP",
}

// defenseProxyHeaderSet 规范化后的代理链头集合（Go 解析请求头时会做
// 首字母大写规范化；直接构造的请求可能保留原始大小写，查表前统一规范化）。
var defenseProxyHeaderSet = func() map[string]bool {
	m := make(map[string]bool, len(defenseProxyHeaderNames))
	for _, name := range defenseProxyHeaderNames {
		m[http.CanonicalHeaderKey(name)] = true
	}
	return m
}()

// defenseMethodOverrideHeaders 方法覆盖头：HTTP 方法与路由判定必须以请求行
// 为准，任何「用头覆盖方法」的客户端都视为异常。
var defenseMethodOverrideHeaders = []string{"X-HTTP-Method-Override", "X-Method-Override"}

// defenseSingletonHeaders 不允许出现多值的单例头（重复即视为头歧义）。
var defenseSingletonHeaders = []string{
	"Host", "Content-Length", "Transfer-Encoding",
	"Authorization", "X-Auth-Token", "X-Vault-Token", "User-Agent",
}

// inspectDefenseHeaders 检查请求头规范性，返回全部命中的问题（可能为空）。
// 本函数只做判定，不做拦截/封禁（处置由 defense.go 统一执行）。
func inspectDefenseHeaders(r *http.Request) []defenseHeaderFinding {
	var out []defenseHeaderFinding
	add := func(sev defenseHeaderSeverity, rule, detail string) {
		out = append(out, defenseHeaderFinding{Rule: rule, Detail: detail, Severity: sev})
	}
	h := r.Header

	// 方法：TRACE/TRACK/CONNECT 不提供任何业务语义，只被用于探测/隧道。
	switch strings.ToUpper(r.Method) {
	case http.MethodTrace, http.MethodConnect, "TRACK":
		add(defenseHeaderBan, "method-blocked", "禁止的 HTTP 方法: "+sanitizeHeaderText(r.Method))
	}

	// 请求走私/长度歧义：CL 与 TE 并存、重复 CL/TE。
	contentLengths := h.Values("Content-Length")
	transferEncodings := h.Values("Transfer-Encoding")
	if len(contentLengths) > 0 && len(transferEncodings) > 0 {
		add(defenseHeaderBan, "cl-te-conflict", "Content-Length 与 Transfer-Encoding 同时出现")
	}
	if len(contentLengths) > 1 {
		add(defenseHeaderBan, "duplicate-content-length", "Content-Length 头重复")
	}
	if len(transferEncodings) > 1 {
		add(defenseHeaderBan, "duplicate-transfer-encoding", "Transfer-Encoding 头重复")
	}
	if len(transferEncodings) == 1 {
		// 只接受单个 chunked：gzip/identity/多个编码或空值都属于歧义形态。
		te := strings.TrimSpace(strings.ToLower(transferEncodings[0]))
		if !strings.EqualFold(te, "chunked") {
			add(defenseHeaderBan, "invalid-transfer-encoding", "Transfer-Encoding 仅允许 chunked")
		}
	}

	// 重复的 Host/认证头/UA：同一字段出现多个值时，不同组件可能取不同值
	// （缓存投毒、认证绕过、UA 封禁绕过），一律按硬违规处理。
	for _, name := range defenseSingletonHeaders {
		if len(h.Values(name)) > 1 {
			add(defenseHeaderBan, "duplicate-"+strings.ToLower(name), name+" 头重复")
		}
	}

	// 逐跳头注入：Connection 中声明 Content-Length/Transfer-Encoding/Host 等
	// 端到端字段，是典型的请求走私手法。
	for _, raw := range h.Values("Connection") {
		for _, token := range strings.Split(raw, ",") {
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "content-length", "transfer-encoding", "host":
				add(defenseHeaderBan, "hop-by-hop-injection", "Connection 头包含端到端字段: "+sanitizeHeaderText(token))
			}
		}
	}

	// 方法覆盖头：路由/权限判定不得被头覆盖。
	for _, name := range defenseMethodOverrideHeaders {
		if len(h.Values(name)) > 0 {
			add(defenseHeaderBlock, "method-override", "请求头包含方法覆盖: "+name)
		}
	}

	// 代理链标记：直连客户端携带转发头，说明流量经过未知中间层或被伪造。
	for name := range h {
		if defenseProxyHeaderSet[http.CanonicalHeaderKey(name)] {
			add(defenseHeaderBlock, "proxy-header", "请求头包含代理链标记: "+http.CanonicalHeaderKey(name))
		}
	}

	// Upgrade：只接受标准 WebSocket 握手（Upgrade: websocket 且
	// Connection 含 upgrade 令牌）；其余升级/伪升级一律拒绝。
	upgrades := h.Values("Upgrade")
	if len(upgrades) > 0 {
		if len(upgrades) > 1 || !strings.EqualFold(strings.TrimSpace(upgrades[0]), "websocket") {
			add(defenseHeaderBlock, "invalid-upgrade", "Upgrade 仅允许 websocket")
		} else if !headerHasToken(h.Values("Connection"), "upgrade") {
			add(defenseHeaderBlock, "upgrade-without-connection", "Upgrade: websocket 缺少 Connection: upgrade")
		}
	} else if headerHasToken(h.Values("Connection"), "upgrade") {
		add(defenseHeaderBlock, "connection-upgrade-without-upgrade", "Connection 声明 upgrade 但缺少 Upgrade 头")
	}

	// Expect：只支持 100-continue（其他值 Go 也会回 417，这里显式判定）。
	if exp := strings.TrimSpace(h.Get("Expect")); exp != "" && !strings.EqualFold(exp, "100-continue") {
		add(defenseHeaderBlock, "invalid-expect", "Expect 仅支持 100-continue")
	}

	// 超长 User-Agent：UA 是封禁键，超长值（>512 字节）既无正常用途，
	// 又会放大跟踪表内存占用。
	if ua := h.Get("User-Agent"); len(ua) > defenseMaxUserAgent {
		add(defenseHeaderBan, "user-agent-too-long", "User-Agent 超过长度上限")
	}

	// 头部数量/总体积：按规范化后的键值累加近似字节数。
	count, total := 0, 0
	for name, values := range h {
		count += len(values)
		total += len(name)
		for _, v := range values {
			total += len(v)
		}
	}
	if count > defenseMaxHeaderCount {
		add(defenseHeaderBan, "too-many-headers", "请求头数量超过上限")
	}
	if total > defenseMaxHeaderBytes {
		add(defenseHeaderBan, "headers-too-large", "请求头总体积超过上限")
	}

	return out
}

// headerHasToken 判断逗号分隔的头值列表中是否含指定令牌（大小写不敏感）。
func headerHasToken(values []string, token string) bool {
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// sanitizeHeaderText 把可能进入日志/状态接口的外部输入限制为可见 ASCII，
// 防止控制字符伪造日志行或污染 JSON（长度截断到 64 字节）。
func sanitizeHeaderText(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
