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
