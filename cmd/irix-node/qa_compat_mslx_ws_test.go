// MSLX 兼容层 SignalR 控制台端到端测试（WebSocket）。
//
// 验证：negotiate → WebSocket 握手 → SignalR 握手 → JoinGroup →
// ReceiveLog 推送 → SendCommand 应答（复用 qa_console_test.go 的 WS 测试客户端）。

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hubReader SignalR 记录读取器（0x1e 分隔，支持跨帧拼接）。
type hubReader struct {
	t   *testing.T
	ws  *wsTestClient
	buf string
}

// next 读取下一条 SignalR 记录。
func (r *hubReader) next(timeout time.Duration) map[string]any {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if idx := strings.Index(r.buf, srRecordSep); idx >= 0 {
			rec := r.buf[:idx]
			r.buf = r.buf[idx+1:]
			if strings.TrimSpace(rec) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(rec), &m); err != nil {
				continue
			}
			return m
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("等待 SignalR 记录超时（已缓存 %q）", r.buf)
		}
		_, payload := r.ws.readFrame(5 * time.Second)
		r.buf += string(payload)
	}
}

// waitTarget 等待服务端推送指定 target，返回该记录的 arguments。
func (r *hubReader) waitTarget(target string, timeout time.Duration) []any {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec := r.next(time.Until(deadline))
		if rec["type"] != float64(1) || rec["target"] != target {
			continue
		}
		args, _ := rec["arguments"].([]any)
		return args
	}
	r.t.Fatalf("未收到 %s 推送", target)
	return nil
}

// TestCompatMSLXHubConsole SignalR 控制台：订阅、日志推送与命令下发。
func TestCompatMSLXHubConsole(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd := filepath.Join(worldDir(t), "srv-mslx-ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })
	instID := d.compatMSLX.idFor(inst.InstanceUuid)

	// negotiate 拿一次性连接凭据
	status, resp := mslxDo(t, http.MethodPost,
		base+"/api/hubs/instanceControlHub/negotiate?negotiateVersion=1", token, nil)
	if status != http.StatusOK {
		t.Fatalf("协商失败: %d %s", status, resp)
	}
	var neg struct {
		ConnectionToken string `json:"connectionToken"`
	}
	if err := json.Unmarshal(resp, &neg); err != nil || neg.ConnectionToken == "" {
		t.Fatalf("协商响应异常: %s", resp)
	}

	ws := dialTestWS(t, base+"/api/hubs/instanceControlHub?id="+url.QueryEscape(neg.ConnectionToken))
	defer ws.conn.Close()
	rd := &hubReader{t: t, ws: ws}

	// SignalR 握手
	ws.sendText(`{"protocol":"json","version":1}` + srRecordSep)
	if rec := rd.next(5 * time.Second); rec["error"] != nil {
		t.Fatalf("SignalR 握手返回错误: %v", rec["error"])
	}

	// 加入控制台分组
	join, _ := json.Marshal(map[string]any{
		"type": 1, "invocationId": "1", "target": "JoinGroup", "arguments": []any{instID},
	})
	ws.sendText(string(join) + srRecordSep)
	rd.waitTarget("PtyStatus", 5*time.Second)

	// 运行中的 ping -t 会持续输出，应收到 ReceiveLog
	rd.waitTarget("ReceiveLog", 8*time.Second)

	// 下发命令并校验应答
	cmd := `{"type":1,"invocationId":"2","target":"SendCommand","arguments":[` +
		itoa(int(instID)) + `,"say hi"]}`
	if len(cmd) >= 126 {
		t.Fatalf("测试命令过长（%d 字节），WS 测试客户端只支持 <126", len(cmd))
	}
	ws.sendText(cmd + srRecordSep)
	args := rd.waitTarget("CommandResult", 8*time.Second)
	if len(args) == 0 {
		t.Fatal("CommandResult 缺少参数")
	}
	payload, _ := args[0].(map[string]any)
	if payload["success"] != true {
		t.Fatalf("命令下发应成功: %v", payload)
	}
}

// TestCompatMSLXHubSystem SignalR 系统监视：JoinMonitor 后收到统计推送。
func TestCompatMSLXHubSystem(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	status, resp := mslxDo(t, http.MethodPost, base+"/api/hubs/system/negotiate?negotiateVersion=1", token, nil)
	if status != http.StatusOK {
		t.Fatalf("协商失败: %d %s", status, resp)
	}
	var neg struct {
		ConnectionToken string `json:"connectionToken"`
	}
	if err := json.Unmarshal(resp, &neg); err != nil || neg.ConnectionToken == "" {
		t.Fatalf("协商响应异常: %s", resp)
	}
	ws := dialTestWS(t, base+"/api/hubs/system?id="+url.QueryEscape(neg.ConnectionToken))
	defer ws.conn.Close()
	rd := &hubReader{t: t, ws: ws}

	ws.sendText(`{"protocol":"json","version":1}` + srRecordSep)
	if rec := rd.next(5 * time.Second); rec["error"] != nil {
		t.Fatalf("SignalR 握手返回错误: %v", rec["error"])
	}
	ws.sendText(`{"type":1,"invocationId":"1","target":"JoinMonitor","arguments":[]}` + srRecordSep)
	args := rd.waitTarget("ReceiveSystemStats", 10*time.Second)
	if len(args) == 0 {
		t.Fatal("ReceiveSystemStats 缺少参数")
	}
	payload, _ := args[0].(map[string]any)
	if _, ok := payload["local"]; !ok {
		t.Fatalf("系统统计缺少 local 段: %v", payload)
	}
}
