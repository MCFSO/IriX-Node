// MCSManager Daemon 兼容层测试（compat.mcsm）：
// 按 MCSManager 面板的真实线上行为驱动——engine.io v4 轮询握手、
// socket.io v4 包、{uuid,data} 请求与 {uuid,status,event,data} 回包。

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcsmTestKey 测试用节点密钥。
const mcsmTestKey = "qa-mcsm-node-key-0000000000"

// setupTestMCSM 在测试守护进程上启用 MCSM 兼容层并返回服务器地址。
func setupTestMCSM(t *testing.T, d *Daemon) string {
	t.Helper()
	d.setupCompat(&nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPrefix:  "/mcsm",
		CompatMCSMKey:     mcsmTestKey,
	})
	if d.compatMC == nil {
		t.Fatalf("MCSM 兼容层未启用（检查 setupCompat 配置校验）")
	}
	srv := newTestServer(t, d)
	t.Cleanup(srv.Close)
	return srv.URL + "/mcsm"
}

// eioClient 引擎层（engine.io v4）测试客户端，只走 HTTP 轮询传输。
type eioClient struct {
	t    *testing.T
	base string // 形如 http://127.0.0.1:PORT/mcsm/socket.io/
	sid  string
}

// newEIOClient 完成 engine.io 握手并拿到 sid。
func newEIOClient(t *testing.T, base string) *eioClient {
	t.Helper()
	c := &eioClient{t: t, base: base}
	body := c.get("?EIO=4&transport=polling")
	if !strings.HasPrefix(body, "0") {
		t.Fatalf("engine.io 握手响应异常: %q", body)
	}
	var hs struct {
		Sid          string   `json:"sid"`
		Upgrades     []string `json:"upgrades"`
		PingInterval int      `json:"pingInterval"`
		PingTimeout  int      `json:"pingTimeout"`
	}
	if err := json.Unmarshal([]byte(body[1:]), &hs); err != nil {
		t.Fatalf("握手载荷解析失败: %v（%q）", err, body)
	}
	if hs.Sid == "" {
		t.Fatalf("握手未返回 sid: %q", body)
	}
	if hs.PingInterval == 0 || hs.PingTimeout == 0 {
		t.Fatalf("握手缺少心跳参数: %q", body)
	}
	c.sid = hs.Sid
	return c
}

// get 发起轮询 GET。
func (c *eioClient) get(query string) string {
	c.t.Helper()
	u := c.base + query
	if c.sid != "" && !strings.Contains(query, "sid=") {
		u = c.base + "?EIO=4&transport=polling&sid=" + url.QueryEscape(c.sid) + query
	}
	resp, err := testClient.Get(u)
	if err != nil {
		c.t.Fatalf("轮询 GET 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// post 发起轮询 POST（body 为一个或多个 engine.io 包）。
func (c *eioClient) post(body string) {
	c.t.Helper()
	u := c.base + "?EIO=4&transport=polling&sid=" + url.QueryEscape(c.sid)
	resp, err := testClient.Post(u, "text/plain;charset=UTF-8", strings.NewReader(body))
	if err != nil {
		c.t.Fatalf("轮询 POST 失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != "ok" {
		c.t.Fatalf("轮询 POST 应答异常: %q", respBody)
	}
}

// waitPacket 轮询直到取到指定前缀的包（自动回应服务端 ping）。
func (c *eioClient) waitPacket(prefix string) string {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body := c.get("")
		if body == "" {
			continue
		}
		for _, pkt := range strings.Split(body, "\x1e") {
			if pkt == "2" {
				c.post("3") // 回 pong
				continue
			}
			if strings.HasPrefix(pkt, prefix) {
				return pkt
			}
		}
	}
	c.t.Fatalf("等待 %q 包超时", prefix)
	return ""
}

// connect 完成 socket.io 命名空间连接，返回服务端分配的 socket id。
func (c *eioClient) connect() string {
	c.t.Helper()
	c.post("40") // engine.io message + socket.io CONNECT
	pkt := c.waitPacket("40")
	payload := strings.TrimPrefix(pkt, "40")
	var body struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil || body.SID == "" {
		c.t.Fatalf("socket.io 连接应答异常: %q", pkt)
	}
	return body.SID
}

// waitEvent 轮询直到取到指定事件与 uuid 的回包（忽略期间的状态推送）。
func (c *eioClient) waitEvent(event, uuid string) string {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body := c.get("")
		if body == "" {
			continue
		}
		for _, pkt := range strings.Split(body, "\x1e") {
			if pkt == "2" {
				c.post("3") // 回 pong
				continue
			}
			if !strings.HasPrefix(pkt, "42") {
				continue
			}
			var arr []json.RawMessage
			if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
				continue
			}
			var name string
			if err := json.Unmarshal(arr[0], &name); err != nil || name != event {
				continue
			}
			var p struct {
				UUID *string `json:"uuid"`
			}
			if err := json.Unmarshal(arr[1], &p); err != nil || p.UUID == nil || *p.UUID != uuid {
				continue
			}
			return pkt
		}
	}
	c.t.Fatalf("等待事件 %q（uuid=%s）回包超时", event, uuid)
	return ""
}

// requestRaw 发送事件请求 {uuid,data} 并返回回包的 status 与 data。
func (c *eioClient) requestRaw(event, uuid string, data any) (int, json.RawMessage) {
	c.t.Helper()
	reqBody, _ := json.Marshal(map[string]any{"uuid": uuid, "data": data})
	eventJSON, _ := json.Marshal(event)
	packet := "42[" + string(eventJSON) + "," + string(reqBody) + "]"
	c.post(packet)
	pkt := c.waitEvent(event, uuid)
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
		c.t.Fatalf("回包格式错误: %q", pkt)
	}
	var packet2 struct {
		UUID   *string         `json:"uuid"`
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(arr[1], &packet2); err != nil {
		c.t.Fatalf("回包载荷解析失败: %v（%q）", err, pkt)
	}
	return packet2.Status, packet2.Data
}

// TestCompatMCSMHandshakeAndAuth engine.io 握手 + socket.io 连接 + 节点密钥认证。
func TestCompatMCSMHandshakeAndAuth(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d) + "/socket.io/"
	c := newEIOClient(t, base)
	if sid := c.connect(); sid == "" {
		t.Fatal("socket.io 连接未返回 sid")
	}

	// 错误密钥：回包 status 500 且带 err
	status, data := c.requestRaw("auth", "u-bad", "wrong-key")
	if status != 500 {
		t.Fatalf("错误密钥应返回 500，实际 %d（%s）", status, data)
	}
	if !strings.Contains(string(data), "err") {
		t.Fatalf("错误回包应包含 err 字段: %s", data)
	}

	// 正确密钥：data 为 true
	status, data = c.requestRaw("auth", "u-ok", mcsmTestKey)
	if status != 200 {
		t.Fatalf("正确密钥应返回 200，实际 %d（%s）", status, data)
	}
	if strings.TrimSpace(string(data)) != "true" {
		t.Fatalf("认证回包应为 true，实际 %s", data)
	}

	// 认证后未支持事件必须明确失败，而不是静默成功
	status, _ = c.requestRaw("no/such/event", "u-x", map[string]any{})
	if status != 500 {
		t.Fatalf("未支持事件应返回 500，实际 %d", status)
	}
}

// TestCompatMCSMInstanceLifecycle 实例列表/详情/创建/启动/停止/删除与状态推送。
func TestCompatMCSMInstanceLifecycle(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d) + "/socket.io/"
	c := newEIOClient(t, base)
	c.connect()
	if status, data := c.requestRaw("auth", "u1", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}

	// 未认证会话不得访问业务事件：换一个新会话验证
	c2 := newEIOClient(t, base)
	c2.connect()
	if status, _ := c2.requestRaw("instance/overview", "u-unauth", map[string]any{}); status != 500 {
		t.Fatalf("未认证会话访问业务事件应失败，实际 %d", status)
	}

	workdir := worldDir(t)
	cwd := filepath.Join(workdir, "srv1")

	// 创建实例
	status, data := c.requestRaw("instance/new", "u2", map[string]any{
		"nickname":     "兼容层实例",
		"startCommand": "ping 127.0.0.1 -t",
		"stopCommand":  "stop",
		"cwd":          cwd,
		"type":         "universal",
		"ie":           "utf-8",
		"oe":           "utf-8",
	})
	if status != 200 {
		t.Fatalf("创建实例失败: %d %s", status, data)
	}
	var created struct {
		InstanceUUID string         `json:"instanceUuid"`
		Config       map[string]any `json:"config"`
		Nickname     string         `json:"nickname"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatalf("创建回包解析失败: %v（%s）", err, data)
	}
	if created.InstanceUUID == "" {
		t.Fatalf("创建实例未返回 uuid: %s", data)
	}
	if created.Config["terminalOption"] == nil || created.Config["docker"] == nil {
		t.Fatalf("实例配置缺少 MCSM 面板依赖的字段: %s", data)
	}
	if _, err := os.Stat(cwd); err != nil {
		t.Fatalf("创建实例后工作目录应已存在: %v", err)
	}

	// 列表
	status, data = c.requestRaw("instance/overview", "u3", map[string]any{})
	if status != 200 {
		t.Fatalf("实例概览失败: %d %s", status, data)
	}
	var overview []map[string]any
	if err := json.Unmarshal(data, &overview); err != nil {
		t.Fatalf("概览解析失败: %v", err)
	}
	if len(overview) != 1 {
		t.Fatalf("概览应返回 1 个实例，实际 %d", len(overview))
	}
	if overview[0]["instanceUuid"] != created.InstanceUUID {
		t.Fatalf("概览实例 uuid 不匹配: %v", overview[0]["instanceUuid"])
	}

	// 分页查询
	status, data = c.requestRaw("instance/select", "u4", map[string]any{
		"page": 1, "pageSize": 10,
		"condition": map[string]any{"instanceName": "兼容层"},
	})
	if status != 200 {
		t.Fatalf("实例分页查询失败: %d %s", status, data)
	}
	var page struct {
		Page    int              `json:"page"`
		MaxPage int              `json:"maxPage"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("分页解析失败: %v", err)
	}
	if len(page.Data) != 1 || page.MaxPage < 1 {
		t.Fatalf("分页结果异常: %s", data)
	}

	// 启动实例
	status, data = c.requestRaw("instance/open", "u5", map[string]any{
		"instanceUuids": []string{created.InstanceUUID},
	})
	if status != 200 {
		t.Fatalf("启动实例失败: %d %s", status, data)
	}
	inst := d.Find(created.InstanceUUID)
	if inst == nil {
		t.Fatal("实例未注册到守护进程")
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
		t.Fatalf("实例应处于运行中，实际 status=%d", st)
	}

	// 下发命令
	if status, data := c.requestRaw("instance/command", "u6", map[string]any{
		"instanceUuid": created.InstanceUUID,
		"command":      "noop",
	}); status != 200 {
		t.Fatalf("下发命令失败: %d %s", status, data)
	}

	// 强杀
	if status, data := c.requestRaw("instance/kill", "u7", map[string]any{
		"instanceUuids": []string{created.InstanceUUID},
	}); status != 200 {
		t.Fatalf("强杀实例失败: %d %s", status, data)
	}

	// 删除
	if status, data := c.requestRaw("instance/delete", "u8", map[string]any{
		"instanceUuids": []string{created.InstanceUUID},
		"deleteFile":    false,
	}); status != 200 {
		t.Fatalf("删除实例失败: %d %s", status, data)
	}
	if d.Find(created.InstanceUUID) != nil {
		t.Fatal("删除后实例仍存在")
	}
}

// TestCompatMCSMFileOpsAndTickets 文件管理事件与直连下载通道。
func TestCompatMCSMFileOpsAndTickets(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d) + "/socket.io/"
	c := newEIOClient(t, base)
	c.connect()
	if status, _ := c.requestRaw("auth", "f0", mcsmTestKey); status != 200 {
		t.Fatal("认证失败")
	}

	cwd := filepath.Join(worldDir(t), "srv-file")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = "ping 127.0.0.1 -t"
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}

	// 新建目录 + 文件 + 写入内容
	if status, data := c.requestRaw("file/mkdir", "f1", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "plugins",
	}); status != 200 {
		t.Fatalf("新建目录失败: %d %s", status, data)
	}
	if status, data := c.requestRaw("file/touch", "f2", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "plugins/a.yml",
	}); status != 200 {
		t.Fatalf("新建文件失败: %d %s", status, data)
	}
	if status, data := c.requestRaw("file/edit", "f3", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "plugins/a.yml", "text": "hello: 1\n",
	}); status != 200 {
		t.Fatalf("编辑文件失败: %d %s", status, data)
	}

	// 列目录
	status, data := c.requestRaw("file/list", "f4", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "/", "page": 0, "pageSize": 40,
	})
	if status != 200 {
		t.Fatalf("列目录失败: %d %s", status, data)
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
			Type int    `json:"type"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("列目录解析失败: %v（%s）", err, data)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Name != "plugins" || list.Items[0].Type != 0 {
		t.Fatalf("列目录结果异常: %s", data)
	}

	// 路径越界必须被拒绝
	if status, _ := c.requestRaw("file/edit", "f5", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "../../etc/passwd", "text": "x",
	}); status != 500 {
		t.Fatalf("越界路径应被拒绝，实际 %d", status)
	}

	// 注册下载通行证并直连下载
	if status, data := c.requestRaw("passport/register", "f6", map[string]any{
		"name":     "download",
		"password": "qa-pass-1",
		"parameter": map[string]any{
			"instanceUuid": inst.InstanceUuid,
			"fileName":     "/plugins/a.yml",
		},
	}); status != 200 {
		t.Fatalf("注册下载通行证失败: %d %s", status, data)
	}
	resp, err := testClient.Get(base[:strings.Index(base, "/socket.io/")] + "/download/qa-pass-1/a.yml")
	if err != nil {
		t.Fatalf("直连下载失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("直连下载状态异常: %d（%s）", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "hello: 1") {
		t.Fatalf("下载内容不符: %q", body)
	}

	// 越权文件名：通行证只绑定申请文件
	resp2, err := testClient.Get(base[:strings.Index(base, "/socket.io/")] + "/download/qa-pass-1/other.yml")
	if err != nil {
		t.Fatalf("直连下载请求失败: %v", err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("通行证不应允许下载其它文件")
	}
}

// TestCompatMCSMNotEnabled 未启用时不注册任何兼容路由。
func TestCompatMCSMNotEnabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	resp, err := testClient.Get(srv.URL + "/mcsm/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未启用兼容层时应 404，实际 %d", resp.StatusCode)
	}
}

// TestCompatThroughFullMiddlewareChain 完整中间件链（防御/审计/CORS/body 限额）
// 下的兼容层可用性：确认这些中间件不会截断或拒绝兼容层流量。
func TestCompatThroughFullMiddlewareChain(t *testing.T) {
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	srv := d.newHTTPServer(ln.Addr().String())
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	base := "http://" + ln.Addr().String()

	// MCSM：握手 + 认证（走完整中间件）
	c := newEIOClient(t, base+"/mcsm/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "mw1", mcsmTestKey); status != 200 {
		t.Fatalf("完整中间件链下认证失败: %d %s", status, data)
	}
	// 业务事件（含 POST 请求体解析）
	if status, data := c.requestRaw("instance/overview", "mw2", map[string]any{}); status != 200 {
		t.Fatalf("完整中间件链下实例概览失败: %d %s", status, data)
	}

	// MSLX：登录（POST + JSON body）
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": mslxTestPass})
	status, resp := mslxDo(t, http.MethodPost, base+"/mslx/api/auth/login", "", body)
	if status != http.StatusOK {
		t.Fatalf("完整中间件链下 MSLX 登录失败: %d %s", status, resp)
	}
	var env mslxEnvelope
	if err := json.Unmarshal(resp, &env); err != nil || env.Code != 200 {
		t.Fatalf("完整中间件链下登录响应异常: %s", resp)
	}
}

// TestCompatPrefixAndAllowParsing 前缀与来源白名单解析规则。
func TestCompatPrefixAndAllowParsing(t *testing.T) {
	if p, err := compatPrefix("", defaultMCSMPrefix); err != nil || p != "/mcsm" {
		t.Fatalf("默认前缀解析异常: %q %v", p, err)
	}
	if p, err := compatPrefix("mcsm2/", defaultMCSMPrefix); err != nil || p != "/mcsm2" {
		t.Fatalf("前缀规范化异常: %q %v", p, err)
	}
	for _, bad := range []string{"/", "//x", "/a b", "/a?b"} {
		if _, err := compatPrefix(bad, defaultMCSMPrefix); err == nil {
			t.Fatalf("非法前缀 %q 应被拒绝", bad)
		}
	}
	nets, err := parseCompatAllow("127.0.0.1\n10.0.0.0/8")
	if err != nil || len(nets) != 2 {
		t.Fatalf("来源白名单解析异常: %v %v", nets, err)
	}
	if _, err := parseCompatAllow("not-an-ip"); err == nil {
		t.Fatal("非法来源条目应报错")
	}
}
