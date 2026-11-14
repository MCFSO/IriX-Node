// MCSManager Daemon 兼容层：传输引擎（engine.io v4 + socket.io v4）。
//
// 这里完全按协议文本自行实现，不使用任何第三方 socket.io/engine.io 代码：
//   - engine.io v4：HTTP 轮询（GET 长轮询取包 / POST 送包）+ WebSocket 升级；
//     心跳由服务端下发 ping（"2"），客户端回 pong（"3"）；
//   - socket.io v4：在 engine.io 的 message 包（"4"）之上承载命名空间包
//     （"0" 连接 / "1" 断开 / "2" 事件 / "3" 应答）。
//
// MCSManager 面板侧的调用形态（与 daemon 侧一致）：
//   emit("<event>", {uuid, data})  →  daemon 处理后再用同名事件回包
//   {uuid, status, event, data}，status 200 成功 / 500 失败。
// 推送事件（instance/stdout 等）的 uuid 为 null。

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// engine.io 会话参数（与 socket.io v4 默认值一致）。
const (
	eioPingIntervalMS = 25000
	eioPingTimeoutMS  = 20000
	eioPollHold       = 25 * time.Second // 轮询 GET 最长挂起时间
	eioMaxPayload     = 8 << 20          // 单次轮询 POST 载荷上限
	eioMaxSessions    = 256              // 同时在线会话上限（防资源耗尽）
)

// eioSession 一个 engine.io 会话（先轮询，可升级为 WebSocket）。
type eioSession struct {
	sid     string
	srv     *mcsmCompat
	d       *Daemon
	created time.Time

	mu         sync.Mutex
	ws         *wsConn
	upgraded   bool
	pending    []string      // 等待轮询取走的包
	notify     chan struct{} // 有新包可取的唤醒信号
	closed     bool
	authed     bool                // 是否已通过节点密钥认证
	lastPong   time.Time           // 最近一次收到 pong 的时间
	sioSid     string              // socket.io 层的 sid
	cleanups   []func()            // 会话关闭时执行（取消输出订阅等）
	forwards   map[string]struct{} // 已订阅输出转发的实例
	streamUUID string              // stream/auth 绑定的实例 uuid
}

// newEIOSession 创建会话并登记到兼容层。
func (mc *mcsmCompat) newEIOSession() (*eioSession, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.sessions) >= eioMaxSessions {
		return nil, errors.New("会话数已达上限")
	}
	s := &eioSession{
		sid:      newUUID(),
		srv:      mc,
		d:        mc.d,
		created:  timesys.Now(),
		notify:   make(chan struct{}, 1),
		lastPong: timesys.Now(),
		forwards: map[string]struct{}{},
	}
	mc.sessions[s.sid] = s
	return s, nil
}

// lookupSession 按 sid 取会话。
func (mc *mcsmCompat) lookupSession(sid string) *eioSession {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.sessions[sid]
}

// handler 兼容层的 HTTP 入口（路径已完成前缀剥离）。
func (mc *mcsmCompat) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowOK(r, mc.allow) {
			http.Error(w, "来源不在 compat.mcsm.allowPanelIps 允许范围内", http.StatusForbidden)
			return
		}
		p := r.URL.Path
		switch {
		case p == "/socket.io" || strings.HasPrefix(p, "/socket.io/"):
			mc.handleEngineIO(w, r)
		case strings.HasPrefix(p, "/download/"):
			mc.handleCompatDownload(w, r)
		case strings.HasPrefix(p, "/upload-new/"):
			mc.handleCompatUploadNew(w, r)
		case strings.HasPrefix(p, "/upload-piece/"):
			mc.handleCompatUploadPiece(w, r)
		case p == "/upload" || strings.HasPrefix(p, "/upload/"):
			mc.handleCompatUpload(w, r)
		case p == "/" || p == "":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, mcsmIndexHTML(mc.prefix))
		default:
			http.NotFound(w, r)
		}
	})
}

// mcsmIndexHTML 兼容层首页（占位说明，面板不会读取内容）。
func mcsmIndexHTML(prefix string) string {
	return "<!doctype html><meta charset=\"utf-8\"><title>IriX Node</title>" +
		"<h3>IriX Node — MCSManager Daemon 兼容层</h3>" +
		"<p>该地址供 MCSManager 面板按「节点」连接使用：</p><ul>" +
		"<li>节点地址：<code>http://&lt;本机&gt;:&lt;端口&gt;" + prefix + "</code></li>" +
		"<li>节点密钥：<code>config.json</code> 中 <code>compat.mcsm.key</code></li>" +
		"</ul>"
}

// handleEngineIO engine.io 入口：轮询与 WebSocket 升级。
func (mc *mcsmCompat) handleEngineIO(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("transport") {
	case "polling":
		switch r.Method {
		case http.MethodGet:
			if sid := q.Get("sid"); sid != "" {
				mc.pollingGet(w, r, sid)
				return
			}
			mc.pollingOpen(w, r)
		case http.MethodPost:
			mc.pollingPost(w, r, q.Get("sid"))
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		}
	case "websocket":
		mc.websocketTransport(w, r, q.Get("sid"))
	default:
		http.Error(w, "EIO=4 且 transport 须为 polling 或 websocket", http.StatusBadRequest)
	}
}

// pollingOpen 建立会话并返回 engine.io 握手包。
func (mc *mcsmCompat) pollingOpen(w http.ResponseWriter, r *http.Request) {
	s, err := mc.newEIOSession()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	handshake := map[string]any{
		"sid":          s.sid,
		"upgrades":     []string{"websocket"},
		"pingInterval": eioPingIntervalMS,
		"pingTimeout":  eioPingTimeoutMS,
		"maxPayload":   eioMaxPayload,
	}
	body, _ := json.Marshal(handshake)
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("0" + string(body)))
}

// pollingPost 处理轮询 POST：请求体为一个或多个 engine.io 包（\x1e 分隔）。
func (mc *mcsmCompat) pollingPost(w http.ResponseWriter, r *http.Request, sid string) {
	s := mc.lookupSession(sid)
	if s == nil {
		http.Error(w, "会话不存在或已过期", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, eioMaxPayload))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	for _, pkt := range strings.Split(string(body), "\x1e") {
		if pkt == "" {
			continue
		}
		s.handleEnginePacket(pkt)
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "ok")
}

// pollingGet 长轮询取包：有包立即返回，无包挂起至多 eioPollHold。
func (mc *mcsmCompat) pollingGet(w http.ResponseWriter, r *http.Request, sid string) {
	s := mc.lookupSession(sid)
	if s == nil {
		http.Error(w, "会话不存在或已过期", http.StatusBadRequest)
		return
	}
	packets := s.drainPolling(eioPollHold)
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, strings.Join(packets, "\x1e"))
}

// websocketTransport 处理 WebSocket 传输（含 engine.io 升级探测）。
func (mc *mcsmCompat) websocketTransport(w http.ResponseWriter, r *http.Request, sid string) {
	conn, err := upgradeWS(w, r)
	if err != nil {
		return // upgradeWS 已写出响应
	}
	// 无 sid：直接以 WebSocket 作为首个传输（少见，但协议允许）。
	var s *eioSession
	if sid == "" {
		s, err = mc.newEIOSession()
		if err != nil {
			_ = conn.writeClose(1013, "会话数已达上限")
			_ = conn.Close()
			return
		}
		s.mu.Lock()
		s.ws = conn
		s.upgraded = true
		s.mu.Unlock()
		handshake := map[string]any{
			"sid":          s.sid,
			"upgrades":     []string{},
			"pingInterval": eioPingIntervalMS,
			"pingTimeout":  eioPingTimeoutMS,
			"maxPayload":   eioMaxPayload,
		}
		body, _ := json.Marshal(handshake)
		_ = conn.writeText("0" + string(body))
	} else {
		s = mc.lookupSession(sid)
		if s == nil {
			_ = conn.writeClose(1011, "会话不存在")
			_ = conn.Close()
			return
		}
		s.mu.Lock()
		s.ws = conn
		s.mu.Unlock()
	}
	defer func() {
		s.closeFromTransport(conn)
		_ = conn.Close()
	}()
	mc.wsReadLoop(s, conn)
}

// wsReadLoop WebSocket 读循环：处理探测帧与心跳，直到连接结束。
func (mc *mcsmCompat) wsReadLoop(s *eioSession, conn *wsConn) {
	for {
		opcode, payload, err := conn.readFrame()
		if err != nil {
			return
		}
		switch opcode {
		case wsPing:
			_ = conn.writePong(payload)
			continue
		case wsClose:
			return
		case wsPong:
			s.touch()
			continue
		}
		for _, pkt := range strings.Split(string(payload), "\x1e") {
			if pkt == "" {
				continue
			}
			// engine.io 升级探测：客户端发 "2probe"，服务端回 "3probe"；
			// 随后客户端发 "5" 表示升级完成。
			if pkt == "2probe" {
				_ = conn.writeText("3probe")
				continue
			}
			if pkt == "5" {
				s.mu.Lock()
				s.upgraded = true
				pending := s.pending
				s.pending = nil
				s.mu.Unlock()
				for _, p := range pending {
					_ = conn.writeText(p)
				}
				continue
			}
			s.handleEnginePacket(pkt)
		}
	}
}

// handleEnginePacket 处理单个 engine.io 包（首字节为包类型）。
func (s *eioSession) handleEnginePacket(pkt string) {
	if pkt == "" {
		return
	}
	switch pkt[0] {
	case '2': // ping（客户端发起）
		s.send("3")
	case '3': // pong
		s.touch()
	case '1': // close
		s.close()
	case '4': // message
		s.handleSocketPacket(pkt[1:])
	}
}

// touch 记录心跳时间。
func (s *eioSession) touch() {
	s.mu.Lock()
	s.lastPong = timesys.Now()
	s.mu.Unlock()
}

// send 发送一个 engine.io 包：已升级走 WebSocket，否则进轮询队列。
func (s *eioSession) send(pkt string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	ws := s.ws
	upgraded := s.upgraded
	if !upgraded || ws == nil {
		// 轮询队列有界：慢客户端丢包而不是拖垮节点。
		if len(s.pending) < 4096 {
			s.pending = append(s.pending, pkt)
		}
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
		return
	}
	s.mu.Unlock()
	_ = ws.writeText(pkt)
}

// drainPolling 取走全部待发包；无包时挂起等待，超时返回一个 ping 包。
func (s *eioSession) drainPolling(hold time.Duration) []string {
	deadline := time.NewTimer(hold)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if len(s.pending) > 0 {
			out := s.pending
			s.pending = nil
			s.mu.Unlock()
			return out
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return []string{"1"}
		}
		select {
		case <-s.notify:
			// 有新包，回到循环顶部取走
		case <-deadline.C:
			// 挂起超时：下发 ping 维持连接（客户端回 pong）
			return []string{"2"}
		}
	}
}

// close 关闭会话并执行清理。
func (s *eioSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ws := s.ws
	cleanups := s.cleanups
	s.cleanups = nil
	s.mu.Unlock()

	for _, fn := range cleanups {
		if fn != nil {
			fn()
		}
	}
	if ws != nil {
		_ = ws.writeClose(1000, "")
		_ = ws.Close()
	}
	s.srv.mu.Lock()
	delete(s.srv.sessions, s.sid)
	s.srv.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// closeFromTransport 由传输层结束时调用（WebSocket 断开）。
func (s *eioSession) closeFromTransport(conn *wsConn) {
	s.mu.Lock()
	if s.ws == conn {
		s.ws = nil
		s.upgraded = false
	}
	s.mu.Unlock()
	s.close()
}

// addCleanup 登记会话关闭时的清理函数。
func (s *eioSession) addCleanup(fn func()) {
	s.mu.Lock()
	s.cleanups = append(s.cleanups, fn)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// socket.io 层
// ---------------------------------------------------------------------------

// handleSocketPacket 处理 socket.io 包（engine.io message 包的载荷）。
func (s *eioSession) handleSocketPacket(p string) {
	if p == "" {
		return
	}
	rest := p[1:]
	// 命名空间前缀："/nsp,"
	if strings.HasPrefix(rest, "/") {
		idx := strings.Index(rest, ",")
		if idx < 0 {
			return
		}
		rest = rest[idx+1:]
	}
	switch p[0] {
	case '0': // CONNECT
		s.mu.Lock()
		if s.sioSid == "" {
			s.sioSid = newUUID()
		}
		sioSid := s.sioSid
		s.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"sid": sioSid})
		s.send("4" + "0" + string(body))
	case '1': // DISCONNECT
		s.close()
	case '2': // EVENT
		s.handleSocketEvent(rest)
	case '3': // ACK（面板不使用，忽略）
	default:
		// 二进制事件（5/6）等本层不处理的包类型直接丢弃
	}
}

// handleSocketEvent 解析并分发事件包。
func (s *eioSession) handleSocketEvent(rest string) {
	// 可选应答 id（数字）
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	ackID := rest[:i]
	payload := rest[i:]
	if !strings.HasPrefix(payload, "[") {
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &arr); err != nil || len(arr) == 0 {
		return
	}
	var event string
	if err := json.Unmarshal(arr[0], &event); err != nil || event == "" {
		return
	}
	var data json.RawMessage
	if len(arr) > 1 {
		data = arr[1]
	}
	uuid := ""
	// 请求包形如 {uuid, data}，响应须带回同一个 uuid。
	if len(data) > 0 {
		var head struct {
			UUID string          `json:"uuid"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &head); err == nil && head.UUID != "" {
			uuid = head.UUID
			data = head.Data
		}
	}
	_ = ackID // MCSManager 使用事件回包而非 ACK，这里不额外应答
	s.dispatch(event, uuid, data)
}

// emitPacket 组装并发送一个事件包 {"uuid","status","event","data"}。
func (s *eioSession) emitPacket(event string, uuid any, status int, data any) {
	packet := map[string]any{
		"uuid":   uuid,
		"status": status,
		"event":  event,
		"data":   data,
	}
	body, err := json.Marshal([]any{event, packet})
	if err != nil {
		return
	}
	s.send("42" + string(body))
}

// reply 回包（成功）。
func (s *eioSession) reply(event, uuid string, data any) {
	var id any
	if uuid != "" {
		id = uuid
	}
	s.emitPacket(event, id, 200, data)
}

// replyErr 回包（失败）。数据形如 {err: "..."}，MCSManager 面板据此弹错误。
func (s *eioSession) replyErr(event, uuid string, err error) {
	var id any
	if uuid != "" {
		id = uuid
	}
	s.emitPacket(event, id, 500, map[string]any{"err": err.Error()})
}

// push 向本会话推送事件（uuid 为 null）。
func (s *eioSession) push(event string, data any) {
	s.emitPacket(event, nil, 200, data)
}

// broadcast 向全部已认证会话推送事件。
func (mc *mcsmCompat) broadcast(event string, data any) {
	mc.mu.Lock()
	sessions := make([]*eioSession, 0, len(mc.sessions))
	for _, s := range mc.sessions {
		sessions = append(sessions, s)
	}
	mc.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		authed := s.authed
		s.mu.Unlock()
		if authed {
			s.push(event, data)
		}
	}
}

// rawData 取原始 JSON 载荷（无载荷时返回空对象）。
func rawData(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("{}")
	}
	return data
}

// decode 把载荷解到目标结构（允许空载荷）。
func decode(data json.RawMessage, out any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// datasString 把载荷当作纯字符串解析（auth 与 stream/auth 的载荷是裸字符串）。
func datasString(data json.RawMessage) string {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return s
	}
	// 兜底：去掉首尾引号
	return strings.Trim(string(data), "\"")
}

// atoiOr 解析整数，失败时返回默认值。
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
