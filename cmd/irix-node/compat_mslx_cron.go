// MSLX 定时任务（面板「定时任务」页）。
//
// 对齐面板使用的接口与字段（ScheduleTask / CronTaskController）：
//
//	GET  /api/instance/tasks/list              → 全部任务
//	GET  /api/instance/tasks/list/{instanceId} → 某实例的任务
//	POST /api/instance/tasks/create            → {id}
//	POST /api/instance/tasks/update            → 更新
//	POST /api/instance/tasks/delete/{id}       → 删除
//
// 任务类型：command（向实例控制台发指令）、start / stop / restart、
// backup（实例快照）、shell（任意命令，需管理员——兼容层只有节点密钥/JWT 这一层，
// 这里按「能登录面板即管理员」处理并记审计日志）。
//
// Cron 为 5 段标准表达式（分 时 日 月 周），支持 * 、a 、a-b 、*/n 、a-b/n 与逗号列表；
// 解析器是本节点自研（无第三方依赖），调度由 startCronScheduler 的分钟级循环驱动。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// mslxCronTask 定时任务（字段与面板 ScheduleTask 一致）。
type mslxCronTask struct {
	ID             string  `json:"id"`
	InstanceID     uint32  `json:"instanceId"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Cron           string  `json:"cron"`
	Payload        string  `json:"payload"`
	Enable         bool    `json:"enable"`
	RunWhenOffline bool    `json:"runWhenOffline"`
	LastRunTime    *string `json:"lastRunTime"`
}

// mslxCronStore 任务表（持久化于 {data}/compat-mslx-tasks.json）。
type mslxCronStore struct {
	mu    sync.Mutex
	items []mslxCronTask
	stop  chan struct{}
	// lastFired 记录每个任务上次触发的分钟（Unix 分钟序号），防止同一分钟重复执行
	lastFired map[string]int64
}

// cronStorePath 任务表文件路径。
func (ms *mslxCompat) cronStorePath() string {
	return filepath.Join(ms.d.DataDir, "compat-mslx-tasks.json")
}

// loadCronTasks 读取任务表（启动时调用，文件不存在视为空表）。
func (ms *mslxCompat) loadCronTasks() {
	ms.cron.mu.Lock()
	defer ms.cron.mu.Unlock()
	ms.cron.lastFired = map[string]int64{}
	data, err := os.ReadFile(ms.cronStorePath())
	if err != nil {
		return
	}
	var items []mslxCronTask
	if err := json.Unmarshal(data, &items); err != nil {
		alog.Printf("MSLX 兼容层：定时任务表解析失败（%v），按空表处理", err)
		return
	}
	ms.cron.items = items
}

// saveCronTasksLocked 原子写任务表（调用方持锁）。
func (ms *mslxCompat) saveCronTasksLocked() {
	data, err := json.MarshalIndent(ms.cron.items, "", "  ")
	if err != nil {
		alog.Printf("MSLX 兼容层：定时任务表序列化失败: %v", err)
		return
	}
	tmp := ms.cronStorePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		alog.Printf("MSLX 兼容层：定时任务表写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, ms.cronStorePath()); err != nil {
		alog.Printf("MSLX 兼容层：定时任务表落盘失败: %v", err)
	}
}

// handleCronTasks 处理 /api/instance/tasks/*。
func (ms *mslxCompat) handleCronTasks(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		mslxFail(w, http.StatusNotFound, "未知的定时任务接口")
		return
	}
	switch segs[0] {
	case "list":
		ms.handleCronList(w, r, segs[1:])
	case "create":
		ms.handleCronCreate(w, r)
	case "update":
		ms.handleCronUpdate(w, r)
	case "delete":
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少任务 id")
			return
		}
		ms.handleCronDelete(w, r, segs[1])
	case "run":
		// 面板的「立即执行」若存在，直接跑一次
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少任务 id")
			return
		}
		ms.handleCronRunNow(w, r, segs[1])
	default:
		mslxFail(w, http.StatusNotImplemented, "irix-node 未实现该定时任务接口："+strings.Join(segs, "/"))
	}
}

// handleCronList 任务列表（可按实例过滤）。
func (ms *mslxCompat) handleCronList(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) > 0 && segs[0] != "" {
		id, err := strconv.ParseUint(segs[0], 10, 32)
		if err != nil {
			mslxFail(w, http.StatusBadRequest, "实例 id 无效")
			return
		}
		ms.cron.mu.Lock()
		out := make([]mslxCronTask, 0, len(ms.cron.items))
		for _, t := range ms.cron.items {
			if t.InstanceID == uint32(id) {
				out = append(out, t)
			}
		}
		ms.cron.mu.Unlock()
		mslxOK(w, out, "获取成功")
		return
	}
	ms.cron.mu.Lock()
	out := make([]mslxCronTask, len(ms.cron.items))
	copy(out, ms.cron.items)
	ms.cron.mu.Unlock()
	mslxOK(w, out, "获取成功")
}

// cronTaskRequest 创建/更新任务的请求体。
type cronTaskRequest struct {
	ID             string `json:"id"`
	InstanceID     uint32 `json:"instanceId"`
	Name           string `json:"name"`
	Cron           string `json:"cron"`
	Type           string `json:"type"`
	Payload        string `json:"payload"`
	Enable         *bool  `json:"enable"`
	RunWhenOffline *bool  `json:"runWhenOffline"`
}

// validate 校验并补齐默认值。
func (req *cronTaskRequest) validate(ms *mslxCompat) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("任务名称不能为空")
	}
	if _, ok := ms.uuidForID(req.InstanceID); !ok {
		return fmt.Errorf("实例 %d 不存在", req.InstanceID)
	}
	if strings.TrimSpace(req.Cron) == "" {
		return errors.New("Cron 表达式不能为空")
	}
	if _, err := parseCronSpec(req.Cron); err != nil {
		return fmt.Errorf("无效的 Cron 表达式: %v", err)
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type == "" {
		req.Type = "command"
	}
	switch req.Type {
	case "command", "start", "stop", "restart", "backup", "shell":
	default:
		return fmt.Errorf("不支持的任务类型 %q（仅支持 command/start/stop/restart/backup/shell）", req.Type)
	}
	if req.Type == "command" && strings.TrimSpace(req.Payload) == "" {
		return errors.New("command 类型必须填写要发送的指令")
	}
	if req.Type == "shell" && strings.TrimSpace(req.Payload) == "" {
		return errors.New("shell 类型必须填写要执行的命令")
	}
	return nil
}

// handleCronCreate 创建任务。
func (ms *mslxCompat) handleCronCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req cronTaskRequest
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if err := req.validate(ms); err != nil {
		mslxFail(w, http.StatusBadRequest, err.Error())
		return
	}
	enable := true
	if req.Enable != nil {
		enable = *req.Enable
	}
	runOffline := true
	if req.RunWhenOffline != nil {
		runOffline = *req.RunWhenOffline
	}
	task := mslxCronTask{
		ID:             newUUID(),
		InstanceID:     req.InstanceID,
		Name:           req.Name,
		Type:           req.Type,
		Cron:           req.Cron,
		Payload:        req.Payload,
		Enable:         enable,
		RunWhenOffline: runOffline,
	}
	ms.cron.mu.Lock()
	ms.cron.items = append(ms.cron.items, task)
	ms.saveCronTasksLocked()
	ms.cron.mu.Unlock()
	alog.Printf("MSLX 兼容层：已创建定时任务 %s（实例 %d，%s，%s）", task.Name, task.InstanceID, task.Type, task.Cron)
	mslxOK(w, task.ID, "任务创建成功")
}

// handleCronUpdate 更新任务。
func (ms *mslxCompat) handleCronUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req cronTaskRequest
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		mslxFail(w, http.StatusBadRequest, "缺少任务 id")
		return
	}
	if err := req.validate(ms); err != nil {
		mslxFail(w, http.StatusBadRequest, err.Error())
		return
	}
	ms.cron.mu.Lock()
	defer ms.cron.mu.Unlock()
	for i := range ms.cron.items {
		if ms.cron.items[i].ID != req.ID {
			continue
		}
		ms.cron.items[i].InstanceID = req.InstanceID
		ms.cron.items[i].Name = req.Name
		ms.cron.items[i].Cron = req.Cron
		ms.cron.items[i].Type = req.Type
		ms.cron.items[i].Payload = req.Payload
		if req.Enable != nil {
			ms.cron.items[i].Enable = *req.Enable
		}
		if req.RunWhenOffline != nil {
			ms.cron.items[i].RunWhenOffline = *req.RunWhenOffline
		}
		ms.saveCronTasksLocked()
		mslxOK(w, nil, "任务更新成功")
		return
	}
	mslxFail(w, http.StatusBadRequest, "未找到指定的任务")
}

// handleCronDelete 删除任务。
func (ms *mslxCompat) handleCronDelete(w http.ResponseWriter, r *http.Request, id string) {
	ms.cron.mu.Lock()
	defer ms.cron.mu.Unlock()
	for i := range ms.cron.items {
		if ms.cron.items[i].ID == id {
			ms.cron.items = append(ms.cron.items[:i], ms.cron.items[i+1:]...)
			delete(ms.cron.lastFired, id)
			ms.saveCronTasksLocked()
			mslxOK(w, nil, "任务已删除")
			return
		}
	}
	mslxFail(w, http.StatusBadRequest, "删除失败，未找到指定 ID 的任务")
}

// handleCronRunNow 立即执行一次（面板「立即执行」）。
func (ms *mslxCompat) handleCronRunNow(w http.ResponseWriter, r *http.Request, id string) {
	ms.cron.mu.Lock()
	var found *mslxCronTask
	for i := range ms.cron.items {
		if ms.cron.items[i].ID == id {
			cp := ms.cron.items[i]
			found = &cp
			break
		}
	}
	ms.cron.mu.Unlock()
	if found == nil {
		mslxFail(w, http.StatusBadRequest, "未找到指定的任务")
		return
	}
	if err := ms.runCronTask(*found); err != nil {
		mslxFail(w, http.StatusBadRequest, "执行失败: "+err.Error())
		return
	}
	mslxOK(w, nil, "任务已执行")
}

// runCronTask 执行一个任务。
func (ms *mslxCompat) runCronTask(task mslxCronTask) error {
	uuid, ok := ms.uuidForID(task.InstanceID)
	if !ok {
		return fmt.Errorf("实例 %d 不存在", task.InstanceID)
	}
	inst := ms.d.Find(uuid)
	if inst == nil {
		return fmt.Errorf("实例 %d 不存在", task.InstanceID)
	}
	inst.mu.Lock()
	proc := inst.Proc
	running := proc != nil && proc.IsRunning()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()

	// 启动类任务不需要实例在运行；其余按 runWhenOffline 决定
	if !running && task.Type != "start" && !task.RunWhenOffline {
		return fmt.Errorf("实例未运行且任务未允许离线执行")
	}

	switch task.Type {
	case "start":
		if err := ms.d.startInstance(inst); err != nil {
			return err
		}
	case "stop":
		if err := ms.d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
			return err
		}
	case "restart":
		if err := ms.d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
			return err
		}
		if err := ms.d.startInstance(inst); err != nil {
			return err
		}
	case "backup":
		// 复用节点既有的快照任务表（{data}/backups/<uuid>/<时间>.zip）
		taskID, tk := ms.d.tasks.create()
		go ms.d.runSnapshot(taskID, tk, uuid, cwd)
	case "command":
		if !running {
			return errors.New("实例未运行，无法发送指令")
		}
		if err := proc.WriteCommand(strings.TrimSpace(task.Payload)); err != nil {
			return err
		}
	case "shell":
		if err := runShellInDir(cwd, task.Payload); err != nil {
			return err
		}
	}
	ms.markCronRan(task.ID)
	alog.Printf("MSLX 兼容层：定时任务 %s（%s）已执行，实例 %s", task.Name, task.Type, inst.Config.Nickname)
	return nil
}

// markCronRan 记录最后执行时间。
func (ms *mslxCompat) markCronRan(id string) {
	stamp := timesys.Now().Format("2006-01-02 15:04:05")
	ms.cron.mu.Lock()
	defer ms.cron.mu.Unlock()
	for i := range ms.cron.items {
		if ms.cron.items[i].ID == id {
			ms.cron.items[i].LastRunTime = &stamp
			ms.cron.lastFired[id] = timesys.Now().Unix() / 60
			ms.saveCronTasksLocked()
			return
		}
	}
}

// startCronScheduler 启动调度循环（随兼容层启动，随节点关停退出）。
func (ms *mslxCompat) startCronScheduler() {
	ms.cron.mu.Lock()
	if ms.cron.stop != nil {
		ms.cron.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	ms.cron.stop = stop
	ms.cron.mu.Unlock()
	safeGo("MSLX 兼容层定时任务调度", func() {
		// 20 秒一跳：分钟内触发误差 ≤ 20s，且重启后不会补跑历史分钟
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ms.tickCron()
			}
		}
	})
}

// stopCronScheduler 停止调度循环。
func (ms *mslxCompat) stopCronScheduler() {
	ms.cron.mu.Lock()
	stop := ms.cron.stop
	ms.cron.stop = nil
	ms.cron.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// tickCron 检查并按分钟触发到期任务。
func (ms *mslxCompat) tickCron() {
	now := timesys.Now()
	minute := now.Unix() / 60
	ms.cron.mu.Lock()
	due := make([]mslxCronTask, 0, 4)
	for _, t := range ms.cron.items {
		if !t.Enable {
			continue
		}
		if ms.cron.lastFired[t.ID] >= minute {
			continue
		}
		spec, err := parseCronSpec(t.Cron)
		if err != nil {
			continue
		}
		if !spec.match(now) {
			continue
		}
		ms.cron.lastFired[t.ID] = minute
		due = append(due, t)
	}
	ms.cron.mu.Unlock()
	for _, t := range due {
		task := t
		safeGo("MSLX 兼容层定时任务执行", func() {
			if err := ms.runCronTask(task); err != nil {
				alog.Printf("MSLX 兼容层：定时任务 %s 执行失败：%v", task.Name, err)
			}
		})
	}
}

// runShellInDir 在实例目录执行一条 shell 命令（shell 类型任务）。
func runShellInDir(dir, command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return runShellInDirContext(ctx, dir, command)
}

// runShellInDirContext 带上下文执行 shell 命令（可取消）。
func runShellInDirContext(ctx context.Context, dir, command string) error {
	// Windows 上 `cmd /C "<含引号的整条命令>"` 会被 Go 的参数转义打乱
	// （内部双引号变成 \" → cmd 解析成功但路径被破坏）。落成临时 .bat 再执行，
	// 绕开引号转义；执行完即删。
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		script, err := os.CreateTemp(dir, ".irix-cron-*.bat")
		if err != nil {
			return err
		}
		name := script.Name()
		if _, err := script.WriteString("@echo off\r\n" + command + "\r\n"); err != nil {
			script.Close()
			_ = os.Remove(name)
			return err
		}
		if err := script.Close(); err != nil {
			_ = os.Remove(name)
			return err
		}
		defer os.Remove(name)
		cmd = exec.CommandContext(ctx, "cmd", "/C", name)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		alog.Printf("MSLX 兼容层：shell 任务输出（%s）：%s", command, truncateForLog(string(out), 2000))
	}
	return err
}

// truncateForLog 截断过长日志。
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…（已截断）"
}

// ---------------------------------------------------------------------------
// Cron 表达式（5 段：分 时 日 月 周）
// ---------------------------------------------------------------------------

// cronField 单个字段的取值集合。
type cronField struct {
	any   bool
	bits  [64]bool
	names map[string]int
}

// cronSpec 完整表达式（second 仅在 6 段表达式里生效）。
type cronSpec struct {
	second                        cronField
	hasSec                        bool
	minute, hour, dom, month, dow cronField
}

var cronMonthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var cronDowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseCronSpec 解析 cron 表达式：支持 5 段（分 时 日 月 周）与
// 6 段（秒 分 时 日 月 周，MCSManager 的 node-schedule 表达式就是 6 段）。
func parseCronSpec(expr string) (*cronSpec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	var sec cronField
	hasSec := false
	switch len(fields) {
	case 5:
	case 6:
		hasSec = true
		var err error
		sec, err = parseCronField(fields[0], 0, 59, nil)
		if err != nil {
			return nil, fmt.Errorf("秒字段: %v", err)
		}
		fields = fields[1:]
	default:
		return nil, fmt.Errorf("需要 5 段（分 时 日 月 周）或 6 段（秒 分 时 日 月 周），实际 %d 段", len(fields))
	}
	minute, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("分钟段: %v", err)
	}
	hour, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("小时段: %v", err)
	}
	dom, err := parseCronField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("日期段: %v", err)
	}
	month, err := parseCronField(fields[3], 1, 12, cronMonthNames)
	if err != nil {
		return nil, fmt.Errorf("月份段: %v", err)
	}
	dow, err := parseCronField(fields[4], 0, 6, cronDowNames)
	if err != nil {
		return nil, fmt.Errorf("星期段: %v", err)
	}
	return &cronSpec{second: sec, hasSec: hasSec, minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// parseCronField 解析单个字段（支持 *、n、a-b、*/n、a-b/n、逗号列表、名称）。
func parseCronField(raw string, lo, hi int, names map[string]int) (cronField, error) {
	f := cronField{names: names}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return f, errors.New("存在空项")
		}
		if part == "*" {
			f.any = true
			for i := lo; i <= hi; i++ {
				f.bits[i] = true
			}
			continue
		}
		step := 1
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			s, err := strconv.Atoi(part[idx+1:])
			if err != nil || s <= 0 {
				return f, fmt.Errorf("步长非法: %q", part)
			}
			step = s
			part = part[:idx]
			if part == "" || part == "*" {
				for i := lo; i <= hi; i += step {
					f.bits[i] = true
				}
				continue
			}
		}
		startStr, endStr := part, part
		if idx := strings.IndexByte(part, '-'); idx >= 0 {
			startStr, endStr = part[:idx], part[idx+1:]
		}
		start, err := parseCronValue(startStr, lo, hi, names)
		if err != nil {
			return f, err
		}
		end, err := parseCronValue(endStr, lo, hi, names)
		if err != nil {
			return f, err
		}
		if end < start {
			return f, fmt.Errorf("区间非法: %q", part)
		}
		for i := start; i <= end; i += step {
			f.bits[i] = true
		}
	}
	return f, nil
}

// parseCronValue 解析单值（数字或名称）。
func parseCronValue(s string, lo, hi int, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("缺少取值")
	}
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			// 星期的 7 视同 0（周日）
			if hi == 6 && v == 0 {
				return 0, nil
			}
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("取值非法: %q", s)
	}
	if hi == 6 && v == 7 {
		v = 0
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("取值 %d 超出范围 %d-%d", v, lo, hi)
	}
	return v, nil
}

// match 判断给定时间是否命中表达式（日与周按 cron 惯例取「或」）。
func (s *cronSpec) match(t time.Time) bool {
	if s.hasSec && !s.second.bits[t.Second()] {
		return false
	}
	if !s.minute.bits[t.Minute()] || !s.hour.bits[t.Hour()] || !s.month.bits[int(t.Month())] {
		return false
	}
	domHit := s.dom.bits[t.Day()]
	dowHit := s.dow.bits[int(t.Weekday())]
	// 两者都被限制时取「或」，只限制其一时按该限制
	if s.dom.any && s.dow.any {
		return true
	}
	if s.dom.any {
		return dowHit
	}
	if s.dow.any {
		return domHit
	}
	return domHit || dowHit
}

// nextRun 计算下一次触发时间（最多向后找一年，用于面板展示与自检）。
func (s *cronSpec) nextRun(from time.Time) (time.Time, bool) {
	step := time.Minute
	limit := 366 * 24 * 60
	if s.hasSec {
		step = time.Second
		limit = 366 * 24 * 60 * 60
	}
	t := from.Truncate(step).Add(step)
	for i := 0; i < limit; i++ {
		if s.match(t) {
			return t, true
		}
		t = t.Add(step)
	}
	return time.Time{}, false
}

// sortCronTasks 便于测试与稳定输出（按实例 id、名称排序）。
func sortCronTasks(items []mslxCronTask) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].InstanceID != items[j].InstanceID {
			return items[i].InstanceID < items[j].InstanceID
		}
		return items[i].Name < items[j].Name
	})
}
