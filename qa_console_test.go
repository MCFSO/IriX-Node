// WebSocket 实时控制台测试：握手、输出流、命令下发、ping/pong、
// 断线补发（since）、进程退出通知、认证失败。

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// wsTestClient 极简 WebSocket 测试客户端（RFC 6455 客户端侧）。
type wsTestClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// dialTestWS 完成客户端握手；非 101 直接测试失败。
func dialTestWS(t *testing.T, rawURL string) *wsTestClient {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := fmt.Sprintf("GET %s?%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.Path, u.RawQuery, u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatalf("发送握手失败: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatalf("读取握手响应失败: %v", err)
	}
	if resp.StatusCode != 101 {
		conn.Close()
		t.Fatalf("升级失败: %d %s", resp.StatusCode, resp.Status)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAccept(key); got != want {
		conn.Close()
		t.Fatalf("Sec-WebSocket-Accept 不匹配: got %s want %s", got, want)
	}
	return &wsTestClient{t: t, conn: conn, br: br}
}

// sendFrame 发送掩码帧（测试载荷 < 126 字节）。
func (c *wsTestClient) sendFrame(opcode int, payload []byte) {
	c.t.Helper()
	if len(payload) >= 126 {
		c.t.Fatalf("测试客户端仅支持 <126 字节载荷")
	}
	hdr := []byte{0x80 | byte(opcode), 0x80 | byte(len(payload))}
	key := []byte{0x11, 0x22, 0x33, 0x44}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	buf := append(hdr, key...)
	buf = append(buf, masked...)
	if _, err := c.conn.Write(buf); err != nil {
		c.t.Fatalf("发送帧失败: %v", err)
	}
}

// sendText 发送文本帧。
func (c *wsTestClient) sendText(s string) {
	c.sendFrame(wsText, []byte(s))
}

// sendPing 发送 ping 控制帧。
func (c *wsTestClient) sendPing() {
	c.sendFrame(wsPing, nil)
}

// sendClose 发送 close 帧。
func (c *wsTestClient) sendClose() {
	c.sendFrame(wsClose, []byte{0x03, 0xe8})
}

// readFrame 读取一帧（带超时）。
func (c *wsTestClient) readFrame(timeout time.Duration) (int, []byte) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		c.t.Fatalf("读取帧头失败: %v", err)
	}
	opcode := int(hdr[0] & 0x0f)
	length := int(hdr[1] & 0x7f)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			c.t.Fatalf("读取扩展长度失败: %v", err)
		}
		length = int(b[0])<<8 | int(b[1])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		c.t.Fatalf("读取载荷失败: %v", err)
	}
	return opcode, payload
}

// wsURL 构造控制台 WS 地址。
func wsURL(srvURL, uuid string) string {
	return srvURL + "/api/instance/console/ws?uuid=" + uuid + "&apikey=test-key"
}

// stopTestProc 停止测试实例进程。
func stopTestProc(t *testing.T, inst *Instance) {
	t.Helper()
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc != nil && proc.IsRunning() {
		_ = proc.Kill()
	}
}

// TestConsoleWSHandshakeAndPong 握手 101 + ping/pong 往返。
func TestConsoleWSHandshakeAndPong(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("ws-hs-uuid", InstanceConfig{
		Nickname: "WS握手", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	c := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid))
	defer c.conn.Close()

	c.sendPing()
	opcode, _ := c.readFrame(3 * time.Second)
	if opcode != wsPong {
		t.Fatalf("ping 未得到 pong，opcode=%d", opcode)
	}
	t.Logf("[验证] WS 握手成功且 ping/pong 正常")
}

// TestConsoleWSOutputStream 运行中实例的输出逐行推送。
func TestConsoleWSOutputStream(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("ws-out-uuid", InstanceConfig{
		Nickname: "WS输出", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer stopTestProc(t, inst)

	c := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid))
	defer c.conn.Close()

	// 等待至少一行输出（长驻输出进程）
	got := false
	for i := 0; i < 20; i++ {
		opcode, payload := c.readFrame(2 * time.Second)
		if opcode == wsText && len(payload) > 0 {
			got = true
			break
		}
	}
	if !got {
		t.Fatalf("未收到任何输出行")
	}
	t.Logf("[验证] 实例输出通过 WS 逐行推送")
}

// TestConsoleWSCommand 命令下发到进程 stdin（sh/cmd 回显）。
func TestConsoleWSCommand(t *testing.T) {
	// Windows 用 cmd 的 more 过滤器（stdin 直通 stdout）；Unix 用 sh 回显循环
	cmd := "sh -c 'while read l; do echo \"cmd:$l\"; done'"
	if runtime.GOOS == "windows" {
		cmd = "cmd /c more"
	}
	d, dir := newTestDaemon(t)
	inst := NewInstance("ws-cmd-uuid", InstanceConfig{
		Nickname:     "WS命令",
		StartCommand: cmd,
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer stopTestProc(t, inst)

	c := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid))
	defer c.conn.Close()

	c.sendText("hello-console")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		opcode, payload := c.readFrame(2 * time.Second)
		if opcode == wsText && string(payload) == "cmd:hello-console" {
			t.Logf("[验证] WS 命令下发到进程 stdin 并回显")
			return
		}
		// Windows more 直通模式：文本原样回显
		if runtime.GOOS == "windows" && opcode == wsText && string(payload) == "hello-console" {
			t.Logf("[验证] WS 命令下发到进程 stdin 并回显（more 直通）")
			return
		}
	}
	t.Fatalf("未收到命令回显")
}

// TestConsoleWSBackfillSince 断线重连补发：since 之后的增量日志。
func TestConsoleWSBackfillSince(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("ws-since-uuid", InstanceConfig{
		Nickname: "WS补发", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 先连接确认输出流正常，再断开
	c1 := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid))
	got := false
	for i := 0; i < 20; i++ {
		opcode, payload := c1.readFrame(2 * time.Second)
		if opcode == wsText && len(payload) > 0 {
			got = true
			break
		}
	}
	c1.conn.Close()
	if !got {
		stopTestProc(t, inst)
		t.Fatalf("首次连接未收到输出")
	}

	before := time.Now().UnixMilli()
	time.Sleep(1500 * time.Millisecond) // 等待更多输出
	stopTestProc(t, inst)               // 停止进程后再补发（行缓冲仍在）

	// 重连并带 since 参数：应收到补发行
	c2 := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid)+"&since="+strconv.FormatInt(before, 10))
	defer c2.conn.Close()
	found := false
	for i := 0; i < 10; i++ {
		opcode, payload := c2.readFrame(3 * time.Second)
		if opcode == wsText && len(payload) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("断线补发未收到 since 之后的增量行")
	}
	t.Logf("[验证] since 断线补发正常")
}

// TestConsoleWSExitNotice 进程退出时推送通知并关闭连接。
func TestConsoleWSExitNotice(t *testing.T) {
	d, dir := newTestDaemon(t)
	cmd := "sh -c 'echo boot; sleep 2'"
	if runtime.GOOS == "windows" {
		cmd = "ping -n 2 127.0.0.1"
	}
	inst := NewInstance("ws-exit-uuid", InstanceConfig{
		Nickname: "WS退出", StartCommand: cmd, Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	c := dialTestWS(t, wsURL(srv.URL, inst.InstanceUuid))
	defer c.conn.Close()

	notified := false
	for i := 0; i < 20; i++ {
		opcode, payload := c.readFrame(3 * time.Second)
		if opcode == wsText && string(payload) == "[节点] 进程已退出，输出结束" {
			notified = true
			break
		}
		if opcode == wsClose {
			break
		}
	}
	if !notified {
		t.Fatalf("未收到进程退出通知")
	}
	t.Logf("[验证] 进程退出时控制台收到通知")
}

// TestConsoleWSAuthFailed 错误 apikey 拒绝升级（403）。
func TestConsoleWSAuthFailed(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := NewInstance("ws-auth-uuid", InstanceConfig{
		Nickname: "WS认证", StartCommand: echoCommand(), Cwd: dir,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/api/instance/console/ws?uuid=" + inst.InstanceUuid + "&apikey=wrong")
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %s?%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.Path, u.RawQuery, u.Host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("错误 apikey 应返回 403，实际 %d", resp.StatusCode)
	}
	t.Logf("[验证] 认证失败拒绝 WS 升级（403）")
}

// TestConsoleWSUnknownInstance 未知 uuid 返回 400（不升级）。
func TestConsoleWSUnknownInstance(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	resp, err := testClient.Get(wsURL(srv.URL, "no-such-uuid"))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知实例应返回 400，实际 %d", resp.StatusCode)
	}
	t.Logf("[验证] 未知实例返回 400 且不升级")
}
