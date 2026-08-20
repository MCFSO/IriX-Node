// 通用异步任务管理器（taskStore）。
//
// 耗时操作（JDK 安装、服务端核心下载、实例备份/恢复等）统一任务化：
// 发起接口返回 taskId，客户端轮询进度（status/percent/message/path），
// 避免大文件下载/压缩等操作长时间占用节点 HTTP 请求线程
// （docs/irix-node-local-parity.md §4 通用约定：所有耗时操作必须异步任务化）。
//
// 与 container.go 的 jobStore（容器长任务，日志行收集）用途不同、相互独立：
// 本表面向「进度可轮询的异步操作」，带百分比/消息/产物路径与过期清理。

package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// 任务状态常量。
const (
	taskStatusRunning = "running" // 执行中
	taskStatusDone    = "done"    // 完成
	taskStatusFailed  = "failed"  // 失败
)

// task 一个异步任务的状态。
// 所有字段访问需持 mu（进度由后台 goroutine 更新，查询接口并发读）。
type task struct {
	mu        sync.Mutex
	status    string    // running | done | failed
	percent   float64   // 0.0 ~ 1.0；-1 表示未知进度（不提供百分比）
	message   string    // 人类可读的进度消息（中文）
	path      string    // 产物路径（如下载完成的文件/安装后的 java 路径）
	err       error     // failed 时的错误
	createdAt time.Time // 创建时间（过期清理依据）
}

// set 更新任务状态。
func (t *task) set(status string, percent float64, message, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = status
	t.percent = percent
	t.message = message
	t.path = path
}

// setError 标记任务失败。
func (t *task) setError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = taskStatusFailed
	t.percent = -1
	t.message = err.Error()
	t.err = err
}

// snapshot 返回查询接口用的状态快照。
func (t *task) snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"status":  t.status,
		"percent": t.percent,
		"message": t.message,
	}
	if t.path != "" {
		out["path"] = t.path
	}
	return out
}

// taskStore 任务表：上限淘汰 + 过期清理，防止任务无限堆积耗尽内存。
type taskStore struct {
	mu    sync.Mutex
	tasks map[string]*task
	order []string // 创建顺序（FIFO，用于超上限淘汰最旧）
}

// maxTasks 任务上限：超过后淘汰最旧任务（被淘汰任务进度不可再查询，但工作不中断）。
const maxTasks = 1024

// taskTTL 已完成/失败任务的保留时长；运行中任务不受过期清理影响。
const taskTTL = 2 * time.Hour

// newTaskStore 创建任务表并启动清理循环。
func newTaskStore() *taskStore {
	s := &taskStore{tasks: map[string]*task{}}
	go s.cleanupLoop()
	return s
}

// create 创建任务并返回 taskId 与任务对象。
func (s *taskStore) create() (string, *task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) >= maxTasks {
		// 超上限：优先淘汰已结束且过期的任务，否则淘汰最旧的任意任务
		now := time.Now()
		for _, id := range s.order {
			t := s.tasks[id]
			t.mu.Lock()
			finished := t.status == taskStatusDone || t.status == taskStatusFailed
			expired := finished && now.Sub(t.createdAt) > taskTTL
			t.mu.Unlock()
			if expired {
				delete(s.tasks, id)
				break
			}
		}
		if len(s.tasks) >= maxTasks && len(s.order) > 0 {
			delete(s.tasks, s.order[0])
			s.order = s.order[1:]
		}
	}
	id := newUUID()
	s.tasks[id] = &task{status: taskStatusRunning, percent: -1, createdAt: time.Now()}
	s.order = append(s.order, id)
	return id, s.tasks[id]
}

// get 按 taskId 获取任务；不存在返回 nil。
func (s *taskStore) get(id string) *task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id]
}

// cleanupLoop 定期清理过期的已完成/失败任务。
func (s *taskStore) cleanupLoop() {
	for {
		time.Sleep(10 * time.Minute)
		s.cleanupOnce(time.Now())
	}
}

// cleanupOnce 执行一次过期清理（供测试直接调用）。
func (s *taskStore) cleanupOnce(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.order[:0]
	for _, id := range s.order {
		t := s.tasks[id]
		t.mu.Lock()
		finished := t.status == taskStatusDone || t.status == taskStatusFailed
		expired := finished && now.Sub(t.createdAt) > taskTTL
		t.mu.Unlock()
		if expired {
			delete(s.tasks, id)
			continue
		}
		keep = append(keep, id)
	}
	s.order = keep
}

// newTask 创建异步任务（Daemon 便捷方法）。
func (d *Daemon) newTask() (string, *task) {
	return d.tasks.create()
}

// writeTaskStatus 输出任务进度（各进度查询接口共用）。
// GET ?jobId=<id> → {status, percent, message, path?}
func (d *Daemon) writeTaskStatus(w http.ResponseWriter, r *http.Request) {
	taskID := queryParam(r, "jobId")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "缺少 jobId 参数")
		return
	}
	t := d.tasks.get(taskID)
	if t == nil {
		writeError(w, http.StatusBadRequest, "任务不存在或已过期")
		return
	}
	writeOK(w, t.snapshot())
}

// formatTaskErr 拼接任务失败信息（含 taskId 与原因）。
func formatTaskErr(taskID string, err error) string {
	return fmt.Sprintf("任务 %s 失败: %v", taskID, err)
}
