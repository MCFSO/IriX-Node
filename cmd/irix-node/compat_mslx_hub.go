// MSLX Daemon 兼容层：SignalR 通道（控制台 / 系统监视）。
//
// 只实现 SignalR JSON 协议里本场景必需的部分（自研实现，未使用任何 SignalR 库）：
//   - POST /api/hubs/{hub}/negotiate → {connectionToken, availableTransports}
//   - GET  /api/hubs/{hub}?id={token}（WebSocket 升级）
//   - 握手 {"protocol":"json","version":1}␞ → 应答 {}␞
//   - 调用 {"type":1,"invocationId","target","arguments"}␞
//   - 结果 {"type":3,"invocationId",...}␞ / 服务端推送 {"type":1,"target","arguments"}␞
//   - 心跳 {"type":6}␞
// 消息分隔符为 0x1e（记录分隔符）。

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"irix/node/internal/sysinfo"
	"irix/node/internal/timesys"
)

// SignalR 记录分隔符。
const srRecordSep = "\x1e"

// srPingInterval 服务端心跳间隔。
const srPingInterval = 15 * time.Second

// mslxHubTicket negotiate 发放的一次性连接凭据。
type mslxHubTicket struct {
	hub     string
	claims  *mslxClaims
	expires time.Time
}

// mslxHubConn 一条已建立的 SignalR 连接。
type mslxHubConn struct {
	id     string
	hub    string
	claims *mslxClaims
	srv    *mslxCompat
	ws     *wsConn

	mu         sync.Mutex
	closed     bool
	console    map[uint32]func() // 实例 id → 取消订阅
	monitoring bool
	stopPing   chan struct{}
}

// SignalR JSON 消息（本层用到的字段）。
type srMessage struct {
	Type         int               `json:"type"`
	InvocationID string            `json:"invocationId,omitempty"`
	Target       string            `json:"target,omitempty"`
	Arguments    []json.RawMessage `json:"arguments,omitempty"`
	Error        string            `json:"error,omitempty"`
	Result       any               `json:"result,omitempty"`
}

// handleHub 处理 /api/hubs/... 下的请求。
func (ms *mslxCompat) handleHub(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 || segs[0] == "" {
		http.NotFound(w, r)
		return
	}
	hub := segs[0]
	// negotiate
	if len(segs) == 2 && segs[1] == "negotiate" {
		ms.negotiateHub(w, r, hub)
		return
	}
	if len(segs) != 1 {
		http.NotFound(w, r)
		return
	}
	ms.connectHub(w, r, hub)
}

// negotiateHub 发放连接凭据。
func (ms *mslxCompat) negotiateHub(w http.ResponseWriter, r *http.Request, hub string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	claims, ok := ms.authenticate(r)
	if !ok {
		mslxFail(w, http.StatusUnauthorized, "未登录或登录已过期，请重新登录")
		return
	}
	token := newUUID()
	ms.mu.Lock()
	if ms.hubTickets == nil {
		ms.hubTickets = map[string]*mslxHubTicket{}
	}
	// 顺手清理过期凭据，避免无界增长
	now := timesys.Now()
	for k, v := range ms.hubTickets {
		if now.After(v.expires) {
			delete(ms.hubTickets, k)
		}
	}
	ms.hubTickets[token] = &mslxHubTicket{hub: hub, claims: claims, expires: now.Add(2 * time.Minute)}
	ms.mu.Unlock()

	writeCompatJSON(w, http.StatusOK, map[string]any{
		"negotiateVersion": 1,
		"connectionId":     token,
		"connectionToken":  token,
		"availableTransports": []any{
			map[string]any{
				"transport":       "WebSockets",
				"transferFormats": []string{"Text"},
			},
		},
	})
}

// connectHub 建立 WebSocket 连接并处理调用。
func (ms *mslxCompat) connectHub(w http.ResponseWriter, r *http.Request, hub string) {
	// 认证：优先查询参数里的凭据（浏览器 WebSocket 无法自定义头）
	token := r.URL.Query().Get("id")
	if token == "" {
		token = r.URL.Query().Get("connectionToken")
	}
	ms.mu.Lock()
	ticket := ms.hubTickets[token]
	if ticket != nil && (ticket.hub != hub || timesys.Now().After(ticket.expires)) {
		ticket = nil
	}
	if ticket != nil {
		delete(ms.hubTickets, token)
	}
	ms.mu.Unlock()

	var claims *mslxClaims
	if ticket != nil {
		claims = ticket.claims
	} else if c, ok := ms.authenticate(r); ok {
		claims = c
	}
	if claims == nil {
		mslxFail(w, http.StatusUnauthorized, "连接凭据无效或已过期")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		// 非 WebSocket 传输（长轮询/SSE）本层不支持，明确拒绝
		mslxFail(w, http.StatusNotImplemented, "irix-node 兼容层仅支持 WebSocket 传输")
		return
	}
	conn, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	hc := &mslxHubConn{
		id:       newUUID(),
		hub:      hub,
		claims:   claims,
		srv:      ms,
		ws:       conn,
		console:  map[uint32]func(){},
		stopPing: make(chan struct{}),
	}
	defer func() {
		hc.shutdown()
		_ = conn.writeClose(1000, "")
		_ = conn.Close()
	}()
	hc.run()
}

// send 发送一条 SignalR 消息。
func (c *mslxHubConn) send(msg srMessage) {
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = c.ws.writeText(string(body) + srRecordSep)
}

// sendInvocation 服务端 → 客户端调用。
func (c *mslxHubConn) sendInvocation(target string, args ...any) {
	raw := make([]json.RawMessage, 0, len(args))
	for _, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		raw = append(raw, b)
	}
	c.send(srMessage{Type: 1, Target: target, Arguments: raw})
}

// complete 回复调用完成。
func (c *mslxHubConn) complete(invocationID string, result any, errMsg string) {
	if invocationID == "" {
		return
	}
	c.send(srMessage{Type: 3, InvocationID: invocationID, Result: result, Error: errMsg})
}

// run 读循环：握手 → 调用 → 心跳。
func (c *mslxHubConn) run() {
	// 握手包
	_, payload, err := c.ws.readFrame()
	if err != nil {
		return
	}
	for _, rec := range strings.Split(string(payload), srRecordSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		var hs struct {
			Protocol string `json:"protocol"`
			Version  int    `json:"version"`
		}
		if err := json.Unmarshal([]byte(rec), &hs); err != nil || hs.Protocol != "json" {
			_ = c.ws.writeText(`{"error":"仅支持 JSON 协议"}` + srRecordSep)
			return
		}
		// 空对象表示握手成功
		_ = c.ws.writeText("{}" + srRecordSep)
		break
	}
	go c.pingLoop()

	for {
		opcode, payload, err := c.ws.readFrame()
		if err != nil {
			return
		}
		if opcode == wsPing {
			_ = c.ws.writePong(payload)
			continue
		}
		if opcode == wsClose {
			return
		}
		for _, rec := range strings.Split(string(payload), srRecordSep) {
			if strings.TrimSpace(rec) == "" {
				continue
			}
			var msg srMessage
			if err := json.Unmarshal([]byte(rec), &msg); err != nil {
				continue
			}
			switch msg.Type {
			case 1: // 调用
				c.handleInvocation(msg)
			case 6: // ping
				c.send(srMessage{Type: 6})
			case 7: // close
				return
			}
		}
	}
}

// pingLoop 周期性下发心跳。
func (c *mslxHubConn) pingLoop() {
	defer recoverLog("MSLX 兼容层 SignalR 心跳")
	t := time.NewTicker(srPingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stopPing:
			return
		case <-t.C:
			c.send(srMessage{Type: 6})
		}
	}
}

// shutdown 关闭连接并取消全部订阅。
func (c *mslxHubConn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	cancels := make([]func(), 0, len(c.console))
	for _, cancel := range c.console {
		cancels = append(cancels, cancel)
	}
	c.console = map[uint32]func(){}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	select {
	case <-c.stopPing:
	default:
		close(c.stopPing)
	}
}

// handleInvocation 处理客户端调用。
func (c *mslxHubConn) handleInvocation(msg srMessage) {
	if c.hub == "system" || c.hub == "SystemMonitorHub" {
		switch msg.Target {
		case "JoinMonitor":
			c.startMonitor()
			c.complete(msg.InvocationID, nil, "")
		case "LeaveMonitor":
			c.stopMonitor()
			c.complete(msg.InvocationID, nil, "")
		default:
			c.complete(msg.InvocationID, nil, "未知的 Hub 方法: "+msg.Target)
		}
		return
	}
	// 默认按控制台 Hub 处理
	instanceID, argsErr := c.hubInstanceArg(msg.Arguments, 0)
	switch msg.Target {
	case "JoinGroup", "JoinPtyGroup":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		c.subscribeConsole(instanceID)
		c.sendInvocation("PtyStatus", map[string]any{
			"isPty":     false,
			"isRunning": c.instanceRunning(instanceID),
			"enablePty": false,
		})
		c.complete(msg.InvocationID, nil, "")
	case "LeaveGroup", "LeavePtyGroup":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		c.unsubscribeConsole(instanceID)
		c.complete(msg.InvocationID, nil, "")
	case "SendCommand":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		command, cmdErr := hubStringArg(msg.Arguments, 1)
		if cmdErr != nil || strings.TrimSpace(command) == "" {
			c.sendInvocation("CommandResult", map[string]any{"success": false, "message": "命令不能为空"})
			c.complete(msg.InvocationID, nil, "")
			return
		}
		// 与 MCSM 兼容层一致：^c / 0x03 等按 Ctrl+C（停止实例）处理，
		// 不能当命令写进标准输入（服务端会报 Unknown or incomplete command ^c）。
		if isConsoleInterrupt(command) {
			if inst := c.instanceByHubID(instanceID); inst != nil {
				alog.Printf("兼容层：控制台收到 Ctrl+C，按停止处理（实例 %s）", inst.Config.Nickname)
				safeGo("MSLX 兼容层控制台中断", func() {
					if err := c.srv.d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
						alog.Printf("MSLX 兼容层：Ctrl+C 停止实例失败（%s）：%v", inst.Config.Nickname, err)
					}
				})
				c.sendInvocation("ReceiveLog", "[irix-node] 收到 Ctrl+C：已向实例发送停止指令\r\n")
			}
			c.sendInvocation("CommandResult", map[string]any{"success": true, "message": "已按 Ctrl+C 停止实例"})
			c.complete(msg.InvocationID, nil, "")
			return
		}
		ok, errText := c.sendCommand(instanceID, command)
		c.sendInvocation("CommandResult", map[string]any{"success": ok, "message": errText})
		c.complete(msg.InvocationID, nil, "")
	case "SendPtyInput":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		if data, err := hubStringArg(msg.Arguments, 1); err == nil {
			clean := strings.Trim(data, "\r\n")
			if clean != "" {
				_, _ = c.sendCommand(instanceID, clean)
			}
		}
		c.complete(msg.InvocationID, nil, "")
	case "ResizePty", "GetPtyStatus":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		c.sendInvocation("PtyStatus", map[string]any{
			"isPty":     false,
			"isRunning": c.instanceRunning(instanceID),
			"enablePty": false,
		})
		c.complete(msg.InvocationID, nil, "")
	case "ResumeConsole":
		if argsErr != nil {
			c.complete(msg.InvocationID, nil, argsErr.Error())
			return
		}
		c.subscribeConsole(instanceID)
		c.complete(msg.InvocationID, map[string]any{
			"log": c.consoleReplay(instanceID),
			"pty": nil,
			"status": map[string]any{
				"isPty":     false,
				"isRunning": c.instanceRunning(instanceID),
				"enablePty": false,
			},
		}, "")
	default:
		c.complete(msg.InvocationID, nil, "未知的 Hub 方法: "+msg.Target)
	}
}

// hubInstanceArg 取第 n 个参数并解析为实例 id。
func (c *mslxHubConn) hubInstanceArg(args []json.RawMessage, n int) (uint32, error) {
	if len(args) <= n {
		return 0, fmt.Errorf("缺少实例 id 参数")
	}
	var raw any
	if err := json.Unmarshal(args[n], &raw); err != nil {
		return 0, fmt.Errorf("实例 id 参数格式错误")
	}
	switch v := raw.(type) {
	case float64:
		return uint32(v), nil
	case string:
		var id uint32
		if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
			return 0, fmt.Errorf("实例 id 参数格式错误")
		}
		return id, nil
	default:
		return 0, fmt.Errorf("实例 id 参数格式错误")
	}
}

// hubStringArg 取第 n 个字符串参数。
func hubStringArg(args []json.RawMessage, n int) (string, error) {
	if len(args) <= n {
		return "", fmt.Errorf("缺少参数")
	}
	var s string
	if err := json.Unmarshal(args[n], &s); err != nil {
		return "", err
	}
	return s, nil
}

// instanceByHubID 按整数 id 取实例。
func (c *mslxHubConn) instanceByHubID(id uint32) *Instance {
	uuid, ok := c.srv.uuidForID(id)
	if !ok {
		return nil
	}
	return c.srv.d.Find(uuid)
}

// instanceRunning 实例是否在运行。
func (c *mslxHubConn) instanceRunning(id uint32) bool {
	inst := c.instanceByHubID(id)
	if inst == nil {
		return false
	}
	inst.mu.Lock()
	status := inst.Status
	inst.mu.Unlock()
	return status == StatusRunning
}

// subscribeConsole 订阅实例输出并推给客户端。
func (c *mslxHubConn) subscribeConsole(id uint32) {
	c.mu.Lock()
	if _, ok := c.console[id]; ok {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	inst := c.instanceByHubID(id)
	if inst == nil {
		return
	}
	// 与 MCSM 兼容层一致：控制台先打开、实例后启动时也要能自动接上实时输出。
	// （面板不会因为实例状态变化重新 JoinPtyGroup，所以这里必须守着等进程出现。）
	stop := make(chan struct{})
	c.mu.Lock()
	c.console[id] = func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	c.mu.Unlock()
	safeGo("MSLX 兼容层控制台推送", func() {
		defer recoverLog("MSLX 兼容层控制台推送")
		for {
			select {
			case <-stop:
				return
			default:
			}
			inst.mu.Lock()
			proc := inst.Proc
			inst.mu.Unlock()
			if proc == nil || !proc.IsRunning() {
				select {
				case <-stop:
					return
				case <-time.After(mcOutputForwardPoll):
				}
				continue
			}
			ch, cancel := proc.Subscribe()
			procStop := make(chan struct{})
			procDone := proc.done
			go func() {
				defer recoverLog("MSLX 兼容层控制台回落")
				select {
				case <-procDone:
				case <-stop:
				}
				close(procStop)
			}()
			// 与 MCSM 兼容层一致：按 30ms 窗口合并输出，避免高输出速率把面板冲垮
			mcPumpOutputSelect(ch, procStop, func(text string) { c.sendInvocation("ReceiveLog", text) })
			cancel()
			select {
			case <-stop:
				return
			case <-procDone:
			}
			c.sendInvocation("ReceiveStatus", map[string]any{
				"id":     id,
				"status": StatusStopped,
				"text":   "stopped",
			})
		}
	})
}

// unsubscribeConsole 取消订阅。
func (c *mslxHubConn) unsubscribeConsole(id uint32) {
	c.mu.Lock()
	cancel, ok := c.console[id]
	delete(c.console, id)
	c.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

// consoleReplay 返回实例当前日志尾部（供 ResumeConsole 回放）。
func (c *mslxHubConn) consoleReplay(id uint32) string {
	inst := c.instanceByHubID(id)
	if inst == nil {
		return ""
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil {
		return ""
	}
	return proc.Log.Tail(0)
}

// sendCommand 向实例下发命令。
func (c *mslxHubConn) sendCommand(id uint32, command string) (bool, string) {
	inst := c.instanceByHubID(id)
	if inst == nil {
		return false, "未找到实例资源"
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		return false, "发送失败，服务器可能未运行"
	}
	if err := proc.WriteCommand(strings.TrimRight(command, "\r\n")); err != nil {
		return false, "发送失败: " + err.Error()
	}
	return true, "命令已发送"
}

// startMonitor 启动系统监视推送。
func (c *mslxHubConn) startMonitor() {
	c.mu.Lock()
	if c.monitoring {
		c.mu.Unlock()
		return
	}
	c.monitoring = true
	c.mu.Unlock()
	safeGo("MSLX 兼容层系统监视", func() {
		defer recoverLog("MSLX 兼容层系统监视")
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			c.mu.Lock()
			stop := c.closed || !c.monitoring
			c.mu.Unlock()
			if stop {
				return
			}
			c.sendInvocation("ReceiveSystemStats", map[string]any{
				"local":  c.systemStats(),
				"slaves": map[string]any{},
			})
			select {
			case <-c.stopPing:
				return
			case <-t.C:
			}
		}
	})
}

// stopMonitor 停止系统监视推送。
func (c *mslxHubConn) stopMonitor() {
	c.mu.Lock()
	c.monitoring = false
	c.mu.Unlock()
}

// systemStats 采集一份主机指标（字段名保持通用，面板按需展示）。
func (c *mslxHubConn) systemStats() map[string]any {
	d := c.srv.d
	diskTotal, diskUsed, diskUsage := sysinfo.CachedDiskInfo(d.DataDir)
	memTotal, memFree, memUse := sysinfo.MemSnapshot()
	netDown, netUp := cachedNetRates()
	return map[string]any{
		"time":            timesys.Now().UnixMilli(),
		"cpuUsage":        sysinfo.CachedSysCPUUsage(),
		"memUsage":        memUse,
		"totalMem":        memTotal,
		"freeMem":         memFree,
		"diskUsage":       diskUsage,
		"totalDisk":       diskTotal,
		"usedDisk":        diskUsed,
		"networkUp":       netUp,
		"networkDown":     netDown,
		"uptime":          sysinfo.UptimeSeconds(),
		"instanceTotal":   len(d.mcAllInstanceDetails()),
		"instanceRunning": d.CountRunning(),
	}
}
