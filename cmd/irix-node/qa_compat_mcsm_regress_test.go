// 真机（真实 MCSManager 面板 / MSLX daemon）联调发现的缺陷的回归用例。
//
// 这些用例对应的现象都在真实面板上复现过：
//   - 节点页「系统信息」空白 + 每 6 秒重连：请求包只有 {uuid} 没有 {data}，
//     信封拆包丢 uuid → 回包 uuid 为 null → 面板匹配不到而超时；
//   - 面板收不到任何回包：把多个 engine.io 包合并进一个 WS 帧（只有轮询允许
//     \x1e 多包），socket.io-client 解析失败；
//   - 只读事件抢在 auth 之前执行被门禁拒绝：面板不等 auth 回包就连发请求；
//   - 面板「停止」报 Request timeout：停止同步等进程退出，超过面板超时。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// postUUIDOnly 发送「只有 uuid、没有 data 键」的请求包（真实面板对 info/overview 就是这样）。
func (c *eioClient) postUUIDOnly(event, uuid string) {
	c.t.Helper()
	eventJSON, _ := json.Marshal(event)
	body, _ := json.Marshal(map[string]string{"uuid": uuid})
	c.post("42[" + string(eventJSON) + "," + string(body) + "]")
}

// waitEventStatus 等待指定事件与 uuid 的回包，返回 status 与原始 data。
func (c *eioClient) waitEventStatus(event, uuid string) (int, json.RawMessage) {
	c.t.Helper()
	pkt := c.waitEvent(event, uuid)
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
		c.t.Fatalf("回包格式错误: %q", pkt)
	}
	var packet struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(arr[1], &packet); err != nil {
		c.t.Fatalf("回包载荷解析失败: %v（%q）", err, pkt)
	}
	return packet.Status, packet.Data
}

// TestMCSMUUIDOnlyEnvelopeKeepsUUID 请求包只有 uuid（没有 data 键）时必须保留 uuid。
func TestMCSMUUIDOnlyEnvelopeKeepsUUID(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "a1", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}

	const reqUUID = "uuid-only-1234567890"
	c.postUUIDOnly("info/overview", reqUUID)
	pkt := c.waitEvent("info/overview", reqUUID) // uuid 丢了就会在这里超时
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
		t.Fatalf("回包格式错误: %q", pkt)
	}
	var env struct {
		UUID   *string        `json:"uuid"`
		Status int            `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(arr[1], &env); err != nil {
		t.Fatalf("回包解析失败: %v", err)
	}
	if env.UUID == nil || *env.UUID != reqUUID {
		t.Fatalf("回包必须回显请求 uuid（否则面板匹配不到、表现为超时重连）：%q", pkt)
	}
	if env.Status != 200 || env.Data["version"] == nil {
		t.Fatalf("info/overview 回包内容异常: %q", pkt)
	}
}

// TestWSOutboxOnePacketPerFrame engine.io 的 WS 传输必须一帧一包（不合并）。
func TestWSOutboxOnePacketPerFrame(t *testing.T) {
	o := newWSOutbox(nil)
	frames := [][]byte{[]byte(`42["a",{}]`), []byte(`42["b",{}]`)}
	payloads := o.encode(frames)
	if len(payloads) != len(frames) {
		t.Fatalf("engine.io 必须每帧一个包，实际编码成 %d 个 payload", len(payloads))
	}
	for i, pl := range payloads {
		if strings.ContainsRune(string(pl), '\x1e') {
			t.Fatalf("engine.io 的 WS 帧内不允许 \\x1e 多包: %q", pl)
		}
		if string(pl) != string(frames[i]) {
			t.Fatalf("第 %d 帧内容被改动: %q", i, pl)
		}
	}
}

// TestWSOutboxSignalRMergesWithSeparators SignalR 允许合并，但每条记录都必须以 0x1e 结尾。
func TestWSOutboxSignalRMergesWithSeparators(t *testing.T) {
	o := newWSOutboxFramed(nil, []byte(srRecordSep), []byte(srRecordSep))
	payloads := o.encode([][]byte{[]byte(`{"type":1}`), []byte(`{"type":3}`)})
	if len(payloads) != 1 {
		t.Fatalf("SignalR 应合并成 1 个 payload，实际 %d", len(payloads))
	}
	got := string(payloads[0])
	if got != `{"type":1}`+"\x1e"+`{"type":3}`+"\x1e" {
		t.Fatalf("SignalR 合并格式错误（记录之间与结尾都要有 0x1e）: %q", got)
	}
}

// TestMCSMFastLaneWaitsForAuth 认证未完成时只读事件必须排队等 auth 完成，
// 不能被门禁拒绝（真实面板不等 auth 回包就连发 info/overview）。
//
// 注意：一次轮询可能同时带回多个回包，所以这里自己收集，不能两次调用 waitEvent
// （后者会把同批里的其它包丢弃）。
func TestMCSMFastLaneWaitsForAuth(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()

	// 先发只读事件（尚未认证），紧接着发 auth（不等回包）
	eventJSON, _ := json.Marshal("instance/overview")
	c.post("42[" + string(eventJSON) + `,{"uuid":"fast-before-auth","data":{}}]`)
	authJSON, _ := json.Marshal("auth")
	c.post("42[" + string(authJSON) + `,{"uuid":"late-auth","data":"` + mcsmTestKey + `"}]`)

	replies := map[string]map[string]any{}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && len(replies) < 2 {
		body := c.get("")
		if body == "" {
			continue
		}
		for _, pkt := range strings.Split(body, "\x1e") {
			if pkt == "2" {
				c.post("3")
				continue
			}
			if !strings.HasPrefix(pkt, "42") {
				continue
			}
			var arr []json.RawMessage
			if err := json.Unmarshal([]byte(pkt[2:]), &arr); err != nil || len(arr) < 2 {
				continue
			}
			var event string
			var packet struct {
				UUID *string         `json:"uuid"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(arr[0], &event) != nil || json.Unmarshal(arr[1], &packet) != nil || packet.UUID == nil {
				continue
			}
			m := map[string]any{"data": string(packet.Data)}
			replies[event+"|"+*packet.UUID] = m
		}
	}

	overview, ok := replies["instance/overview|fast-before-auth"]
	if !ok {
		t.Fatalf("认证前的只读事件没有回包（可能被门禁拒绝或丢失）：%v", replies)
	}
	if strings.Contains(fmt.Sprint(overview["data"]), "未通过节点密钥认证") {
		t.Fatalf("只读事件被门禁拒绝（并发通道抢在 auth 之前执行）: %v", overview["data"])
	}
	if _, ok := replies["auth|late-auth"]; !ok {
		t.Fatalf("auth 未回包：%v", replies)
	}
}

// TestMCSMStopRepliesImmediately 停止必须立刻回包（软关停等待放到后台）。
func TestMCSMStopRepliesImmediately(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)

	cwd, err := os.MkdirTemp("", "irix-stop-async-")
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

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "s0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}

	start := time.Now()
	status, data := c.requestRaw("instance/stop", "s1", map[string]any{
		"instanceUUIDs": []string{inst.InstanceUuid},
	})
	elapsed := time.Since(start)
	if status != 200 {
		t.Fatalf("停止应成功: %d %s", status, data)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("停止回包耗时 %s，必须异步回包（面板会 Request timeout）", elapsed)
	}
}
