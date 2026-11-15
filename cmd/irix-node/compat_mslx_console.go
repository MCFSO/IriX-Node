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
	"sync"
	"time"

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

// 保证 timesys 引用（游标时间语义与节点时间一致时使用）。
var _ = func() time.Time { return timesys.Now() }
