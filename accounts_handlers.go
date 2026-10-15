// 账户管理 HTTP 处理器与路由注册（docs/accounts-design.md）。
//
// 认证通道：
//   - 账户会话 token（Authorization: Bearer / X-Auth-Token / ?token=）→
//     按端点开关鉴权；管理员（is_admin 账户）旁路端点开关；
//   - 直连票据（/download/ /upload/）自带授权，维持不变。
//
// 首次启动引导（auth.go）：账户库中不存在管理员账户时随机生成一个
// 管理员账户（用户名/密码仅终端显示一次），登录后可在账户管理里
// 创建更多账户、修改自身密码、逐端点开启权限。
//
// 密码规则：账户密码 ≥ 8 位；管理员可直接重置任意账户密码
// （PUT /api/accounts/password 带 username + password，无需旧密码）。

package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// identity 认证后的请求身份。
type identity struct {
	username string
	isAdmin  bool
	via      string
}

type ctxIdentityKey struct{}

// withIdentity 把身份注入请求上下文。
func withIdentity(r *http.Request, id *identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxIdentityKey{}, id))
}

// identityOf 读取请求上下文中的身份（仅 auth 包装器之后可用）。
func identityOf(r *http.Request) *identity {
	id, _ := r.Context().Value(ctxIdentityKey{}).(*identity)
	return id
}

// bearerToken 提取会话 token：
//  1. X-Auth-Token 头；
//  2. Authorization: Bearer <token>；
//  3. ?token= 查询参数（浏览器 WebSocket 无法自定义请求头等场景；
//     审计日志已对 token 参数打码，优先使用前两种方式）。
func bearerToken(r *http.Request) string {
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		return t
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// authenticate 认证请求：会话 token → 账户身份
// （token 不存在/过期/账户已删 → nil，由 auth 包装器拒绝）。
func (d *Daemon) authenticate(r *http.Request) *identity {
	if d.accounts == nil {
		return nil
	}
	token := bearerToken(r)
	if token == "" {
		return nil
	}
	sess, ok := d.accounts.lookupSession(token)
	if !ok || sess.ExpiresAt <= time.Now().UnixMilli() {
		return nil
	}
	// 滑动续期：剩余不足一半时延长一整个 TTL（每天最多写一次，避免频繁落库）
	if remain := sess.ExpiresAt - time.Now().UnixMilli(); remain < int64(accountSessionTTL/2/time.Millisecond) {
		sess.ExpiresAt = time.Now().Add(accountSessionTTL).UnixMilli()
		d.accounts.putSession(token, sess, accountSessionTTL)
	}
	return &identity{username: sess.Username, isAdmin: sess.IsAdmin, via: "token"}
}

// requireAdmin 要求管理员身份（is_admin 账户）；不满足时写 403 并返回 false。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id := identityOf(r)
	if id != nil && id.isAdmin {
		return true
	}
	writeError(w, http.StatusForbidden, "需要管理员权限")
	return false
}

// registerAccountRoutes 注册账户管理路由（认证端点纳入权限目录「账户」组）。
func (d *Daemon) registerAccountRoutes(mux *http.ServeMux) {
	// 登录入口：公开（拿凭证的接口），不在权限目录内
	mux.HandleFunc("POST /api/auth/login", d.handleAccountLogin)
	perm("账户", "POST /api/auth/logout", "退出登录")
	mux.HandleFunc("POST /api/auth/logout", d.auth(d.handleAccountLogout))
	perm("账户", "GET /api/accounts/me", "查看当前账户信息")
	mux.HandleFunc("GET /api/accounts/me", d.auth(d.handleAccountMe))
	perm("账户", "GET /api/accounts/catalog", "查看权限目录")
	mux.HandleFunc("GET /api/accounts/catalog", d.auth(d.handleAccountCatalog))
	perm("账户", "PUT /api/accounts/password", "修改密码")
	mux.HandleFunc("PUT /api/accounts/password", d.auth(d.handleAccountPassword))
	perm("账户", "GET /api/accounts", "查看账户列表")
	mux.HandleFunc("GET /api/accounts", d.auth(d.handleAccountsList))
	perm("账户", "POST /api/accounts", "创建账户")
	mux.HandleFunc("POST /api/accounts", d.auth(d.handleAccountCreate))
	perm("账户", "DELETE /api/accounts", "删除账户")
	mux.HandleFunc("DELETE /api/accounts", d.auth(d.handleAccountDelete))
	perm("账户", "PUT /api/accounts/permissions", "修改账户权限开关")
	mux.HandleFunc("PUT /api/accounts/permissions", d.auth(d.handleAccountPermissions))
}

// accountLoginResponse 登录响应。
type accountLoginResponse struct {
	Token     string `json:"token"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	ExpiresAt int64  `json:"expiresAt"`
}

// handleAccountLogin 登录：校验账户密码并签发会话 token。
// POST /api/auth/login body: {username, password}
func (d *Daemon) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		writeError(w, http.StatusBadRequest, "缺少 username 参数")
		return
	}
	d.accounts.purgeExpiredSessions()

	a, ok := d.accounts.checkPassword(body.Username, body.Password)
	if !ok {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	sess := accountSession{Username: a.Username, IsAdmin: a.IsAdmin, ExpiresAt: time.Now().Add(accountSessionTTL).UnixMilli()}
	token, err := newAccountToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成会话失败: "+err.Error())
		return
	}
	d.accounts.putSession(token, sess, accountSessionTTL)
	writeOK(w, accountLoginResponse{
		Token:     token,
		Username:  sess.Username,
		IsAdmin:   sess.IsAdmin,
		ExpiresAt: sess.ExpiresAt,
	})
}

// handleAccountLogout 退出登录：删除当前会话。
// POST /api/auth/logout
func (d *Daemon) handleAccountLogout(w http.ResponseWriter, r *http.Request) {
	if token := bearerToken(r); token != "" && d.accounts != nil {
		d.accounts.delSession(token)
	}
	writeOK(w, true)
}

// handleAccountMe 当前账户信息（含自身端点开关）。
// GET /api/accounts/me
func (d *Daemon) handleAccountMe(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	if id == nil {
		writeError(w, http.StatusForbidden, "未认证")
		return
	}
	a, err := d.accounts.getAccount(id.username)
	if err != nil || a == nil {
		writeError(w, http.StatusUnauthorized, "账户不存在")
		return
	}
	writeOK(w, map[string]any{
		"username":    a.Username,
		"isAdmin":     a.IsAdmin,
		"permissions": a.Permissions,
		"createdAt":   a.CreatedAt,
	})
}

// handleAccountCatalog 权限目录（分组 + 端点 + 描述，供开关 UI 渲染）。
// GET /api/accounts/catalog
func (d *Daemon) handleAccountCatalog(w http.ResponseWriter, r *http.Request) {
	writeOK(w, permCatalog())
}

// handleAccountsList 账户列表。
// GET /api/accounts
func (d *Daemon) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	list, err := d.accounts.listAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账户列表失败: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{
			"username":  a.Username,
			"isAdmin":   a.IsAdmin,
			"createdAt": a.CreatedAt,
			"updatedAt": a.UpdatedAt,
		})
	}
	writeOK(w, out)
}

// handleAccountCreate 创建账户（管理员）。
// POST /api/accounts body: {username, password, isAdmin}
func (d *Daemon) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"isAdmin"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if !validUsername(body.Username) {
		writeError(w, http.StatusBadRequest, "用户名格式无效（1-64 位字母/数字/下划线/连字符）")
		return
	}
	if len(body.Password) < accountPassMinLen {
		writeError(w, http.StatusBadRequest, "密码长度不能少于 8 位")
		return
	}
	if a, err := d.accounts.getAccount(body.Username); err != nil {
		writeError(w, http.StatusInternalServerError, "读取账户失败: "+err.Error())
		return
	} else if a != nil {
		writeError(w, http.StatusConflict, "账户已存在")
		return
	}
	if err := d.accounts.createAccount(body.Username, body.Password, body.IsAdmin); err != nil {
		writeError(w, http.StatusInternalServerError, "创建账户失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"username": body.Username})
}

// handleAccountDelete 删除账户（管理员；连带删除其全部会话）。
// DELETE /api/accounts?username=
func (d *Daemon) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	username := strings.TrimSpace(queryParam(r, "username"))
	if username == "" {
		writeError(w, http.StatusBadRequest, "缺少 username 参数")
		return
	}
	if id := identityOf(r); id != nil && id.username == username {
		writeError(w, http.StatusBadRequest, "不能删除当前登录的账户")
		return
	}
	if err := d.accounts.deleteAccount(username); err != nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	writeOK(w, true)
}

// handleAccountPassword 修改密码（两种模式）：
//   - 自己改密：{oldPassword, newPassword}（任何登录账户，旧密码为当前密码）；
//   - 管理员重置：{username, password}（可直接重置任意账户密码，无需旧密码）。
//
// PUT /api/accounts/password
func (d *Daemon) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	if id == nil {
		writeError(w, http.StatusForbidden, "未认证")
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	// 自己改密（任何登录账户）
	if body.OldPassword != "" || body.NewPassword != "" {
		if body.OldPassword == "" || body.NewPassword == "" {
			writeError(w, http.StatusBadRequest, "自己改密需要 oldPassword 与 newPassword")
			return
		}
		if _, ok := d.accounts.checkPassword(id.username, body.OldPassword); !ok {
			writeError(w, http.StatusUnauthorized, "原密码错误")
			return
		}
		if len(body.NewPassword) < accountPassMinLen {
			writeError(w, http.StatusBadRequest, "密码长度不能少于 8 位")
			return
		}
		if err := d.accounts.putPassword(id.username, body.NewPassword); err != nil {
			writeError(w, http.StatusInternalServerError, "修改密码失败: "+err.Error())
			return
		}
		writeOK(w, map[string]any{"username": id.username})
		return
	}
	// 管理员直接重置（无需旧密码）
	if !requireAdmin(w, r) {
		return
	}
	target := strings.TrimSpace(body.Username)
	if target == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "管理员重置需要 username 与 password")
		return
	}
	a, err := d.accounts.getAccount(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账户失败: "+err.Error())
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	if len(body.Password) < accountPassMinLen {
		writeError(w, http.StatusBadRequest, "密码长度不能少于 8 位")
		return
	}
	if err := d.accounts.putPassword(target, body.Password); err != nil {
		writeError(w, http.StatusInternalServerError, "修改密码失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"username": target})
}

// handleAccountPermissions 修改账户端点开关（管理员）。
// PUT /api/accounts/permissions body:
//   - 整组开关：{username, group: "文件", enabled: true}
//   - 逐条开关：{username, permissions: {"GET /api/files/list": true, ...}}
func (d *Daemon) handleAccountPermissions(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Username    string          `json:"username"`
		Group       string          `json:"group"`
		Enabled     *bool           `json:"enabled"`
		Permissions map[string]bool `json:"permissions"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		writeError(w, http.StatusBadRequest, "缺少 username 参数")
		return
	}
	a, err := d.accounts.getAccount(body.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账户失败: "+err.Error())
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	if a.IsAdmin {
		writeError(w, http.StatusBadRequest, "管理员账户不受端点开关限制")
		return
	}
	if body.Group == "" && len(body.Permissions) == 0 {
		writeError(w, http.StatusBadRequest, "缺少权限变更参数（group+enabled 或 permissions）")
		return
	}
	cur := d.accounts.loadPermissions(body.Username)
	changed := false
	// 整组开关：对组内全部端点统一开/关
	if body.Group != "" {
		if body.Enabled == nil {
			writeError(w, http.StatusBadRequest, "整组开关需要 enabled 参数")
			return
		}
		found := false
		for _, g := range permCatalog() {
			if g.Name != body.Group {
				continue
			}
			found = true
			for _, e := range g.Entries {
				cur[e.Key] = *body.Enabled
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "未知权限分组: "+body.Group)
			return
		}
		changed = true
	}
	// 逐条开关：只更新出现的键（未出现的保持不变）
	for k, v := range body.Permissions {
		if !permKeyExists(k) {
			writeError(w, http.StatusBadRequest, "未知权限端点: "+k)
			return
		}
		cur[k] = v
		changed = true
	}
	if !changed {
		writeError(w, http.StatusBadRequest, "没有可应用的权限变更")
		return
	}
	if err := d.accounts.setPermissions(body.Username, cur); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, cur)
}
