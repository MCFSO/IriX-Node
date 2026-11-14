// MCSManager 兼容层异步化的用例与基准：验证「读循环永不被业务阻塞」、
// 「只读事件并发」以及队列/出站队列的边界行为。
package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// injectHandler 临时替换事件处理器（测试内使用），返回恢复函数。
func injectHandler(t *testing.T, event string, fn mcEventFunc) {
	t.Helper()
	old, existed := mcEventHandlers[event]
	mcEventHandlers[event] = fn
	t.Cleanup(func() {
		if existed {
			mcEventHandlers[event] = old
		} else {
			delete(mcEventHandlers, event)
		}
	})
}

// TestMCJobQueueNeverBlocks 队列写入必须永不阻塞、永不内联执行。
//
// 这是「极致异步」的核心保证：原来队列满时会同步 dispatch，一个慢事件就把
// HTTP 读循环拖死。现在无论积压多少，enqueue 都只做一次内存追加。
func TestMCJobQueueNeverBlocks(t *testing.T) {
	q := newMCJobQueue()
	job := mcJob{event: "x", uuid: "u"}

	start := time.Now()
	const n = 20000
	for i := 0; i < n; i++ {
		q.push(job)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("入队 %d 个任务耗时 %s，说明写入被阻塞", n, elapsed)
	}
	// 超过上限应丢弃最旧而不是阻塞
	if q.dropped.Load() == 0 {
		t.Fatalf("写入 %d 个任务（上限 %d）应有丢弃计数", n, mcJobQueueCap)
	}
	if got := q.length(); got != mcJobQueueCap {
		t.Fatalf("队列长度应稳定在上限 %d，实际 %d", mcJobQueueCap, got)
	}

	// 关闭后写入直接丢弃，不 panic、不阻塞
	q.close()
	q.push(job)
}

// TestMCJobQueuePopBatch 批量出队与关闭语义。
func TestMCJobQueuePopBatch(t *testing.T) {
	q := newMCJobQueue()
	for i := 0; i < 10; i++ {
		q.push(mcJob{event: "e", uuid: string(rune('a' + i))})
	}
	got := q.popBatch(4)
	if len(got) != 4 || got[0].uuid != "a" || got[3].uuid != "d" {
		t.Fatalf("批量出队应保序取前 4 个: %+v", got)
	}
	if rest := q.popBatch(100); len(rest) != 6 {
		t.Fatalf("剩余应为 6 个，实际 %d", len(rest))
	}
	if q.popBatch(1) != nil {
		t.Fatal("空队列出队应返回 nil")
	}
}

// TestMCSMEnqueueDoesNotBlockHandler 慢事件不阻塞后续事件入队（HTTP 侧等价路径）。
func TestMCSMEnqueueDoesNotBlockHandler(t *testing.T) {
	d, _ := newTestDaemon(t)
	setupTestMCSM(t, d)
	mc := d.compatMC

	gate := make(chan struct{})
	var running sync.WaitGroup
	injectHandler(t, "qa/slow", func(*mcsmCompat, *eioSession, json.RawMessage) (any, error) {
		running.Done()
		<-gate
		return true, nil
	})

	s, err := mc.newEIOSession()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authed = true
	s.mu.Unlock()

	// 第一个慢事件占住工作协程；随后 1000 个事件入队必须立刻返回
	running.Add(1)
	s.enqueue(mcJob{event: "qa/slow", uuid: "j0"})
	running.Wait()

	start := time.Now()
	for i := 0; i < 1000; i++ {
		s.enqueue(mcJob{event: "qa/fast", uuid: "j"})
	}
	enqueueCost := time.Since(start)
	if enqueueCost > 500*time.Millisecond {
		t.Fatalf("慢事件执行期间入队 1000 个事件耗时 %s（不应被阻塞）", enqueueCost)
	}
	if dropped := s.jobs.dropped.Load(); dropped != 0 {
		t.Fatalf("1000 个事件不应触发丢弃，实际丢弃 %d", dropped)
	}
	close(gate)
	s.close()
}

// TestMCSMReadOnlyEventsRunConcurrently 只读事件（file/list 等）并发执行。
func TestMCSMReadOnlyEventsRunConcurrently(t *testing.T) {
	d, _ := newTestDaemon(t)
	setupTestMCSM(t, d)
	mc := d.compatMC

	const workers = 4
	const sleeps = 250 * time.Millisecond

	var done sync.WaitGroup
	done.Add(workers)
	injectHandler(t, "file/list", func(*mcsmCompat, *eioSession, json.RawMessage) (any, error) {
		time.Sleep(sleeps)
		done.Done()
		return []any{}, nil
	})

	s, err := mc.newEIOSession()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authed = true
	s.mu.Unlock()
	t.Cleanup(s.close)

	start := time.Now()
	for i := 0; i < workers; i++ {
		s.enqueue(mcJob{event: "file/list", uuid: "c"})
	}
	waitCh := make(chan struct{})
	go func() { done.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("并发事件未完成")
	}
	elapsed := time.Since(start)
	// 串行需要 1s；并发应在 ~250ms 内（留足 CI 抖动余量）
	if elapsed > 700*time.Millisecond {
		t.Fatalf("%d 个只读事件并发执行耗时 %s，疑似仍是串行", workers, elapsed)
	}
}

// TestMCSMStatefulEventsStaySerial 有状态事件仍按会话串行（顺序语义不能被并发破坏）。
func TestMCSMStatefulEventsStaySerial(t *testing.T) {
	d, _ := newTestDaemon(t)
	setupTestMCSM(t, d)
	mc := d.compatMC

	var mu sync.Mutex
	var order []string
	injectHandler(t, "instance/command", func(_ *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
		mu.Lock()
		order = append(order, string(data))
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return true, nil
	})

	s, err := mc.newEIOSession()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authed = true
	s.mu.Unlock()
	t.Cleanup(s.close)

	for i := 0; i < 6; i++ {
		s.enqueue(mcJob{event: "instance/command", uuid: "s", data: json.RawMessage(`"` + string(rune('0'+i)) + `"`)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 6 {
		t.Fatalf("应有 6 个事件执行，实际 %d", len(order))
	}
	want := []string{`"0"`, `"1"`, `"2"`, `"3"`, `"4"`, `"5"`}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("有状态事件必须保序: 期望 %v，实际 %v", want, order)
		}
	}
}

// TestWSOutboxBatchesAndDrops 出站队列：满则丢最旧、关闭后写入被忽略。
func TestWSOutboxBatchesAndDrops(t *testing.T) {
	o := newWSOutbox(nil) // conn 为空：只验证队列行为
	for i := 0; i < wsOutboxCap+10; i++ {
		o.push([]byte("42[]"))
	}
	if o.dropped.Load() != 10 {
		t.Fatalf("应丢弃 10 帧，实际 %d", o.dropped.Load())
	}
	o.mu.Lock()
	n := len(o.frames)
	o.mu.Unlock()
	if n != wsOutboxCap {
		t.Fatalf("队列应稳定在 %d 帧，实际 %d", wsOutboxCap, n)
	}
	o.close()
	o.push([]byte("42[]")) // 关闭后不应 panic
}

// BenchmarkMCJobQueuePush 入队吞吐（单协程）。
func BenchmarkMCJobQueuePush(b *testing.B) {
	q := newMCJobQueue()
	job := mcJob{event: "file/list", uuid: "u"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.push(job)
		if i%mcJobBatch == 0 {
			q.popBatch(mcJobBatch)
		}
	}
}

// BenchmarkMCJobQueuePushParallel 入队吞吐（并发）。
func BenchmarkMCJobQueuePushParallel(b *testing.B) {
	q := newMCJobQueue()
	job := mcJob{event: "file/list", uuid: "u"}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.push(job)
		}
	})
}

// TestMCSMSlowEventDoesNotBlockFastLane 慢的有状态事件不得挡住只读请求。
//
// 两条通道的意义就在这里：面板做一次大目录 file/list（或一次慢的实例操作）时，
// 同一会话里的 info/overview 轮询、文件浏览不能被排在它后面干等。
func TestMCSMSlowEventDoesNotBlockFastLane(t *testing.T) {
	d, _ := newTestDaemon(t)
	setupTestMCSM(t, d)
	mc := d.compatMC

	gate := make(chan struct{})
	var slowDone sync.WaitGroup
	slowDone.Add(1)
	injectHandler(t, "instance/command", func(*mcsmCompat, *eioSession, json.RawMessage) (any, error) {
		defer slowDone.Done()
		<-gate // 一直占住有序通道
		return true, nil
	})
	var fastDone sync.WaitGroup
	fastDone.Add(1)
	injectHandler(t, "info/overview", func(*mcsmCompat, *eioSession, json.RawMessage) (any, error) {
		defer fastDone.Done()
		return map[string]any{"ok": true}, nil
	})

	s, err := mc.newEIOSession()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authed = true
	s.mu.Unlock()
	t.Cleanup(s.close)

	s.enqueue(mcJob{event: "instance/command", uuid: "slow"})
	// 等有序通道真正开始处理慢事件
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	s.enqueue(mcJob{event: "info/overview", uuid: "fast"})
	fastWait := make(chan struct{})
	go func() { fastDone.Wait(); close(fastWait) }()
	select {
	case <-fastWait:
	case <-time.After(3 * time.Second):
		close(gate)
		t.Fatal("只读事件被慢的有状态事件阻塞（两条通道没生效）")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		close(gate)
		t.Fatalf("只读事件执行耗时 %s，应在慢事件进行中立即完成", elapsed)
	}
	close(gate)
	slowDone.Wait()
}
