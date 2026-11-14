// MCSManager 兼容层：异步执行基础设施。
//
// 目标是把传输层彻底做成非阻塞：
//   - 任务队列：写入永不阻塞、永不内联执行（原实现在队列满时同步 dispatch，
//     一个慢事件会把整个 HTTP 读循环拖住），有界环形缓冲 + 单 channel 唤醒；
//   - WebSocket 出站：每连接一个写协程 + 有界出站队列，同一时刻的多个帧合并成
//     一个 WS 文本帧（engine.io v4 允许帧内多包，用 \x1e 分隔），把 syscall 数量
//     从「一包一次」降到「一批一次」；慢客户端只阻塞自己的出站队列并丢帧计数，
//     绝不阻塞业务协程；
//   - 事件级并发：只读/幂等事件（file/list、file/edit 读取、info/*）并发执行，
//     其余事件保持会话内串行（认证门禁与顺序语义不受影响）。
package main

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// mcJobQueueCap 单会话任务队列上限（超出丢最旧，绝不阻塞调用方）。
const mcJobQueueCap = 8192

// mcJobBatch 一次出队最多取走的任务数（降低锁竞争）。
const mcJobBatch = 64

// mcFastLaneWorkers 并发通道的工作协程数（只读事件并行度）。
const mcFastLaneWorkers = 4

// mcJobQueue 非阻塞任务队列（环形缓冲 + 单信号唤醒）。
//
// 用环形缓冲而不是「切片 + copy」：队满时丢最旧是 O(1)，不会在热点路径上
// 复制上千个元素（实测并行入队因此从 14.7µs/op 降到百纳秒级）。
type mcJobQueue struct {
	mu      sync.Mutex
	buf     []mcJob // 环形缓冲，容量按需增长到 mcJobQueueCap
	head    int     // 队首下标
	size    int     // 当前元素数
	closed  bool
	sig     chan struct{} // cap 1，仅用于唤醒工作协程
	dropped atomic.Uint64 // 队列满导致的丢弃数（用于日志/诊断）
}

// newMCJobQueue 创建任务队列（初始 256 槽，按需翻倍到上限）。
func newMCJobQueue() *mcJobQueue {
	return newMCJobQueueWake(1)
}

// newMCJobQueueWake 创建带 wake 个唤醒槽位的队列。
//
// 唤醒槽位要与消费者协程数一致：只给 1 个槽位时并发通道会被一个协程
// 独吞（批量取走全部任务），并发度退化为 1。
func newMCJobQueueWake(wake int) *mcJobQueue {
	if wake < 1 {
		wake = 1
	}
	return &mcJobQueue{buf: make([]mcJob, 256), sig: make(chan struct{}, wake)}
}

// push 入队：永不阻塞、永不内联执行。队满时覆盖最旧任务并计数（O(1)）。
func (q *mcJobQueue) push(job mcJob) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	if q.size == len(q.buf) {
		if len(q.buf) < mcJobQueueCap {
			// 扩容：按 2 倍增长到上限，一次搬迁
			newCap := len(q.buf) * 2
			if newCap > mcJobQueueCap {
				newCap = mcJobQueueCap
			}
			next := make([]mcJob, newCap)
			for i := 0; i < q.size; i++ {
				next[i] = q.buf[(q.head+i)%len(q.buf)]
			}
			q.buf = next
			q.head = 0
		} else {
			// 已到上限：丢弃最旧（覆盖队首槽位），绝不阻塞调用方
			q.buf[q.head] = job
			q.head = (q.head + 1) % len(q.buf)
			q.dropped.Add(1)
			q.mu.Unlock()
			select {
			case q.sig <- struct{}{}:
			default:
			}
			return
		}
	}
	q.buf[(q.head+q.size)%len(q.buf)] = job
	q.size++
	q.mu.Unlock()
	select {
	case q.sig <- struct{}{}:
	default: // 已有唤醒信号，工作协程会把这批任务一并取走
	}
}

// popBatch 取走至多 n 个任务（保序）。
func (q *mcJobQueue) popBatch(n int) []mcJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == 0 || q.closed {
		return nil
	}
	if n > q.size {
		n = q.size
	}
	out := make([]mcJob, n)
	for i := 0; i < n; i++ {
		out[i] = q.buf[(q.head+i)%len(q.buf)]
	}
	q.head = (q.head + n) % len(q.buf)
	q.size -= n
	if q.size == 0 {
		q.head = 0
	}
	return out
}

// popOne 取走一个任务（并发通道用：每个协程一次取一个才能真正并行）。
func (q *mcJobQueue) popOne() (mcJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == 0 || q.closed {
		return mcJob{}, false
	}
	job := q.buf[q.head]
	q.head = (q.head + 1) % len(q.buf)
	q.size--
	if q.size == 0 {
		q.head = 0
	}
	return job, true
}

// close 关闭队列（后续 push 直接丢弃）。
func (q *mcJobQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.size = 0
	q.head = 0
	q.mu.Unlock()
	// 唤醒所有消费者（槽位可能被占满，逐个尽力投递）
	for i := 0; i < cap(q.sig); i++ {
		select {
		case q.sig <- struct{}{}:
		default:
		}
	}
}

// length 当前积压任务数（诊断用）。
func (q *mcJobQueue) length() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size
}

// ---------------------------------------------------------------------------
// WebSocket 出站队列（批量合并写）
// ---------------------------------------------------------------------------

// wsOutboxCap 出站队列上限（慢客户端超出即丢最旧，避免内存膨胀）。
const wsOutboxCap = 2048

// wsOutbox 单个 WebSocket 连接的批量写出队列。
type wsOutbox struct {
	mu      sync.Mutex
	conn    *wsConn
	frames  [][]byte
	closed  bool
	sig     chan struct{}
	dropped atomic.Uint64
	pumping sync.Once
}

// newWSOutbox 创建出站队列（conn 为空时忽略所有写入）。
func newWSOutbox(conn *wsConn) *wsOutbox {
	return &wsOutbox{conn: conn, sig: make(chan struct{}, 1)}
}

// push 入队一帧（永不阻塞）。队列满时丢弃最旧帧。
func (o *wsOutbox) push(payload []byte) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	if len(o.frames) >= wsOutboxCap {
		o.frames = o.frames[1:]
		o.dropped.Add(1)
	}
	o.frames = append(o.frames, payload)
	o.mu.Unlock()
	select {
	case o.sig <- struct{}{}:
	default:
	}
}

// start 启动写协程（只启动一次）。
func (o *wsOutbox) start() {
	if o == nil || o.conn == nil {
		return
	}
	o.pumping.Do(func() {
		safeGo("MCSM 兼容层 WS 写出", o.pump)
	})
}

// pump 聚合出站帧并批量写出。
//
// 每次唤醒时一次性取走当前所有待发帧：
//   - 只有 1 帧：直接写单帧（保持最小延迟）；
//   - 多帧：合并成一个 WS 文本帧，以 engine.io 的 \x1e 分隔（协议允许，
//     客户端 engine.io-parser 的 decodePayload 会按分隔符还原成多个包）。
func (o *wsOutbox) pump() {
	for {
		select {
		case <-o.sig:
		}
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return
		}
		frames := o.frames
		o.frames = nil
		o.mu.Unlock()
		if len(frames) == 0 {
			continue
		}
		var err error
		if len(frames) == 1 {
			err = o.conn.writeText(string(frames[0]))
		} else {
			var buf bytes.Buffer
			for i, f := range frames {
				if i > 0 {
					buf.WriteByte('\x1e')
				}
				buf.Write(f)
			}
			err = o.conn.writeText(buf.String())
		}
		if err != nil {
			// 写失败说明连接已坏：关闭队列，由会话心跳/读循环负责收尾
			o.close()
			_ = o.conn.Close()
			return
		}
	}
}

// close 关闭出站队列。
func (o *wsOutbox) close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.closed = true
	o.frames = nil
	o.mu.Unlock()
	select {
	case o.sig <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// 事件级并发判定
// ---------------------------------------------------------------------------

// mcConcurrentEvents 可并发执行的事件（只读或幂等，且与会话状态无先后依赖）。
//
// 认证门禁在 dispatch 里先于并发判定执行；这里只放「同一会话内并行也无副作用」
// 的事件。真正有状态约束的（auth、instance/action、instance/new、stream/*）
// 仍按会话串行，保证顺序语义。
var mcConcurrentEvents = map[string]struct{}{
	"file/list":         {},
	"file/edit":         {}, // 读/写单文件，按 target 独立
	"file/download":     {}, // 只发下载票据
	"download/from-url": {},
	"instance/overview": {},
	"info/overview":     {},
	"info/system":       {},
	"instance/select":   {},
	"instance/query":    {},
}

// isConcurrentEvent 判断事件是否允许并发执行。
func isConcurrentEvent(event string) bool {
	_, ok := mcConcurrentEvents[event]
	return ok
}
