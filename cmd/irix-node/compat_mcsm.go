// MCSManager Daemon 兼容层：事件分发与实例/信息类事件实现。
//
// 事件语义对齐 MCSManager daemon 的 socket.io 路由（同名事件、同名字段），
// 但处理逻辑全部落到本节点自身能力上（d.startInstance、d.stopInstance、
// tickets、NormalizePath、sysinfo、timesys……）。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"irix/node/internal/sysinfo"
	"irix/node/internal/timesys"
)

// mcMission 一次性的直连/控制台通行证（对齐 MCSM 的 mission passport 语义）。
type mcMission struct {
	name      string         // download | upload | stream_channel
	parameter map[string]any // 任务参数（instanceUuid / fileName / uploadDir …）
	expires   time.Time
}

// mcEventFunc 事件处理器：返回回包数据；error 表示执行失败。
type mcEventFunc func(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error)

// mcEventHandlers 事件表。表外事件一律回「不支持」，避免面板误以为成功。
var mcEventHandlers = map[string]mcEventFunc{
	"auth":          mcEventAuth,
	"info/overview": mcEventInfoOverview,
	"info/setting":  mcEventInfoSetting,

	"instance/select":    mcEventInstanceSelect,
	"instance/overview":  mcEventInstanceOverview,
	"instance/section":   mcEventInstanceSection,
	"instance/detail":    mcEventInstanceDetail,
	"instance/new":       mcEventInstanceNew,
	"instance/update":    mcEventInstanceUpdate,
	"instance/delete":    mcEventInstanceDelete,
	"instance/open":      mcEventInstanceOpen,
	"instance/stop":      mcEventInstanceStop,
	"instance/restart":   mcEventInstanceRestart,
	"instance/kill":      mcEventInstanceKill,
	"instance/command":   mcEventInstanceCommand,
	"instance/forward":   mcEventInstanceForward,
	"instance/outputlog": mcEventInstanceOutputLog,

	"instance/process_config/list": mcEventProcessConfigList,
	"instance/process_config/file": mcEventProcessConfigFile,

	"instance/asynchronous":       mcEventAsyncUnsupported,
	"instance/query_asynchronous": mcEventAsyncQuery,
	"instance/stop_asynchronous":  mcEventAsyncStop,

	"passport/register": mcEventPassportRegister,

	"stream/auth":   mcEventStreamAuth,
	"stream/detail": mcEventStreamDetail,
	"stream/input":  mcEventStreamInput,
	"stream/write":  mcEventStreamWrite,
	"stream/resize": mcEventStreamResize,

	"java_manager/list":     mcEventJavaList,
	"java_manager/add":      mcEventJavaUnsupported,
	"java_manager/download": mcEventJavaUnsupported,
	"java_manager/delete":   mcEventJavaUnsupported,
	"java_manager/using":    mcEventJavaUnsupported,

	"upgrade/info":   mcEventUpgradeInfo,
	"upgrade/daemon": mcEventUpgradeUnsupported,

	"schedule/register": mcEventScheduleRegister,
	"schedule/list":     mcEventScheduleList,
	"schedule/delete":   mcEventScheduleDelete,

	// 文件管理（实现在 compat_mcsm_file.go）
	"file/list":                   mcEventFileList,
	"file/status":                 mcEventFileStatus,
	"file/touch":                  mcEventFileTouch,
	"file/mkdir":                  mcEventFileMkdir,
	"file/delete":                 mcEventFileDelete,
	"file/edit":                   mcEventFileEdit,
	"file/copy":                   mcEventFileCopy,
	"file/move":                   mcEventFileMove,
	"file/compress":               mcEventFileCompress,
	"file/chmod":                  mcEventFileChmodUnsupported,
	"file/chmod_batch":            mcEventFileChmodUnsupported,
	"file/download_from_url":      mcEventFileDownloadFromURL,
	"file/download_from_url_stop": mcEventFileDownloadFromURLStop,
}

// dispatch 事件入口：认证门禁 + 分发 + 回包。
func (s *eioSession) dispatch(event, uuid string, data json.RawMessage) {
	// 认证门禁：除 auth 与 stream/auth 外，未认证会话一律拒绝（与 MCSM 一致）。
	if event != "auth" && event != "stream/auth" {
		s.mu.Lock()
		authed := s.authed
		s.mu.Unlock()
		if !authed {
			s.replyErr(event, uuid, errors.New("未通过节点密钥认证，拒绝访问"))
			return
		}
	}
	h, ok := mcEventHandlers[event]
	if !ok {
		s.replyErr(event, uuid, fmt.Errorf("本节点不支持事件 %s", event))
		return
	}
	res, err := h(s.srv, s, data)
	if err != nil {
		s.replyErr(event, uuid, err)
		return
	}
	s.reply(event, uuid, res)
}

// mcEventAuth 节点密钥认证。载荷为裸字符串（面板直接发 apiKey）。
func mcEventAuth(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	key := datasString(data)
	if !subtleEqual(key, mc.key) {
		alog.Printf("MCSManager 兼容层：来源 %s 密钥错误，认证失败", s.peerHint())
		return nil, errors.New("节点密钥错误")
	}
	s.mu.Lock()
	s.authed = true
	s.mu.Unlock()
	mc.ensureWatcher()
	alog.Printf("MCSManager 兼容层：来源 %s 认证成功", s.peerHint())
	return true, nil
}

// peerHint 返回会话对端描述（日志用）。
func (s *eioSession) peerHint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws != nil && s.ws.conn != nil {
		return s.ws.conn.RemoteAddr().String()
	}
	return "轮询会话 " + s.sid
}

// subtleEqual 常量时间比较两个字符串（避免密钥被计时侧信道猜测）。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// mcEventInfoOverview 节点概览（面板「节点信息」页）。
func mcEventInfoOverview(mc *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	d := mc.d
	mc.ensureChart()
	instances := d.mcAllInstanceDetails()
	running := d.CountRunning()
	return map[string]any{
		"version": mc.reportVersion(),
		"process": map[string]any{
			"cpu":    0,
			"memory": sysinfo.ProcessAllocCached(),
			"cwd":    d.DataDir,
		},
		"instance": map[string]any{
			"running": running,
			"total":   len(instances),
		},
		"system": mcSystemView(d),
		// 与 MCSM VisualDataSubsystem.getSystemChartArray() 完全同形：
		// 200 个 {cpu, mem} 整数百分比，3 秒一个采样点。
		"cpuMemChart": mc.chartSnapshot(),
		"config": map[string]any{
			"language":                    "zh_cn",
			"uploadSpeedRate":             0,
			"downloadSpeedRate":           0,
			"maxDownloadFromUrlFileCount": 0,
			"portRangeStart":              10010,
			"portRangeEnd":                65500,
			"portAssignInterval":          5,
			"port":                        d.Port,
			"outputBufferSize":            256,
			"enableSoftShutdown":          true,
			"softShutdownSkipDocker":      true,
			"softShutdownWaitSeconds":     30,
		},
		"dockerPlatforms": nil,
	}, nil
}

// mcEventInfoSetting 面板下发节点设置：本节点只接受并忽略（无对应开关）。
func mcEventInfoSetting(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return true, nil
}

// mcEventInstanceSelect 分页 + 条件查询实例列表。
func mcEventInstanceSelect(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		Page      int `json:"page"`
		PageSize  int `json:"pageSize"`
		Condition struct {
			InstanceName string   `json:"instanceName"`
			Status       string   `json:"status"`
			Tag          []string `json:"tag"`
		} `json:"condition"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize < 1 {
		pageSize = 40
	}
	if pageSize > 100 {
		pageSize = 100
	}
	all := mc.d.mcAllInstanceDetails()
	keyword := strings.ToLower(strings.TrimSpace(req.Condition.InstanceName))
	wantStatus := strings.TrimSpace(req.Condition.Status)
	tags := req.Condition.Tag

	filtered := make([]map[string]any, 0, len(all))
	tagSet := map[string]struct{}{}
	for _, item := range all {
		if keyword != "" {
			name := strings.ToLower(fmt.Sprint(item["config"].(map[string]any)["nickname"]))
			if !strings.Contains(name, keyword) {
				continue
			}
		}
		if wantStatus != "" {
			if fmt.Sprint(item["status"]) != wantStatus {
				continue
			}
		}
		cfgTags, _ := item["config"].(map[string]any)["tag"].([]string)
		for _, t := range cfgTags {
			tagSet[t] = struct{}{}
		}
		if len(tags) > 0 {
			hit := 0
			for _, want := range tags {
				for _, have := range cfgTags {
					if have == want {
						hit++
						break
					}
				}
			}
			if hit != len(tags) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	allTags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		allTags = append(allTags, t)
	}
	sort.Strings(allTags)
	if len(allTags) > 60 {
		allTags = allTags[:60]
	}
	total := len(filtered)
	maxPage := (total + pageSize - 1) / pageSize
	if maxPage < 1 {
		maxPage = 1
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return map[string]any{
		"page":     page,
		"pageSize": pageSize,
		"maxPage":  maxPage,
		"allTags":  allTags,
		"data":     filtered[start:end],
	}, nil
}

// mcEventInstanceOverview 全部实例概览。
func mcEventInstanceOverview(mc *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return mc.d.mcAllInstanceDetails(), nil
}

// mcEventInstanceSection 指定 uuid 集合的实例概览。
func mcEventInstanceSection(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUIDs []string `json:"instanceUuids"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	return mc.d.mcInstanceSection(req.InstanceUUIDs), nil
}

// mcEventInstanceDetail 单个实例详情。
func mcEventInstanceDetail(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	inst, err := mc.requireInstance(data, "instanceUuid")
	if err != nil {
		return nil, err
	}
	detail := mcInstanceDetail(inst)
	detail["space"] = int64(0)
	detail["processInfo"] = inst.Detail()["processInfo"]
	return detail, nil
}

// requireInstance 按字段名从载荷取实例 uuid 并查找实例。
func (mc *mcsmCompat) requireInstance(data json.RawMessage, field string) (*Instance, error) {
	var req map[string]json.RawMessage
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	var uuid string
	if raw, ok := req[field]; ok {
		_ = json.Unmarshal(raw, &uuid)
	}
	if strings.TrimSpace(uuid) == "" {
		return nil, fmt.Errorf("缺少 %s 参数", field)
	}
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", uuid)
	}
	return inst, nil
}

// mcEventInstanceNew 创建实例。载荷即 MCSM 的实例配置对象。
func mcEventInstanceNew(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var in mcConfigInput
	if err := json.Unmarshal(rawData(data), &in); err != nil {
		return nil, fmt.Errorf("实例配置格式错误: %w", err)
	}
	cfg := InstanceConfig{}
	in.apply(&cfg)
	cfg.FillDefaults()
	if strings.TrimSpace(cfg.Nickname) == "" {
		cfg.Nickname = "未命名实例"
	}
	mc.d.applyVaultDefault(&cfg)
	abs, err := mc.d.normalizeInstanceCwd(cfg.Cwd)
	if err != nil {
		return nil, err
	}
	cfg.Cwd = abs
	// 与 MCSM 一致：创建时补齐工作目录（失败不阻断，启动时会再校验）
	if err := os.MkdirAll(abs, 0o755); err != nil {
		alog.Printf("MCSManager 兼容层：创建实例目录 %s 失败: %v", abs, err)
	}
	cfg.CreateDatetime = timesys.Now().UnixMilli()
	cfg.LastDatetime = cfg.CreateDatetime
	inst := NewInstance("", cfg)
	if err := mc.d.Add(inst); err != nil {
		return nil, fmt.Errorf("保存实例失败: %w", err)
	}
	mc.broadcast("instance/overview", mc.d.mcAllInstanceDetails())
	return map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"config":       mcConfigView(cfg),
		"nickname":     cfg.Nickname,
	}, nil
}

// mcEventInstanceUpdate 更新实例配置（按面板下发的字段增量合并）。
func mcEventInstanceUpdate(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string          `json:"instanceUuid"`
		Config       json.RawMessage `json:"config"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	inst := mc.d.Find(req.InstanceUUID)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", req.InstanceUUID)
	}
	var in mcConfigInput
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &in); err != nil {
			return nil, fmt.Errorf("实例配置格式错误: %w", err)
		}
	}
	inst.mu.Lock()
	cfg := inst.Config
	inst.mu.Unlock()
	in.apply(&cfg)
	cfg.FillDefaults()
	abs, err := mc.d.normalizeInstanceCwd(cfg.Cwd)
	if err != nil {
		return nil, err
	}
	cfg.Cwd = abs
	if err := mc.d.UpdateInstance(inst, cfg); err != nil {
		return nil, err
	}
	mc.broadcast("instance/overview", mc.d.mcAllInstanceDetails())
	return map[string]any{"instanceUuid": req.InstanceUUID}, nil
}

// mcEventInstanceDelete 删除实例。
func mcEventInstanceDelete(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUIDs []string `json:"instanceUuids"`
		DeleteFile    bool     `json:"deleteFile"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	instances := make([]map[string]any, 0, len(req.InstanceUUIDs))
	for _, uuid := range req.InstanceUUIDs {
		inst := mc.d.Find(uuid)
		if inst == nil {
			continue
		}
		inst.mu.Lock()
		status := inst.Status
		nickname := inst.Config.Nickname
		inst.mu.Unlock()
		if status != StatusStopped {
			return nil, fmt.Errorf("实例 %s 正在运行，请先停止再删除", nickname)
		}
		instances = append(instances, map[string]any{"instanceUuid": uuid, "nickname": nickname})
	}
	for _, uuid := range req.InstanceUUIDs {
		if err := mc.d.Remove(uuid, req.DeleteFile); err != nil {
			return nil, err
		}
	}
	mc.broadcast("instance/overview", mc.d.mcAllInstanceDetails())
	return map[string]any{"instanceUuids": req.InstanceUUIDs, "instances": instances}, nil
}

// mcInstanceBatch 批处理启动/停止类事件（同 MCSM：逐个执行、逐个回包）。
func mcInstanceBatch(mc *mcsmCompat, s *eioSession, data json.RawMessage, event string) (any, error) {
	var req struct {
		InstanceUUIDs   []string `json:"instanceUuids"`
		DisableResponse bool     `json:"disableResponse"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if len(req.InstanceUUIDs) == 0 {
		return nil, errors.New("缺少 instanceUuids 参数")
	}
	instances := make([]map[string]any, 0, len(req.InstanceUUIDs))
	var lastErr error
	for _, uuid := range req.InstanceUUIDs {
		inst := mc.d.Find(uuid)
		nickname := ""
		if inst != nil {
			inst.mu.Lock()
			nickname = inst.Config.Nickname
			inst.mu.Unlock()
		}
		instances = append(instances, map[string]any{"instanceUuid": uuid, "nickname": nickname})
		if inst == nil {
			lastErr = fmt.Errorf("实例 %s 不存在", uuid)
			continue
		}
		var err error
		switch event {
		case "instance/open":
			err = mc.d.startInstance(inst)
			if err == nil {
				inst.mu.Lock()
				inst.Started++
				inst.mu.Unlock()
				mc.d.markDirty()
			}
		case "instance/stop":
			err = mc.d.stopInstance(inst)
			if errors.Is(err, errNotRunning) {
				err = nil
			}
		case "instance/restart":
			err = mc.d.stopInstance(inst)
			if err != nil && !errors.Is(err, errNotRunning) {
				break
			}
			err = mc.d.startInstance(inst)
		case "instance/kill":
			err = mc.killInstance(inst)
		}
		if err != nil {
			lastErr = err
			if !req.DisableResponse {
				s.push(event, map[string]any{"instanceUuid": uuid, "nickname": nickname, "err": err.Error()})
			}
			continue
		}
		if !req.DisableResponse {
			s.push(event, map[string]any{"instanceUuid": uuid, "instances": instances})
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return map[string]any{"instanceUuid": req.InstanceUUIDs[0], "instances": instances}, nil
}

// killInstance 强制终止实例（实现见 Daemon.compatKillInstance，两层共用）。
func (mc *mcsmCompat) killInstance(inst *Instance) error {
	return mc.d.compatKillInstance(inst)
}

func mcEventInstanceOpen(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	return mcInstanceBatch(mc, s, data, "instance/open")
}

func mcEventInstanceStop(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	return mcInstanceBatch(mc, s, data, "instance/stop")
}

func mcEventInstanceRestart(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	return mcInstanceBatch(mc, s, data, "instance/restart")
}

func mcEventInstanceKill(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	return mcInstanceBatch(mc, s, data, "instance/kill")
}

// mcEventInstanceCommand 向实例下发标准输入命令。
func mcEventInstanceCommand(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Command      string `json:"command"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	inst := mc.d.Find(req.InstanceUUID)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", req.InstanceUUID)
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		return nil, errors.New("实例未在运行")
	}
	cmd := strings.TrimRight(req.Command, "\r\n")
	if err := proc.WriteCommand(cmd); err != nil {
		return nil, err
	}
	return map[string]any{"instanceUuid": req.InstanceUUID}, nil
}

// mcEventInstanceForward 订阅/取消订阅实例输出（实时控制台）。
func mcEventInstanceForward(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Forward      bool   `json:"forward"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	inst := mc.d.Find(req.InstanceUUID)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", req.InstanceUUID)
	}
	if req.Forward {
		mc.forwardOutput(inst, s)
	} else {
		s.stopForward(inst.InstanceUuid)
	}
	return map[string]any{"instanceUuid": req.InstanceUUID}, nil
}

// forwardOutput 把实例输出转发为 instance/stdout 推送。
func (mc *mcsmCompat) forwardOutput(inst *Instance, s *eioSession) {
	s.mu.Lock()
	if _, ok := s.forwards[inst.InstanceUuid]; ok {
		s.mu.Unlock()
		return
	}
	s.forwards[inst.InstanceUuid] = struct{}{}
	s.mu.Unlock()

	// 停止信号：面板取消转发（instance/forward=false）或会话关闭时触发。
	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	s.addCleanup(closeStop)

	safeGo("MCSM 兼容层输出转发", func() {
		defer recoverLog("MCSM 兼容层输出转发")
		for {
			select {
			case <-stop:
				return
			default:
			}
			inst.mu.Lock()
			proc := inst.Proc
			inst.mu.Unlock()
			if proc == nil || !proc.IsRunning() {
				// 实例未运行：**守在这里等它启动**。
				// 面板在控制台已打开时不会重新 stream/auth，若此处直接返回，
				// 用户从面板启动实例后就再也收不到实时输出，只能退出控制台
				// 再进一次（重新 auth）才看得到内容——正是线上反馈的现象。
				select {
				case <-stop:
					return
				case <-time.After(mcOutputForwardPoll):
				}
				continue
			}
			ch, cancel := proc.Subscribe()
			procDone := proc.done
			// 进程退出或会话关闭都结束本轮推送
			procStop := make(chan struct{})
			go func() {
				defer recoverLog("MCSM 兼容层输出转发回收")
				select {
				case <-procDone:
				case <-stop:
				}
				close(procStop)
			}()
			mcPumpOutputSelect(ch, procStop, func(text string) {
				s.push("instance/stdout", map[string]any{
					"instanceUuid": inst.InstanceUuid,
					"text":         text,
				})
			})
			cancel()
			select {
			case <-stop:
				return
			case <-procDone:
				// 进程退出：若实例自动重启，下一轮循环会重新订阅新进程
			}
		}
	})
}

// mcOutputFrameMax 单帧输出上限（字节）。
//
// 合并推送的上限：既避免高输出速率把帧数推爆，也避免超大帧占满面板内存。
const mcOutputFrameMax = 16 << 10

// mcOutputFlushInterval 输出合并窗口。
//
// 逐行推一帧会让高输出速率的服务端把面板冲垮：实测一台狂刷日志的实例
// 3 秒内产生近 5 万帧（面板端表现为忽而卡死、忽而一次性涌出，很不流畅），
// 而真实 MCSM daemon 推的是 stdout 数据块。这里按 30ms 窗口合并：
// 高输出时每窗口一帧（约 33 帧/秒，终端观感即时），积压超过 16KB 立即发，
// 低输出时的延迟最坏只有 30ms。
const mcOutputFlushInterval = 30 * time.Millisecond

// mcOutputForwardPoll 控制台待启动时的轮询间隔（仅在有控制台订阅时生效）。
const mcOutputForwardPoll = 250 * time.Millisecond

// mcPumpOutput 把订阅通道里的输出行按窗口合并后交给 pushFn 推送。
//
// 保证：不丢行、顺序不变、单帧不超过 mcOutputFrameMax 字节、通道关闭时冲刷剩余内容。
func mcPumpOutput(ch <-chan string, pushFn func(text string)) {
	mcPumpOutputSelect(ch, nil, pushFn)
}

// mcPumpOutputSelect 同 mcPumpOutput，但可用 stop 提前结束（nil 表示只看通道关闭）。
//
// 进程退出时订阅通道不会关闭（Subscribe 只登记/注销订阅者），因此控制台转发
// 需要 stop 通道来收尾并冲刷剩余内容。
func mcPumpOutputSelect(ch <-chan string, stop <-chan struct{}, pushFn func(text string)) {
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		pushFn(b.String())
		b.Reset()
	}
	t := time.NewTicker(mcOutputFlushInterval)
	defer t.Stop()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				flush()
				return
			}
			b.WriteString(line)
			b.WriteByte('\n')
			if b.Len() >= mcOutputFrameMax {
				flush()
			}
		case <-t.C:
			flush()
		case <-stop:
			flush()
			return
		}
	}
}

// stopForward 取消某个实例的输出转发。
func (s *eioSession) stopForward(uuid string) {
	s.mu.Lock()
	delete(s.forwards, uuid)
	s.mu.Unlock()
}

// mcEventInstanceOutputLog 返回实例控制台历史日志（内存环形缓冲）。
func mcEventInstanceOutputLog(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	inst, err := mc.requireInstance(data, "instanceUuid")
	if err != nil {
		return nil, err
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil {
		return "", nil
	}
	return proc.Log.Tail(0), nil
}

// mcEventProcessConfigList 检查实例工作目录下哪些配置文件存在。
func mcEventProcessConfigList(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string   `json:"instanceUuid"`
		Files        []string `json:"files"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(req.Files))
	for _, f := range req.Files {
		abs, err := safeResolve(cwd, f)
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && !st.IsDir() {
			out = append(out, map[string]any{"file": f, "check": true})
		}
	}
	return out, nil
}

// mcEventProcessConfigFile 读取/写入实例配置文件。
// 支持 properties / json / txt / yml / toml 中的「纯文本直传」子集：
// 面板传入 JSON 对象时按类型序列化，传入字符串时原样写入。
func mcEventProcessConfigFile(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string          `json:"instanceUuid"`
		FileName     string          `json:"fileName"`
		Type         string          `json:"type"`
		Config       json.RawMessage `json:"config"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.FileName) == "" {
		return nil, errors.New("缺少 fileName 参数")
	}
	abs, err := safeResolve(cwd, req.FileName)
	if err != nil {
		return nil, err
	}
	if len(req.Config) == 0 || string(req.Config) == "null" {
		content, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
		switch strings.ToLower(req.Type) {
		case "json":
			var v any
			if err := json.Unmarshal(content, &v); err != nil {
				return nil, fmt.Errorf("配置文件不是合法 JSON: %w", err)
			}
			return v, nil
		case "properties", "properties_not_unicode":
			return mcParseProperties(string(content)), nil
		default:
			return string(content), nil
		}
	}
	// 写入
	var text string
	if err := json.Unmarshal(req.Config, &text); err != nil {
		// 非字符串：JSON 对象按缩进写出（properties/yml/toml 的完整序列化
		// 由面板侧负责，这里只保证 JSON 类型可用）。
		var obj any
		if err2 := json.Unmarshal(req.Config, &obj); err2 != nil {
			return nil, fmt.Errorf("配置内容格式错误: %w", err2)
		}
		buf, _ := json.MarshalIndent(obj, "", "  ")
		text = string(buf)
	}
	if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
		return nil, fmt.Errorf("写入配置文件失败: %w", err)
	}
	return true, nil
}

// mcParseProperties 解析 Java properties 文本为 JSON 对象（简版）。
func mcParseProperties(text string) map[string]any {
	out := map[string]any{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		idx := strings.IndexAny(line, "=:")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		out[key] = val
	}
	return out
}

// mcEventAsyncUnsupported 复杂异步任务（快速安装/更新）本节点不支持。
func mcEventAsyncUnsupported(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return nil, errors.New("本节点不支持 MCSManager 的快速安装/更新异步任务")
}

// mcEventAsyncQuery 异步任务查询：本节点不产生该类任务，返回空列表。
func mcEventAsyncQuery(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return []any{}, nil
}

// mcEventAsyncStop 停止异步任务：无任务可停，直接成功。
func mcEventAsyncStop(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return true, nil
}

// mcEventPassportRegister 注册一次性通行证（下载/上传/控制台）。
func mcEventPassportRegister(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		Name      string         `json:"name"`
		Password  string         `json:"password"`
		Parameter map[string]any `json:"parameter"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Password) == "" {
		return nil, errors.New("name 与 password 不能为空")
	}
	mc.mu.Lock()
	if mc.missions == nil {
		mc.missions = map[string]*mcMission{}
	}
	mc.missions[req.Password] = &mcMission{
		name:      req.Name,
		parameter: req.Parameter,
		expires:   timesys.Now().Add(time.Hour),
	}
	mc.mu.Unlock()
	return true, nil
}

// takeMission 取通行证（不消费，便于断线重连；过期即失效）。
func (mc *mcsmCompat) takeMission(password, name string) *mcMission {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	m := mc.missions[password]
	if m == nil || m.name != name || timesys.Now().After(m.expires) {
		return nil
	}
	return m
}

// missionString 取通行证参数里的字符串字段。
func missionString(m *mcMission, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m.parameter[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mcEventStreamAuth 控制台流认证：凭 ticket 密码绑定实例并开始转发输出。
func mcEventStreamAuth(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	m := mc.takeMission(req.Password, "stream_channel")
	if m == nil {
		return nil, errors.New("控制台通行证无效或已过期")
	}
	uuid := missionString(m, "instanceUuid")
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, fmt.Errorf("实例 %s 不存在", uuid)
	}
	s.mu.Lock()
	s.authed = true
	s.streamUUID = uuid
	s.mu.Unlock()
	mc.forwardOutput(inst, s)
	return true, nil
}

// mcEventStreamDetail 控制台流视图的实例详情。
func mcEventStreamDetail(mc *mcsmCompat, s *eioSession, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	uuid := s.streamUUID
	s.mu.Unlock()
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, errors.New("控制台会话未绑定实例")
	}
	detail := mcInstanceDetail(inst)
	detail["watcher"] = 1
	return detail, nil
}

// mcEventStreamInput 控制台命令行输入。
func mcEventStreamInput(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	s.mu.Lock()
	uuid := s.streamUUID
	s.mu.Unlock()
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, errors.New("控制台会话未绑定实例")
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		return nil, errors.New("实例未在运行")
	}
	return true, proc.WriteCommand(strings.TrimRight(req.Command, "\r\n"))
}

// mcEventStreamWrite 控制台原始字节写入（PTY 场景）。
func mcEventStreamWrite(mc *mcsmCompat, s *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		Input string `json:"input"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	s.mu.Lock()
	uuid := s.streamUUID
	s.mu.Unlock()
	inst := mc.d.Find(uuid)
	if inst == nil {
		return nil, errors.New("控制台会话未绑定实例")
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		return nil, errors.New("实例未在运行")
	}
	// 与 MCSM daemon 的 process.write(buf) 一致：**原样**写入标准输入，
	// 不加换行。Ctrl+C(0x03)、方向键(ESC 序列) 等控制字符必须原样转发，
	// 按行写入会被服务端当成一行普通命令（用户反馈「^C 被当成命令」）。
	return true, proc.WriteRaw(req.Input)
}

// mcEventStreamResize 终端尺寸调整：本节点非 PTY，直接成功。
func mcEventStreamResize(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return true, nil
}

// mcEventJavaList Java 运行时列表：返回本节点已安装的 JDK。
func mcEventJavaList(mc *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return mc.d.mcJavaList(), nil
}

// mcEventJavaUnsupported Java 管理写操作不支持（面板只读展示已安装运行时）。
func mcEventJavaUnsupported(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return nil, errors.New("本节点不支持通过 MCSManager 面板管理 Java 运行时")
}

// mcEventUpgradeInfo 版本信息：本节点不支持远程自更新。
func mcEventUpgradeInfo(mc *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	v := mc.reportVersion()
	return map[string]any{
		"currentVersion":  v,
		"onlineVersion":   v,
		"latestVersion":   v,
		"updateAvailable": false,
		"message":         "irix-node 不提供远程自更新",
	}, nil
}

// mcEventUpgradeUnsupported 自更新：明确拒绝。
func mcEventUpgradeUnsupported(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return nil, errors.New("irix-node 不提供远程自更新")
}

// mcEventScheduleRegister 计划任务：本节点不支持 MCSM 的计划任务，明确拒绝。
func mcEventScheduleRegister(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return nil, errors.New("本节点不支持 MCSManager 计划任务，请在面板侧关闭该功能")
}

// mcEventScheduleList 计划任务列表：始终为空。
func mcEventScheduleList(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return []any{}, nil
}

// mcEventScheduleDelete 计划任务删除：无任务，直接成功。
func mcEventScheduleDelete(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return true, nil
}

// mcJavaList 输出 MCSM 面板可读的 Java 运行时列表（读取 {data}/runtime 目录）。
func (d *Daemon) mcJavaList() []map[string]any {
	root := filepath.Join(d.DataDir, "runtime")
	entries, err := os.ReadDir(root)
	out := make([]map[string]any, 0, len(entries))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, map[string]any{
			"name":    e.Name(),
			"path":    filepath.Join(root, e.Name()),
			"version": "",
			"type":    "jdk",
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 实例状态监视：把「运行中/已停止」变化推给面板（面板据此刷新实例卡片）
// ---------------------------------------------------------------------------

// ensureWatcher 惰性启动状态监视（首次认证成功时调用，随节点关停一起退出）。
func (mc *mcsmCompat) ensureWatcher() {
	mc.watchOnce.Do(func() {
		mc.d.workers.start(func(ctx context.Context) { mc.watchLoop(ctx) })
	})
}

// watchLoop 周期比对实例状态，变化时推送 opened/stopped/failure。
func (mc *mcsmCompat) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for waitNextTick(ctx, ticker.C) {
		mc.syncInstanceState()
	}
}

// syncInstanceState 同步一次实例状态快照并推送差异。
func (mc *mcsmCompat) syncInstanceState() {
	mc.d.mu.Lock()
	insts := make([]*Instance, len(mc.d.Instances))
	copy(insts, mc.d.Instances)
	mc.d.mu.Unlock()

	current := make(map[string]int, len(insts))
	names := make(map[string]string, len(insts))
	for _, inst := range insts {
		inst.mu.Lock()
		current[inst.InstanceUuid] = inst.Status
		names[inst.InstanceUuid] = inst.Config.Nickname
		inst.mu.Unlock()
	}

	mc.stateMu.Lock()
	prev := mc.lastStatus
	mc.lastStatus = current
	mc.stateMu.Unlock()

	if prev == nil {
		return
	}
	for uuid, status := range current {
		old, existed := prev[uuid]
		if !existed {
			mc.broadcast("instance/overview", mc.d.mcAllInstanceDetails())
			continue
		}
		if old == status {
			continue
		}
		payload := map[string]any{"instanceUuid": uuid, "instanceName": names[uuid]}
		switch {
		case status == StatusRunning && old != StatusRunning:
			mc.broadcast("instance/opened", payload)
		case status == StatusStopped && old != StatusStopped:
			mc.broadcast("instance/stopped", payload)
		case status == StatusStopped && old == StatusStarting:
			mc.broadcast("instance/failure", payload)
		}
	}
}
