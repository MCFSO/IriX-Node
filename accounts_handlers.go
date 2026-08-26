// 账户管理 HTTP 处理器与路由注册（docs/accounts-design.md）。
//
// 认证通道：
//   - apikey（配对码 / 固定密钥）→ root 管理员，兼容既有前端，不做端点限制；
//   - 账户会话 token → 按端点开关鉴权；root 首次登录（尚未设置独立密码）的
//     会话处于「强制改密」状态：除改密/登出/查看自身与权限目录外一律 403。
//
// 密码规则：
//   - root 初始凭据 = 配对码（或 -apikey），首次登录后必须设置新密码
//     （PUT /api/accounts/password 带 oldPassword），设置后配对码不再用于登录
//     （但 apikey 通道为兼容既有客户端保持不变）；
//   - 管理员可直接重置任意账户密码（含 root，带 username + password），无需旧密码。

package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// identity 认证后的请求身份。
type identity struct {
	username   string
	isAdmin    bool
	mustChange bool // 强制改密状态（root 首次登录，配对码尚未替换）
	via        string
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

// bearerToken 提取会话 token：Authorization: Bearer <token> 或 X-Auth-Token 头。
func bearerToken(r *http.Request) string {
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		return t
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// authenticate 认证请求：
//  1. apikey（查询参数 / X-Api-Key 头）→ root 管理员（配对码机制不变）；
//  2. 会话 token → 账户身份（token 不存在/过期/账户已删 → nil）；
//     root 会话在尚未设置独立密码时标记 mustChange（强制首次改密）。
func (d *Daemon) authenticate(r *http.Request) *identity {
	if d.authOK(r) {
		return &identity{username: accountRoot, isAdmin: true, via: "apikey"}
	}
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
	id := &identity{username: sess.Username, isAdmin: sess.IsAdmin, via: "token"}
	if sess.Username == accountRoot && !d.accounts.rootPasswordSet() {
		id.mustChange = true
	}
	return id
}

// requireAdmin 要求管理员身份（root / is_admin 账户）；不满足时写 403 并返回 false。
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
	Token              string `json:"token"`
	Username           string `json:"username"`
	IsAdmin            bool   `json:"isAdmin"`
	MustChangePassword bool   `json:"mustChangePassword"`
	ExpiresAt          int64  `json:"expiresAt"`
}

// handleAccountLogin 登录：root（配对码/固定 apikey，首次强制改密）
// 或普通账户（密码）。
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

	var (
		sess       accountSession
		mustChange bool
	)
	switch {
	case body.Username == accountRoot:
		// 已设置独立密码：只认新密码；否则只认配对码/固定 apikey（首次登录）
		if d.accounts.rootPasswordSet() {
			if _, ok := d.accounts.checkPassword(accountRoot, body.Password); !ok {
				writeError(w, http.StatusUnauthorized, "用户名或密码错误")
				return
			}
		} else {
			if !d.checkRootPassword(body.Password) {
				writeError(w, http.StatusUnauthorized, "用户名或密码错误")
				return
			}
			mustChange = true
		}
		sess = accountSession{Username: accountRoot, IsAdmin: true, ExpiresAt: time.Now().Add(accountSessionTTL).UnixMilli()}
	default:
		a, ok := d.accounts.checkPassword(body.Username, body.Password)
		if !ok {
			writeError(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		sess = accountSession{Username: a.Username, IsAdmin: a.IsAdmin, ExpiresAt: time.Now().Add(accountSessionTTL).UnixMilli()}
	}
	token, err := newAccountToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成会话失败: "+err.Error())
		return
	}
	d.accounts.putSession(token, sess, accountSessionTTL)
	writeOK(w, accountLoginResponse{
		Token:              token,
		Username:           sess.Username,
		IsAdmin:            sess.IsAdmin,
		MustChangePassword: mustChange,
		ExpiresAt:          sess.ExpiresAt,
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
	if id.username == accountRoot {
		writeOK(w, map[string]any{
			"username":           accountRoot,
			"isAdmin":            true,
			"mustChangePassword": !d.accounts.rootPasswordSet(),
			"permissions":        nil,
		})
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

// handleAccountsList 账户列表（root 虚拟条目在前，含强制改密状态）。
// GET /api/accounts
func (d *Daemon) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	out := []map[string]any{{
		"username":           accountRoot,
		"isAdmin":            true,
		"builtin":            true,
		"mustChangePassword": !d.accounts.rootPasswordSet(),
	}}
	list, err := d.accounts.listAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账户列表失败: "+err.Error())
		return
	}
	for _, a := range list {
		if a.Username == accountRoot {
			continue // root 行只在设置了独立密码后存在，列表用上面的内置条目表示
		}
		out = append(out, map[string]any{
			"username":  a.Username,
			"isAdmin":   a.IsAdmin,
			"builtin":   false,
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
	if body.Username == accountRoot {
		writeError(w, http.StatusBadRequest, "root 为内置管理员账户，不可创建")
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

// handleAccountDelete 删除账户（管理员；root 不可删）。
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
	if username == accountRoot {
		writeError(w, http.StatusBadRequest, "root 为内置管理员账户，不可删除")
		return
	}
	if err := d.accounts.deleteAccount(username); err != nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	writeOK(w, true)
}

// handleAccountPassword 修改密码（两种模式）：
//   - 自己改密：{oldPassword, newPassword}（root 首次登录强制改密走这里；
//     oldPassword = 配对码/固定 apikey 或已设置的密码）；
//   - 管理员重置：{username, password}（可直接重置任意账户含 root，无需旧密码）。
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
	// 自己改密（任何登录用户，含 root 首次登录强制改密）
	if body.OldPassword != "" || body.NewPassword != "" {
		if body.OldPassword == "" || body.NewPassword == "" {
			writeError(w, http.StatusBadRequest, "自己改密需要 oldPassword 与 newPassword")
			return
		}
		ok := false
		if id.username == accountRoot {
			if d.accounts.rootPasswordSet() {
				_, ok = d.accounts.checkPassword(accountRoot, body.OldPassword)
			} else {
				ok = d.checkRootPassword(body.OldPassword)
			}
		} else {
			_, ok = d.accounts.checkPassword(id.username, body.OldPassword)
		}
		if !ok {
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
	// 管理员直接重置（含 root，无需旧密码）
	if !requireAdmin(w, r) {
		return
	}
	target := strings.TrimSpace(body.Username)
	if target == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "管理员重置需要 username 与 password")
		return
	}
	if target != accountRoot {
		a, err := d.accounts.getAccount(target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取账户失败: "+err.Error())
			return
		}
		if a == nil {
			writeError(w, http.StatusNotFound, "账户不存在")
			return
		}
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

// handleAccountPermissions 修改账户端点开关（管理员；root 恒全权限不可改）。
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
	if body.Username == accountRoot {
		writeError(w, http.StatusBadRequest, "root 为内置管理员，不受端点开关限制")
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
