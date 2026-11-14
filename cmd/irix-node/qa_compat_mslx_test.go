// MSLX Daemon 兼容层测试（compat.mslx）：
// 登录/JWT、实例列表与操作、文件管理、SignalR 协商与鉴权。

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mslxTestPass 测试用登录密码。
const mslxTestPass = "qa-mslx-pass-000000000000"

// setupTestMSLX 启用 MSLX 兼容层并返回其基址。
func setupTestMSLX(t *testing.T, d *Daemon) string {
	t.Helper()
	d.setupCompat(&nodeOptions{
		CompatMSLXEnabled: true,
		CompatMSLXPrefix:  "/mslx",
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    mslxTestPass,
	})
	if d.compatMSLX == nil {
		t.Fatalf("MSLX 兼容层未启用（检查 setupCompat 配置校验）")
	}
	srv := newTestServer(t, d)
	t.Cleanup(srv.Close)
	return srv.URL + "/mslx"
}

// mslxDo 发起带可选令牌的请求并返回状态码与响应体。
func mslxDo(t *testing.T, method, u, token string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("x-user-token", token)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// mslxEnvelope MSLX 响应信封。
type mslxEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// mslxLogin 登录并返回 token。
func mslxLogin(t *testing.T, base string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": mslxTestPass})
	status, resp := mslxDo(t, http.MethodPost, base+"/api/auth/login", "", body)
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, resp)
	}
	var env mslxEnvelope
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("登录响应解析失败: %v（%s）", err, resp)
	}
	if env.Code != 200 {
		t.Fatalf("登录 code 应为 200，实际 %d（%s）", env.Code, env.Message)
	}
	var data struct {
		Token string         `json:"token"`
		User  map[string]any `json:"user"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Token == "" {
		t.Fatalf("登录未返回 token: %s", env.Data)
	}
	if data.User["role"] != "admin" {
		t.Fatalf("登录用户角色应为 admin: %v", data.User)
	}
	return data.Token
}

// TestCompatMSLXAuthFlow 登录、鉴权与拒绝路径。
func TestCompatMSLXAuthFlow(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	// 匿名接口
	if status, body := mslxDo(t, http.MethodGet, base+"/api/ping", "", nil); status != http.StatusOK {
		t.Fatalf("ping 应可匿名访问，实际 %d（%s）", status, body)
	}

	// 错误密码
	bad, _ := json.Marshal(map[string]any{"username": "admin", "password": "wrong"})
	if status, _ := mslxDo(t, http.MethodPost, base+"/api/auth/login", "", bad); status != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", status)
	}

	// 无令牌访问受保护接口
	if status, _ := mslxDo(t, http.MethodGet, base+"/api/instance/list", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", status)
	}

	token := mslxLogin(t, base)

	// 伪造令牌（改签名）必须被拒
	forged := token[:len(token)-2] + "aa"
	if forged == token {
		forged = token[:len(token)-3] + "bbb"
	}
	if status, _ := mslxDo(t, http.MethodGet, base+"/api/instance/list", forged, nil); status != http.StatusUnauthorized {
		t.Fatalf("伪造令牌应 401，实际 %d", status)
	}

	// 查询参数传令牌（WebSocket 场景）同样可用
	if status, body := mslxDo(t, http.MethodGet, base+"/api/user/me?access_token="+url.QueryEscape(token), "", nil); status != http.StatusOK {
		t.Fatalf("access_token 查询参数应可用，实际 %d（%s）", status, body)
	}

	// 节点协议信息
	if status, body := mslxDo(t, http.MethodGet, base+"/api/node/protocol", "", nil); status != http.StatusOK {
		t.Fatalf("node/protocol 应可匿名访问，实际 %d（%s）", status, body)
	}
}

// TestCompatMSLXInstanceFlow 实例列表 / 详情 / 启动 / 强制结束。
func TestCompatMSLXInstanceFlow(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd := filepath.Join(worldDir(t), "srv-mslx")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}

	// 列表
	status, resp := mslxDo(t, http.MethodGet, base+"/api/instance/list", token, nil)
	if status != http.StatusOK {
		t.Fatalf("实例列表失败: %d %s", status, resp)
	}
	var env mslxEnvelope
	_ = json.Unmarshal(resp, &env)
	var list []map[string]any
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("实例列表解析失败: %v（%s）", err, env.Data)
	}
	if len(list) != 1 {
		t.Fatalf("实例列表应有 1 项，实际 %d", len(list))
	}
	idFloat, ok := list[0]["id"].(float64)
	if !ok || idFloat <= 0 {
		t.Fatalf("实例缺少整数 id: %v", list[0])
	}
	id := uint32(idFloat)
	if list[0]["statusText"] != "stopped" {
		t.Fatalf("初始状态应为 stopped: %v", list[0]["statusText"])
	}

	// 详情
	status, resp = mslxDo(t, http.MethodGet,
		base+"/api/instance/info?id="+itoa(int(id)), token, nil)
	if status != http.StatusOK {
		t.Fatalf("实例详情失败: %d %s", status, resp)
	}

	// 启动
	action, _ := json.Marshal(map[string]any{"id": id, "action": "start"})
	if status, body := mslxDo(t, http.MethodPost, base+"/api/instance/action", token, action); status != http.StatusOK {
		t.Fatalf("启动失败: %d %s", status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		st := inst.Status
		inst.mu.Unlock()
		if st == StatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	inst.mu.Lock()
	st := inst.Status
	inst.mu.Unlock()
	if st != StatusRunning {
		t.Fatalf("实例应处于运行中，实际 %d", st)
	}

	// 强制结束（杀进程是异步回收，留出轮询窗口）
	action, _ = json.Marshal(map[string]any{"id": id, "action": "forceExit"})
	statusForce, bodyForce := mslxDo(t, http.MethodPost, base+"/api/instance/action", token, action)
	if statusForce != http.StatusOK {
		t.Fatalf("强制结束请求失败: %d %s", statusForce, bodyForce)
	}
	var envForce mslxEnvelope
	_ = json.Unmarshal(bodyForce, &envForce)
	if strings.Contains(envForce.Message, "失败") {
		t.Fatalf("强制结束动作报告失败: %s", envForce.Message)
	}
	stopped := false
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		st = inst.Status
		inst.mu.Unlock()
		if st == StatusStopped {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatalf("强制结束后应为已停止，实际 %d", st)
	}

	// 未知动作
	action, _ = json.Marshal(map[string]any{"id": id, "action": "selfDestruct"})
	if status, _ := mslxDo(t, http.MethodPost, base+"/api/instance/action", token, action); status != http.StatusBadRequest {
		t.Fatalf("未知动作应 400，实际 %d", status)
	}
}

// TestCompatMSLXFileFlow 文件列表 / 读写 / 新建 / 重命名 / 上传 / 下载 / 删除。
func TestCompatMSLXFileFlow(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd := filepath.Join(worldDir(t), "srv-mslx-files")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	id := itoa(int(d.compatMSLX.idFor(inst.InstanceUuid)))
	filesBase := base + "/api/files/instance/" + id

	// 新建目录
	body, _ := json.Marshal(map[string]any{"path": "", "name": "plugins"})
	if status, resp := mslxDo(t, http.MethodPost, filesBase+"/directory", token, body); status != http.StatusOK {
		t.Fatalf("新建目录失败: %d %s", status, resp)
	}

	// 写入文件
	body, _ = json.Marshal(map[string]any{"path": "/plugins/a.yml", "content": "k: v\n"})
	if status, resp := mslxDo(t, http.MethodPost, filesBase+"/content", token, body); status != http.StatusOK {
		t.Fatalf("写入文件失败: %d %s", status, resp)
	}

	// 读取文件
	status, resp := mslxDo(t, http.MethodGet, filesBase+"/content?path="+url.QueryEscape("/plugins/a.yml"), token, nil)
	if status != http.StatusOK {
		t.Fatalf("读取文件失败: %d %s", status, resp)
	}
	var env mslxEnvelope
	_ = json.Unmarshal(resp, &env)
	var content string
	if err := json.Unmarshal(env.Data, &content); err != nil || !strings.Contains(content, "k: v") {
		t.Fatalf("文件内容不符: %s", env.Data)
	}

	// 列目录（全量）
	status, resp = mslxDo(t, http.MethodGet, filesBase+"/lists?path="+url.QueryEscape("/"), token, nil)
	if status != http.StatusOK {
		t.Fatalf("列目录失败: %d %s", status, resp)
	}
	_ = json.Unmarshal(resp, &env)
	var items []map[string]any
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatalf("列目录解析失败: %v（%s）", err, env.Data)
	}
	if len(items) != 1 || items[0]["name"] != "plugins" || items[0]["type"] != "folder" {
		t.Fatalf("列目录结果异常: %s", env.Data)
	}

	// 越界路径必须被拒
	if status, _ := mslxDo(t, http.MethodGet,
		filesBase+"/content?path="+url.QueryEscape("../../secret.txt"), token, nil); status != http.StatusForbidden {
		t.Fatalf("越界读取应 403，实际 %d", status)
	}

	// 上传（multipart）
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "server.properties")
	_, _ = part.Write([]byte("motd=hello\n"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, filesBase+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("x-user-token", token)
	respUp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	upBody, _ := io.ReadAll(respUp.Body)
	respUp.Body.Close()
	if respUp.StatusCode != http.StatusOK {
		t.Fatalf("上传应成功，实际 %d（%s）", respUp.StatusCode, upBody)
	}
	if _, err := os.Stat(filepath.Join(cwd, "server.properties")); err != nil {
		t.Fatalf("上传后文件不存在: %v", err)
	}

	// 下载
	status, resp = mslxDo(t, http.MethodGet,
		filesBase+"/download?path="+url.QueryEscape("/server.properties"), token, nil)
	if status != http.StatusOK || !strings.Contains(string(resp), "motd=hello") {
		t.Fatalf("下载内容不符: %d %s", status, resp)
	}

	// 重命名
	body, _ = json.Marshal(map[string]any{"oldPath": "/server.properties", "newPath": "/srv.properties"})
	if status, resp := mslxDo(t, http.MethodPost, filesBase+"/rename", token, body); status != http.StatusOK {
		t.Fatalf("重命名失败: %d %s", status, resp)
	}
	if _, err := os.Stat(filepath.Join(cwd, "srv.properties")); err != nil {
		t.Fatalf("重命名后文件不存在: %v", err)
	}

	// 删除
	body, _ = json.Marshal(map[string]any{"paths": []string{"/srv.properties"}})
	if status, resp := mslxDo(t, http.MethodPost, filesBase+"/delete", token, body); status != http.StatusOK {
		t.Fatalf("删除失败: %d %s", status, resp)
	}
	if _, err := os.Stat(filepath.Join(cwd, "srv.properties")); !os.IsNotExist(err) {
		t.Fatal("删除后文件仍存在")
	}
}

// TestCompatMSLXHubNegotiate SignalR 协商与凭据校验。
func TestCompatMSLXHubNegotiate(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	status, resp := mslxDo(t, http.MethodPost,
		base+"/api/hubs/instanceControlHub/negotiate?negotiateVersion=1", token, nil)
	if status != http.StatusOK {
		t.Fatalf("协商失败: %d %s", status, resp)
	}
	var neg struct {
		ConnectionToken     string `json:"connectionToken"`
		AvailableTransports []struct {
			Transport string `json:"transport"`
		} `json:"availableTransports"`
	}
	if err := json.Unmarshal(resp, &neg); err != nil {
		t.Fatalf("协商响应解析失败: %v（%s）", err, resp)
	}
	if neg.ConnectionToken == "" {
		t.Fatalf("协商未返回 connectionToken: %s", resp)
	}
	if len(neg.AvailableTransports) == 0 || neg.AvailableTransports[0].Transport != "WebSockets" {
		t.Fatalf("协商传输类型异常: %s", resp)
	}

	// 无凭据协商必须 401
	if status, _ := mslxDo(t, http.MethodPost,
		base+"/api/hubs/instanceControlHub/negotiate?negotiateVersion=1", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("无凭据协商应 401，实际 %d", status)
	}

	// 未实现接口明确返回 501，避免面板误以为成功
	if status, _ := mslxDo(t, http.MethodGet, base+"/api/docker/status", token, nil); status != http.StatusNotImplemented {
		t.Fatalf("未实现接口应 501，实际 %d", status)
	}
}

// TestCompatMSLXNotEnabled 未启用时不注册路由。
func TestCompatMSLXNotEnabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	if status, _ := mslxDo(t, http.MethodGet, srv.URL+"/mslx/api/ping", "", nil); status != http.StatusNotFound {
		t.Fatalf("未启用兼容层时应 404，实际 %d", status)
	}
}

// TestCompatBothLayersEnabled 两层同时开启互不影响。
func TestCompatBothLayersEnabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.setupCompat(&nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPrefix:  "/mcsm",
		CompatMCSMKey:     mcsmTestKey,
		CompatMSLXEnabled: true,
		CompatMSLXPrefix:  "/mslx",
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    mslxTestPass,
	})
	if d.compatMC == nil || d.compatMSLX == nil {
		t.Fatal("两层应可同时启用")
	}
	srv := newTestServer(t, d)
	defer srv.Close()

	// MCSM 层：engine.io 握手
	c := newEIOClient(t, srv.URL+"/mcsm/socket.io/")
	c.connect()
	if status, _ := c.requestRaw("auth", "b1", mcsmTestKey); status != 200 {
		t.Fatal("MCSM 层认证失败")
	}

	// MSLX 层：登录 + 实例列表
	mslxBase := srv.URL + "/mslx"
	token := mslxLogin(t, mslxBase)
	if status, body := mslxDo(t, http.MethodGet, mslxBase+"/api/instance/list", token, nil); status != http.StatusOK {
		t.Fatalf("MSLX 层实例列表失败: %d（%s）", status, body)
	}

	// 节点自身 API 不受影响
	if status, _ := doReq(t, srv.URL+"/api/overview?token="+qaTestToken()); status != http.StatusOK {
		t.Fatalf("节点自身 API 应不受兼容层影响，实际 %d", status)
	}
}

// itoa 整数转字符串（测试辅助，避免引入 strconv 影响可读性）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
