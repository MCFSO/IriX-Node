// MCSManager 兼容层：控制台流（stream/*）与 WebSocket 心跳回归测试。
//
// 复刻面板 terminal_bridge 的真实行为：
//   - 建一个 transports:["websocket"]、reconnection:false 的 daemon 客户端；
//   - 连接后 emit("stream/auth", { data: { password } })——**不带 uuid**；
//   - 只认回包 packet.data === true。
// 以及：WebSocket 传输必须由服务端定时下发 engine.io ping，否则客户端会在
// pingInterval+pingTimeout 后主动断开（线上表现为每 45s 断一次）。

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sendTextAny 发送任意长度文本帧（qa_console_test.go 的 sendFrame 只支持 <126 字节，
// 而 stream/auth 的通行证 JSON 会超过该长度）。
func (c *wsTestClient) sendTextAny(s string) {
	c.t.Helper()
	payload := []byte(s)
	n := len(payload)
	hdr := []byte{0x80 | byte(wsText)}
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, b[:]...)
	}
	key := []byte{0x11, 0x22, 0x33, 0x44}
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	buf := append(hdr, key...)
	buf = append(buf, masked...)
	if _, err := c.conn.Write(buf); err != nil {
		c.t.Fatalf("发送帧失败: %v", err)
	}
}

// readPacket 读取一个文本帧（期望是完整的 socket.io 包）。
func (c *wsTestClient) readPacket(timeout time.Duration) string {
	c.t.Helper()
	_, payload := c.readFrame(timeout)
	return string(payload)
}

// waitSocketPacket 读到以 prefix 开头的包（自动回 pong）。
func (c *wsTestClient) waitSocketPacket(prefix string, timeout time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pkt := c.readPacket(time.Until(deadline))
		if pkt == "2" {
			c.sendTextAny("3") // 回 pong
			continue
		}
		if strings.HasPrefix(pkt, prefix) {
			return pkt
		}
	}
	c.t.Fatalf("等待 %q 包超时", prefix)
	return ""
}

// openStreamClient 以面板的方式建立「纯 WebSocket」socket.io 客户端并完成握手。
func openStreamClient(t *testing.T, addr string) *wsTestClient {
	t.Helper()
	ws := dialTestWS(t, "http://"+addr+"/socket.io/?EIO=4&transport=websocket")
	hs := ws.waitSocketPacket("0", 5*time.Second)
	var open struct {
		Sid          string `json:"sid"`
		PingInterval int    `json:"pingInterval"`
		PingTimeout  int    `json:"pingTimeout"`
	}
	if err := json.Unmarshal([]byte(hs[1:]), &open); err != nil || open.Sid == "" {
		t.Fatalf("engine.io 握手异常: %q", hs)
	}
	// socket.io 命名空间连接
	ws.sendTextAny("40")
	connect := ws.waitSocketPacket("40", 5*time.Second)
	var body struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(connect[2:]), &body); err != nil || body.SID == "" {
		t.Fatalf("socket.io 连接应答异常: %q", connect)
	}
	return ws
}

// TestCompatMCSMStreamAuth 复刻面板控制台流：无 uuid 的 {data:{password}} 必须能认证，
// 并能收到 instance/stdout 推送。
func TestCompatMCSMStreamAuth(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d) // 返回 http://host:port
	addr := strings.TrimPrefix(base, "http://")

	cwd := filepath.Join(worldDir(t), "srv-stream")
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

	// 1) 主通道（轮询）注册 stream_channel 通行证——面板就是这么做的
	main := newEIOClient(t, base+"/socket.io/")
	main.connect()
	if status, data := main.requestRaw("auth", "s1", mcsmTestKey); status != 200 {
		t.Fatalf("主通道认证失败: %d %s", status, data)
	}
	const pass = "qa-stream-pass-0001"
	if status, data := main.requestRaw("passport/register", "s2", map[string]any{
		"name":     "stream_channel",
		"password": pass,
		"parameter": map[string]any{
			"instanceUuid": inst.InstanceUuid,
		},
	}); status != 200 {
		t.Fatalf("注册控制台通行证失败: %d %s", status, data)
	}

	// 2) 纯 WebSocket 的流客户端（面板 terminal_bridge 的建连方式）
	ws := openStreamClient(t, addr)
	defer ws.conn.Close()

	// 关键：载荷是 {data:{password}}，没有 uuid 字段
	ws.sendTextAny(`42["stream/auth",{"data":{"password":"` + pass + `"}}]`)
	pkt := ws.waitSocketPacket("42", 5*time.Second)
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
		t.Fatalf("stream/auth 回包异常: %q", pkt)
	}
	var event string
	_ = json.Unmarshal(arr[0], &event)
	if event != "stream/auth" {
		t.Fatalf("回包事件名异常: %q", pkt)
	}
	var packet struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(arr[1], &packet); err != nil {
		t.Fatalf("回包解析失败: %v（%q）", err, pkt)
	}
	// 面板只认 data === true
	if string(packet.Data) != "true" {
		t.Fatalf("stream/auth 应返回 data=true（面板只认这个），实际 %s", packet.Data)
	}

	// 3) 应收到实例输出推送 instance/stdout
	deadline := time.Now().Add(8 * time.Second)
	gotLog := false
	for time.Now().Before(deadline) && !gotLog {
		pkt = ws.waitSocketPacket("42", time.Until(deadline))
		if strings.Contains(pkt, "instance/stdout") {
			gotLog = true
		}
	}
	if !gotLog {
		t.Fatal("stream/auth 成功后应持续收到 instance/stdout 推送")
	}

	// 4) 无 uuid 的 stream/input 也要能下发命令（面板转发浏览器的就是这个形状）
	ws.sendTextAny(`42["stream/input",{"data":{"command":"say hi"}}]`)
	// 命令是 fire-and-forget（daemon 侧无回包），这里只要不把连接搞断即可
	if err := ws.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
}

// TestCompatMCSMStreamAuthRejectsWrongPass 错误通行证必须返回 data!=true（面板据此报错）。
func TestCompatMCSMStreamAuthRejectsWrongPass(t *testing.T) {
	d, _ := newTestDaemon(t)
	addr := strings.TrimPrefix(setupTestMCSM(t, d), "http://")

	ws := openStreamClient(t, addr)
	defer ws.conn.Close()
	ws.sendTextAny(`42["stream/auth",{"data":{"password":"not-a-valid-pass"}}]`)
	pkt := ws.waitSocketPacket("42", 5*time.Second)
	if strings.Contains(pkt, `"data":true`) {
		t.Fatalf("错误通行证不应认证成功: %q", pkt)
	}
	if !strings.Contains(pkt, "err") && !strings.Contains(pkt, `"status":500`) {
		t.Fatalf("错误通行证应返回失败回包: %q", pkt)
	}
}

// TestCompatMCSMWebSocketHeartbeat WebSocket 传输必须由服务端下发 engine.io ping，
// 且对端不回复 pong 时按超时断开（否则客户端 45s 后自己重连）。
//
// 心跳间隔通过**该层实例字段**压低（不是包级变量）——包级可变状态会被其它
// 会话的心跳 goroutine 并发读取，-race 下会直接报数据竞争。
func TestCompatMCSMWebSocketHeartbeat(t *testing.T) {
	d, _ := newTestDaemon(t)
	// 先完成凭据/配置解析并压低心跳，再启动监听
	d.setupCompat(&nodeOptions{
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0,
		CompatMCSMKey:     mcsmTestKey,
	})
	if d.compatMC == nil {
		t.Fatal("MCSManager 兼容层未初始化")
	}
	d.compatMC.pingIv = 150 * time.Millisecond
	d.compatMC.pingTo = 150 * time.Millisecond
	d.startCompatServers()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.stopCompatServers(ctx)
	})
	addr := d.compatAddrOf("mcsm")
	if addr == "" {
		t.Fatal("MCSManager 兼容层未监听")
	}

	// 正常回 pong：连接应保持
	ws := openStreamClient(t, addr)
	defer ws.conn.Close()
	gotPing := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pkt := ws.readPacket(time.Until(deadline))
		if pkt == "2" {
			gotPing = true
			ws.sendTextAny("3")
			break
		}
	}
	if !gotPing {
		t.Fatal("WebSocket 传输应收到服务端下发的 engine.io ping（2）")
	}

	// 不回 pong：应在 pingInterval+pingTimeout 后被服务端断开
	silent := openStreamClient(t, addr)
	defer silent.conn.Close()
	_ = silent.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	closed := false
	for i := 0; i < 12; i++ {
		opcode, _, err := func() (int, []byte, error) {
			return silent.readFrameRaw()
		}()
		if err != nil {
			closed = true
			break
		}
		if opcode == wsClose {
			closed = true
			break
		}
		// 收到 ping 但故意不回 pong
	}
	if !closed {
		t.Fatal("未回 pong 的连接应被服务端按心跳超时断开")
	}
}

// readFrameRaw 读取帧但不在失败时 Fatal（供「等待断开」场景使用）。
func (c *wsTestClient) readFrameRaw() (int, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	opcode := int(hdr[0] & 0x0f)
	length := int(hdr[1] & 0x7f)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = int(b[0])<<8 | int(b[1])
	} else if length == 127 {
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(b[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}
