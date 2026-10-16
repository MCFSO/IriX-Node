// 异步任务管理器（taskStore）测试：创建/查询/快照、上限淘汰、过期清理、并发安全。

package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTaskStoreCreateSnapshot 创建任务后状态为 running，进度更新可查询。
func TestTaskStoreCreateSnapshot(t *testing.T) {
	s := newTaskStore()
	id, task := s.create()
	if id == "" || task == nil {
		t.Fatalf("create 返回空 id 或空任务")
	}
	snap := task.snapshot()
	if snap["status"] != taskStatusRunning {
		t.Fatalf("初始状态应为 running，实际 %v", snap["status"])
	}
	if p, ok := snap["percent"].(float64); !ok || p != -1 {
		t.Fatalf("初始进度应为 -1，实际 %v", snap["percent"])
	}
	task.set(taskStatusRunning, 0.5, "正在下载", "/tmp/x.jar")
	snap = task.snapshot()
	if snap["percent"] != 0.5 || snap["message"] != "正在下载" || snap["path"] != "/tmp/x.jar" {
		t.Fatalf("进度更新未生效: %v", snap)
	}
	task.setError(errTaskFail)
	snap = task.snapshot()
	if snap["status"] != taskStatusFailed {
		t.Fatalf("失败状态未生效: %v", snap)
	}
	// get 找不到不存在的任务
	if s.get("no-such-task") != nil {
		t.Fatalf("不存在的任务应返回 nil")
	}
	t.Logf("[验证] 任务创建/进度更新/失败标记/查询均正确")
}

// errTaskFail 测试用错误。
var errTaskFail = &taskFailErr{}

type taskFailErr struct{}

func (e *taskFailErr) Error() string { return "模拟失败" }

// TestTaskStoreEviction 超过上限时淘汰最旧任务。
func TestTaskStoreEviction(t *testing.T) {
	s := newTaskStore()
	first, _ := s.create()
	for i := 0; i < maxTasks+10; i++ {
		s.create()
	}
	if len(s.tasks) > maxTasks {
		t.Fatalf("任务数超过上限: %d", len(s.tasks))
	}
	if s.get(first) != nil {
		t.Fatalf("最旧任务应被淘汰")
	}
	t.Logf("[验证] 超过上限后淘汰最旧任务（当前 %d 个）", len(s.tasks))
}

// TestTaskStoreCleanup 过期任务被清理，运行中任务保留。
func TestTaskStoreCleanup(t *testing.T) {
	s := newTaskStore()
	_, doneTask := s.create()
	doneTask.set(taskStatusDone, 1, "完成", "")
	doneTask.createdAt = time.Now().Add(-3 * time.Hour) // 已过期
	_, runningTask := s.create()
	runningTask.createdAt = time.Now().Add(-3 * time.Hour) // 运行中不过期

	s.cleanupOnce(time.Now())

	if s.get(taskIDOf(s, doneTask)) != nil {
		t.Fatalf("过期的已完成任务应被清理")
	}
	if s.get(taskIDOf(s, runningTask)) == nil {
		t.Fatalf("运行中的任务不应被过期清理")
	}
	t.Logf("[验证] 过期清理只移除已结束任务")
}

// TestTaskStoreOrderTasksConsistency 任务表 order 与 tasks 必须始终一致。
// 回归 [高-8]：过期淘汰只删 tasks、不从 order 摘除 id，会在 order 留下悬挂 id，
// 后续遍历取到 nil 会在 t.mu 上 panic；该 panic 发生在后台 goroutine 中，
// 会直接终止整个节点进程（全部实例中断）。
func TestTaskStoreOrderTasksConsistency(t *testing.T) {
	s := newTaskStore()
	// 塞满任务表，全部为「已结束且已过期」，逼迫 create 命中过期淘汰分支
	for i := 0; i < maxTasks; i++ {
		_, task := s.create()
		task.set(taskStatusDone, 1, "完成", "")
		task.createdAt = time.Now().Add(-3 * time.Hour)
	}
	s.create() // 修复前：淘汰过期任务后 order 残留悬挂 id

	if len(s.tasks) != len(s.order) {
		t.Fatalf("tasks 与 order 长度不一致: tasks=%d order=%d", len(s.tasks), len(s.order))
	}
	for _, id := range s.order {
		if s.tasks[id] == nil {
			t.Fatalf("order 中存在悬挂 id: %s", id)
		}
	}
	s.cleanupOnce(time.Now())
	if len(s.tasks) != len(s.order) {
		t.Fatalf("清理后 tasks 与 order 长度不一致: tasks=%d order=%d", len(s.tasks), len(s.order))
	}
	t.Logf("[验证] order/tasks 始终一致（tasks=%d）", len(s.tasks))
}

// TestTaskStoreDanglingOrderID cleanupOnce 遇到悬挂 id 应跳过，
// 而不是在对 nil 取 t.mu 时 panic（回归 [高-8]）。
func TestTaskStoreDanglingOrderID(t *testing.T) {
	s := newTaskStore()
	id, task := s.create()
	task.set(taskStatusDone, 1, "完成", "")
	// 人为制造不一致（模拟历史遗留数据或并发淘汰留下的状态）
	delete(s.tasks, id)

	s.cleanupOnce(time.Now()) // 修复前：此处 nil 解引用 panic

	if len(s.order) != 0 {
		t.Fatalf("悬挂 id 应被丢弃，order 仍剩 %d 项", len(s.order))
	}
	t.Logf("[验证] 悬挂 id 被安全丢弃，未 panic")
}

// taskIDOf 反查任务 id（测试用）。
func taskIDOf(s *taskStore, target *task) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tasks {
		if t == target {
			return id
		}
	}
	return ""
}

// TestTaskStoreConcurrent 并发创建与更新不崩溃（配合 -race 检测数据竞争）。
func TestTaskStoreConcurrent(t *testing.T) {
	s := newTaskStore()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id, task := s.create()
				task.set(taskStatusRunning, 0.1*float64(i%10), "进度", "")
				_ = s.get(id)
				_ = task.snapshot()
			}
		}()
	}
	wg.Wait()
	if len(s.tasks) > maxTasks {
		t.Fatalf("任务数超过上限: %d", len(s.tasks))
	}
	t.Logf("[验证] 并发创建/更新/查询正常（剩余 %d 个任务）", len(s.tasks))
}

// TestTaskFormatErr 失败信息拼接。
func TestTaskFormatErr(t *testing.T) {
	msg := formatTaskErr("t-1", errTaskFail)
	if !strings.Contains(msg, "t-1") || !strings.Contains(msg, "模拟失败") {
		t.Fatalf("失败信息不完整: %s", msg)
	}
	t.Logf("[验证] 失败信息包含 taskId 与原因: %s", msg)
}
