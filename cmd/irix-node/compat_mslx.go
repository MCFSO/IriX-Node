// MSLX Daemon 兼容层：REST 接口（/api/...）与认证。
//
// 对齐 MSLX Daemon 的线上形态：统一响应信封 {code, message, data}、
// JWT（HS256，「x-user-token」头 / access_token 查询参数）、实例以递增整数 id 表示。
// 处理逻辑全部落到本节点自身能力上，未引入其源码。
//
// 已实现的接口覆盖「面板日常运维」所需的子集：登录、实例列表/详情/控制、
// 实例设置、文件管理（列表/读写/新建/删除/重命名/复制/移动/上传/下载）、
// 系统状态、SignalR 控制台（见 compat_mslx_hub.go）。
// 未实现的接口统一返回 {code:501} 的中文说明，面板会提示功能不可用，
// 而不是静默失效。

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"irix/node/internal/timesys"
)

// MSLX 兼容层的固定标识与限制。
const (
	mslxTokenTTL     = 24 * time.Hour // 与 MSLX 一致：token 有效期 1 天
	mslxMaxLoginFail = 10             // 5 分钟内失败上限
	mslxBanDuration  = time.Hour      // 超限封禁 60 分钟
	mslxFailWindow   = 5 * time.Minute
	mslxUserID       = "irix-node-admin"
	mslxIDMapFile    = "compat-mslx-ids.json"
)

// mslxResponse MSLX 的响应信封（字段为 camelCase）。
type mslxResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// mslxOK 输出成功响应。
func mslxOK(w http.ResponseWriter, data any, msg string) {
	if msg == "" {
		msg = "OK"
	}
	writeCompatJSON(w, http.StatusOK, mslxResponse{Code: 200, Message: msg, Data: data})
}

// mslxFail 输出失败响应（HTTP 状态与 code 一致）。
func mslxFail(w http.ResponseWriter, status int, msg string) {
	if status < 400 {
		status = http.StatusBadRequest
	}
	writeCompatJSON(w, status, mslxResponse{Code: status, Message: msg})
}

// mslxNotImplemented 未实现的接口：明确告知而不是静默成功。
func mslxNotImplemented(w http.ResponseWriter, what string) {
	writeCompatJSON(w, http.StatusNotImplemented, mslxResponse{
		Code:    501,
		Message: "irix-node 未实现该 MSLX 接口：" + what,
	})
}

// handler MSLX 兼容层 HTTP 入口（路径已完成前缀剥离）。
func (ms *mslxCompat) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowOK(r, ms.allow) {
			http.Error(w, "来源不在 compat.mslx.allowPanelIps 允许范围内", http.StatusForbidden)
			return
		}
		// 跨源响应头由节点自身的 CORS 中间件按来源白名单统一下发（含
		// Access-Control-Allow-Origin / -Credentials）。这里**不再**写
		// Access-Control-Allow-Origin: * —— 那会绕过白名单让任意站点读取
		// 兼容层响应，并与中间件下发的 Allow-Credentials 冲突。
		w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") {
			if p == "/" || p == "" {
				addr := ms.addr
				if addr == "" {
					addr = "127.0.0.1:" + strconv.Itoa(defaultMSLXCompatPort)
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, "<!doctype html><meta charset=\"utf-8\"><title>IriX Node</title>"+
					"<h3>IriX Node — MSLX Daemon 兼容层</h3>"+
					"<p>WebPanel 后端地址：<code>"+addr+"</code>；"+
					"账号与密码见 <code>config.json</code> 的 <code>compat.mslx</code>。</p>")
				return
			}
			http.NotFound(w, r)
			return
		}
		ms.route(w, r)
	})
}

// route 按路径分派 MSLX 接口。
func (ms *mslxCompat) route(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		http.NotFound(w, r)
		return
	}
	// 公开接口
	switch {
	case segs[0] == "ping":
		mslxOK(w, map[string]any{"time": timesys.Now().UnixMilli()}, "pong")
		return
	case segs[0] == "auth" && len(segs) == 2 && segs[1] == "login":
		ms.handleLogin(w, r)
		return
	case segs[0] == "node" && len(segs) == 2 && segs[1] != "status":
		// 子节点链接协议（MSLX 节点管理）：protocol / accept-link /
		// update-link / remove-link 都是主节点匿名调用的；status 需登录。
		ms.handleNode(w, r, segs[1:])
		return
	case segs[0] == "hubs":
		ms.handleHub(w, r, segs[1:])
		return
	}

	// 面板探活：MSLX WebPanel 判定子节点在线的方式是直接请求
	// GET {节点}/api/status（带 x-node-id 头、3 秒超时）。原生 MSLX 在这里走
	// 节点授权（x-node-id + 主节点签发的节点令牌）；我们无法验证主节点签名的
	// 令牌，因此该接口按「公开状态」处理——与 MSLX 匿名访问时的行为一致
	// （返回未登录用户 + 版本信息），不涉及任何账户数据。
	if segs[0] == "status" && len(segs) == 1 {
		ms.handleStatus(w, r)
		return
	}

	// 其余接口需要有效 JWT：先认本节点账户令牌，
	// 再认「主节点代理」请求（x-node-id + 主节点签发的节点令牌 → 向主节点核对）
	claims, ok := ms.authenticate(r)
	if !ok {
		if c, okNode := ms.authorizeNodeRequest(r); okNode {
			claims, ok = c, true
		}
	}
	if !ok {
		mslxFail(w, http.StatusUnauthorized, "未登录或登录已过期，请重新登录")
		return
	}

	switch segs[0] {
	case "update":
		mslxOK(w, map[string]any{
			"currentVersion":  Version,
			"onlineVersion":   Version,
			"updateAvailable": false,
			"message":         "irix-node 不提供远程自更新",
		}, "OK")
	case "user":
		ms.handleUser(w, r, segs[1:], claims)
	case "instance":
		ms.handleInstance(w, r, segs[1:])
	case "files":
		ms.handleFiles(w, r, segs[1:])
	case "settings":
		ms.handleSettings(w, r)
	case "node":
		ms.handleNode(w, r, segs[1:])
	case "java":
		mslxOK(w, ms.d.mcJavaList(), "获取成功")
	case "docker", "plugins", "frp", "resource", "tasks", "admin":
		mslxNotImplemented(w, "/api/"+strings.Join(segs, "/"))
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// JWT（HS256，纯标准库）
// ---------------------------------------------------------------------------

// mslxClaims JWT 载荷。
type mslxClaims struct {
	UserID string
	Name   string
	Role   string
	Exp    int64
}

// b64url 无填充 base64url 编码。
func b64url(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// b64urlDecode 解码无填充 base64url。
func b64urlDecode(s string) ([]byte, error) {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(s)
}

// signJWT 生成 HS256 JWT。
func (ms *mslxCompat) signJWT(claims mslxClaims) string {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := timesys.Now()
	payload := map[string]any{
		"UserId":       claims.UserID,
		"name":         claims.Name,
		"role":         claims.Role,
		"TokenVersion": "1",
		"jti":          newUUID(),
		"iat":          now.Unix(),
		"nbf":          now.Add(-time.Minute).Unix(),
		"exp":          claims.Exp,
	}
	body, _ := json.Marshal(payload)
	signing := header + "." + b64url(body)
	mac := hmac.New(sha256.New, ms.secret)
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil))
}

// parseJWT 校验并解析 JWT（签名、exp/nbf）。
func (ms *mslxCompat) parseJWT(token string) (*mslxClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("令牌格式错误")
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, ms.secret)
	mac.Write([]byte(signing))
	want := mac.Sum(nil)
	got, err := b64urlDecode(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return nil, errors.New("令牌签名校验失败")
	}
	raw, err := b64urlDecode(parts[1])
	if err != nil {
		return nil, errors.New("令牌载荷解码失败")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("令牌载荷解析失败")
	}
	exp, _ := payload["exp"].(float64)
	now := timesys.Now().Unix()
	if int64(exp) <= now {
		return nil, errors.New("令牌已过期")
	}
	if nbf, ok := payload["nbf"].(float64); ok && int64(nbf) > now+60 {
		return nil, errors.New("令牌尚未生效")
	}
	c := &mslxClaims{Exp: int64(exp)}
	if v, ok := payload["UserId"].(string); ok {
		c.UserID = v
	}
	if v, ok := payload["name"].(string); ok {
		c.Name = v
	}
	if v, ok := payload["role"].(string); ok {
		c.Role = v
	}
	if c.UserID == "" {
		return nil, errors.New("令牌缺少 UserId")
	}
	return c, nil
}

// tokenFromRequest 从请求中取 JWT（头优先，其次查询参数）。
func tokenFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-user-token")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	for _, key := range []string{"access_token", "token", "x-user-token"} {
		if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// authenticate 校验请求身份。
func (ms *mslxCompat) authenticate(r *http.Request) (*mslxClaims, bool) {
	tok := tokenFromRequest(r)
	if tok == "" {
		return nil, false
	}
	claims, err := ms.parseJWT(tok)
	if err != nil {
		return nil, false
	}
	return claims, true
}

// isBanned 判定来源是否处于登录封禁期。
func (ms *mslxCompat) isBanned(ip string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	until, ok := ms.banned[ip]
	if !ok {
		return false
	}
	if timesys.Now().After(until) {
		delete(ms.banned, ip)
		return false
	}
	return true
}

// noteLoginFail 记录一次登录失败；超过阈值封禁来源。
func (ms *mslxCompat) noteLoginFail(ip string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	now := timesys.Now()
	if since, ok := ms.failSince[ip]; !ok || now.Sub(since) > mslxFailWindow {
		ms.failSince[ip] = now
		ms.failCount[ip] = 0
	}
	ms.failCount[ip]++
	if ms.failCount[ip] >= mslxMaxLoginFail {
		ms.banned[ip] = now.Add(mslxBanDuration)
		delete(ms.failCount, ip)
		delete(ms.failSince, ip)
	}
}

// clearLoginFail 登录成功后清空失败计数。
func (ms *mslxCompat) clearLoginFail(ip string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	delete(ms.failCount, ip)
	delete(ms.failSince, ip)
}

// handleLogin 登录并签发 JWT。
func (ms *mslxCompat) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	ip := compatClientIP(r)
	ipKey := "unknown"
	if ip != nil {
		ipKey = ip.String()
	}
	if ms.isBanned(ipKey) {
		mslxFail(w, http.StatusForbidden, "登录失败次数过多，请稍后再试")
		return
	}
	var req struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		TwoFactorCode string `json:"twoFactorCode"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Username) != ms.user || !subtleEqual(req.Password, ms.pass) {
		ms.noteLoginFail(ipKey)
		mslxFail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	ms.clearLoginFail(ipKey)
	now := timesys.Now()
	claims := mslxClaims{
		UserID: mslxUserID,
		Name:   ms.user,
		Role:   "admin",
		Exp:    now.Add(mslxTokenTTL).Unix(),
	}
	token := ms.signJWT(claims)
	mslxOK(w, map[string]any{
		"token":     token,
		"expiresIn": int(mslxTokenTTL.Seconds()),
		"user":      ms.userView(),
	}, "登录成功")
}

// userView 当前账号视图（MSLX 的 UserInfo 子集）。
func (ms *mslxCompat) userView() map[string]any {
	return map[string]any{
		"id":        mslxUserID,
		"username":  ms.user,
		"name":      ms.user,
		"role":      "admin",
		"avatar":    "",
		"resources": []string{},
	}
}

// handleUser 用户相关接口（me / logout）。
func (ms *mslxCompat) handleUser(w http.ResponseWriter, r *http.Request, segs []string, claims *mslxClaims) {
	if len(segs) == 0 {
		http.NotFound(w, r)
		return
	}
	switch segs[0] {
	case "me":
		if r.Method == http.MethodPost && len(segs) > 1 && segs[1] == "update" {
			mslxFail(w, http.StatusNotImplemented, "irix-node 兼容层不支持修改该账号（请改 config.json 后重启）")
			return
		}
		mslxOK(w, ms.userView(), "获取成功")
	case "logout":
		// 无服务端会话状态：客户端丢弃 token 即可
		mslxOK(w, nil, "已退出登录")
	default:
		_ = claims
		mslxNotImplemented(w, "/api/user/"+strings.Join(segs, "/"))
	}
}

// handleStatus 节点运行状态。
// handleStatus 节点状态。
//
// 该接口同时是 MSLX 面板的子节点探活入口（GET /api/status + x-node-id，3 秒超时），
// 因此**允许匿名**访问；但匿名/节点代理请求只返回公开信息（版本、节点标识、
// 链接状态），主机指标与实例统计仅对通过本节点账户认证的调用者返回，
// 避免把兼容层端口变成信息泄露面。
func (ms *mslxCompat) handleStatus(w http.ResponseWriter, r *http.Request) {
	d := ms.d
	if nodeID := strings.TrimSpace(r.Header.Get("x-node-id")); nodeID != "" {
		ms.noteStatusProbe(nodeID)
	}
	link := ms.currentNodeLink()
	pub := map[string]any{
		"version":         Version,
		"daemonName":      "irix-node",
		"slaveMode":       ms.slaveMode,
		"linked":          link != nil,
		"protocolVersion": mslxNodeProtocolVersion,
		"time":            timesys.Now().UnixMilli(),
	}
	if link != nil {
		pub["nodeId"] = link.NodeID
	}
	// 注意：这里**只**认本节点账户令牌，绝不做回源主节点的核验。
	// 面板的探活是 3 秒超时，若在主节点不可达时回源（5 秒），探活就会超时，
	// 节点随即被判离线——这正是「链接成功但一直显示离线」的又一陷阱。
	if _, ok := ms.authenticate(r); !ok {
		mslxOK(w, pub, "获取成功")
		return
	}
	total := len(d.mcAllInstanceDetails())
	running := d.CountRunning()
	full := map[string]any{
		"version":         Version,
		"daemonName":      "irix-node",
		"instanceTotal":   total,
		"instanceRunning": running,
		"system":          mcSystemView(d),
		"slaveMode":       ms.slaveMode,
		"linked":          link != nil,
		"protocolVersion": mslxNodeProtocolVersion,
		"time":            timesys.Now().UnixMilli(),
	}
	if link != nil {
		full["nodeId"] = link.NodeID
	}
	mslxOK(w, full, "获取成功")
}

// ---------------------------------------------------------------------------
// 实例 id 映射（MSLX 用递增整数 id，节点内部用 uuid）
// ---------------------------------------------------------------------------

// mslxIDMap 持久化的 uuid ↔ 整数 id 映射。
type mslxIDMap struct {
	Next uint32            `json:"next"`
	IDs  map[string]uint32 `json:"ids"`
}

// idMapPath 映射文件路径。
func (ms *mslxCompat) idMapPath() string {
	dir := ms.d.DataDir
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	return filepath.Join(dir, mslxIDMapFile)
}

// loadIDMap 读取映射（不存在则初始化）。
func (ms *mslxCompat) loadIDMap() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.ids == nil {
		ms.ids = &mslxIDMap{Next: 1, IDs: map[string]uint32{}}
	}
	data, err := os.ReadFile(ms.idMapPath())
	if err != nil {
		return
	}
	var m mslxIDMap
	if err := json.Unmarshal(data, &m); err != nil || m.IDs == nil {
		return
	}
	if m.Next == 0 {
		m.Next = 1
	}
	ms.ids = &m
}

// saveIDMap 原子写回映射。
func (ms *mslxCompat) saveIDMap() {
	ms.mu.Lock()
	snapshot, _ := json.Marshal(ms.ids)
	path := ms.idMapPath()
	ms.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, snapshot, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// idFor 取（或分配）实例的整数 id。
func (ms *mslxCompat) idFor(uuid string) uint32 {
	ms.mu.Lock()
	if ms.ids == nil {
		ms.ids = &mslxIDMap{Next: 1, IDs: map[string]uint32{}}
	}
	if id, ok := ms.ids.IDs[uuid]; ok {
		ms.mu.Unlock()
		return id
	}
	id := ms.ids.Next
	ms.ids.Next++
	ms.ids.IDs[uuid] = id
	ms.mu.Unlock()
	ms.saveIDMap()
	return id
}

// uuidForID 反查实例 uuid。
func (ms *mslxCompat) uuidForID(id uint32) (string, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.ids == nil {
		return "", false
	}
	for uuid, v := range ms.ids.IDs {
		if v == id {
			return uuid, true
		}
	}
	return "", false
}

// instanceByID 解析 {id} 参数并返回实例。
func (ms *mslxCompat) instanceByID(raw string) (*Instance, uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return nil, 0, fmt.Errorf("实例 id 非法: %s", raw)
	}
	uuid, ok := ms.uuidForID(uint32(n))
	if !ok {
		return nil, uint32(n), fmt.Errorf("实例 %d 不存在", n)
	}
	inst := ms.d.Find(uuid)
	if inst == nil {
		return nil, uint32(n), fmt.Errorf("实例 %d 不存在", n)
	}
	return inst, uint32(n), nil
}

// mslxStatusText 实例状态文本（MSLX 用 stopped/running/starting…）。
func mslxStatusText(status int) string {
	switch status {
	case StatusRunning:
		return "running"
	case StatusStarting:
		return "starting"
	case StatusStopping:
		return "stopping"
	default:
		return "stopped"
	}
}

// ---------------------------------------------------------------------------
// 实例接口
// ---------------------------------------------------------------------------

// handleInstance 实例相关接口。
func (ms *mslxCompat) handleInstance(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		http.NotFound(w, r)
		return
	}
	switch segs[0] {
	case "list":
		ms.handleInstanceList(w, r)
	case "info":
		ms.handleInstanceInfo(w, r)
	case "action":
		ms.handleInstanceAction(w, r)
	case "settings":
		ms.handleInstanceSettings(w, r, segs[1:])
	case "backups":
		if len(segs) > 1 && segs[1] == "all" {
			mslxOK(w, []any{}, "获取成功")
			return
		}
		mslxNotImplemented(w, "/api/instance/backups")
	case "icon":
		// 本节点不提供图标：返回 404，面板会回退默认图标
		http.NotFound(w, r)
	case "createServer", "delete", "cancelCreation":
		mslxFail(w, http.StatusNotImplemented, "irix-node 兼容层不支持通过面板创建/删除实例（请使用节点 API 或配置文件）")
	case "tasks", "players", "export":
		mslxNotImplemented(w, "/api/instance/"+strings.Join(segs, "/"))
	default:
		http.NotFound(w, r)
	}
}

// handleInstanceList 实例列表。
func (ms *mslxCompat) handleInstanceList(w http.ResponseWriter, r *http.Request) {
	details := ms.d.mcAllInstanceDetails()
	out := make([]map[string]any, 0, len(details))
	for _, item := range details {
		uuid, _ := item["instanceUuid"].(string)
		cfg, _ := item["config"].(map[string]any)
		name, _ := cfg["nickname"].(string)
		cwd, _ := cfg["cwd"].(string)
		status, _ := item["status"].(int)
		out = append(out, map[string]any{
			"id":         ms.idFor(uuid),
			"name":       name,
			"basePath":   cwd,
			"java":       "",
			"core":       "none",
			"icon":       "custom",
			"status":     status,
			"statusText": mslxStatusText(status),
			"expireTime": nil,
			"extra": map[string]any{
				"onlinePlayers": 0,
			},
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a]["id"].(uint32) > out[b]["id"].(uint32)
	})
	mslxOK(w, out, "获取成功")
}

// handleInstanceInfo 单个实例详情。
func (ms *mslxCompat) handleInstanceInfo(w http.ResponseWriter, r *http.Request) {
	inst, id, err := ms.instanceByID(r.URL.Query().Get("id"))
	if err != nil {
		mslxFail(w, http.StatusNotFound, err.Error())
		return
	}
	inst.mu.Lock()
	cfg := inst.Config
	status := inst.Status
	started := inst.Started
	inst.mu.Unlock()
	mslxOK(w, map[string]any{
		"id":             id,
		"name":           cfg.Nickname,
		"basePath":       cfg.Cwd,
		"startCommand":   cfg.StartCommand,
		"stopCommand":    cfg.StopCommand,
		"status":         status,
		"statusText":     mslxStatusText(status),
		"started":        started,
		"createDatetime": cfg.CreateDatetime,
		"lastDatetime":   cfg.LastDatetime,
		"type":           cfg.Type,
		"extra": map[string]any{
			"onlinePlayers": 0,
			"autoStart":     cfg.EventTask.AutoStart,
			"autoRestart":   cfg.EventTask.AutoRestart,
		},
	}, "获取成功")
}

// handleInstanceAction 实例操作（start/stop/restart/forceExit/agreeEula/backup）。
func (ms *mslxCompat) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req struct {
		ID     *uint32 `json:"id"`
		Action string  `json:"action"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if req.ID == nil {
		mslxFail(w, http.StatusBadRequest, "缺少实例 id")
		return
	}
	uuid, ok := ms.uuidForID(*req.ID)
	if !ok {
		mslxFail(w, http.StatusNotFound, fmt.Sprintf("实例 %d 不存在", *req.ID))
		return
	}
	inst := ms.d.Find(uuid)
	if inst == nil {
		mslxFail(w, http.StatusNotFound, fmt.Sprintf("实例 %d 不存在", *req.ID))
		return
	}
	action := strings.TrimSpace(req.Action)
	if idx := strings.Index(action, "?"); idx >= 0 {
		action = action[:idx]
	}
	switch action {
	case "start":
		if err := ms.d.startInstance(inst); err != nil {
			mslxOK(w, nil, "服务器开启失败："+err.Error())
			return
		}
		inst.mu.Lock()
		inst.Started++
		inst.mu.Unlock()
		ms.d.markDirty()
		mslxOK(w, nil, "服务器已启动")
	case "stop":
		if err := ms.d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
			mslxOK(w, nil, "服务器停止失败："+err.Error())
			return
		}
		mslxOK(w, nil, "已发送停止服务器指令")
	case "restart":
		err := ms.d.stopInstance(inst)
		if err != nil && !errors.Is(err, errNotRunning) {
			mslxOK(w, nil, "服务器重启失败："+err.Error())
			return
		}
		if err := ms.d.startInstance(inst); err != nil {
			mslxOK(w, nil, "服务器重启失败："+err.Error())
			return
		}
		mslxOK(w, nil, "服务器已重启")
	case "forceExit":
		if err := ms.d.compatKillInstance(inst); err != nil {
			mslxOK(w, nil, "实例强制结束失败："+err.Error())
			return
		}
		mslxOK(w, nil, "已发送强制结束进程指令")
	case "agreeEula":
		mslxOK(w, nil, "执行成功")
	case "backup":
		mslxFail(w, http.StatusNotImplemented, "irix-node 兼容层暂不支持面板发起的备份")
	default:
		mslxFail(w, http.StatusBadRequest, "无效的操作: "+req.Action)
	}
}

// handleInstanceSettings 实例设置（general/{id}）。
func (ms *mslxCompat) handleInstanceSettings(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 2 || segs[0] != "general" {
		mslxNotImplemented(w, "/api/instance/settings/"+strings.Join(segs, "/"))
		return
	}
	inst, _, err := ms.instanceByID(segs[1])
	if err != nil {
		mslxFail(w, http.StatusNotFound, err.Error())
		return
	}
	inst.mu.Lock()
	cfg := inst.Config
	inst.mu.Unlock()
	if r.Method == http.MethodGet {
		mslxOK(w, map[string]any{
			"name":         cfg.Nickname,
			"startCommand": cfg.StartCommand,
			"stopCommand":  cfg.StopCommand,
			"basePath":     cfg.Cwd,
			"ie":           cfg.IE,
			"oe":           cfg.OE,
			"autoStart":    cfg.EventTask.AutoStart,
			"autoRestart":  cfg.EventTask.AutoRestart,
			"tags":         cfg.Tag,
		}, "获取成功")
		return
	}
	var in mcConfigInput
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		mslxFail(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	// MSLX 设置页用 name/basePath/autoStart 等字段名，映射到本节点配置字段。
	var extra struct {
		Name        *string `json:"name"`
		BasePath    *string `json:"basePath"`
		AutoStart   *bool   `json:"autoStart"`
		AutoRestart *bool   `json:"autoRestart"`
	}
	_ = json.Unmarshal(body, &extra)
	if extra.Name != nil {
		in.Nickname = extra.Name
	}
	if extra.BasePath != nil {
		in.Cwd = extra.BasePath
	}
	if extra.AutoStart != nil || extra.AutoRestart != nil {
		if in.EventTask == nil {
			in.EventTask = &struct {
				AutoStart   *bool `json:"autoStart"`
				AutoRestart *bool `json:"autoRestart"`
				Ignore      *bool `json:"ignore"`
			}{}
		}
		if extra.AutoStart != nil {
			in.EventTask.AutoStart = extra.AutoStart
		}
		if extra.AutoRestart != nil {
			in.EventTask.AutoRestart = extra.AutoRestart
		}
	}

	if err := ms.applySettings(inst, &in); err != nil {
		mslxFail(w, http.StatusBadRequest, err.Error())
		return
	}
	mslxOK(w, nil, "保存成功")
}

// applySettings 应用设置变更（按本节点支持的字段增量合并）。
func (ms *mslxCompat) applySettings(inst *Instance, in *mcConfigInput) error {
	inst.mu.Lock()
	cfg := inst.Config
	inst.mu.Unlock()
	in.apply(&cfg)
	cfg.FillDefaults()
	abs, err := ms.d.normalizeInstanceCwd(cfg.Cwd)
	if err != nil {
		return err
	}
	cfg.Cwd = abs
	return ms.d.UpdateInstance(inst, cfg)
}

// handleSettings 节点设置（只读 + 记录型写入）。
func (ms *mslxCompat) handleSettings(w http.ResponseWriter, r *http.Request) {
	d := ms.d
	if r.Method == http.MethodGet {
		mslxOK(w, map[string]any{
			"listenPort":  d.Port,
			"daemonName":  "irix-node",
			"version":     Version,
			"language":    "zh_cn",
			"dataPath":    d.DataDir,
			"readOnly":    true,
			"compatLayer": ms.addr,
		}, "获取成功")
		return
	}
	mslxFail(w, http.StatusNotImplemented,
		"irix-node 兼容层不支持通过面板修改节点设置（请修改 config.json 后重启）")
}
