// MCSManager 面板「快速安装 / 异步任务」兼容层。
//
// 面板的「服务端模板市场」点 Install 走的是一条异步任务链（真实 daemon 的
// AsyncTask + TaskCenter）：
//
//	instance/asynchronous        {instanceUuid, taskName:"quick_install",
//	                              role, parameter:{newInstanceName, targetLink, setupInfo}}
//	                             → 立即返回 task.toObject()（含 taskId）
//	instance/query_asynchronous  {taskName:"quick_install", parameter:{taskId}}
//	                             → {taskId, status, detail}
//	instance/stop_asynchronous   {instanceUuid, parameter:{taskId}} → true
//
// status：0 已停止/空闲、1 运行中、-1 出错（与 AsyncTask.STATUS_* 一致）。
// 任务流程按真实实现：下载模板包 → 解压到实例目录 → 读取包内
// mcsmanager-config.json（或用面板传的 setupInfo）→ 建立/更新实例。
// 部分能力（Docker 实例、updateCommand 覆盖安装）本节点不提供，会给出明确中文原因。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// 异步任务状态（与 MCSManager AsyncTask.STATUS_* 一致）。
const (
	mcAsyncStatusStop    = 0
	mcAsyncStatusRunning = 1
	mcAsyncStatusError   = -1
)

// mcAsyncTaskTTL 已完成任务的保留时长（面板轮询停止后即可清理）。
const mcAsyncTaskTTL = 10 * time.Minute

// mcAsyncDownloadProgress 下载进度（字段名与面板一致）。
type mcAsyncDownloadProgress struct {
	Percentage      int     `json:"percentage"`
	DownloadedBytes int64   `json:"downloadedBytes"`
	TotalBytes      int64   `json:"totalBytes"`
	Speed           float64 `json:"speed"`
	ETA             float64 `json:"eta"`
}

// mcAsyncTask 一个异步任务（当前只实现快速安装）。
type mcAsyncTask struct {
	mu       sync.Mutex
	id       string
	typ      string
	instance string
	status   int
	errMsg   string
	progress mcAsyncDownloadProgress
	finished time.Time
	cancel   context.CancelFunc
}

// statusOf 读取状态。
func (t *mcAsyncTask) statusOf() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// setStatus 更新状态。
func (t *mcAsyncTask) setStatus(s int) {
	t.mu.Lock()
	t.status = s
	if s == mcAsyncStatusStop || s == mcAsyncStatusError {
		t.finished = timesys.Now()
	}
	t.mu.Unlock()
}

// setProgress 更新下载进度。
func (t *mcAsyncTask) setProgress(p mcAsyncDownloadProgress) {
	t.mu.Lock()
	t.progress = p
	t.mu.Unlock()
}

// toObject 面板轮询时读的 detail（字段与 QuickInstallTask.toObject 一致）。
func (t *mcAsyncTask) toObject(mc *mcsmCompat) map[string]any {
	t.mu.Lock()
	status := t.status
	errMsg := t.errMsg
	progress := t.progress
	t.mu.Unlock()
	out := map[string]any{
		"taskId":           t.id,
		"status":           status,
		"instanceUuid":     t.instance,
		"instanceStatus":   0,
		"instanceConfig":   nil,
		"downloadProgress": progress,
	}
	if inst := mc.d.Find(t.instance); inst != nil {
		inst.mu.Lock()
		out["instanceStatus"] = inst.Status
		out["instanceConfig"] = inst.Config
		inst.mu.Unlock()
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

// registerAsyncTask 登记任务（同实例只保留一个正在运行的任务）。
func (mc *mcsmCompat) registerAsyncTask(t *mcAsyncTask, cancel context.CancelFunc) error {
	mc.asyncMu.Lock()
	defer mc.asyncMu.Unlock()
	if mc.asyncTasks == nil {
		mc.asyncTasks = map[string]*mcAsyncTask{}
	}
	now := timesys.Now()
	for id, old := range mc.asyncTasks {
		// 清理过期已结束任务
		old.mu.Lock()
		done := old.status != mcAsyncStatusRunning
		finished := old.finished
		old.mu.Unlock()
		if done && !finished.IsZero() && now.Sub(finished) > mcAsyncTaskTTL {
			delete(mc.asyncTasks, id)
		}
	}
	for _, old := range mc.asyncTasks {
		old.mu.Lock()
		running := old.status == mcAsyncStatusRunning
		sameInstance := old.instance == t.instance
		old.mu.Unlock()
		if running && sameInstance {
			return errors.New("该实例已有正在执行的异步任务，请等待完成或先停止")
		}
	}
	t.cancel = cancel
	mc.asyncTasks[t.id] = t
	return nil
}

// asyncTaskByID 按 id 取任务。
func (mc *mcsmCompat) asyncTaskByID(id string) *mcAsyncTask {
	mc.asyncMu.Lock()
	defer mc.asyncMu.Unlock()
	return mc.asyncTasks[id]
}

// asyncTasksByType 取某类型的全部任务。
func (mc *mcsmCompat) asyncTasksByType(typ string) []*mcAsyncTask {
	mc.asyncMu.Lock()
	defer mc.asyncMu.Unlock()
	out := make([]*mcAsyncTask, 0, len(mc.asyncTasks))
	for _, t := range mc.asyncTasks {
		if t.typ == typ {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 事件处理
// ---------------------------------------------------------------------------

// mcEventAsyncStart 处理 instance/asynchronous。
func mcEventAsyncStart(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string          `json:"instanceUuid"`
		TaskName     string          `json:"taskName"`
		Role         json.RawMessage `json:"role"`
		Parameter    struct {
			NewInstanceName string          `json:"newInstanceName"`
			TargetLink      string          `json:"targetLink"`
			SetupInfo       json.RawMessage `json:"setupInfo"`
			TaskID          string          `json:"taskId"`
			URL             string          `json:"url"`
		} `json:"parameter"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(req.TaskName)) {
	case "quick_install":
		return mc.startQuickInstall(req.Parameter.NewInstanceName, req.Parameter.TargetLink, req.Parameter.SetupInfo)
	case "install_instance":
		// 已存在的实例内安装（下载模板包解压到实例目录）
		if strings.TrimSpace(req.Parameter.TargetLink) == "" && strings.TrimSpace(req.Parameter.URL) == "" {
			return nil, errors.New("install_instance 需要 targetLink（模板包下载地址）")
		}
		link := req.Parameter.TargetLink
		if link == "" {
			link = req.Parameter.URL
		}
		return mc.startInstallIntoInstance(req.InstanceUUID, link)
	case "update":
		return mc.startInstanceUpdate(req.InstanceUUID)
	default:
		return nil, fmt.Errorf("本节点不支持的异步任务 %q（支持 quick_install / install_instance / update）", req.TaskName)
	}
}

// startQuickInstall 快速安装：下载模板包 → 解压 → 建实例。
func (mc *mcsmCompat) startQuickInstall(name, targetLink string, setupInfo json.RawMessage) (any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("模板安装缺少实例名（newInstanceName）")
	}
	if strings.TrimSpace(targetLink) == "" {
		return nil, errors.New("模板安装缺少下载地址（targetLink）")
	}
	// 模板包下载与解压都放在实例目录的**父目录**下进行，避免实例 cwd 校验冲突：
	// 本节点不允许实例工作目录落在节点数据目录内。
	base := filepath.Join(filepath.Dir(mc.d.DataDir), "instance-data", sanitizeInstanceDir(name))
	cwd := base
	cfg := InstanceConfig{
		Nickname:     name,
		Cwd:          cwd,
		StopCommand:  "^c",
		StartCommand: "java -jar server.jar nogui",
		Type:         "universal",
		IE:           "utf-8",
		OE:           "utf-8",
	}
	if len(setupInfo) > 0 {
		var in mcConfigInput
		if err := json.Unmarshal(setupInfo, &in); err == nil {
			in.apply(&cfg)
		}
	}
	inst := NewInstance(newUUID(), cfg)
	inst.Config.CreateDatetime = timesys.Now().Unix()
	if err := mc.d.Add(inst); err != nil {
		return nil, fmt.Errorf("创建实例记录失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := &mcAsyncTask{
		id:       fmt.Sprintf("QuickInstallTask-%s-%s", inst.InstanceUuid, newUUID()),
		typ:      "QuickInstallTask",
		instance: inst.InstanceUuid,
		status:   mcAsyncStatusRunning,
	}
	if err := mc.registerAsyncTask(task, cancel); err != nil {
		_ = mc.d.Remove(inst.InstanceUuid, false)
		cancel()
		return nil, err
	}
	safeGo("MCSM 兼容层快速安装", func() {
		mc.runQuickInstall(ctx, task, inst, targetLink)
	})
	return task.toObject(mc), nil
}

// runQuickInstall 执行下载 + 解压 + 配置。
func (mc *mcsmCompat) runQuickInstall(ctx context.Context, task *mcAsyncTask, inst *Instance, targetLink string) {
	defer func() {
		if r := recover(); r != nil {
			task.mu.Lock()
			task.errMsg = fmt.Sprintf("安装过程异常: %v", r)
			task.mu.Unlock()
			task.setStatus(mcAsyncStatusError)
		}
	}()
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		mc.failAsyncTask(task, fmt.Errorf("创建实例目录失败: %w", err))
		return
	}
	packPath := filepath.Join(cwd, "mcsm_install_package.zip")
	if err := mc.downloadWithProgress(ctx, task, targetLink, packPath); err != nil {
		mc.failAsyncTask(task, err)
		return
	}
	if err := extractZipInto(packPath, cwd); err != nil {
		_ = os.Remove(packPath)
		mc.failAsyncTask(task, fmt.Errorf("解压模板包失败: %w", err))
		return
	}
	_ = os.Remove(packPath)

	// 包内自带 mcsmanager-config.json 时以它为准（与真实 daemon 一致）
	cfgPath := filepath.Join(cwd, "mcsmanager-config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		var in mcConfigInput
		if err := json.Unmarshal(data, &in); err == nil {
			inst.mu.Lock()
			in.apply(&inst.Config)
			inst.mu.Unlock()
			mc.d.markDirty()
		}
	}
	task.setProgress(mcAsyncDownloadProgress{Percentage: 100, DownloadedBytes: task.progress.TotalBytes, TotalBytes: task.progress.TotalBytes})
	task.setStatus(mcAsyncStatusStop)
	alog.Printf("MCSM 兼容层：快速安装完成（实例 %s，模板 %s）", inst.Config.Nickname, targetLink)
}

// failAsyncTask 标记失败。
func (mc *mcsmCompat) failAsyncTask(task *mcAsyncTask, err error) {
	task.mu.Lock()
	task.errMsg = err.Error()
	task.mu.Unlock()
	task.setStatus(mcAsyncStatusError)
	alog.Printf("MCSM 兼容层：异步任务 %s 失败: %v", task.id, err)
}

// startInstallIntoInstance 在已有实例内安装模板包。
func (mc *mcsmCompat) startInstallIntoInstance(uuid, link string) (any, error) {
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", uuid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := &mcAsyncTask{
		id:       fmt.Sprintf("QuickInstallTask-%s-%s", uuid, newUUID()),
		typ:      "QuickInstallTask",
		instance: uuid,
		status:   mcAsyncStatusRunning,
	}
	if err := mc.registerAsyncTask(task, cancel); err != nil {
		cancel()
		return nil, err
	}
	safeGo("MCSM 兼容层实例内安装", func() {
		inst.mu.Lock()
		cwd := inst.Config.Cwd
		inst.mu.Unlock()
		packPath := filepath.Join(cwd, "mcsm_install_package.zip")
		if err := mc.downloadWithProgress(ctx, task, link, packPath); err != nil {
			mc.failAsyncTask(task, err)
			return
		}
		if err := extractZipInto(packPath, cwd); err != nil {
			_ = os.Remove(packPath)
			mc.failAsyncTask(task, fmt.Errorf("解压模板包失败: %w", err))
			return
		}
		_ = os.Remove(packPath)
		task.setStatus(mcAsyncStatusStop)
	})
	return task.toObject(mc), nil
}

// startInstanceUpdate 触发实例的 updateCommand（本节点没有预设体系，直接执行命令）。
func (mc *mcsmCompat) startInstanceUpdate(uuid string) (any, error) {
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", uuid)
	}
	inst.mu.Lock()
	cmd := strings.TrimSpace(inst.Config.UpdateCommand)
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if cmd == "" {
		return nil, errors.New("该实例没有配置 updateCommand，无法执行更新任务（可在实例设置里填写更新命令）")
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := &mcAsyncTask{
		id:       fmt.Sprintf("UpdateTask-%s-%s", uuid, newUUID()),
		typ:      "QuickInstallTask",
		instance: uuid,
		status:   mcAsyncStatusRunning,
	}
	if err := mc.registerAsyncTask(task, cancel); err != nil {
		cancel()
		return nil, err
	}
	safeGo("MCSM 兼容层实例更新", func() {
		if err := runShellContext(ctx, cwd, cmd); err != nil {
			mc.failAsyncTask(task, fmt.Errorf("执行更新命令失败: %w", err))
			return
		}
		task.setStatus(mcAsyncStatusStop)
	})
	return task.toObject(mc), nil
}

// mcEventAsyncQuery 处理 instance/query_asynchronous。
func mcEventAsyncQuery(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		TaskName  string `json:"taskName"`
		Parameter struct {
			TaskID string `json:"taskId"`
		} `json:"parameter"`
	}
	_ = decode(data, &req)
	if id := strings.TrimSpace(req.Parameter.TaskID); id != "" {
		task := mc.asyncTaskByID(id)
		if task == nil {
			return nil, fmt.Errorf("异步任务 %s 不存在或已过期", id)
		}
		return map[string]any{
			"taskId": task.id,
			"status": task.statusOf(),
			"detail": task.toObject(mc),
		}, nil
	}
	tasks := mc.asyncTasksByType("QuickInstallTask")
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, map[string]any{
			"taskId": t.id,
			"status": t.statusOf(),
			"detail": t.toObject(mc),
		})
	}
	return out, nil
}

// mcEventAsyncStop 处理 instance/stop_asynchronous。
func mcEventAsyncStop(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Parameter    struct {
			TaskID string `json:"taskId"`
		} `json:"parameter"`
	}
	_ = decode(data, &req)
	var target *mcAsyncTask
	if id := strings.TrimSpace(req.Parameter.TaskID); id != "" {
		target = mc.asyncTaskByID(id)
	} else {
		for _, t := range mc.asyncTasksByType("QuickInstallTask") {
			if t.instance == req.InstanceUUID {
				target = t
				break
			}
		}
	}
	if target == nil {
		return nil, errors.New("没有正在执行的异步任务")
	}
	target.mu.Lock()
	cancel := target.cancel
	target.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	target.setStatus(mcAsyncStatusStop)
	return true, nil
}

// downloadWithProgress 带进度回调的下载（复用节点的 SSRF 拨号守卫）。
func (mc *mcsmCompat) downloadWithProgress(ctx context.Context, task *mcAsyncTask, rawURL, dest string) error {
	if err := validateCoreURL(rawURL); err != nil {
		return err
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: mc.d.coreDownloadDialContext()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载模板包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载模板包失败: HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	start := time.Now()
	var written int64
	buf := make([]byte, 256<<10)
	for {
		if ctx.Err() != nil {
			f.Close()
			_ = os.Remove(tmp)
			return errors.New("任务已取消")
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				_ = os.Remove(tmp)
				return werr
			}
			written += int64(n)
			p := mcAsyncDownloadProgress{DownloadedBytes: written, TotalBytes: total}
			if total > 0 {
				p.Percentage = int(written * 100 / total)
			}
			if elapsed := time.Since(start).Seconds(); elapsed > 0.2 {
				p.Speed = float64(written) / elapsed
				if p.Speed > 0 && total > written {
					p.ETA = float64(total-written) / p.Speed
				}
			}
			task.setProgress(p)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("下载中断: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// runShellContext 在指定目录执行一条 shell 命令（可取消）。
func runShellContext(ctx context.Context, dir, command string) error {
	return runShellInDirContext(ctx, dir, command)
}
