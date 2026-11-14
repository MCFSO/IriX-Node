// MCSManager Daemon 兼容层测试（compat.mcsm）：
// 按 MCSManager 面板的真实线上行为驱动——engine.io v4 轮询握手、
// socket.io v4 包、{uuid,data} 请求与 {uuid,status,event,data} 回包。

package main

import (
	"context"
	"encoding/json"
	"io"
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

// setupTestMCSM 在测试守护进程上启用 MCSM 兼容层（独立端口，0 = 系统分配），
// 返回该端口的基址（如 http://127.0.0.1:54321）。
func setupTestMCSM(t *testing.T, d *Daemon) string {
	t.Helper()
	return startTestCompat(t, d, &nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0, // 端口 0 = 由系统分配（测试专用）
		CompatMCSMKey:     mcsmTestKey,
	}, "mcsm")
}

// startTestCompat 应用兼容层配置并启动监听，返回指定层的基址。
func startTestCompat(t *testing.T, d *Daemon, opts *nodeOptions, layer string) string {
	t.Helper()
	d.setupCompat(opts)
	if d.compatMC == nil && d.compatMSLX == nil {
		t.Fatalf("兼容层未启用（检查 setupCompat 配置校验）")
	}
	d.startCompatServers()
	t.Cleanup(func() {
		_ = d.stopCompatServers(context.Background())
	})
	addr := d.compatAddrOf(layer)
	if addr == "" {
		t.Fatalf("%s 兼容层没有监听地址（端口占用或未启用？）", layer)
	}
	return "http://" + addr
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
		"startCommand": echoCommand(),
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

	// 强杀（杀进程是异步回收，等状态落定再删，避免删除被「正在运行」拒绝）
	if status, data := c.requestRaw("instance/kill", "u7", map[string]any{
		"instanceUuids": []string{created.InstanceUUID},
	}); status != 200 {
		t.Fatalf("强杀实例失败: %d %s", status, data)
	}
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		st = inst.Status
		inst.mu.Unlock()
		if st == StatusStopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st != StatusStopped {
		t.Fatalf("强杀后实例应为已停止，实际 status=%d", st)
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
	inst.Config.StartCommand = echoCommand()
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

// TestCompatMCSMNotEnabled 未启用时既不注册主路由，也不开监听端口。
func TestCompatMCSMNotEnabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()
	// 主服务（节点自身 API 端口）上不再挂兼容层路径
	resp, err := testClient.Get(srv.URL + "/socket.io/?EIO=4&transport=polling")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("未启用兼容层时主端口不应提供 socket.io，实际 %d", resp.StatusCode)
	}
	// 也不应启动任何兼容层监听
	d.startCompatServers()
	if addr := d.compatAddrOf("mcsm"); addr != "" {
		t.Fatalf("未启用时不应监听兼容层端口，实际 %s", addr)
	}
	if len(d.compatServers) != 0 {
		t.Fatalf("未启用时不应登记监听器，实际 %d 个", len(d.compatServers))
	}
}

// TestCompatListenersUseMiddleware 兼容层独立端口走与主服务一致的中间件：
//   - allowPanelIps 不在白名单 → 403（来源限制在监听入口生效）；
//   - 防御的静态 UA 封禁 → 非 200（防御中间件确实挂在兼容层上）。
func TestCompatListenersUseMiddleware(t *testing.T) {
	d, _ := newTestDaemon(t)
	// 只允许一个不可能命中的网段 → 环回来源会被拒
	base := startTestCompat(t, d, &nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0,
		CompatMCSMKey:     mcsmTestKey,
		CompatMCSMIPs:     "10.99.0.0/16",
	}, "mcsm")

	resp, err := testClient.Get(base + "/socket.io/?EIO=4&transport=polling")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("来源不在白名单时应 403，实际 %d（%s）", resp.StatusCode, body)
	}

	// 放开来源限制，但把该 User-Agent 静态封禁
	d2, _ := newTestDaemon(t)
	d2.defense.applyOptions(defenseOptions{BlockedUserAgents: []string{"compat-blocked/1.0"}})
	base2 := startTestCompat(t, d2, &nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0,
		CompatMCSMKey:     mcsmTestKey,
	}, "mcsm")
	req, _ := http.NewRequest(http.MethodGet, base2+"/socket.io/?EIO=4&transport=polling", nil)
	req.Header.Set("User-Agent", "compat-blocked/1.0")
	resp2, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("被静态封禁的 User-Agent 不应拿到 200（防御中间件未生效）")
	}
}

// TestCompatBothLayersOnOwnPorts 两层同时开启：各自独立端口，互不影响。
func TestCompatBothLayersOnOwnPorts(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.setupCompat(&nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0,
		CompatMCSMKey:     mcsmTestKey,
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0,
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    mslxTestPass,
	})
	if d.compatMC == nil || d.compatMSLX == nil {
		t.Fatal("两层应可同时启用")
	}
	d.startCompatServers()
	t.Cleanup(func() { _ = d.stopCompatServers(context.Background()) })

	mcAddr := d.compatAddrOf("mcsm")
	msAddr := d.compatAddrOf("mslx")
	if mcAddr == "" || msAddr == "" {
		t.Fatalf("两层都应监听：mcsm=%q mslx=%q", mcAddr, msAddr)
	}
	if mcAddr == msAddr {
		t.Fatalf("两层必须使用不同端口：%s", mcAddr)
	}

	// MCSM 端口上只有 socket.io / 直连通道，没有 /api/
	resp, err := testClient.Get("http://" + mcAddr + "/api/ping")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("MCSM 端口不应提供 MSLX 的 /api/，实际 %d", resp.StatusCode)
	}
	// MSLX 端口上只有 /api/，没有 socket.io
	resp2, err := testClient.Get("http://" + msAddr + "/socket.io/?EIO=4&transport=polling")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("MSLX 端口不应提供 socket.io，实际 %d", resp2.StatusCode)
	}
	// 两个端口都活着
	c := newEIOClient(t, "http://"+mcAddr+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "pp1", mcsmTestKey); status != 200 {
		t.Fatalf("MCSM 端口认证失败: %d %s", status, data)
	}
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": mslxTestPass})
	status, respLogin := mslxDo(t, http.MethodPost, "http://"+msAddr+"/api/auth/login", "", body)
	if status != http.StatusOK {
		t.Fatalf("MSLX 端口登录失败: %d %s", status, respLogin)
	}
}

// TestCompatAllowParsing 来源白名单解析规则。
func TestCompatAllowParsing(t *testing.T) {
	nets, err := parseCompatAllow("127.0.0.1\n10.0.0.0/8")
	if err != nil || len(nets) != 2 {
		t.Fatalf("来源白名单解析异常: %v %v", nets, err)
	}
	if _, err := parseCompatAllow("not-an-ip"); err == nil {
		t.Fatal("非法来源条目应报错")
	}
	if nets, err := parseCompatAllow("  "); err != nil || nets != nil {
		t.Fatalf("空白配置应视为不限制: %v %v", nets, err)
	}
}

// TestCompatListenAddr 监听地址组装：空 bind 沿用节点 bind，端口原样拼接。
func TestCompatListenAddr(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.BindHost = "0.0.0.0"
	if got := d.compatListenAddr("", defaultMCSMCompatPort); got != "0.0.0.0:24444" {
		t.Fatalf("空 bind 应沿用节点 bind: %q", got)
	}
	if got := d.compatListenAddr("192.168.1.9", defaultMSLXCompatPort); got != "192.168.1.9:7891" {
		t.Fatalf("bind 覆盖异常: %q", got)
	}
	d.BindHost = "::"
	if got := d.compatListenAddr("", defaultMCSMCompatPort); got != "[::]:24444" {
		t.Fatalf("IPv6 监听地址拼接异常: %q", got)
	}
	d.BindHost = ""
	if got := d.compatListenAddr("", defaultMCSMCompatPort); got != "127.0.0.1:24444" {
		t.Fatalf("空 bind 且节点未设置时应回落环回: %q", got)
	}
}

// TestCompatMCSMOverviewChartAndVersion 节点概览必须给出与 MCSM 完全同形的
// 系统曲线（200 个 {cpu,mem} 整数百分比）与可配置的上报版本号。
func TestCompatMCSMOverviewChartAndVersion(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "cv1", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	status, data := c.requestRaw("info/overview", "cv2", map[string]any{})
	if status != 200 {
		t.Fatalf("info/overview 失败: %d %s", status, data)
	}
	var overview struct {
		Version     string `json:"version"`
		CPUMemChart []struct {
			CPU *int `json:"cpu"`
			Mem *int `json:"mem"`
		} `json:"cpuMemChart"`
		System map[string]any `json:"system"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(data, &overview); err != nil {
		t.Fatalf("概览解析失败: %v（%s）", err, data)
	}
	// 面板 NodeOverview 画 200 格曲线，缺字段会直接空白
	if len(overview.CPUMemChart) != mcChartSize {
		t.Fatalf("cpuMemChart 应为 %d 个采样点，实际 %d", mcChartSize, len(overview.CPUMemChart))
	}
	for i, point := range overview.CPUMemChart {
		if point.CPU == nil || point.Mem == nil {
			t.Fatalf("第 %d 个采样点缺少 cpu/mem 字段（面板据此画图）", i)
		}
		if *point.CPU < 0 || *point.CPU > 100 || *point.Mem < 0 || *point.Mem > 100 {
			t.Fatalf("第 %d 个采样点百分比越界: cpu=%d mem=%d", i, *point.CPU, *point.Mem)
		}
	}
	// 面板还要读 system.cpuUsage / memUsage 与 config 段
	for _, key := range []string{"cpuUsage", "memUsage", "totalmem", "freemem", "uptime"} {
		if _, ok := overview.System[key]; !ok {
			t.Fatalf("system 段缺少 %s（面板节点卡片依赖）", key)
		}
	}
	for _, key := range []string{"port", "language", "outputBufferSize"} {
		if _, ok := overview.Config[key]; !ok {
			t.Fatalf("config 段缺少 %s", key)
		}
	}
	if overview.Version != Version {
		t.Fatalf("默认上报版本应为本节点 Version=%q，实际 %q", Version, overview.Version)
	}

	// 版本可覆盖：面板按主次版本号比较，配成面板期望值即显示为最新
	d2, _ := newTestDaemon(t)
	base2 := startTestCompat(t, d2, &nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0,
		CompatMCSMKey:     mcsmTestKey,
		CompatMCSMVersion: "4.5.0",
	}, "mcsm")
	c2 := newEIOClient(t, base2+"/socket.io/")
	c2.connect()
	if status, _ := c2.requestRaw("auth", "cv3", mcsmTestKey); status != 200 {
		t.Fatal("认证失败")
	}
	_, data2 := c2.requestRaw("info/overview", "cv4", map[string]any{})
	var ov2 struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data2, &ov2); err != nil {
		t.Fatalf("概览解析失败: %v", err)
	}
	if ov2.Version != "4.5.0" {
		t.Fatalf("compat.mcsm.version 应覆盖上报版本，实际 %q", ov2.Version)
	}
	// upgrade/info 也用同一版本
	_, up := c2.requestRaw("upgrade/info", "cv5", map[string]any{})
	if !strings.Contains(string(up), "4.5.0") {
		t.Fatalf("upgrade/info 应使用覆盖后的版本: %s", up)
	}
}
