// MSLX 子节点链接协议（Protocol V1）回归测试。
//
// 复刻 MSLX WebPanel「设置 → 节点管理 → 添加节点」的真实流程：
//  1. 主节点 GET  {node}/api/node/protocol              → protocolVersion=1
//  2. 主节点 POST {node}/api/node/accept-link            → LinkKey + PairingId + MasterUrl
//  3. 子节点反向 GET {master}/api/node/protocol?pairingId=… → 必须回 200 + 同一 pairingId
//  4. 子节点返回 {nodeId, commsKey}；之后每 2 秒 POST {master}/api/node/report-stats
//     （头 x-api-key: commsKey）
//
// 这里用一个假主节点（httptest）承担 3 与 4 的角色。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMSLXMaster 假主节点：实现 protocol / report-stats 两个端点。
type fakeMSLXMaster struct {
	srv *httptest.Server

	mu        sync.Mutex
	pairingID string      // 当前认可的配对标识（空 = 不认可）
	reports   []fakeStats // 收到的状态上报
	keys      []string    // 收到的 x-api-key
	gone      bool        // 回 410 模拟关系撤销
	commsKey  string      // 链接后主节点登记的通讯密钥（verify-token 校验用）
}

type fakeStats struct {
	CPU       float64 `json:"cpu"`
	MemTotal  float64 `json:"memTotal"`
	MemUsed   float64 `json:"memUsed"`
	MemUsage  float64 `json:"memUsage"`
	Timestamp string  `json:"timestamp"`
}

func newFakeMSLXMaster(t *testing.T, pairingID string) *fakeMSLXMaster {
	t.Helper()
	m := &fakeMSLXMaster{pairingID: pairingID}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/node/protocol", func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("pairingId")
		m.mu.Lock()
		expect := m.pairingID
		m.mu.Unlock()
		if expect == "" || got != expect {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "message": "此主节点没有对应的配对请求"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "成功",
			"data": map[string]any{
				"protocolVersion": mslxNodeProtocolVersion,
				"pairingId":       got,
				"features":        map[string]any{"nodeSync": true},
			},
		})
	})
	mux.HandleFunc("/api/node/report-stats", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		gone := m.gone
		m.mu.Unlock()
		if gone {
			writeCompatJSON(w, http.StatusGone, mslxResponse{Code: 410, Message: "节点关系已撤销"})
			return
		}
		var st fakeStats
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		_ = json.Unmarshal(body, &st)
		m.mu.Lock()
		m.reports = append(m.reports, st)
		m.keys = append(m.keys, r.Header.Get("x-api-key"))
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "上报成功"})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// postJSON 向兼容层发 JSON POST。
func postJSONToCompat(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestCompatMSLXNodeProtocol 协议探测必须返回 MSLX 要求的 protocolVersion。
func TestCompatMSLXNodeProtocol(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	code, out := postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{}) // 只为确保路由存在
	_ = code
	_ = out

	resp, err := testClient.Get(base + "/api/node/protocol")
	if err != nil {
		t.Fatalf("请求协议接口失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Code int `json:"code"`
		Data struct {
			ProtocolVersion int  `json:"protocolVersion"`
			SlaveMode       bool `json:"slaveMode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("协议响应解析失败: %v（%s）", err, raw)
	}
	if env.Code != 200 || env.Data.ProtocolVersion != mslxNodeProtocolVersion {
		t.Fatalf("协议号必须是 %d: %s", mslxNodeProtocolVersion, raw)
	}
	if !env.Data.SlaveMode {
		t.Fatalf("默认应为子节点模式（否则 MSLX 会拒绝链接）: %s", raw)
	}

	// 携带 pairingId 时本节点不是主节点，必须明确拒绝
	resp2, err := testClient.Get(base + "/api/node/protocol?pairingId=" + strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("带 pairingId 的探测应 400，实际 %d", resp2.StatusCode)
	}
}

// TestCompatMSLXNodeAcceptLink 链接握手：校验、反向探测、返回节点标识与通讯密钥。
func TestCompatMSLXNodeAcceptLink(t *testing.T) {
	d, _ := newTestDaemon(t)
	const linkKey = "qa-mslx-link-key-000000000001"
	base := startTestCompat(t, d, &nodeOptions{
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0,
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    mslxTestPass,
		CompatMSLXLinkKey: linkKey,
		CompatMSLXMaster:  false,
	}, "mslx")

	pairing := strings.Repeat("b", 32)
	master := newFakeMSLXMaster(t, pairing)

	// 1) 错误 LinkKey → 401
	code, out := postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterId":        "m-1",
		"masterUrl":       master.srv.URL,
		"linkKey":         "wrong-key",
		"pairingId":       pairing,
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误 LinkKey 应 401，实际 %d %s", code, out)
	}

	// 2) 配对标识非法 → 400
	code, out = postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterId":        "m-1",
		"masterUrl":       master.srv.URL,
		"linkKey":         linkKey,
		"pairingId":       "not-a-guid",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("非法配对标识应 400，实际 %d %s", code, out)
	}

	// 3) 主节点不认可本次配对 → 400 且提示未确认配对
	master.mu.Lock()
	master.pairingID = ""
	master.mu.Unlock()
	code, out = postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterId":        "m-1",
		"masterUrl":       master.srv.URL,
		"linkKey":         linkKey,
		"pairingId":       pairing,
	})
	if code != http.StatusBadRequest || !strings.Contains(out["message"].(string), "配对") {
		t.Fatalf("主节点未确认配对时应 400: %d %v", code, out)
	}

	// 4) 正常链接
	master.mu.Lock()
	master.pairingID = pairing
	master.mu.Unlock()
	code, out = postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterId":        "m-1",
		"masterUrl":       master.srv.URL,
		"linkKey":         linkKey,
		"pairingId":       pairing,
	})
	if code != http.StatusOK {
		t.Fatalf("链接应成功: %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	nodeID, _ := data["nodeId"].(string)
	commsKey, _ := data["commsKey"].(string)
	if len(nodeID) != 32 || len(commsKey) < 16 {
		t.Fatalf("应返回 32 位 nodeId 与通讯密钥: %v", data)
	}

	// 5) 链接关系已持久化
	raw, err := os.ReadFile(d.compatMSLX.nodeStatePath())
	if err != nil {
		t.Fatalf("链接关系未落盘: %v", err)
	}
	if !strings.Contains(string(raw), nodeID) || !strings.Contains(string(raw), master.srv.URL) {
		t.Fatalf("落盘内容不含节点标识/主节点地址: %s", raw)
	}

	// 6) 上报循环会把 CPU/内存打到主节点（带 x-api-key）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		master.mu.Lock()
		n := len(master.reports)
		master.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	master.mu.Lock()
	reports, keys := append([]fakeStats(nil), master.reports...), append([]string(nil), master.keys...)
	master.mu.Unlock()
	if len(reports) == 0 {
		t.Fatal("链接后应每 2 秒向主节点上报状态")
	}
	if keys[len(keys)-1] != commsKey {
		t.Fatalf("上报应带 x-api-key=commsKey，实际 %q", keys[len(keys)-1])
	}
	if reports[0].Timestamp == "" || reports[0].MemTotal <= 0 {
		t.Fatalf("上报内容应含 timestamp 与内存总量: %+v", reports[0])
	}

	// 7) update-link：通讯密钥错误 → 401；正确 → 成功
	code, out = postJSONToCompat(t, base+"/api/node/update-link", map[string]any{
		"masterUrl": master.srv.URL, "commsKey": "bad-key", "nodeId": nodeID,
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误 commsKey 应 401，实际 %d %v", code, out)
	}
	code, out = postJSONToCompat(t, base+"/api/node/update-link", map[string]any{
		"masterUrl": master.srv.URL + "/", "commsKey": commsKey, "nodeId": nodeID,
	})
	if code != http.StatusOK {
		t.Fatalf("update-link 应成功: %d %v", code, out)
	}

	// 8) 主节点回 410 → 自动清除本地关系
	master.mu.Lock()
	master.gone = true
	master.mu.Unlock()
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if d.compatMSLX.currentNodeLink() == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d.compatMSLX.currentNodeLink() != nil {
		t.Fatal("主节点撤销关系（410）后应清除本地链接")
	}

	// 9) remove-link 幂等成功
	code, out = postJSONToCompat(t, base+"/api/node/remove-link", map[string]any{
		"nodeId": nodeID, "commsKey": commsKey,
	})
	if code != http.StatusOK {
		t.Fatalf("remove-link 应成功: %d %v", code, out)
	}
}

// TestCompatMSLXNodeRejectsNonSlaveMode 非子节点模式必须拒绝链接（与 MSLX 一致）。
func TestCompatMSLXNodeRejectsNonSlaveMode(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := startTestCompat(t, d, &nodeOptions{
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0,
		CompatMSLXLinkKey: "qa-link-key-0000000000000002",
		CompatMSLXMaster:  true,
	}, "mslx")

	pairing := strings.Repeat("c", 32)
	master := newFakeMSLXMaster(t, pairing)

	code, out := postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterUrl":       master.srv.URL,
		"linkKey":         "qa-link-key-0000000000000002",
		"pairingId":       pairing,
	})
	if code != http.StatusBadRequest || !strings.Contains(out["message"].(string), "子节点模式") {
		t.Fatalf("非子节点模式应拒绝: %d %v", code, out)
	}
}

// TestCompatMSLXStatusProbeIsAnonymous 面板判定子节点在线的方式是
// GET {节点}/api/status（带 x-node-id 头、3 秒超时）—— 必须匿名 200，
// 否则节点永远显示离线。
func TestCompatMSLXStatusProbeIsAnonymous(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	for _, withHeader := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodGet, base+"/api/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		if withHeader {
			req.Header.Set("x-node-id", "probe-node")
		}
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("探活请求失败: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("探活必须 200（x-node-id=%v），实际 %d %s", withHeader, resp.StatusCode, raw)
		}
		var env struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Code != 200 {
			t.Fatalf("探活响应异常: %s", raw)
		}
		if env.Data["version"] == nil {
			t.Fatalf("探活响应应含版本信息: %s", raw)
		}
		// 匿名探活不返回主机指标/实例统计（避免信息泄露）
		if _, ok := env.Data["system"]; ok {
			t.Fatalf("匿名探活不应返回主机指标: %s", raw)
		}
	}
}

// TestCompatMSLXNodeProxiedRequest 主节点代理请求（x-node-id + 主节点令牌）
// 必须经主节点 /api/node/verify-token 核对后放行。
func TestCompatMSLXNodeProxiedRequest(t *testing.T) {
	d, _ := newTestDaemon(t)
	const linkKey = "qa-mslx-link-key-000000000003"
	base := startTestCompat(t, d, &nodeOptions{
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0,
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    mslxTestPass,
		CompatMSLXLinkKey: linkKey,
	}, "mslx")

	pairing := strings.Repeat("d", 32)
	master := newFakeMSLXMaster(t, pairing)
	// 主节点侧增加 verify-token 端点
	master.srv.Config.Handler.(*http.ServeMux).HandleFunc("/api/node/verify-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != master.commsKey {
			writeCompatJSON(w, http.StatusUnauthorized, mslxResponse{Code: 401, Message: "无效的通讯密钥"})
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Token != "node-token-ok" {
			writeCompatJSON(w, http.StatusUnauthorized, mslxResponse{Code: 401, Message: "无效的节点令牌"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "验证成功",
			"data": map[string]any{"userId": "u-1", "name": "admin", "role": "admin"},
		})
	})

	code, out := postJSONToCompat(t, base+"/api/node/accept-link", map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"masterId":        "m-1",
		"masterUrl":       master.srv.URL,
		"linkKey":         linkKey,
		"pairingId":       pairing,
	})
	if code != http.StatusOK {
		t.Fatalf("链接失败: %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	nodeID, _ := data["nodeId"].(string)
	master.mu.Lock()
	master.commsKey, _ = data["commsKey"].(string)
	master.mu.Unlock()

	// 无令牌的节点请求（除 status 外）必须 401
	req, _ := http.NewRequest(http.MethodGet, base+"/api/instance/list", nil)
	req.Header.Set("x-node-id", nodeID)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌的节点请求应 401，实际 %d", resp.StatusCode)
	}

	// 主节点核验通过的令牌 → 放行
	req, _ = http.NewRequest(http.MethodGet, base+"/api/instance/list", nil)
	req.Header.Set("x-node-id", nodeID)
	req.Header.Set("x-user-token", "node-token-ok")
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("主节点核验通过的请求应放行，实际 %d %s", resp.StatusCode, raw)
	}

	// 主节点拒绝的令牌 → 401
	req, _ = http.NewRequest(http.MethodGet, base+"/api/instance/list", nil)
	req.Header.Set("x-node-id", nodeID)
	req.Header.Set("x-user-token", "bad-token")
	resp, err = testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("主节点拒绝的令牌应 401，实际 %d", resp.StatusCode)
	}
}

// TestCompatMSLXCorsPreflightForNodeProbe 面板对子节点的探活是**跨源**请求，
// 带 x-node-id / x-user-token 两个自定义头，浏览器会先发预检。
// 允许头清单里缺任何一个，预检就会失败，面板随即判定节点离线。
func TestCompatMSLXCorsPreflightForNodeProbe(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	req, err := http.NewRequest(http.MethodOptions, base+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:1027")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "x-node-id,x-user-token")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("预检应 204/200，实际 %d", resp.StatusCode)
	}
	allowOrigin := resp.Header.Get("Access-Control-Allow-Origin")
	if allowOrigin == "" {
		t.Fatalf("预检必须回 Access-Control-Allow-Origin（来源 http://localhost:1027）")
	}
	allowHeaders := strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers"))
	for _, h := range []string{"x-node-id", "x-user-token", "x-api-key", "content-type"} {
		if !strings.Contains(allowHeaders, h) {
			t.Fatalf("Allow-Headers 缺少 %s（面板探活会因预检失败而判离线）：%q", h, allowHeaders)
		}
	}
	t.Logf("[验证] 预检放行：origin=%s headers=%s", allowOrigin, allowHeaders)
}

// TestCompatMSLXStatusCorsOnActualGet 实际 GET 也要带 CORS 头，否则浏览器读不到响应。
func TestCompatMSLXStatusCorsOnActualGet(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	req, err := http.NewRequest(http.MethodGet, base+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:1027")
	req.Header.Set("x-node-id", "node-x")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("探活应 200，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("实际响应缺少 Access-Control-Allow-Origin，浏览器会丢弃响应")
	}
}

// TestCompatMSLXPluginsListEmpty 面板打开「服务端列表」前会拉 /api/plugins/list
// 注册插件页面；该请求失败会让整页显示「目标节点插件加载失败」并挡住实例操作。
// 本节点没有 MSLX 插件，必须返回空列表（200）而不是 501。
func TestCompatMSLXPluginsListEmpty(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)

	// 认证（面板带 JWT）
	token := mslxLogin(t, base)

	for _, path := range []string{"/api/plugins/list", "/api/plugins/list?includeSyncInfo=true", "/api/plugins"} {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-user-token", token)
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200（否则面板整页报插件加载失败），实际 %d %s", path, resp.StatusCode, raw)
		}
		var env struct {
			Code int      `json:"code"`
			Data []string `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s 响应解析失败: %s", path, raw)
		}
		if env.Code != 200 || len(env.Data) != 0 {
			t.Fatalf("%s 应返回空插件列表: %s", path, raw)
		}
	}

	// 写操作必须明确报不支持（而不是静默成功）
	reqAction, _ := http.NewRequest(http.MethodPost, base+"/api/plugins/action", strings.NewReader(`{"id":"x","action":"enable"}`))
	reqAction.Header.Set("Content-Type", "application/json")
	reqAction.Header.Set("x-user-token", token)
	resp, err := testClient.Do(reqAction)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("插件写操作应 501 并给出说明，实际 %d", resp.StatusCode)
	}
}

// TestCompatMSLXCreateServerAndTasks 面板「创建服务端」走的是内置接口（不是插件）：
// POST /api/instance/createServer → {serverId}，随后轮询 GET /api/tasks 看状态。
// 兼容层必须真的把实例建出来，否则用户根本没法创建实例。
func TestCompatMSLXCreateServerAndTasks(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	root, err := os.MkdirTemp("", "irix-mslx-create-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cwd := filepath.Join(root, "myserver")

	body := fmt.Sprintf(`{"name":"测试服","core":"server.jar","path":%q,"java":"java","minM":512,"maxM":2048,"ignoreEula":false}`, cwd)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/instance/createServer", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-user-token", token)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createServer 应 200: %d %s", resp.StatusCode, raw)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			ServerID uint32 `json:"serverId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 200 || env.Data.ServerID == 0 {
		t.Fatalf("createServer 应返回 serverId: %s", raw)
	}

	listReq, _ := http.NewRequest(http.MethodGet, base+"/api/instance/list", nil)
	listReq.Header.Set("x-user-token", token)
	lr, err := testClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	lraw, _ := io.ReadAll(lr.Body)
	lr.Body.Close()
	if !strings.Contains(string(lraw), "测试服") {
		t.Fatalf("实例列表应包含新建实例: %s", lraw)
	}

	// 目录与 eula 已就绪
	if _, err := os.Stat(filepath.Join(cwd, "eula.txt")); err != nil {
		t.Fatalf("应写入 eula.txt: %v", err)
	}

	// 任务列表：创建任务应显示为已完成（state=2），面板据此停止轮询
	deadline := time.Now().Add(8 * time.Second)
	for {
		tr, _ := http.NewRequest(http.MethodGet, base+"/api/tasks", nil)
		tr.Header.Set("x-user-token", token)
		trs, err := testClient.Do(tr)
		if err != nil {
			t.Fatal(err)
		}
		traw, _ := io.ReadAll(trs.Body)
		trs.Body.Close()
		var taskEnv struct {
			Data []struct {
				ID    string `json:"id"`
				State int    `json:"state"`
				Title string `json:"title"`
			} `json:"data"`
		}
		_ = json.Unmarshal(traw, &taskEnv)
		if len(taskEnv.Data) > 0 && taskEnv.Data[0].State == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("创建任务应完成为 state=2: %s", traw)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 删除实例（面板「删除服务端」）
	delBody := fmt.Sprintf(`{"id":%d,"deleteFiles":true}`, env.Data.ServerID)
	dr, _ := http.NewRequest(http.MethodPost, base+"/api/instance/delete", strings.NewReader(delBody))
	dr.Header.Set("Content-Type", "application/json")
	dr.Header.Set("x-user-token", token)
	drs, err := testClient.Do(dr)
	if err != nil {
		t.Fatal(err)
	}
	dres, _ := io.ReadAll(drs.Body)
	drs.Body.Close()
	if drs.StatusCode != http.StatusOK {
		t.Fatalf("delete 应 200: %d %s", drs.StatusCode, dres)
	}
}
