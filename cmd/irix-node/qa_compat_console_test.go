// 控制台交互与实时输出的回归测试：
//
//  1. Ctrl+C（0x03）等控制字符必须**原样**写入标准输入，不能被当成一行命令
//     （用户反馈：面板里按 ^C，服务端收到的是命令文本）。
//  2. 控制台先打开、实例后启动时，实时输出必须自动接上：面板在控制台已打开时
//     不会重新 stream/auth，若转发在「实例未运行」时直接返回，用户就只能退出
//     控制台再进一次才看得到内容。

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStdinWriteRawDoesNotAppendNewline 原样写入不能加换行。
func TestStdinWriteRawDoesNotAppendNewline(t *testing.T) {
	pr, pw := io.Pipe()
	sp := &stdinPipe{pipe: pw}
	go func() {
		_ = sp.WriteRaw("\x03")
		_ = sp.WriteRaw("\x1b[A") // 方向键（ESC 序列）
		_ = pw.Close()
	}()
	data, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "\x03\x1b[A" {
		t.Fatalf("原样写入被改动: %q", data)
	}

	// 对照：WriteLine 仍应补换行（命令语义）
	pr2, pw2 := io.Pipe()
	sp2 := &stdinPipe{pipe: pw2}
	go func() {
		_ = sp2.WriteLine("say hi")
		_ = pw2.Close()
	}()
	data2, _ := io.ReadAll(pr2)
	if string(data2) != "say hi\n" {
		t.Fatalf("WriteLine 行为变化: %q", data2)
	}
}

// TestCompatMCSMConsoleSurvivesLateStart 控制台先开、实例后启动，
// 必须能收到实时输出（不需要退出重进）。
func TestCompatMCSMConsoleSurvivesLateStart(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	addr := strings.TrimPrefix(base, "http://")

	cwd := filepath.Join(worldDir(t), "srv-late-start")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })

	// 主通道：注册通行证（实例此刻**未启动**）
	main := newEIOClient(t, base+"/socket.io/")
	main.connect()
	if status, data := main.requestRaw("auth", "c1", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	const pass = "qa-late-start-pass"
	if status, data := main.requestRaw("passport/register", "c2", map[string]any{
		"name":      "stream_channel",
		"password":  pass,
		"parameter": map[string]any{"instanceUuid": inst.InstanceUuid},
	}); status != 200 {
		t.Fatalf("注册通行证失败: %d %s", status, data)
	}

	// 打开控制台流（此时进程是 nil）
	ws := openStreamClient(t, addr)
	defer ws.conn.Close()
	ws.sendTextAny(`42["stream/auth",{"data":{"password":"` + pass + `"}}]`)
	pkt := ws.waitSocketPacket("42", 5*time.Second)
	if !strings.Contains(pkt, `"data":true`) {
		t.Fatalf("stream/auth 应成功: %q", pkt)
	}

	// 稍后再启动实例——控制台已打开，面板不会重新 auth
	time.Sleep(200 * time.Millisecond)
	if status, data := main.requestRaw("instance/open", "c3", map[string]any{
		"instanceUuids": []string{inst.InstanceUuid},
	}); status != 200 {
		t.Fatalf("启动实例失败: %d %s", status, data)
	}

	// 应能收到实时输出
	deadline := time.Now().Add(10 * time.Second)
	got := false
	for time.Now().Before(deadline) && !got {
		p := ws.waitSocketPacket("42", time.Until(deadline))
		if strings.Contains(p, "instance/stdout") {
			var arr []json.RawMessage
			if err := json.Unmarshal([]byte(p[2:]), &arr); err == nil && len(arr) >= 2 {
				var packet struct {
					Data struct {
						Text string `json:"text"`
					} `json:"data"`
				}
				if err := json.Unmarshal(arr[1], &packet); err == nil && packet.Data.Text != "" {
					got = true
				}
			}
		}
	}
	if !got {
		t.Fatal("实例后启动时控制台应自动收到实时输出（否则用户只能退出重进）")
	}
}

// TestIsConsoleInterrupt Ctrl+C 的各种写法都要识别，普通命令不能误判。
func TestIsConsoleInterrupt(t *testing.T) {
	yes := []string{"^c", "^C", "\x03", "ctrl+c", "CTRL+C", " Ctrl+C ", "^c\n", "^c\r\n"}
	for _, s := range yes {
		if !isConsoleInterrupt(s) {
			t.Fatalf("%q 应识别为 Ctrl+C", s)
		}
	}
	no := []string{"stop", "say ^c", "c", "^", "", "list", "help ^C"}
	for _, s := range no {
		if isConsoleInterrupt(s) {
			t.Fatalf("%q 不应识别为 Ctrl+C", s)
		}
	}
}

// TestCompatMCSMConsoleCtrlCStopsInstance 控制台发 ^c 必须按「停止」处理，
// 而不是把 ^c 当成一行命令写给服务端（线上日志会变成
// 「Unknown or incomplete command ... ^c<--[HERE]」）。
func TestCompatMCSMConsoleCtrlCStopsInstance(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	addr := strings.TrimPrefix(base, "http://")

	// 注意：这里不用 t.TempDir()——停止实例后子进程可能短暂占住工作目录，
	// Windows 上会让 TempDir 清理报错（与被测行为无关）。
	cwd, err := os.MkdirTemp("", "irix-ctrlc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
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

	main := newEIOClient(t, base+"/socket.io/")
	main.connect()
	if status, data := main.requestRaw("auth", "k1", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	const pass = "qa-ctrlc-pass"
	if status, data := main.requestRaw("passport/register", "k2", map[string]any{
		"name": "stream_channel", "password": pass,
		"parameter": map[string]any{"instanceUuid": inst.InstanceUuid},
	}); status != 200 {
		t.Fatalf("注册通行证失败: %d %s", status, data)
	}
	ws := openStreamClient(t, addr)
	defer ws.conn.Close()
	ws.sendTextAny(`42["stream/auth",{"data":{"password":"` + pass + `"}}]`)
	if pkt := ws.waitSocketPacket("42", 5*time.Second); !strings.Contains(pkt, `"data":true`) {
		t.Fatalf("stream/auth 应成功: %q", pkt)
	}

	// 面板的指令输入框发 ^c
	ws.sendTextAny(`42["stream/input",{"data":{"command":"^c"}}]`)

	// 应被翻译成停止：实例状态在期限内置为已停止/停止中
	deadline := time.Now().Add(15 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		st := inst.Status
		inst.mu.Unlock()
		if st == StatusStopped || st == StatusStopping {
			stopped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("^c 应触发停止（而不是作为命令写入标准输入）")
	}
	// 等子进程真正退出：Windows 上进程未退干净会占住工作目录，
	// 导致 TempDir 清理失败（与被测逻辑无关）。
	for time.Now().Before(deadline) {
		inst.mu.Lock()
		proc := inst.Proc
		inst.mu.Unlock()
		if proc == nil || !proc.IsRunning() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
