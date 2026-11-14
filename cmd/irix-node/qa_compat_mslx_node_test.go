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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
