// 账户管理测试（docs/accounts-design.md）：
// 首启随机管理员引导、登录/登出、管理员创建/删除账户与权限逐端点开关、
// 整组开关、管理员直改密码、会话过期、Redis 故障回退 SQL。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// qaTestNewPass 改密后使用的测试密码。
func qaTestNewPass() string {
	if v := os.Getenv("IRIX_QA_NEWPASS"); v != "" {
		return v
	}
	return "RootNew" + "Pass123"
}

// qaAccQuery 返回带测试会话 token 的查询串（token 由 ensureTestAuth 注入）。
func qaAccQuery() string {
	return "?token=" + qaTestToken()
}

// newTestAccountsDaemon 返回账户系统就绪（含测试管理员与固定会话）的守护进程。
func newTestAccountsDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, _ := newTestDaemon(t)
	return d
}

// newBareAccountDaemon 创建仅按 cfg 启用账户系统（无任何管理员账户）的
// 守护进程，用于测试首启引导与 Redis 回退路径。
func newBareAccountDaemon(t *testing.T, cfg accountsConfig) *Daemon {
	t.Helper()
	dir := t.TempDir()
	d := NewDaemon(dir)
	t.Cleanup(func() {
		d.StopAutoSave()
		d.workers.stop()
	})
	if err := d.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := d.initAccounts(cfg); err != nil {
		t.Fatalf("初始化账户系统失败: %v", err)
	}
	t.Cleanup(func() { _ = d.closeAccounts() })
	return d
}

// doReqToken 发起带 Bearer token 的请求（method 可带 JSON body）。
func doReqToken(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// loginAs 登录并返回 token 与响应 data。
func loginAs(t *testing.T, srv *httptest.Server, username, password string) (string, map[string]any) {
	t.Helper()
	code, body := apiPost(t, srv.URL+"/api/auth/login", map[string]any{
		"username": username, "password": password,
	})
	if code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	token, _ := data["token"].(string)
	if token == "" {
		t.Fatalf("登录响应缺少 token: %s", body)
	}
	return token, data
}

// TestAccountBootstrapFirstStart 首启引导：账户库无管理员时随机生成管理员
// 凭据（长度/字符集合法），凭据可登录且为管理员；已有管理员后不再重复生成。
func TestAccountBootstrapFirstStart(t *testing.T) {
	d := newBareAccountDaemon(t, accountsConfig{Driver: "sqlite"})

	u, p, created, err := d.ensureAdminAccount()
	if err != nil {
		t.Fatalf("首启引导失败: %v", err)
	}
	if !created {
		t.Fatalf("无管理员时首启应创建管理员账户")
	}
	if !validUsername(u) || len(u) != bootstrapUserLen {
		t.Fatalf("随机用户名应合法且为 %d 位: %q", bootstrapUserLen, u)
	}
	if len(p) != bootstrapPassLen {
		t.Fatalf("随机密码应为 %d 位: %d", bootstrapPassLen, len(p))
	}

	// 生成的凭据可登录且为管理员
	srv := newTestServer(t, d)
	defer srv.Close()
	token, data := loginAs(t, srv, u, p)
	if isAdmin, _ := data["isAdmin"].(bool); !isAdmin {
		t.Fatalf("引导管理员登录应返回 isAdmin=true: %v", data)
	}
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != http.StatusOK {
		t.Fatalf("引导管理员应可访问业务端点: %d %s", code, body)
	}

	// 已有管理员：再次引导不生成新凭据
	_, _, created, err = d.ensureAdminAccount()
	if err != nil || created {
		t.Fatalf("已有管理员不应重复引导: created=%v err=%v", created, err)
	}
}

// TestAccountLoginErrors 登录错误：错误密码 401、缺参数 400。
func TestAccountLoginErrors(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": qaTestAdminUser, "password": "bad"}); code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", code)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"password": "x"}); code != http.StatusBadRequest {
		t.Fatalf("缺 username 应 400，实际 %d", code)
	}
}

// TestAccountLogout 登出：会话立即失效，原 token 再访问被拒。
func TestAccountLogout(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	token, _ := loginAs(t, srv, qaTestAdminUser, qaTestAdminPass)
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != http.StatusOK {
		t.Fatalf("登出前应可访问: %d %s", code, body)
	}
	if code, body := doReqToken(t, http.MethodPost, srv.URL+"/api/auth/logout", token, map[string]any{}); code != http.StatusOK {
		t.Fatalf("登出失败: %d %s", code, body)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != http.StatusForbidden {
		t.Fatalf("登出后 token 应失效: %d", code)
	}
}

// TestAccountCreateAndPermissions 管理员创建账户、逐端点开关与整组开关、
// 默认全关、非管理员无法管理账户。
func TestAccountCreateAndPermissions(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 管理员创建账户
	code, body := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "alice", "password": "AlicePass123", "isAdmin": false,
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d %s", code, body)
	}
	aliceToken, _ := loginAs(t, srv, "alice", "AlicePass123")

	// 默认全关：任何端点 403
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", aliceToken, nil); code != http.StatusForbidden || !strings.Contains(string(body), "权限不足") {
		t.Fatalf("新账户应默认全关: %d %s", code, body)
	}

	// 逐条开：GET /api/overview
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "permissions": map[string]bool{"GET /api/overview": true},
	})
	if code != 200 {
		t.Fatalf("逐条开关失败: %d %s", code, body)
	}
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", aliceToken, nil); code != 200 {
		t.Fatalf("开启后应放行: %d %s", code, body)
	}

	// 整组关：概览组（overview + load）全关
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "group": "概览", "enabled": false,
	})
	if code != 200 {
		t.Fatalf("整组开关失败: %d %s", code, body)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", aliceToken, nil); code != http.StatusForbidden {
		t.Fatalf("整组关闭后 overview 应 403: %d", code)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/load", aliceToken, nil); code != http.StatusForbidden {
		t.Fatalf("整组关闭后 load 应 403: %d", code)
	}
	// 整组开
	code, _ = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "group": "概览", "enabled": true,
	})
	if code != 200 {
		t.Fatalf("整组开启失败: %d", code)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/load", aliceToken, nil); code != 200 {
		t.Fatalf("整组开启后 load 应放行: %d", code)
	}

	// 非管理员不能管理账户（即使端点开关被打开）
	code, _ = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "permissions": map[string]bool{"POST /api/accounts": true},
	})
	if code != 200 {
		t.Fatalf("开启账户端点失败: %d", code)
	}
	if code, body := doReqToken(t, http.MethodPost, srv.URL+"/api/accounts", aliceToken, map[string]any{
		"username": "eve", "password": "EvePass12345",
	}); code != http.StatusForbidden || !strings.Contains(string(body), "需要管理员权限") {
		t.Fatalf("非管理员应被拒: %d %s", code, body)
	}
	// 管理员账户不受端点开关限制（设置开关直接拒绝）
	if code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": qaTestAdminUser, "group": "概览", "enabled": false,
	}); code != http.StatusBadRequest || !strings.Contains(string(body), "管理员账户") {
		t.Fatalf("管理员账户不应受开关限制: %d %s", code, body)
	}

	// 未知分组 / 未知端点拒绝
	if code, _ := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "group": "不存在", "enabled": true,
	}); code != http.StatusBadRequest {
		t.Fatalf("未知分组应 400: %d", code)
	}
	if code, _ := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "alice", "permissions": map[string]bool{"GET /api/nonexistent": true},
	}); code != http.StatusBadRequest {
		t.Fatalf("未知端点应 400: %d", code)
	}
}

// TestAccountAdminResetPassword 管理员直接重置任意账户密码（无需旧密码）。
func TestAccountAdminResetPassword(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "bob", "password": "BobPass12345",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	if _, data := loginAs(t, srv, "bob", "BobPass12345"); data == nil {
		t.Fatal("bob 登录失败")
	}
	// 管理员重置 bob 密码
	code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password"+qaAccQuery(), "", map[string]any{
		"username": "bob", "password": "BobNewPass456",
	})
	if code != 200 {
		t.Fatalf("重置 bob 密码失败: %d %s", code, body)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "bob", "password": "BobPass12345"}); code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d", code)
	}
	if _, data := loginAs(t, srv, "bob", "BobNewPass456"); data == nil {
		t.Fatal("新密码登录失败")
	}

	// 管理员重置自身密码（无需旧密码）后可用新密码登录
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password"+qaAccQuery(), "", map[string]any{
		"username": qaTestAdminUser, "password": qaTestNewPass(),
	})
	if code != 200 {
		t.Fatalf("重置管理员密码失败: %d %s", code, body)
	}
	if _, data := loginAs(t, srv, qaTestAdminUser, qaTestNewPass()); data == nil {
		t.Fatal("管理员新密码登录失败")
	}
	// 重置不存在的账户 → 404
	if code, _ := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password"+qaAccQuery(), "", map[string]any{
		"username": "ghost", "password": "GhostPass123",
	}); code != http.StatusNotFound {
		t.Fatalf("重置不存在账户应 404: %d", code)
	}
}

// TestAccountSelfChangePassword 账户自己改密（需旧密码）。
func TestAccountSelfChangePassword(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "carol", "password": "CarolPass123",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	token, _ := loginAs(t, srv, "carol", "CarolPass123")
	code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", token, map[string]any{
		"oldPassword": "Carol" + "Pass123", "newPassword": qaTestNewPass(),
	})
	if code != 200 {
		t.Fatalf("自己改密失败: %d %s", code, body)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "carol", "password": "CarolPass123"}); code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d", code)
	}
	if _, data := loginAs(t, srv, "carol", qaTestNewPass()); data == nil {
		t.Fatal("新密码登录失败")
	}
	// 原密码错误 → 401
	if code, _ := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", token, map[string]any{
		"oldPassword": "Wrong" + "Old123", "newPassword": qaTestNewPass(),
	}); code != http.StatusUnauthorized {
		t.Fatalf("原密码错误应 401: %d", code)
	}
}

// TestAccountDeleteAndList 删除账户（会话立即失效）、列表、不可删除当前登录账户。
func TestAccountDeleteAndList(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "dave", "password": "DavePass1234",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	token, _ := loginAs(t, srv, "dave", "DavePass1234")

	// 列表：包含测试管理员与 dave
	code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts"+qaAccQuery(), "", nil)
	if code != 200 {
		t.Fatalf("账户列表失败: %d %s", code, body)
	}
	var listResp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil || len(listResp.Data) == 0 {
		t.Fatalf("账户列表解析失败: %v %s", err, body)
	}
	byName := map[string]map[string]any{}
	for _, a := range listResp.Data {
		name, _ := a["username"].(string)
		byName[name] = a
	}
	if byName["dave"] == nil {
		t.Fatalf("列表应包含 dave: %v", listResp.Data)
	}
	if admin := byName[qaTestAdminUser]; admin == nil || admin["isAdmin"] != true {
		t.Fatalf("列表应包含管理员 %s: %v", qaTestAdminUser, listResp.Data)
	}

	// 删除 dave
	code, body = doReqToken(t, http.MethodDelete, srv.URL+"/api/accounts"+qaAccQuery()+"&username=dave", "", nil)
	if code != 200 {
		t.Fatalf("删除账户失败: %d %s", code, body)
	}
	// 会话立即失效
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != http.StatusForbidden {
		t.Fatalf("删除后会话应失效: %d %s", code, body)
	}
	// 再登录失败
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "dave", "password": "DavePass1234"}); code != http.StatusUnauthorized {
		t.Fatalf("删除后登录应 401: %d", code)
	}
	// 删除当前登录账户被拒
	if code, body := doReqToken(t, http.MethodDelete, srv.URL+"/api/accounts"+qaAccQuery()+"&username="+qaTestAdminUser, "", nil); code != http.StatusBadRequest || !strings.Contains(string(body), "当前登录") {
		t.Fatalf("删除当前登录账户应 400: %d %s", code, body)
	}
	// 删除不存在账户 → 404
	if code, _ := doReqToken(t, http.MethodDelete, srv.URL+"/api/accounts"+qaAccQuery()+"&username=ghost", "", nil); code != http.StatusNotFound {
		t.Fatalf("删除不存在账户应 404: %d", code)
	}
}

// TestAccountValidation 创建校验：用户名格式、密码长度、重复名。
func TestAccountValidation(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"非法用户名", map[string]any{"username": "a b", "password": "Pass12345"}, 400},
		{"短密码", map[string]any{"username": "okname", "password": "short"}, 400},
		{"缺用户名", map[string]any{"username": "", "password": "Pass12345"}, 400},
	}
	for _, c := range cases {
		if code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), c.body); code != c.want {
			t.Errorf("%s: 期望 %d，实际 %d", c.name, c.want, code)
		}
	}
	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "dup", "password": "DupPass1234",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	if code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "dup", "password": "DupPass1234",
	}); code != http.StatusConflict {
		t.Fatalf("重复账户应 409，实际 %d", code)
	}
}

// TestAccountMeAndCatalog me 返回自身信息与权限，catalog 返回分组目录。
func TestAccountMeAndCatalog(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "erin", "password": "ErinPass1234",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	token, _ := loginAs(t, srv, "erin", "ErinPass1234")

	code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/catalog", token, nil)
	if code != 200 {
		t.Fatalf("catalog 失败: %d %s", code, body)
	}
	var groups []permGroup
	if err := json.Unmarshal(body, &struct {
		Data *[]permGroup `json:"data"`
	}{&groups}); err != nil || len(groups) == 0 {
		t.Fatalf("catalog 应返回非空分组: %v %s", err, body)
	}
	foundOverview := false
	for _, g := range groups {
		for _, e := range g.Entries {
			if e.Key == "GET /api/overview" {
				foundOverview = true
			}
		}
	}
	if !foundOverview {
		t.Fatalf("catalog 缺少 GET /api/overview")
	}

	code, body = doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil)
	if code != 200 {
		t.Fatalf("me 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	if data["username"] != "erin" || data["isAdmin"] != false {
		t.Fatalf("me 内容错误: %v", data)
	}
	// 测试管理员的 me：isAdmin=true
	code, body = doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", qaTestToken(), nil)
	if code != 200 {
		t.Fatalf("管理员 me 失败: %d %s", code, body)
	}
	data = decodeData(t, body)
	if data["username"] != qaTestAdminUser || data["isAdmin"] != true {
		t.Fatalf("管理员 me 内容错误: %v", data)
	}
}

// TestAccountSessionsUnit 会话数据面：写入/查找/过期清理/删账户连删会话。
func TestAccountSessionsUnit(t *testing.T) {
	d := newTestAccountsDaemon(t)
	s := d.accounts

	// 正常会话
	s.putSession("tok-alive", accountSession{Username: "alice", IsAdmin: false, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, time.Hour)
	if sess, ok := s.lookupSession("tok-alive"); !ok || sess.Username != "alice" {
		t.Fatalf("会话查找失败: %+v %v", sess, ok)
	}
	// 过期会话：写入 1ms 过期后不可查找
	s.putSession("tok-dead", accountSession{Username: "alice", ExpiresAt: time.Now().Add(time.Millisecond).UnixMilli()}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.lookupSession("tok-dead"); ok {
		t.Fatalf("过期会话不应命中")
	}
	// 过期清理
	s.putSession("tok-purge", accountSession{Username: "alice", ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}, time.Second)
	s.purgeExpiredSessions()
	if _, ok := s.lookupSession("tok-purge"); ok {
		t.Fatalf("清理后过期会话不应命中")
	}
	// 删除
	s.putSession("tok-del", accountSession{Username: "alice", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, time.Hour)
	s.delSession("tok-del")
	if _, ok := s.lookupSession("tok-del"); ok {
		t.Fatalf("删除后会话不应命中")
	}
	// 管理员会话回填 admin 标志（清缓存模拟重启后从 SQL 载入）
	s.putSession("tok-admin", accountSession{Username: qaTestAdminUser, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, time.Hour)
	s.invalidateSession("tok-admin")
	if sess, ok := s.lookupSession("tok-admin"); !ok || !sess.IsAdmin {
		t.Fatalf("管理员会话应回填 admin 标志: %+v %v", sess, ok)
	}
	// 账户不存在：会话仍可查找但 IsAdmin=false（后续按端点开关鉴权）
	s.putSession("tok-ghost", accountSession{Username: "ghost-user", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, time.Hour)
	s.invalidateSession("tok-ghost")
	if sess, ok := s.lookupSession("tok-ghost"); !ok || sess.IsAdmin {
		t.Fatalf("不存在账户的会话不应为 admin: %+v %v", sess, ok)
	}
}

// TestAccountRedisFallback Redis 不可用时（连接被拒）自动回退 SQL：
// 初始化不失败、首启引导/登录/权限照常工作、进入降级冷却。
func TestAccountRedisFallback(t *testing.T) {
	// 127.0.0.1:1 无监听 → 立即连接拒绝
	d := newBareAccountDaemon(t, accountsConfig{Driver: "sqlite", RedisAddr: "127.0.0.1:1"})
	if d.accounts.redisReady() {
		t.Fatalf("Redis 不可用时应进入降级状态")
	}

	// 首启引导与登录走 SQL（注意：须在 newTestServer 注入测试管理员之前引导，
	// 否则库中已有管理员，引导不再生成凭据）
	u, p, created, err := d.ensureAdminAccount()
	if err != nil || !created {
		t.Fatalf("Redis 故障时首启引导应走 SQL: created=%v err=%v", created, err)
	}
	srv := newTestServer(t, d)
	defer srv.Close()
	adminToken, _ := loginAs(t, srv, u, p)

	// 管理操作（创建账户）经登录 token 走 SQL
	code, body := doReqToken(t, http.MethodPost, srv.URL+"/api/accounts", adminToken, map[string]any{
		"username": "frank", "password": "FrankPass123",
	})
	if code != 200 {
		t.Fatalf("Redis 故障时创建账户应回退 SQL: %d %s", code, body)
	}
	token, _ := loginAs(t, srv, "frank", "FrankPass123")
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions", adminToken, map[string]any{
		"username": "frank", "permissions": map[string]bool{"GET /api/overview": true},
	})
	if code != 200 {
		t.Fatalf("Redis 故障时权限开关应回退 SQL: %d %s", code, body)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != 200 {
		t.Fatalf("Redis 故障时按 SQL 权限放行失败: %d", code)
	}
}

// ---------------------------------------------------------------------------
// 蓝方加固回归测试
// ---------------------------------------------------------------------------

// TestLoginBruteForceLockout 登录防暴力破解限速（蓝方加固）：
// 同一用户名连续失败达到阈值后进入锁定期，锁定期内即使密码正确也返回 429；
// 成功登录会清零计数（对照组不受影响）。
func TestLoginBruteForceLockout(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	if code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "brute", "password": "BrutePass123",
	}); code != 200 {
		t.Fatalf("创建账户失败")
	}

	// 阈值前的失败：均返回 401
	for i := 0; i < accountLoginMaxFails; i++ {
		if code, body := apiPost(t, srv.URL+"/api/auth/login", map[string]any{
			"username": "brute", "password": "WrongPass999",
		}); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401: %d %s", i+1, code, body)
		}
	}
	// 达到阈值后：正确密码也被锁定拒绝（429），且不泄露是密码正确与否
	if code, body := apiPost(t, srv.URL+"/api/auth/login", map[string]any{
		"username": "brute", "password": "BrutePass123",
	}); code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密码也应 429: %d %s", code, body)
	}
	// IP 维度同锁：同来源 IP 累计失败达到 IP 阈值后，换用户名登录也被拒。
	// 「账户+IP」组合键 5 次锁定目标账户（不锁其他账户），IP 维度阈值更高
	// （NAT/反代后多用户共享出口 IP，避免少量失败连坐封禁）。
	for i := accountLoginMaxFails; i < accountLoginMaxFailsIP; i++ {
		apiPost(t, srv.URL+"/api/auth/login", map[string]any{
			"username": fmt.Sprintf("spray-%d", i), "password": "WrongPass999",
		})
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{
		"username": qaTestAdminUser, "password": "whatever-pass",
	}); code != http.StatusTooManyRequests {
		t.Fatalf("IP 维度限速应拒绝同 IP 其他用户名登录: %d", code)
	}
	t.Logf("[验证] 「账户+IP」%d 次锁定目标账户，IP 维度 %d 次锁定出口（锁定 %s）",
		accountLoginMaxFails, accountLoginMaxFailsIP, accountLoginLockout)

	// 对照组：无失败记录的用户名不受影响（新守护进程避免 IP 锁干扰）
	d2 := newTestAccountsDaemon(t)
	srv2 := newTestServer(t, d2)
	defer srv2.Close()
	token, _ := loginAs(t, srv2, qaTestAdminUser, qaTestAdminPass)
	if token == "" {
		t.Fatal("正常登录不应受影响")
	}
}

// TestPasswordChangeRevokesSessions 改密后旧会话撤销（蓝方加固）：
//   - 管理员重置 bob 密码 → bob 的全部既有会话立即作废（被偷走的 token 止损）；
//   - carol 自己改密 → 当前会话保留，第二会话作废（不被踢出也不留旧门）。
func TestPasswordChangeRevokesSessions(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 管理员重置：bob 的两个会话全部作废
	if code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "bob", "password": "BobPass12345",
	}); code != 200 {
		t.Fatalf("创建 bob 失败")
	}
	tokenA, _ := loginAs(t, srv, "bob", "BobPass12345")
	tokenB, _ := loginAs(t, srv, "bob", "BobPass12345")
	if code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password"+qaAccQuery(), "", map[string]any{
		"username": "bob", "password": "BobNewPass678",
	}); code != 200 {
		t.Fatalf("管理员重置 bob 失败: %d %s", code, body)
	}
	for name, tok := range map[string]string{"会话A": tokenA, "会话B": tokenB} {
		if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", tok, nil); code != http.StatusForbidden {
			t.Fatalf("重置后 bob 的%s应已作废（403）: %d", name, code)
		}
	}
	if _, data := loginAs(t, srv, "bob", "BobNewPass678"); data == nil {
		t.Fatal("bob 应能用新密码重新登录")
	}
	t.Logf("[验证] 管理员重置密码后目标账户全部会话立即作废")

	// 自改密：当前会话保留、第二会话作废
	if code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "carol", "password": "CarolPass123",
	}); code != 200 {
		t.Fatalf("创建 carol 失败")
	}
	cur, _ := loginAs(t, srv, "carol", "CarolPass123")
	stolen, _ := loginAs(t, srv, "carol", "CarolPass123")
	if code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", cur, map[string]any{
		"oldPassword": "CarolPass123", "newPassword": qaTestNewPass(),
	}); code != 200 {
		t.Fatalf("carol 自改密失败: %d %s", code, body)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", cur, nil); code != 200 {
		t.Fatalf("自改密后当前会话应保留: %d", code)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", stolen, nil); code != http.StatusForbidden {
		t.Fatalf("自改密后第二会话应作废（403）: %d", code)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{
		"username": "carol", "password": "CarolPass123",
	}); code != http.StatusUnauthorized {
		t.Fatalf("carol 旧密码应失效: %d", code)
	}
	t.Logf("[验证] 自改密保留当前会话、其余会话作废")
}
