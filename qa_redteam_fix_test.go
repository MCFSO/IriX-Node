// 红队审计修复回归测试（P0/P1 修复项，替代临时 PoC zz_redteam_poc_test.go）：
//   - 严重-1 登录并发 map 崩溃（purgeLoginFails 补锁）
//   - 严重-2 备份区 uuid 越界（白名单 + 实例存在性 + 符号链接二次校验）
//   - 严重-3 实例 cwd 指向数据目录（创建/更新/导入/加载全部拒绝）
//   - 严重-4 FRP 上传即执行（上传收归管理员、请求内不再执行）
//   - 严重-5 deleteFile 对未校验 cwd 的 RemoveAll（删除前强制校验）
//   - 高-1 审计日志泄 token（查询串/请求体打码）
//   - 高-2 %2F 进 PathValue 的路径拼接（safePathSegment 白名单）
//   - 高-5 登录限速 fail-open（表满 LRU 淘汰、计数不丢弃）
//   - 高-6 目录列表全量 sha256（默认不算，显式 hash=1）
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestFixBackupUUIDTraversalRejected 严重-2：备份区 uuid 白名单与实例存在性校验。
func TestFixBackupUUIDTraversalRejected(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, worldDir(t))
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, d)
	defer srv.Close()

	// 穿越形态的 uuid 一律 400（含 ../、反斜杠、绝对路径、卷根逃逸）
	for _, evil := range []string{"..", "../..", `..\..\..`, "a/b", "a\\b", "/etc", "."} {
		code, body := rtReq(t, http.MethodPost,
			srv.URL+"/api/instance/backups/download?token="+qaTestToken()+"&uuid="+evil,
			map[string]any{"path": filepath.Join(dir, "accounts.db")})
		if code != http.StatusBadRequest {
			t.Errorf("uuid=%q 应被拒绝，实际 %d %s", evil, code, body)
		}
	}
	// 格式合法但不存在的实例同样拒绝（备份区目录名必须来自实例记录）
	code, body := rtReq(t, http.MethodPost,
		srv.URL+"/api/instance/backups/download?token="+qaTestToken()+"&uuid=deadbeef-0000-4000-8000-000000000000",
		map[string]any{"path": filepath.Join(dir, "accounts.db")})
	if code != http.StatusBadRequest {
		t.Errorf("不存在实例的 uuid 应被拒绝，实际 %d %s", code, body)
	}
	// 合法实例 + 区内文件正常放行（回归保护：不要误伤正常流程）
	backupDir := d.backupsDirOf(inst.InstanceUuid)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(backupDir, "ok.zip")
	if err := os.WriteFile(zipPath, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body = rtReq(t, http.MethodPost,
		srv.URL+"/api/instance/backups/download?token="+qaTestToken()+"&uuid="+inst.InstanceUuid,
		map[string]any{"path": zipPath})
	if code != http.StatusOK {
		t.Fatalf("区内备份下载票据应放行，实际 %d %s", code, body)
	}
	t.Logf("[修复回归] 严重-2：uuid 白名单 + 实例存在性校验生效")
}

// TestFixDataDirCwdRejected 严重-3：实例 cwd 不得位于数据目录内。
func TestFixDataDirCwdRejected(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	for _, evil := range []string{dir, filepath.Join(dir, "vault"), filepath.Join(dir, "backups", "x")} {
		code, body := rtReq(t, http.MethodPost, srv.URL+"/api/instance?token="+qaTestToken(),
			map[string]any{"nickname": "evil", "startCommand": "x", "cwd": evil})
		if code != http.StatusBadRequest {
			t.Errorf("cwd=%q 创建实例应被拒绝，实际 %d %s", evil, code, body)
		}
	}
	// 正常 cwd 放行（回归保护）
	code, body := rtReq(t, http.MethodPost, srv.URL+"/api/instance?token="+qaTestToken(),
		map[string]any{"nickname": "ok", "startCommand": "x", "cwd": worldDir(t)})
	if code != http.StatusOK {
		t.Fatalf("正常 cwd 应放行，实际 %d %s", code, body)
	}
	t.Logf("[修复回归] 严重-3：数据目录内 cwd 创建被拒")
}

// TestFixLoadSkipsInvalidInstances 严重-3：加载 instances.json 时重新校验
// uuid 与 cwd，非法条目跳过（持久化信任边界）。
func TestFixLoadSkipsInvalidInstances(t *testing.T) {
	dir := t.TempDir()
	good := worldDir(t)
	list := []map[string]any{
		{"instanceUuid": "good-1", "config": map[string]any{"nickname": "好", "cwd": good}},
		{"instanceUuid": "a/b", "config": map[string]any{"nickname": "坏uuid", "cwd": good}},
		{"instanceUuid": "bad-cwd", "config": map[string]any{"nickname": "坏cwd", "cwd": dir}},
		{"instanceUuid": "../escape", "config": map[string]any{"nickname": "逃逸", "cwd": good}},
	}
	raw, _ := json.Marshal(list)
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDaemon(dir)
	if err := d.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(d.Instances) != 1 || d.Find("good-1") == nil {
		t.Fatalf("应只加载合法实例，实际 %d 个", len(d.Instances))
	}
	t.Logf("[修复回归] 严重-3：Load 跳过非法 uuid/cwd 条目")
}

// TestFixRemoveAllGuard 严重-5：deleteFile 对未校验 cwd 拒绝 RemoveAll。
func TestFixRemoveAllGuard(t *testing.T) {
	d, dir := newTestDaemon(t)
	// 模拟旧版本/被篡改 instances.json：cwd 直接指向数据目录
	sentinel := filepath.Join(dir, "accounts.db")
	if err := os.WriteFile(sentinel, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := NewInstance("evil-del", InstanceConfig{Nickname: "破坏", Cwd: dir})
	d.Instances = append(d.Instances, inst)

	if err := d.Remove("evil-del", true); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("数据目录文件不应被 RemoveAll 删除: %v", err)
	}
	// 正常工作目录的 deleteFile 仍然生效（回归保护）
	world := worldDir(t)
	marker := filepath.Join(world, "marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.Instances = append(d.Instances, NewInstance("ok-del", InstanceConfig{Nickname: "正常", Cwd: world}))
	if err := d.Remove("ok-del", true); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if _, err := os.Stat(world); !os.IsNotExist(err) {
		t.Fatalf("正常 cwd 的 deleteFile 应删除工作目录: %v", err)
	}
	t.Logf("[修复回归] 严重-5：RemoveAll 前置校验拒绝数据目录")
}

// TestFixLoginFailMapRace 严重-1：purgeLoginFails 补锁后并发触发不再崩溃。
// 模拟未认证登录入口的并发路径（此前 fatal error: concurrent map iteration
// and map write，进程直接死亡且不走 StopAll）。
func TestFixLoginFailMapRace(t *testing.T) {
	d := newTestAccountsDaemon(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				u := fmt.Sprintf("user-%d-%d", i, j)
				d.accounts.recordLoginFailure(u, "10.0.0.1")
				d.accounts.purgeLoginFails()
				d.accounts.loginBlocked(u, "10.0.0.1")
			}
		}(i)
	}
	wg.Wait()
	t.Logf("[修复回归] 严重-1：并发 record/purge/loginBlocked 无崩溃")
}

// TestFixLoginRateLimitFailClosed 高-5：限速表满后计数不丢弃（fail-closed）。
func TestFixLoginRateLimitFailClosed(t *testing.T) {
	d := newTestAccountsDaemon(t)
	// 用随机用户名把限速表撑满（模拟攻击者填充）
	for i := 0; i < accountLoginMaxKeys+10; i++ {
		d.accounts.recordLoginFailure(fmt.Sprintf("filler-%d", i), "192.0.2.1")
	}
	// 表满后受害者的失败仍须计数并锁定
	for i := 0; i < accountLoginMaxFails; i++ {
		d.accounts.recordLoginFailure("victim", "192.0.2.2")
	}
	if !d.accounts.loginBlocked("victim", "192.0.2.2") {
		t.Fatal("限速表满后受害者失败计数被丢弃（fail-open）")
	}
	t.Logf("[修复回归] 高-5：表满后仍计数并锁定（fail-closed）")
}

// TestFixRedactBodyMasksTokens 高-1：请求体中的 token/secret 字段打码
// （集群 transfer 的 source.token、FRP config.token 等此前明文落盘）。
func TestFixRedactBodyMasksTokens(t *testing.T) {
	body := `{"source":{"token":"super-secret-token","addr":"a"},"config":{"Token":"frp-tok","serverAddr":"s"},"password":"p"}`
	got := redactBody(body, false)
	for _, leak := range []string{"super-secret-token", "frp-tok", `"p"`} {
		if strings.Contains(got, leak) {
			t.Errorf("请求体凭据未打码: %s", got)
		}
	}
	if !strings.Contains(got, `"token":"***"`) || !strings.Contains(got, `"Token":"***"`) {
		t.Errorf("token 字段应保留键名只打码值: %s", got)
	}
	t.Logf("[修复回归] 高-1：请求体凭据打码生效")
}

// TestFixSafePathSegment 高-2：进入文件路径拼接的单段白名单。
func TestFixSafePathSegment(t *testing.T) {
	bad := []string{"", "..", "a/b", "a\\b", "a..b", "%2f", ".", "-x", "x\x00y", strings.Repeat("a", 65)}
	for _, s := range bad {
		if safePathSegment(s) {
			t.Errorf("safePathSegment(%q) 应拒绝", s)
		}
	}
	good := []string{"a", "abc-123", "01234567-89ab-cdef-0123-456789abcdef", "my_jail.1"}
	for _, s := range good {
		if !safePathSegment(s) {
			t.Errorf("safePathSegment(%q) 应放行", s)
		}
	}
	// Bastille 路径拼接入口同样拒绝非法 name/session（%2F 编码穿越）
	if _, err := bastilleRunStatus("..", "x", 0, 0); err == nil {
		t.Error("bastilleRunStatus 应拒绝非法 jail 名")
	}
	if _, err := bastilleRunStatus("ok-jail", "../etc", 0, 0); err == nil {
		t.Error("bastilleRunStatus 应拒绝非法会话 id")
	}
	if _, err := bastilleConfigGet("../.."); err == nil {
		t.Error("bastilleConfigGet 应拒绝非法 jail 名")
	}
	t.Logf("[修复回归] 高-2：路径段白名单生效")
}

// TestFixListDirNoHashByDefault 高-6：目录列表默认不算 sha256（hash=1 才算）。
func TestFixListDirNoHashByDefault(t *testing.T) {
	d, _ := newTestDaemon(t)
	cwd := worldDir(t)
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("sync"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(2, cwd)
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(t, d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/files/list?"+qaKV()+"&uuid="+inst.InstanceUuid)
	if code != http.StatusOK {
		t.Fatalf("files/list 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	items, _ := data["items"].([]any)
	it := items[0].(map[string]any)
	if it["sha256"] != "" {
		t.Fatalf("默认列表不应计算 sha256（大目录 IO 放大）: %v", it["sha256"])
	}
	// 显式 hash=1 仍可按需取摘要（回归保护）
	code, body = doReq(t, srv.URL+"/api/files/list?"+qaKV()+"&uuid="+inst.InstanceUuid+"&hash=1")
	if code != http.StatusOK {
		t.Fatalf("files/list?hash=1 失败: %d %s", code, body)
	}
	data = decodeData(t, body)
	it = data["items"].([]any)[0].(map[string]any)
	if it["sha256"] == "" {
		t.Fatalf("hash=1 应返回摘要: %v", it)
	}
	t.Logf("[修复回归] 高-6：列表默认不算摘要")
}

// TestFixFRPUploadRequiresAdmin 严重-4：FRP 上传收归管理员，且上传请求内
// 不再执行刚上传的二进制（version 恒为空）。
func TestFixFRPUploadRequiresAdmin(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 创建仅带 FRP 上传权限的低权账户
	lowUser, lowPass := "frp-low", "frp-low-pass-1"
	code, body := rtReq(t, http.MethodPost, srv.URL+"/api/accounts?token="+qaTestToken(),
		map[string]any{"username": lowUser, "password": lowPass, "isAdmin": false})
	if code != 200 {
		t.Fatalf("创建低权账户失败: %d %s", code, body)
	}
	code, body = rtReq(t, http.MethodPut, srv.URL+"/api/accounts/permissions?token="+qaTestToken(),
		map[string]any{"username": lowUser,
			"permissions": map[string]bool{"POST /api/frp/binary": true}})
	if code != 200 {
		t.Fatalf("设置权限失败: %d %s", code, body)
	}
	lowTok := rtLogin(t, srv.URL, lowUser, lowPass)

	upload := func(token string) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "frpc")
		fw.Write([]byte("#!/bin/sh\necho pwned\n"))
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/frp/binary?token="+token, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("上传失败: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, out := upload(lowTok); code != http.StatusForbidden {
		t.Fatalf("低权账户上传 FRP 二进制应 403，实际 %d %v", code, out)
	}
	code, out := upload(qaTestToken())
	if code != http.StatusOK {
		t.Fatalf("管理员上传应成功: %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if v, _ := data["version"].(string); v != "" {
		t.Fatalf("上传响应不应执行探测返回版本号（严重-4）: %q", v)
	}
	t.Logf("[修复回归] 严重-4：上传收归管理员且请求内不执行")
}

// rtReq / rtLogin 由 zz_redteam_poc_test.go 迁移而来（该 PoC 已删除）。
// rtReq 带 JSON 请求体的请求助手。
func rtReq(t *testing.T, method, url string, payload any) (int, []byte) {
	t.Helper()
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// rtLogin 用账户名+密码换会话 token。
func rtLogin(t *testing.T, base, user, pass string) string {
	t.Helper()
	code, body := rtReq(t, http.MethodPost, base+"/api/auth/login",
		map[string]any{"username": user, "password": pass})
	if code != 200 {
		t.Fatalf("登录 %s 失败: HTTP %d %s", user, code, body)
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Data.Token == "" {
		t.Fatalf("登录响应无 token: %s", body)
	}
	return out.Data.Token
}
