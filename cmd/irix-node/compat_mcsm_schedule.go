// MCSManager 面板「计划任务（Scheduled Task）」兼容层。
//
// 面板通过 daemon 事件操作计划任务（真实 daemon 的 schedule_router.ts）：
//
//	schedule/register  data = IScheduleTask  → true
//	schedule/list      data = {instanceUuid} → 该实例的任务数组
//	schedule/delete    data = {instanceUuid, name} → true
//
// IScheduleTask：{instanceUuid, name, count, time, actions:[{type, payload}], type}
//   - type 1 Interval：time = 间隔秒数（最小 3）；count = 次数，-1 表示无限
//   - type 2 Cycle   ：time = node-schedule 表达式（6 段，含秒），如 "0 30 4 * * *"
//   - type 3 Specify ：time = "2006-01-02 15:04:05" 这样的绝对时间，只执行一次
//   - actions[].type：delay / command / stop / start / restart / kill
//
// 限制与真实 daemon 一致：同一实例最多 8 个任务、任务名不可重复、动作最多 10 个。
// 任务表持久化于 {data}/compat-mcsm-schedules.json，节点重启后自动恢复并重新挂表。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// MCSM 计划任务常量（与真实 daemon 的枚举一致）。
const (
	mcScheduleTypeInterval = 1
	mcScheduleTypeCycle    = 2
	mcScheduleTypeSpecify  = 3

	mcScheduleMaxTasksPerInstance = 8
	mcScheduleMaxActions          = 10
	mcScheduleMinIntervalSec      = 3
)

// mcScheduleAction 计划任务的一个动作。
type mcScheduleAction struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// mcScheduleTask 计划任务配置（字段名与面板 IScheduleTask 一致）。
type mcScheduleTask struct {
	InstanceUUID string             `json:"instanceUuid"`
	Name         string             `json:"name"`
	Count        int64              `json:"count"`
	Time         string             `json:"time"`
	Actions      []mcScheduleAction `json:"actions"`
	Type         int                `json:"type"`
	// NextRun/LastRun 仅用于对外展示与诊断，面板不依赖
	NextRun string `json:"nextRunTime,omitempty"`
	LastRun string `json:"lastRunTime,omitempty"`
}

// mcScheduleStore 计划任务表（持久化）。
type mcScheduleStore struct {
	mu    sync.Mutex
	items []mcScheduleTask
	stop  chan struct{}
	// lastFired 记录每个任务的上次触发时刻（Unix 秒），避免同一秒重复触发
	lastFired map[string]int64
}

// key 任务唯一键（实例 + 名称，与真实 daemon 的存储键一致）。
func (t *mcScheduleTask) key() string { return t.InstanceUUID + "_" + t.Name }

// scheduleStorePath 任务表文件路径。
func (mc *mcsmCompat) scheduleStorePath() string {
	return filepath.Join(mc.d.DataDir, "compat-mcsm-schedules.json")
}

// loadSchedules 读取任务表并启动调度循环（兼容层启动时调用）。
func (mc *mcsmCompat) loadSchedules() {
	mc.schedules.mu.Lock()
	mc.schedules.lastFired = map[string]int64{}
	data, err := os.ReadFile(mc.scheduleStorePath())
	if err == nil {
		var items []mcScheduleTask
		if err := json.Unmarshal(data, &items); err != nil {
			alog.Printf("MCSM 兼容层：计划任务表解析失败（%v），按空表处理", err)
		} else {
			mc.schedules.items = items
			if len(items) > 0 {
				alog.Printf("MCSM 兼容层：已恢复 %d 个计划任务", len(items))
			}
		}
	}
	mc.schedules.mu.Unlock()
	mc.startScheduleLoop()
}

// saveLocked 原子写任务表（调用方持锁）。
func (mc *mcsmCompat) saveSchedulesLocked() {
	data, err := json.MarshalIndent(mc.schedules.items, "", "  ")
	if err != nil {
		alog.Printf("MCSM 兼容层：计划任务表序列化失败: %v", err)
		return
	}
	tmp := mc.scheduleStorePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		alog.Printf("MCSM 兼容层：计划任务表写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, mc.scheduleStorePath()); err != nil {
		alog.Printf("MCSM 兼容层：计划任务表落盘失败: %v", err)
	}
}

// startScheduleLoop 启动秒级调度循环（只启动一次）。
func (mc *mcsmCompat) startScheduleLoop() {
	mc.schedules.mu.Lock()
	if mc.schedules.stop != nil {
		mc.schedules.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	mc.schedules.stop = stop
	mc.schedules.mu.Unlock()
	safeGo("MCSM 兼容层计划任务调度", func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mc.tickSchedules()
			}
		}
	})
}

// stopScheduleLoop 停止调度循环（兼容层关停时调用）。
func (mc *mcsmCompat) stopScheduleLoop() {
	mc.schedules.mu.Lock()
	stop := mc.schedules.stop
	mc.schedules.stop = nil
	mc.schedules.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// ---------------------------------------------------------------------------
// 事件处理
// ---------------------------------------------------------------------------

// mcEventScheduleRegister 注册（创建）计划任务。
func mcEventScheduleRegister(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var task mcScheduleTask
	if err := decode(data, &task); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if err := mc.registerSchedule(&task); err != nil {
		return nil, err
	}
	return true, nil
}

// registerSchedule 校验并登记任务。
func (mc *mcsmCompat) registerSchedule(task *mcScheduleTask) error {
	task.InstanceUUID = strings.TrimSpace(task.InstanceUUID)
	task.Name = strings.TrimSpace(task.Name)
	if task.InstanceUUID == "" || task.Name == "" {
		return fmt.Errorf("实例与任务名不能为空")
	}
	if mc.d.Find(task.InstanceUUID) == nil {
		return fmt.Errorf("实例 %s 不存在", task.InstanceUUID)
	}
	if len(task.Actions) > mcScheduleMaxActions {
		return fmt.Errorf("单个计划任务最多 %d 个动作", mcScheduleMaxActions)
	}
	if len(task.Actions) == 0 {
		return fmt.Errorf("计划任务至少需要一个动作")
	}
	for i := range task.Actions {
		t := strings.ToLower(strings.TrimSpace(task.Actions[i].Type))
		switch t {
		case "delay", "command", "stop", "start", "restart", "kill":
		default:
			return fmt.Errorf("不支持的动作类型 %q（仅支持 delay/command/stop/start/restart/kill）", task.Actions[i].Type)
		}
		task.Actions[i].Type = t
	}
	if task.Count == 0 {
		task.Count = 1
	}
	// 校验时间表达式
	switch task.Type {
	case mcScheduleTypeInterval:
		sec, err := strconv.Atoi(strings.TrimSpace(task.Time))
		if err != nil || sec < mcScheduleMinIntervalSec {
			return fmt.Errorf("间隔任务的 time 必须是 >= %d 的秒数", mcScheduleMinIntervalSec)
		}
	case mcScheduleTypeCycle:
		if _, err := parseCronSpec(task.Time); err != nil {
			return fmt.Errorf("无效的 cron 表达式: %v", err)
		}
	case mcScheduleTypeSpecify:
		// 面板对「Specific Time Task」也是拼 6 段 cron（`秒 分 时 日 月 周`），
		// 见 frontend/hooks/useSchedule.ts 的 createTaskTypeSpecify：
		//   time = `${s} ${m} ${h} ${dd} ${mm} *`
		// 命中一次即可（count 默认 1，执行后自动删除）。
		// 同时兼容「2006-01-02 15:04:05」这种绝对时间写法（历史/手工调用）。
		if _, err := parseCronSpec(task.Time); err != nil {
			if _, terr := parseScheduleTime(task.Time); terr != nil {
				return fmt.Errorf("无法解析时间 %q：既不是 6 段 cron（秒 分 时 日 月 周），也不是 2006-01-02 15:04:05 格式", task.Time)
			}
		}
	default:
		return fmt.Errorf("不支持的任务类型 %d（1=间隔 / 2=cron / 3=指定时间）", task.Type)
	}

	mc.schedules.mu.Lock()
	defer mc.schedules.mu.Unlock()
	n := 0
	for i := range mc.schedules.items {
		if mc.schedules.items[i].InstanceUUID != task.InstanceUUID {
			continue
		}
		n++
		if mc.schedules.items[i].Name == task.Name {
			// 与真实 daemon 一致：同名视为更新（面板的「编辑」也走 register）
			mc.schedules.items[i] = *task
			mc.saveSchedulesLocked()
			return nil
		}
	}
	if n >= mcScheduleMaxTasksPerInstance {
		return fmt.Errorf("同一实例最多 %d 个计划任务", mcScheduleMaxTasksPerInstance)
	}
	mc.schedules.items = append(mc.schedules.items, *task)
	mc.saveSchedulesLocked()
	alog.Printf("MCSM 兼容层：已登记计划任务 %s（实例 %s，类型 %d）", task.Name, task.InstanceUUID, task.Type)
	return nil
}

// mcEventScheduleList 列出某实例的计划任务。
func mcEventScheduleList(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
	}
	_ = decode(data, &req)
	mc.schedules.mu.Lock()
	out := make([]mcScheduleTask, 0, len(mc.schedules.items))
	for _, t := range mc.schedules.items {
		if req.InstanceUUID == "" || t.InstanceUUID == req.InstanceUUID {
			out = append(out, t)
		}
	}
	mc.schedules.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mcEventScheduleDelete 删除计划任务（按实例 + 名称）。
func mcEventScheduleDelete(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Name         string `json:"name"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	mc.schedules.mu.Lock()
	kept := mc.schedules.items[:0]
	removed := false
	for _, t := range mc.schedules.items {
		if t.InstanceUUID == req.InstanceUUID && t.Name == req.Name {
			removed = true
			delete(mc.schedules.lastFired, t.key())
			continue
		}
		kept = append(kept, t)
	}
	mc.schedules.items = kept
	if removed {
		mc.saveSchedulesLocked()
	}
	mc.schedules.mu.Unlock()
	// 与真实 daemon 一致：删除是幂等的，找不到也回 true
	return true, nil
}

// removeSchedulesOfInstance 实例被删除时清理其计划任务。
func (mc *mcsmCompat) removeSchedulesOfInstance(uuid string) {
	mc.schedules.mu.Lock()
	kept := mc.schedules.items[:0]
	for _, t := range mc.schedules.items {
		if t.InstanceUUID != uuid {
			kept = append(kept, t)
			continue
		}
		delete(mc.schedules.lastFired, t.key())
	}
	mc.schedules.items = kept
	mc.saveSchedulesLocked()
	mc.schedules.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 调度
// ---------------------------------------------------------------------------

// tickSchedules 每秒检查一次到期任务。
func (mc *mcsmCompat) tickSchedules() {
	now := timesys.Now()
	type dueTask struct{ task mcScheduleTask }
	var due []dueTask
	mc.schedules.mu.Lock()
	for i := range mc.schedules.items {
		t := &mc.schedules.items[i]
		if !scheduleDue(t, now, mc.schedules.lastFired[t.key()]) {
			continue
		}
		mc.schedules.lastFired[t.key()] = now.Unix()
		due = append(due, dueTask{task: *t})
	}
	mc.schedules.mu.Unlock()

	for _, d := range due {
		task := d.task
		safeGo("MCSM 兼容层计划任务执行", func() {
			mc.runSchedule(task)
		})
	}
}

// scheduleDue 判断任务在 now 是否到期（已触发过的时刻不重复触发）。
func scheduleDue(t *mcScheduleTask, now time.Time, last int64) bool {
	switch t.Type {
	case mcScheduleTypeInterval:
		sec, err := strconv.Atoi(strings.TrimSpace(t.Time))
		if err != nil || sec < mcScheduleMinIntervalSec {
			return false
		}
		return last == 0 || now.Unix()-last >= int64(sec)
	case mcScheduleTypeCycle:
		spec, err := parseCronSpec(t.Time)
		if err != nil {
			return false
		}
		if last == now.Unix() {
			return false // 同一秒只触发一次
		}
		return spec.match(now)
	case mcScheduleTypeSpecify:
		// 面板给的是 6 段 cron：命中即触发（同一秒只触发一次）
		if spec, err := parseCronSpec(t.Time); err == nil {
			if last == now.Unix() {
				return false
			}
			return spec.match(now)
		}
		// 兼容绝对时间写法
		at, err := parseScheduleTime(t.Time)
		if err != nil {
			return false
		}
		return last == 0 && !now.Before(at) && now.Sub(at) < 24*time.Hour
	}
	return false
}

// parseScheduleTime 解析「指定时间」任务的 time（面板传本地时间字符串）。
func parseScheduleTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("指定时间任务必须填写时间")
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC3339,
		"2006/01/02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q（示例：2026-11-15 08:13:50）", v)
}

// runSchedule 执行任务的动作序列，并按 count 维护任务生命周期。
func (mc *mcsmCompat) runSchedule(task mcScheduleTask) {
	inst := mc.d.Find(task.InstanceUUID)
	if inst == nil {
		mc.removeSchedulesOfInstance(task.InstanceUUID)
		return
	}
	for _, act := range task.Actions {
		switch act.Type {
		case "delay":
			ms, err := strconv.Atoi(strings.TrimSpace(act.Payload))
			if err != nil || ms <= 0 {
				continue
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "start":
			inst.mu.Lock()
			status := inst.Status
			inst.mu.Unlock()
			if status == StatusStopped {
				if err := mc.d.startInstance(inst); err != nil {
					alog.Printf("MCSM 兼容层：计划任务 %s 启动实例失败: %v", task.Name, err)
				}
			}
		case "stop":
			inst.mu.Lock()
			status := inst.Status
			inst.mu.Unlock()
			if status == StatusRunning {
				// 异步关停，避免计划任务协程被软关停等待拖住
				safeGo("MCSM 兼容层计划任务停止实例", func() {
					_ = mc.d.stopInstance(inst)
				})
			}
		case "restart":
			safeGo("MCSM 兼容层计划任务重启实例", func() {
				if err := mc.d.stopInstance(inst); err != nil && err != errNotRunning {
					alog.Printf("MCSM 兼容层：计划任务 %s 重启（停止阶段）失败: %v", task.Name, err)
					return
				}
				if err := mc.d.startInstance(inst); err != nil {
					alog.Printf("MCSM 兼容层：计划任务 %s 重启（启动阶段）失败: %v", task.Name, err)
				}
			})
		case "command":
			inst.mu.Lock()
			proc := inst.Proc
			running := proc != nil && proc.IsRunning()
			inst.mu.Unlock()
			if !running {
				continue
			}
			cmd := strings.TrimSpace(act.Payload)
			if cmd == "" {
				continue
			}
			if isConsoleInterrupt(cmd) {
				handleConsoleInterrupt(mc, nil, inst)
				continue
			}
			if err := proc.WriteCommand(cmd); err != nil {
				alog.Printf("MCSM 兼容层：计划任务 %s 下发指令失败: %v", task.Name, err)
			}
		case "kill":
			if err := mc.d.compatKillInstance(inst); err != nil {
				alog.Printf("MCSM 兼容层：计划任务 %s 强杀实例失败: %v", task.Name, err)
			}
		}
	}

	// count 维护：-1 无限；1 执行后删除；其它减一
	stamp := timesys.Now().Format("2006-01-02 15:04:05")
	mc.schedules.mu.Lock()
	defer mc.schedules.mu.Unlock()
	for i := range mc.schedules.items {
		cur := &mc.schedules.items[i]
		if cur.InstanceUUID != task.InstanceUUID || cur.Name != task.Name {
			continue
		}
		cur.LastRun = stamp
		if cur.Count == -1 {
			if cur.Type == mcScheduleTypeSpecify {
				// 指定时间任务只跑一次
				mc.schedules.items = append(mc.schedules.items[:i], mc.schedules.items[i+1:]...)
				delete(mc.schedules.lastFired, task.key())
				mc.saveSchedulesLocked()
			}
			return
		}
		if cur.Count <= 1 {
			mc.schedules.items = append(mc.schedules.items[:i], mc.schedules.items[i+1:]...)
			delete(mc.schedules.lastFired, task.key())
			mc.saveSchedulesLocked()
			return
		}
		cur.Count--
		mc.saveSchedulesLocked()
		return
	}
}

// scheduleHandler 处理 /api/schedule 相关的 HTTP 便车请求（面板未使用，保留 404）。
func (mc *mcsmCompat) scheduleHandler(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
