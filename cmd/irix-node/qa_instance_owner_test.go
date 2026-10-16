// 实例属主模型回归测试（红队审计 高-3：无实例级授权 IDOR）。
//
// 覆盖：
//   - 非管理员创建的实例自动归属自己，且不能指定他人为属主；
//   - 管理员可为实例指定/变更属主（空串 = 公共实例）；
//   - 跨账户访问实例详情/启停/删除一律 403（管理员旁路）；
//   - 实例列表对非管理员只返回「自己拥有的 + 无属主的公共实例」；
//   - 实时控制台的 stdin 下发需独立持有「向实例发送命令」端点开关。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// grantPerms 覆盖式授予账户端点开关。
func grantPerms(t *testing.T, d *Daemon, username string, keys ...string) {
	t.Helper()
	perms := map[string]bool{}
	for _, k := range keys {
		perms[k] = true
	}
	if err := d.accounts.setPermissions(username, perms); err != nil {
		t.Fatalf("授予 %s 权限失败: %v", username, err)
	}
}

// createAccountAsAdmin 管理员创建普通账户。
func createAccountAsAdmin(t *testing.T, d *Daemon, srv *httptest.Server, username, password string) {
	t.Helper()
	code, body := apiPost(t, srv.URL+"/api/accounts"+qaAccQuery(), map[string]any{
		"username": username, "password": password, "isAdmin": false,
	})
	if code != http.StatusOK {
		t.Fatalf("创建账户 %s 失败: %d %s", username, code, body)
	}
}

// instanceBody 构造创建实例的请求体（cwd 位于数据目录之外）。
func instanceBody(cwd, owner string) map[string]any {
	b := map[string]any{
		"nickname":     "属主测试",
		"startCommand": "ping 127.0.0.1 -t",
		"cwd":          cwd,
		"type":         "universal",
	}
	if owner != "" {
		b["owner"] = owner
	}
	return b
}

// postInstance 以指定会话 token 创建实例，返回状态码与响应 data。
func postInstance(t *testing.T, srv *httptest.Server, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	url := srv.URL + "/api/instance"
	if token != "" {
		url += "?token=" + token
	}
	code, raw := apiPost(t, url, body)
	if code != http.StatusOK {
		return code, nil
	}
	return code, decodeData(t, raw)
}

// TestInstanceOwnerCreateRules 创建实例时的属主归属规则。
func TestInstanceOwnerCreateRules(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "alice", "AlicePass123")
	createAccountAsAdmin(t, d, srv, "bob", "BobPass12345")
	aliceTok, _ := loginAs(t, srv, "alice", "AlicePass123")
	bobTok, _ := loginAs(t, srv, "bob", "BobPass12345")
	grantPerms(t, d, "alice", "POST /api/instance")
	grantPerms(t, d, "bob", "POST /api/instance")

	// 非管理员创建：自动归属自己（body 未指定 owner）
	code, data := postInstance(t, srv, aliceTok, instanceBody(worldDir(t), ""))
	if code != http.StatusOK {
		t.Fatalf("alice 创建实例应成功: %d", code)
	}
	if owner, _ := data["owner"].(string); owner != "alice" {
		t.Fatalf("非管理员创建的实例应归属自己，实际 owner=%q", owner)
	}
	aliceUUID, _ := data["instanceUuid"].(string)

	// 非管理员指定他人为属主：拒绝
	code, _ = postInstance(t, srv, bobTok, instanceBody(worldDir(t), "alice"))
	if code != http.StatusForbidden {
		t.Fatalf("非管理员指定属主应 403，实际 %d", code)
	}

	// 管理员未指定 owner：公共实例（属主为空，兼容历史行为）
	code, data = postInstance(t, srv, qaTestToken(), instanceBody(worldDir(t), ""))
	if code != http.StatusOK {
		t.Fatalf("管理员创建实例应成功: %d", code)
	}
	if owner, _ := data["owner"].(string); owner != "" {
		t.Fatalf("管理员未指定属主应为公共实例，实际 owner=%q", owner)
	}
	publicUUID, _ := data["instanceUuid"].(string)

	// 管理员指定属主：放行
	code, data = postInstance(t, srv, qaTestToken(), instanceBody(worldDir(t), "bob"))
	if code != http.StatusOK {
		t.Fatalf("管理员指定属主应成功: %d", code)
	}
	if owner, _ := data["owner"].(string); owner != "bob" {
		t.Fatalf("管理员指定的属主未生效: owner=%q", owner)
	}
	bobUUID, _ := data["instanceUuid"].(string)

	// 管理员指定非法属主名：拒绝
	code, _ = postInstance(t, srv, qaTestToken(), instanceBody(worldDir(t), "bad name!"))
	if code != http.StatusForbidden && code != http.StatusBadRequest {
		t.Fatalf("非法属主名应 400/403，实际 %d", code)
	}

	// 属主已落库：内存中三个实例的属主分别为 alice / "" / bob
	for uuid, want := range map[string]string{aliceUUID: "alice", publicUUID: "", bobUUID: "bob"} {
		inst := d.Find(uuid)
		if inst == nil {
			t.Fatalf("实例 %s 未创建", uuid)
		}
		if got := inst.OwnerOf(); got != want {
			t.Fatalf("实例 %s 属主应为 %q，实际 %q", uuid, want, got)
		}
	}
}

// TestInstanceOwnerIsolation 跨账户访问实例一律 403；公共实例对所有账户开放。
func TestInstanceOwnerIsolation(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "alice", "AlicePass123")
	createAccountAsAdmin(t, d, srv, "bob", "BobPass12345")
	aliceTok, _ := loginAs(t, srv, "alice", "AlicePass123")
	bobTok, _ := loginAs(t, srv, "bob", "BobPass12345")

	// 两个账户拿到完全相同的端点开关（模拟「有权限但跨租户」）
	insPerms := []string{
		"GET /api/instance", "GET /api/service/remote_service_instances",
		"GET /api/protected_instance/open", "GET /api/protected_instance/stop",
		"DELETE /api/instance", "GET /api/instance/console/ws",
	}
	grantPerms(t, d, "alice", insPerms...)
	grantPerms(t, d, "bob", insPerms...)

	// 直接建实例：alice 私有 + 公共
	aliceInst := sampleInst(901, worldDir(t))
	aliceInst.Owner = "alice"
	publicInst := sampleInst(902, worldDir(t))
	if err := d.Add(aliceInst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	if err := d.Add(publicInst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}

	// 属主本人：放行
	if code, body := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+aliceInst.InstanceUuid, aliceTok, nil); code != http.StatusOK {
		t.Fatalf("属主访问自己的实例应 200: %d %s", code, body)
	}
	// 跨账户：403 且提示属主
	code, body := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+aliceInst.InstanceUuid, bobTok, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "无权访问该实例") {
		t.Fatalf("非属主访问应 403 并提示无权: %d %s", code, body)
	}
	// 启停同理
	if code, _ := doReqToken(t, http.MethodGet,
		srv.URL+"/api/protected_instance/open?uuid="+aliceInst.InstanceUuid, bobTok, nil); code != http.StatusForbidden {
		t.Fatalf("非属主启动实例应 403: %d", code)
	}
	// 公共实例：任何账户均可访问
	if code, body := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+publicInst.InstanceUuid, bobTok, nil); code != http.StatusOK {
		t.Fatalf("公共实例应放行: %d %s", code, body)
	}
	// 管理员旁路
	if code, body := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+aliceInst.InstanceUuid, qaTestToken(), nil); code != http.StatusOK {
		t.Fatalf("管理员应旁路实例属主: %d %s", code, body)
	}
	// 不存在的实例仍是 400（保持既有契约）
	if code, _ := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+sampleUUID(999), bobTok, nil); code != http.StatusBadRequest {
		t.Fatalf("不存在的实例应 400: %d", code)
	}
}

// TestInstanceOwnerListFilter 列表对非管理员只返回自己的 + 公共实例。
func TestInstanceOwnerListFilter(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "alice", "AlicePass123")
	aliceTok, _ := loginAs(t, srv, "alice", "AlicePass123")
	grantPerms(t, d, "alice", "GET /api/service/remote_service_instances")

	mine := sampleInst(911, worldDir(t))
	mine.Owner = "alice"
	others := sampleInst(912, worldDir(t))
	others.Owner = "bob"
	public := sampleInst(913, worldDir(t))
	for _, i := range []*Instance{mine, others, public} {
		if err := d.Add(i); err != nil {
			t.Fatalf("添加实例失败: %v", err)
		}
	}

	listAs := func(token string) map[string]bool {
		t.Helper()
		code, body := doReqToken(t, http.MethodGet,
			srv.URL+"/api/service/remote_service_instances?page=1&page_size=50", token, nil)
		if code != http.StatusOK {
			t.Fatalf("列表应 200: %d %s", code, body)
		}
		var env struct {
			Data struct {
				Data []struct {
					InstanceUUID string `json:"instanceUuid"`
				} `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("解析列表失败: %v", err)
		}
		got := map[string]bool{}
		for _, it := range env.Data.Data {
			got[it.InstanceUUID] = true
		}
		return got
	}

	got := listAs(aliceTok)
	if !got[mine.InstanceUuid] || !got[public.InstanceUuid] {
		t.Fatalf("alice 应看到自己的与公共实例: %v", got)
	}
	if got[others.InstanceUuid] {
		t.Fatalf("alice 不应看到 bob 的实例: %v", got)
	}
	// 管理员看到全部
	admin := listAs(qaTestToken())
	if !admin[mine.InstanceUuid] || !admin[others.InstanceUuid] || !admin[public.InstanceUuid] {
		t.Fatalf("管理员应看到全部实例: %v", admin)
	}
}

// TestInstanceOwnerDeleteGuard 批量删除前整体鉴权：跨账户删除被拒且实例仍在。
func TestInstanceOwnerDeleteGuard(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "bob", "BobPass12345")
	bobTok, _ := loginAs(t, srv, "bob", "BobPass12345")
	grantPerms(t, d, "bob", "DELETE /api/instance")

	aliceInst := sampleInst(921, worldDir(t))
	aliceInst.Owner = "alice"
	if err := d.Add(aliceInst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}

	code, _ := doReqToken(t, http.MethodDelete, srv.URL+"/api/instance", bobTok,
		map[string]any{"uuids": []string{aliceInst.InstanceUuid}, "deleteFile": false})
	if code != http.StatusForbidden {
		t.Fatalf("跨账户删除应 403: %d", code)
	}
	if d.Find(aliceInst.InstanceUuid) == nil {
		t.Fatal("被拒绝的删除不应生效")
	}
	// 混合请求（自己 + 他人）：整体拒绝，不部分删除
	own := sampleInst(922, worldDir(t))
	own.Owner = "bob"
	if err := d.Add(own); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	code, _ = doReqToken(t, http.MethodDelete, srv.URL+"/api/instance", bobTok,
		map[string]any{"uuids": []string{own.InstanceUuid, aliceInst.InstanceUuid}, "deleteFile": false})
	if code != http.StatusForbidden {
		t.Fatalf("含越权 uuid 的批量删除应整体 403: %d", code)
	}
	if d.Find(own.InstanceUuid) == nil || d.Find(aliceInst.InstanceUuid) == nil {
		t.Fatal("整体拒绝时不应删除任何实例")
	}
	// 仅自己的实例：放行
	code, _ = doReqToken(t, http.MethodDelete, srv.URL+"/api/instance", bobTok,
		map[string]any{"uuids": []string{own.InstanceUuid}, "deleteFile": false})
	if code != http.StatusOK {
		t.Fatalf("删除自己的实例应 200: %d", code)
	}
	if d.Find(own.InstanceUuid) != nil {
		t.Fatal("删除自己的实例应生效")
	}
}

// TestInstanceOwnerUpdateGuard 属主变更仅管理员可做。
func TestInstanceOwnerUpdateGuard(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "alice", "AlicePass123")
	aliceTok, _ := loginAs(t, srv, "alice", "AlicePass123")
	grantPerms(t, d, "alice", "PUT /api/instance")

	inst := sampleInst(931, worldDir(t))
	inst.Owner = "alice"
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}

	// 非管理员试图把属主改给别人：403
	code, _ := doReqToken(t, http.MethodPut,
		srv.URL+"/api/instance?uuid="+inst.InstanceUuid, aliceTok,
		instanceBody(inst.Config.Cwd, "bob"))
	if code != http.StatusForbidden {
		t.Fatalf("非管理员变更属主应 403: %d", code)
	}
	if got := inst.OwnerOf(); got != "alice" {
		t.Fatalf("属主不应被改动: %q", got)
	}

	// 管理员改为 bob：放行
	code, body := doReqToken(t, http.MethodPut,
		srv.URL+"/api/instance?uuid="+inst.InstanceUuid, qaTestToken(),
		instanceBody(inst.Config.Cwd, "bob"))
	if code != http.StatusOK {
		t.Fatalf("管理员变更属主应 200: %d %s", code, body)
	}
	if got := inst.OwnerOf(); got != "bob" {
		t.Fatalf("属主应变更为 bob，实际 %q", got)
	}
	// 变更后 alice 不再可访问
	if code, _ := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance?uuid="+inst.InstanceUuid, aliceTok, nil); code != http.StatusForbidden {
		t.Fatalf("过户后原属主应 403: %d", code)
	}
}

// TestInstanceOwnerPersist 属主随实例表持久化并在重载后恢复。
func TestInstanceOwnerPersist(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(941, worldDir(t))
	inst.Owner = "alice"
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	d.StopAutoSave()
	d.workers.stop()

	// 重新加载同一数据目录
	d2 := NewDaemon(dir)
	defer func() {
		d2.StopAutoSave()
		d2.workers.stop()
	}()
	if err := d2.Load(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	got := d2.Find(inst.InstanceUuid)
	if got == nil {
		t.Fatal("重载后实例丢失")
	}
	if owner := got.OwnerOf(); owner != "alice" {
		t.Fatalf("重载后属主应为 alice，实际 %q", owner)
	}
}

// TestConsoleCommandPermGate 控制台命令下发的独立权限判定。
func TestConsoleCommandPermGate(t *testing.T) {
	d := newTestAccountsDaemon(t)
	if err := d.accounts.createAccount("alice", "AlicePass123", false); err != nil {
		t.Fatalf("创建账户失败: %v", err)
	}

	req := func(username string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/instance/console/ws", nil)
		return withIdentity(r, &identity{username: username})
	}

	// 未授予「向实例发送命令」：拒绝
	if d.consoleCommandAllowed(req("alice")) {
		t.Fatal("未授予命令权限时应拒绝")
	}
	// 授予后：放行
	grantPerms(t, d, "alice", "GET /api/protected_instance/command")
	if !d.consoleCommandAllowed(req("alice")) {
		t.Fatal("授予命令权限后应放行")
	}
	// 管理员旁路（即使无开关）
	if !d.consoleCommandAllowed(withIdentity(
		httptest.NewRequest(http.MethodGet, "/api/instance/console/ws", nil),
		&identity{username: qaTestAdminUser, isAdmin: true})) {
		t.Fatal("管理员应旁路命令权限判定")
	}
	// 账户系统未启用（无身份）：不引入约束
	if !d.consoleCommandAllowed(httptest.NewRequest(http.MethodGet, "/api/instance/console/ws", nil)) {
		t.Fatal("无身份上下文时应放行（认证层已拦截）")
	}
}

// TestConsoleWSCommandDeniedCrossTenant 非属主连接控制台被拒（403，非 101）。
func TestConsoleWSCommandDeniedCrossTenant(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	createAccountAsAdmin(t, d, srv, "bob", "BobPass12345")
	bobTok, _ := loginAs(t, srv, "bob", "BobPass12345")
	grantPerms(t, d, "bob", "GET /api/instance/console/ws")

	inst := sampleInst(951, worldDir(t))
	inst.Owner = "alice"
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}

	// 普通 HTTP GET（不升级）：属主校验在 upgradeWS 之前，应直接 403。
	code, _ := doReqToken(t, http.MethodGet,
		srv.URL+"/api/instance/console/ws?uuid="+inst.InstanceUuid, bobTok, nil)
	if code != http.StatusForbidden {
		t.Fatalf("非属主连接控制台应 403: %d", code)
	}
}
