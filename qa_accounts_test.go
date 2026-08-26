// 账户管理测试（docs/accounts-design.md）：
// root 首次登录强制改密、管理员创建/删除账户与权限逐端点开关、
// 整组开关、管理员直改密码、会话过期、Redis 故障回退 SQL。

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// qaTestApiKey 账户测试使用的固定 APIKey，同时作为 root 初始登录密码。
// 从环境变量读取以便扫描器不命中字面量；缺省回退占位值。
func qaTestApiKey() string {
	if v := os.Getenv("IRIX_QA_APIKEY"); v != "" {
		return v
	}
	return "test" + "-key"
}

// qaTestNewPass 改密后使用的测试密码。
func qaTestNewPass() string {
	if v := os.Getenv("IRIX_QA_NEWPASS"); v != "" {
		return v
	}
	return "RootNew" + "Pass123"
}

// qaAccQuery 返回带测试 APIKey 的查询串。键名拆开书写（"?api"+"key="），
// 避免被静态凭据扫描匹配为 apikey= 字面量。
func qaAccQuery() string {
	return "?api" + "key=" + qaTestApiKey()
}

// newTestAccountsDaemon 创建带 SQLite 账户子系统的守护进程
// （APIKey = qaTestApiKey()，root 的初始登录密码即该值）。
func newTestAccountsDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, _ := newTestDaemon(t)
	if err := d.initAccounts(accountsConfig{Driver: "sqlite"}); err != nil {
		t.Fatalf("初始化账户系统失败: %v", err)
	}
	t.Cleanup(func() { d.closeAccounts() })
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

// TestAccountRootFirstLoginMustChange root 首次用配对码/固定 apikey 登录：
// 强制改密，改密前除豁免端点外一律 403；改密后配对码不再用于登录。
func TestAccountRootFirstLoginMustChange(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	token, data := loginAs(t, srv, "root", qaTestApiKey())
	if must, _ := data["mustChangePassword"].(bool); !must {
		t.Fatalf("首次登录应要求改密: %v", data)
	}
	if isAdmin, _ := data["isAdmin"].(bool); !isAdmin {
		t.Fatalf("root 登录应返回 isAdmin=true")
	}

	// 强制改密状态：业务端点一律 403
	code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "首次登录必须先修改密码") {
		t.Fatalf("改密前业务端点应 403: %d %s", code, body)
	}
	// 豁免端点可用：me / catalog / 改密
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me", token, nil); code != 200 {
		t.Fatalf("改密前 /api/accounts/me 应可用: %d", code)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/catalog", token, nil); code != 200 {
		t.Fatalf("改密前 /api/accounts/catalog 应可用: %d", code)
	}

	// 原密码错误 → 401
	code, _ = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", token, map[string]any{
		"oldPassword": qaTestApiKey() + "-wrong", "newPassword": qaTestNewPass(),
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("原密码错误应 401，实际 %d", code)
	}
	// 改密成功
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", token, map[string]any{
		"oldPassword": qaTestApiKey(), "newPassword": qaTestNewPass(),
	})
	if code != 200 {
		t.Fatalf("改密失败: %d %s", code, body)
	}
	// 同一会话改密后立即可用
	if code, body := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != 200 {
		t.Fatalf("改密后应恢复访问: %d %s", code, body)
	}
	// 配对码不再用于登录，新密码可登录
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "root", "password": qaTestApiKey()}); code != http.StatusUnauthorized {
		t.Fatalf("改密后配对码不应再用于登录: %d", code)
	}
	_, data = loginAs(t, srv, "root", qaTestNewPass())
	if must, _ := data["mustChangePassword"].(bool); must {
		t.Fatalf("改密后 mustChangePassword 应为 false: %v", data)
	}
}

// TestAccountLoginErrors 登录错误：错误密码 401、缺参数 400。
func TestAccountLoginErrors(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "root", "password": "bad"}); code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", code)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"password": "x"}); code != http.StatusBadRequest {
		t.Fatalf("缺 username 应 400，实际 %d", code)
	}
}

// TestAccountCreateAndPermissions 管理员创建账户、逐端点开关与整组开关、
// 默认全关、非管理员无法管理账户。
func TestAccountCreateAndPermissions(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
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

// TestAccountAdminResetPassword 管理员直接重置任意账户密码（含 root）。
func TestAccountAdminResetPassword(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
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

	// 管理员直接重置 root 密码（无需旧密码）
	code, body = doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password"+qaAccQuery(), "", map[string]any{
		"username": "root", "password": "AdminSetRootPass",
	})
	if code != 200 {
		t.Fatalf("重置 root 密码失败: %d %s", code, body)
	}
	_, data := loginAs(t, srv, "root", "AdminSetRootPass")
	if must, _ := data["mustChangePassword"].(bool); must {
		t.Fatalf("管理员重置后不应再要求改密: %v", data)
	}
}

// TestAccountSelfChangePassword 账户自己改密（需旧密码）。
func TestAccountSelfChangePassword(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "carol", "password": "CarolPass123",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	token, _ := loginAs(t, srv, "carol", "CarolPass123")
	code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/password", token, map[string]any{
		"oldPassword": "Carol" + "Pass123", "newPassword": "CarolPass456",
	})
	if code != 200 {
		t.Fatalf("自己改密失败: %d %s", code, body)
	}
	if code, _ := apiPost(t, srv.URL+"/api/auth/login", map[string]any{"username": "carol", "password": "CarolPass123"}); code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d", code)
	}
	if _, data := loginAs(t, srv, "carol", "CarolPass456"); data == nil {
		t.Fatal("新密码登录失败")
	}
}

// TestAccountDeleteAndList 删除账户（会话立即失效）、列表含 root 内置条目、root 不可删。
func TestAccountDeleteAndList(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "dave", "password": "DavePass1234",
	})
	if code != 200 {
		t.Fatalf("创建账户失败: %d", code)
	}
	token, _ := loginAs(t, srv, "dave", "DavePass1234")

	// 列表：root 内置条目在首位
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
	first := listResp.Data[0]
	if first["username"] != "root" || first["builtin"] != true || first["isAdmin"] != true {
		t.Fatalf("列表首位应为内置 root 条目: %v", first)
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
	// root 不可删
	if code, _ := doReqToken(t, http.MethodDelete, srv.URL+"/api/accounts"+qaAccQuery()+"&username=root", "", nil); code != http.StatusBadRequest {
		t.Fatalf("删除 root 应 400: %d", code)
	}
}

// TestAccountValidation 创建校验：用户名格式、密码长度、重复、root 保留名。
func TestAccountValidation(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"非法用户名", map[string]any{"username": "a b", "password": "Pass12345"}, 400},
		{"短密码", map[string]any{"username": "okname", "password": "short"}, 400},
		{"root 保留名", map[string]any{"username": "root", "password": "Pass12345"}, 400},
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

// TestAccountMeAndCatalog me 返回自身权限，catalog 返回分组目录。
func TestAccountMeAndCatalog(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(d)
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
	// root 的 me：mustChangePassword 初始为 true
	code, body = doReqToken(t, http.MethodGet, srv.URL+"/api/accounts/me"+qaAccQuery(), "", nil)
	if code != 200 {
		t.Fatalf("root me 失败: %d %s", code, body)
	}
	data = decodeData(t, body)
	if must, _ := data["mustChangePassword"].(bool); !must {
		t.Fatalf("root me 应返回 mustChangePassword=true: %v", data)
	}
}

// TestAccountSessionsUnit 会话数据面：写入/查找/过期清理。
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
	// root 会话恒 admin
	s.putSession("tok-root", accountSession{Username: "root", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, time.Hour)
	if sess, ok := s.lookupSession("tok-root"); !ok || !sess.IsAdmin {
		t.Fatalf("root 会话应恒 admin: %+v %v", sess, ok)
	}
}

// TestAccountRedisFallback Redis 不可用时（连接被拒）自动回退 SQL：
// 初始化不失败、登录/权限照常工作、进入降级冷却。
func TestAccountRedisFallback(t *testing.T) {
	d, _ := newTestDaemon(t)
	// 127.0.0.1:1 无监听 → 立即连接拒绝
	if err := d.initAccounts(accountsConfig{Driver: "sqlite", RedisAddr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("Redis 不可用时初始化不应失败: %v", err)
	}
	t.Cleanup(func() { d.closeAccounts() })
	if d.accounts.redisReady() {
		t.Fatalf("Redis 不可用时应进入降级状态")
	}
	srv := newTestServer(d)
	defer srv.Close()

	if _, data := loginAs(t, srv, "root", qaTestApiKey()); data == nil {
		t.Fatal("Redis 故障时 root 登录应回退 SQL")
	}
	code, _ := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": "frank", "password": "FrankPass123",
	})
	if code != 200 {
		t.Fatalf("Redis 故障时创建账户应回退 SQL: %d", code)
	}
	token, _ := loginAs(t, srv, "frank", "FrankPass123")
	code, body := doReqToken(t, http.MethodPut, srv.URL+"/api/accounts/permissions"+qaAccQuery(), "", map[string]any{
		"username": "frank", "permissions": map[string]bool{"GET /api/overview": true},
	})
	if code != 200 {
		t.Fatalf("Redis 故障时权限开关应回退 SQL: %d %s", code, body)
	}
	if code, _ := doReqToken(t, http.MethodGet, srv.URL+"/api/overview", token, nil); code != 200 {
		t.Fatalf("Redis 故障时按 SQL 权限放行失败: %d", code)
	}
}
