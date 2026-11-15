// MSLX 兼容层 SignalR 控制台端到端测试（WebSocket）。
//
// 验证：negotiate → WebSocket 握手 → SignalR 握手 → JoinGroup →
// ReceiveLog 推送 → SendCommand 应答（复用 qa_console_test.go 的 WS 测试客户端）。

package main

import (
	"encoding/json"
	"fmt"
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

// TestMSLXHubAsyncFraming SignalR 出站批量合并必须保持记录分隔（每条以 0x1e 结尾），
// 且读循环不被慢调用阻塞。
func TestMSLXHubAsyncFraming(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	status, resp := mslxDo(t, http.MethodPost,
		base+"/api/hubs/system/negotiate?negotiateVersion=1", token, nil)
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

	ws.sendText(`{"protocol":"json","version":1}` + srRecordSep)
	// 一次发 3 条调用：应答会被批量合并进同一个帧，客户端必须逐条解出
	for i := 1; i <= 3; i++ {
		ws.sendText(fmt.Sprintf(`{"type":1,"invocationId":"c%d","target":"JoinMonitor"}`, i) + srRecordSep)
	}

	rd := &hubReader{t: t, ws: ws}
	seen := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < 3 && time.Now().Before(deadline) {
		rec := rd.next(time.Until(deadline))
		if rec["type"] == float64(3) {
			if id, ok := rec["invocationId"].(string); ok {
				seen[id] = true
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("应收到 3 条调用应答（帧内多记录），实际 %v", seen)
	}
}

// TestMSLXHubInvocationQueuedAsync 读循环只入队：慢调用不会阻塞后续调用。
func TestMSLXHubInvocationQueuedAsync(t *testing.T) {
	d, _ := newTestDaemon(t)
	setupTestMSLX(t, d)
	ms := d.compatMSLX

	c := &mslxHubConn{
		id:       "test-hub",
		hub:      "instanceControlHub",
		srv:      ms,
		jobs:     newMCJobQueue(),
		stopPing: make(chan struct{}),
		console:  map[uint32]func(){},
	}
	c.outbox = newWSOutboxFramed(nil, []byte(srRecordSep), []byte(srRecordSep))
	c.startWorkers()
	t.Cleanup(func() {
		c.jobs.close()
		close(c.stopPing)
	})

	// 入队 200 条调用的耗时应远小于执行耗时（读循环只做解析 + 入队）
	start := time.Now()
	for i := 0; i < 200; i++ {
		c.jobs.push(mcJob{event: "JoinMonitor", uuid: "x", data: []byte(`{"type":1,"invocationId":"x","target":"JoinMonitor"}`)})
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("入队 200 条调用耗时 %s，读循环不应同步执行", elapsed)
	}
	if c.jobs.length() == 0 && c.jobs.dropped.Load() == 0 {
		// 工作协程可能已经全部取走，属正常
		t.Log("调用已被工作协程取空")
	}
}

// TestMSLXConsoleCursorProtocol 面板 v1.7.6+ 的控制台游标协议：
// ResumeConsole 必须返回 ConsoleReplay 对象（streamId/sequence/gap/frames），
// 且实时 ReceiveLog 必须带 cursor —— 早先返回字符串 / 不带游标，前端会报
// 「Cannot read properties of undefined (reading length)」并丢掉所有输出帧。
func TestMSLXConsoleCursorProtocol(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd := filepath.Join(worldDir(t), "srv-mslx-cursor")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	inst.Config.StopCommand = "stop"
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })
	instID := d.compatMSLX.idFor(inst.InstanceUuid)

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
	ws.sendText(`{"protocol":"json","version":1}` + srRecordSep)
	rd.next(5 * time.Second)

	// ResumeConsole（游标为 null，模拟首次进入）
	resume, _ := json.Marshal(map[string]any{
		"type": 1, "invocationId": "r1", "target": "ResumeConsole",
		"arguments": []any{instID, nil, nil, false, 80, 24},
	})
	ws.sendText(string(resume) + srRecordSep)

	var result map[string]any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && result == nil {
		rec := rd.next(time.Until(deadline))
		if rec["type"] == float64(3) && rec["invocationId"] == "r1" {
			result, _ = rec["result"].(map[string]any)
		}
	}
	if result == nil {
		t.Fatal("ResumeConsole 没有回包")
	}
	log, ok := result["log"].(map[string]any)
	if !ok {
		t.Fatalf("ResumeConsole.log 必须是对象（ConsoleReplay），实际 %T：%v", result["log"], result["log"])
	}
	for _, field := range []string{"streamId", "sequence", "gap", "frames"} {
		if _, ok := log[field]; !ok {
			t.Fatalf("ConsoleReplay 缺少字段 %s: %v", field, log)
		}
	}
	if _, ok := log["frames"].([]any); !ok {
		t.Fatalf("frames 必须是数组: %v", log["frames"])
	}
	statusObj, _ := result["status"].(map[string]any)
	if statusObj == nil || statusObj["isRunning"] != true {
		t.Fatalf("status.isRunning 应为 true: %v", result["status"])
	}

	// 实时输出必须带 cursor（第二个参数是 {streamId, sequence}）
	logArgs := rd.waitTarget("ReceiveLog", 8*time.Second)
	if len(logArgs) < 2 {
		t.Fatalf("ReceiveLog 必须带 cursor 参数（否则前端丢弃），实际只有 %d 个参数", len(logArgs))
	}
	cursor, ok := logArgs[1].(map[string]any)
	if !ok {
		t.Fatalf("ReceiveLog 第二个参数应为 cursor 对象，实际 %T", logArgs[1])
	}
	if _, ok := cursor["streamId"].(string); !ok {
		t.Fatalf("cursor.streamId 缺失: %v", cursor)
	}
	if _, ok := cursor["sequence"].(float64); !ok {
		t.Fatalf("cursor.sequence 缺失: %v", cursor)
	}

	// 带同一游标再 Resume：应回放“之后”的帧且不报 gap
	resume2, _ := json.Marshal(map[string]any{
		"type": 1, "invocationId": "r2", "target": "ResumeConsole",
		"arguments": []any{instID, map[string]any{"streamId": cursor["streamId"], "sequence": cursor["sequence"]}, nil, false, 80, 24},
	})
	ws.sendText(string(resume2) + srRecordSep)
	deadline = time.Now().Add(8 * time.Second)
	var got2 map[string]any
	for time.Now().Before(deadline) && got2 == nil {
		rec := rd.next(time.Until(deadline))
		if rec["type"] == float64(3) && rec["invocationId"] == "r2" {
			got2, _ = rec["result"].(map[string]any)
		}
	}
	if got2 == nil {
		t.Fatal("第二次 ResumeConsole 没有回包")
	}
	log2, _ := got2["log"].(map[string]any)
	if log2 == nil {
		t.Fatalf("第二次回放缺少 log: %v", got2)
	}
	if log2["gap"] == true {
		t.Fatalf("游标连续时不应报 gap: %v", log2)
	}
}
