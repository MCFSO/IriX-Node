// MSLX 控制台游标协议（面板 v1.7.6+ 的 ResumeConsole / ReceiveLog 形态）。
//
// 面板侧每个控制台流都有一个游标：
//
//	ConsoleCursor {streamId: string, sequence: number}
//	ConsoleReplay {streamId, sequence, gap, frames:[{sequence, text}]}
//
// 实时输出也必须带游标：`ReceiveLog(text, cursor)`。只发 text（旧形态）会被前端
// 直接丢弃（`receiveFrame` 里 `if (!cursor) return;`），表现为「控制台一片空白」。
//
// 回放语义与 MSLX 的 ConsoleOutputBuffer.Replay 一致：
//   - 同一个 streamId 且 cursor.sequence <= 当前序号：从 cursor.sequence 之后开始回放；
//   - streamId 变了、或 cursor 序号比缓冲里最早的帧还旧：gap=true，前端会重建缓冲。
package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"irix/node/internal/sysinfo"
	"irix/node/internal/timesys"
)

// mslxConsoleMaxFrames 每个控制台流保留的帧数上限（超出丢最旧，对应 gap）。
const mslxConsoleMaxFrames = 2000

// mslxConsoleFrame 一帧控制台输出。
type mslxConsoleFrame struct {
	Sequence int64  `json:"sequence"`
	Text     string `json:"text"`
}

// mslxConsoleCursor 面板传来的游标。
type mslxConsoleCursor struct {
	StreamID string `json:"streamId"`
	Sequence int64  `json:"sequence"`
}

// mslxConsoleStream 单个实例的控制台输出流。
type mslxConsoleStream struct {
	mu       sync.Mutex
	streamID string
	seq      int64
	frames   []mslxConsoleFrame
}

// newConsoleStream 创建输出流（新 streamId 表示一次新的进程生命周期）。
func newConsoleStream() *mslxConsoleStream {
	return &mslxConsoleStream{streamID: newUUID()}
}

// reset 进程重启后调用：换 streamId 并清空历史，前端据此重建缓冲。
func (s *mslxConsoleStream) reset() {
	s.mu.Lock()
	s.streamID = newUUID()
	s.seq = 0
	s.frames = nil
	s.mu.Unlock()
}

// append 追加一帧输出并返回 (text, cursor)。
func (s *mslxConsoleStream) append(text string) (string, mslxConsoleCursor) {
	s.mu.Lock()
	s.seq++
	frame := mslxConsoleFrame{Sequence: s.seq, Text: text}
	s.frames = append(s.frames, frame)
	if len(s.frames) > mslxConsoleMaxFrames {
		// 丢最旧：前端下次带旧游标时会得到 gap
		s.frames = s.frames[len(s.frames)-mslxConsoleMaxFrames:]
	}
	cursor := mslxConsoleCursor{StreamID: s.streamID, Sequence: s.seq}
	s.mu.Unlock()
	return text, cursor
}

// replay 生成回放快照（与 ConsoleOutputBuffer.Replay 同语义）。
func (s *mslxConsoleStream) replay(cursor *mslxConsoleCursor) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	sameStream := cursor != nil && cursor.StreamID == s.streamID && cursor.Sequence >= 0 && cursor.Sequence <= s.seq
	after := int64(0)
	if sameStream {
		after = cursor.Sequence
	}
	gap := cursor != nil && (!sameStream || (len(s.frames) > 0 && after < s.frames[0].Sequence-1))
	frames := make([]mslxConsoleFrame, 0, len(s.frames))
	for _, f := range s.frames {
		if f.Sequence > after {
			frames = append(frames, f)
		}
	}
	return map[string]any{
		"streamId": s.streamID,
		"sequence": s.seq,
		"gap":      gap,
		"frames":   frames,
	}
}

// consoleStreamOf 取（必要时创建）实例的控制台流。
func (ms *mslxCompat) consoleStreamOf(uuid string) *mslxConsoleStream {
	ms.streamMu.Lock()
	defer ms.streamMu.Unlock()
	if ms.streams == nil {
		ms.streams = map[string]*mslxConsoleStream{}
	}
	s, ok := ms.streams[uuid]
	if !ok {
		s = newConsoleStream()
		ms.streams[uuid] = s
	}
	return s
}

// resetConsoleStreamOf 进程重启时重置该实例的控制台流。
func (ms *mslxCompat) resetConsoleStreamOf(uuid string) {
	ms.consoleStreamOf(uuid).reset()
}

// consoleFrameCursor 把一帧转成面板要的 cursor 对象。
func consoleCursorPayload(c mslxConsoleCursor) map[string]any {
	return map[string]any{"streamId": c.StreamID, "sequence": c.Sequence}
}

// ---------------------------------------------------------------------------
// 实例状态监视（面板状态刷新 + 资源监控）
// ---------------------------------------------------------------------------
//
// 面板控制台页**不会轮询**实例状态：它只在收到形如 `[MSLX] ... 已启动/已停止`
// 的控制台通知行时才 emits('update') 触发重新拉取实例信息
// （ServerTerminal.vue 的 onLog 判定）。真实 daemon 在生命周期变化时正是把这些
// 通知写进控制台记录。我们不写，就会出现「服务明明在跑，页面显示已停止」。
//
// 同一条监视循环还负责 `ReceiveStatus(instanceId, cpu, memBytes)`：面板的
// 「实例概览 → 监控」与顶部 CPU/内存就靠它。

// mslxInstanceSample 资源采样状态（算 CPU% 需要上次的累计 CPU tick）。
type mslxInstanceSample struct {
	lastTicks uint64
	lastAt    time.Time
}

// startInstanceMonitor 启动实例状态监视（随兼容层启动，只启动一次）。
func (ms *mslxCompat) startInstanceMonitor() {
	ms.monitorOnce.Do(func() {
		ms.monitorMu.Lock()
		if ms.lastRun == nil {
			ms.lastRun = map[string]bool{}
		}
		ms.monitorMu.Unlock()
		safeGo("MSLX 兼容层实例监视", func() {
			defer recoverLog("MSLX 兼容层实例监视")
			t := time.NewTicker(700 * time.Millisecond)
			defer t.Stop()
			lastStatusPush := map[string]time.Time{}
			samples := map[string]*mslxInstanceSample{}
			for range t.C {
				ms.d.mu.Lock()
				instances := make([]*Instance, len(ms.d.Instances))
				copy(instances, ms.d.Instances)
				ms.d.mu.Unlock()
				for _, inst := range instances {
					inst.mu.Lock()
					proc := inst.Proc
					running := proc != nil && proc.IsRunning()
					pid := 0
					if proc != nil && proc.cmd != nil && proc.cmd.Process != nil {
						pid = proc.cmd.Process.Pid
					}
					nickname := inst.Config.Nickname
					inst.mu.Unlock()

					ms.monitorMu.Lock()
					prev, seen := ms.lastRun[inst.InstanceUuid]
					if !seen || prev != running {
						ms.lastRun[inst.InstanceUuid] = running
						ms.monitorMu.Unlock()
						// 生命周期变化：写通知行并推给正在看控制台的连接
						var text string
						if running {
							text = fmt.Sprintf("[MSLX] 服务器进程已启动，PID: %d", pid)
						} else {
							text = "[MSLX] 服务器进程已停止"
						}
						ms.emitConsoleNotice(inst.InstanceUuid, text)
						alog.Printf("MSLX 兼容层：实例 %s %s", nickname, strings.TrimPrefix(text, "[MSLX] "))
						if !running {
							delete(samples, inst.InstanceUuid)
						}
						continue
					}
					ms.monitorMu.Unlock()

					if !running || pid <= 0 {
						continue
					}
					// 资源上报：2 秒一次，够面板画曲线又不浪费 CPU
					now := timesys.Now()
					if last, ok := lastStatusPush[inst.InstanceUuid]; ok && now.Sub(last) < 2*time.Second {
						continue
					}
					lastStatusPush[inst.InstanceUuid] = now
					cpu := 0.0
					if ticks, ok := sysinfo.ProcCPUTicks(pid); ok {
						s := samples[inst.InstanceUuid]
						if s != nil && !s.lastAt.IsZero() {
							dt := now.Sub(s.lastAt).Seconds()
							if dt > 0 {
								delta := float64(ticks - s.lastTicks)
								cpu = delta / sysinfo.ProcTicksPerSec() / dt * 100
								if cpu < 0 {
									cpu = 0
								}
							}
						}
						samples[inst.InstanceUuid] = &mslxInstanceSample{lastTicks: ticks, lastAt: now}
					}
					memBytes := sysinfo.ProcMemoryBytes(pid)
					ms.broadcastConsoleEvent(inst.InstanceUuid, "ReceiveStatus",
						ms.idFor(inst.InstanceUuid), math.Round(cpu*100)/100, memBytes)
				}
			}
		})
	})
}

// emitConsoleNotice 把一条 daemon 通知写进实例控制台流并推给订阅者。
func (ms *mslxCompat) emitConsoleNotice(uuid, text string) {
	stream := ms.consoleStreamOf(uuid)
	payload, cursor := stream.append(text)
	ms.broadcastConsoleEvent(uuid, "ReceiveLog", payload, consoleCursorPayload(cursor))
}

// broadcastConsoleEvent 给订阅了该实例控制台的连接推送事件。
func (ms *mslxCompat) broadcastConsoleEvent(uuid, target string, args ...any) {
	id := ms.idFor(uuid)
	ms.hubMu.Lock()
	conns := make([]*mslxHubConn, 0, len(ms.hubConns))
	for _, c := range ms.hubConns {
		conns = append(conns, c)
	}
	ms.hubMu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		_, subscribed := c.console[id]
		c.mu.Unlock()
		if !subscribed {
			continue
		}
		c.sendInvocation(target, args...)
	}
}
