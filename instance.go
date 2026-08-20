// 实例相关 API 处理器与路由注册。
// 路由与响应格式完全对齐 MCSManager（见 apis/api_instance.md）。

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// writeJSON 输出 MCSM 风格的统一响应体 {status, data, time}，
// 并把 status 透传为真实 HTTP 状态码：成功 200，错误与业务状态一致（writeError 传入）。
// 此前恒 200 的约定让 WAF/监控/负载均衡无法识别失败请求，审计报告 #6 要求透传。
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"data":   data,
		"time":   time.Now().UnixMilli(),
	})
}

// writeOK 输出 200 响应。
func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, 200, data)
}

// writeError 输出错误响应（data 为错误消息字符串）。
func writeError(w http.ResponseWriter, httpStatus int, msg string) {
	writeJSON(w, httpStatus, msg)
}

// parseJSONBody 解析请求体 JSON。
func parseJSONBody(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(out)
}

// queryParam 读取查询参数。
func queryParam(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

// authOK 校验请求凭证：优先校验 -apikey，否则校验配对码（apikey 参数 / X-Api-Key 头）。
func (d *Daemon) authOK(r *http.Request) bool {
	got := r.URL.Query().Get("apikey")
	if got == "" {
		got = r.Header.Get("X-Api-Key")
	}
	if d.APIKey != "" {
		return got == d.APIKey
	}
	if d.PairingHash == "" {
		return true
	}
	return checkPairing(got, d.PairingHash)
}

// RegisterRoutes 注册全部路由。
func (d *Daemon) RegisterRoutes(mux *http.ServeMux) {
	// 概览
	mux.HandleFunc("GET /api/overview", d.auth(d.handleOverview))
	mux.HandleFunc("GET /api/load", d.auth(d.handleLoad))

	// 实例列表 / 详情 / 增删改
	mux.HandleFunc("GET /api/service/remote_service_instances", d.auth(d.handleInstanceList))
	mux.HandleFunc("GET /api/instance", d.auth(d.handleInstanceDetail))
	mux.HandleFunc("POST /api/instance", d.auth(d.handleInstanceCreate))
	mux.HandleFunc("PUT /api/instance", d.auth(d.handleInstanceUpdate))
	mux.HandleFunc("DELETE /api/instance", d.auth(d.handleInstanceDelete))

	// 导入目录创建实例（docs/irix-node-local-parity.md §4.2.4）
	mux.HandleFunc("POST /api/instance/import", d.auth(d.handleInstanceImport))

	// 实例操作
	mux.HandleFunc("GET /api/protected_instance/open", d.auth(d.handleInstanceStart))
	mux.HandleFunc("GET /api/protected_instance/stop", d.auth(d.handleInstanceStop))
	mux.HandleFunc("GET /api/protected_instance/restart", d.auth(d.handleInstanceRestart))
	mux.HandleFunc("GET /api/protected_instance/kill", d.auth(d.handleInstanceKill))
	mux.HandleFunc("GET /api/protected_instance/command", d.auth(d.handleInstanceCommand))
	mux.HandleFunc("GET /api/protected_instance/outputlog", d.auth(d.handleInstanceOutputLog))

	// 实例日志（docs/irix-node-local-parity.md §4.1.2）
	mux.HandleFunc("GET /api/instance/logs", d.auth(d.handleInstanceLogs))
	mux.HandleFunc("DELETE /api/instance/logs", d.auth(d.handleInstanceLogsClear))

	// 实例级指标（docs/irix-node-local-parity.md §4.3）
	mux.HandleFunc("GET /api/instance/stats", d.auth(d.handleInstanceStats))

	// 插件/Mod 元数据（docs/irix-node-local-parity.md §4.4）
	mux.HandleFunc("GET /api/instance/plugins", d.auth(d.handleInstancePlugins))

	// 实例备份/恢复（docs/irix-node-local-parity.md §4.5，任务化）
	mux.HandleFunc("POST /api/instance/snapshot", d.auth(d.handleInstanceSnapshot))
	mux.HandleFunc("GET /api/instance/snapshot-progress", d.auth(d.writeSnapshotStatus))
	mux.HandleFunc("POST /api/instance/restore", d.auth(d.handleInstanceRestore))
	mux.HandleFunc("GET /api/instance/backups", d.auth(d.handleBackupsList))
	mux.HandleFunc("DELETE /api/instance/backups", d.auth(d.handleBackupsDelete))
	mux.HandleFunc("POST /api/instance/backups/download", d.auth(d.handleBackupDownloadTicket))

	// 实时控制台 WebSocket（docs/irix-node-local-parity.md §4.1.1）
	mux.HandleFunc("GET /api/instance/console/ws", d.auth(d.handleConsoleWS))

	// 核心下载（docs/irix-node-local-parity.md §4.2.3，任务化）
	mux.HandleFunc("POST /api/instance/download-core", d.auth(d.handleDownloadCore))
	mux.HandleFunc("GET /api/instance/download-core-progress", d.auth(d.writeTaskStatus))
	// Java 运行时（docs/irix-node-local-parity.md §4.2.1）
	mux.HandleFunc("GET /api/runtime/java", d.auth(d.handleRuntimeJava))
	// JDK 安装/卸载（docs/irix-node-local-parity.md §4.2.2，任务化）
	mux.HandleFunc("POST /api/runtime/java/install", d.auth(d.handleInstallJava))
	mux.HandleFunc("GET /api/runtime/java/install-progress", d.auth(d.writeTaskStatus))
	mux.HandleFunc("DELETE /api/runtime/java", d.auth(d.handleUninstallJava))

	// 文件管理
	mux.HandleFunc("GET /api/files/list", d.auth(d.handleFileList))
	mux.HandleFunc("PUT /api/files/", d.auth(d.handleFileReadWrite))
	mux.HandleFunc("DELETE /api/files", d.auth(d.handleFileDelete))
	mux.HandleFunc("PUT /api/files/move", d.auth(d.handleFileMove))
	mux.HandleFunc("POST /api/files/copy", d.auth(d.handleFileCopy))
	mux.HandleFunc("POST /api/files/compress", d.auth(d.handleFileCompress))
	mux.HandleFunc("POST /api/files/mkdir", d.auth(d.handleFileMkdir))
	mux.HandleFunc("POST /api/files/touch", d.auth(d.handleFileTouch))
	mux.HandleFunc("POST /api/files/download", d.auth(d.handleFileDownloadTicket))
	mux.HandleFunc("POST /api/files/upload", d.auth(d.handleFileUploadTicket))

	// 实例级回收站（docs/irix-node-local-parity.md §4.6）
	mux.HandleFunc("POST /api/files/trash", d.auth(d.handleFileTrash))
	mux.HandleFunc("GET /api/files/trash/list", d.auth(d.handleTrashList))
	mux.HandleFunc("POST /api/files/trash/restore", d.auth(d.handleTrashRestore))
	mux.HandleFunc("POST /api/files/trash/empty", d.auth(d.handleTrashEmpty))

	// 节点端内网穿透 FRP（docs/irix-node-local-parity.md §4.7）
	mux.HandleFunc("GET /api/frp/status", d.auth(d.handleFRPStatus))
	mux.HandleFunc("POST /api/frp/tunnels", d.auth(d.handleFRPCreate))
	mux.HandleFunc("POST /api/frp/tunnels/{id}/start", d.auth(d.handleFRPStart))
	mux.HandleFunc("POST /api/frp/tunnels/{id}/stop", d.auth(d.handleFRPStop))
	mux.HandleFunc("DELETE /api/frp/tunnels/{id}", d.auth(d.handleFRPDelete))
	mux.HandleFunc("GET /api/frp/tunnels/{id}/logs", d.auth(d.handleFRPLogs))
	mux.HandleFunc("POST /api/frp/binary", d.auth(d.handleFRPUploadBinary))

	// AI 日志查询与监控历史（docs/irix-node-local-parity.md §4.8）
	mux.HandleFunc("GET /api/instance/logs/query", d.auth(d.handleLogsQuery))
	mux.HandleFunc("GET /api/instance/metrics", d.auth(d.handleInstanceMetrics))

	// 下载/上传直连通道
	mux.HandleFunc("GET /download/", d.handleDirectDownload)
	mux.HandleFunc("POST /upload/", d.handleDirectUpload)

	// 容器环境（Docker / Bastille，NODE_API.md §6.1）
	d.registerContainerRoutes(mux)

	// 集群节点 API（P0-P2，docs/cluster-node-api.md）
	d.registerClusterRoutes(mux)
}

// auth 包装器：校验 API 密钥。
func (d *Daemon) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.authOK(r) {
			writeError(w, http.StatusForbidden, "API 密钥无效")
			return
		}
		next(w, r)
	}
}

// requireInstance 读取 uuid 参数并解析出实例对象。
//
// 必须一次性拿到实例指针：若先校验存在、后再 d.Find 二次查找，
// 两次查找之间实例可能被并发删除（DELETE /api/instance），
// 第二次返回 nil 会导致处理器 nil 解引用崩溃。
func (d *Daemon) requireInstance(r *http.Request) (*Instance, error) {
	uuid := queryParam(r, "uuid")
	if uuid == "" {
		return nil, errors.New("缺少 uuid 参数")
	}
	inst := d.Find(uuid)
	if inst == nil {
		return nil, errors.New("实例不存在")
	}
	return inst, nil
}

// handleInstanceList 获取实例列表。
// GET /api/service/remote_service_instances?daemonId&page&page_size&instance_name&status
// 惰性分页：只构造当前页的实例详情，避免大实例数下全量序列化。
func (d *Daemon) handleInstanceList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(queryParam(r, "page"))
	pageSize, _ := strconv.Atoi(queryParam(r, "page_size"))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	nameFilter := queryParam(r, "instance_name")
	statusFilter := queryParam(r, "status")

	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()

	// 提取排序键（避免在排序回调中反复加锁）
	type pair struct {
		inst *Instance
		ct   int64
	}
	pairs := make([]pair, 0, len(insts))
	for _, inst := range insts {
		inst.mu.Lock()
		pairs = append(pairs, pair{inst, inst.Config.CreateDatetime})
		inst.mu.Unlock()
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].ct < pairs[b].ct })

	// 过滤
	matched := make([]*Instance, 0, len(pairs))
	for _, p := range pairs {
		inst := p.inst
		inst.mu.Lock()
		nickname, status := inst.Config.Nickname, inst.Status
		inst.mu.Unlock()
		if nameFilter != "" && !strings.Contains(nickname, nameFilter) {
			continue
		}
		if statusFilter != "" && strconv.Itoa(status) != statusFilter {
			continue
		}
		matched = append(matched, inst)
	}

	start := (page - 1) * pageSize
	if start > len(matched) {
		start = len(matched)
	}
	end := start + pageSize
	if end > len(matched) {
		end = len(matched)
	}
	maxPage := 1
	if len(matched) > 0 {
		maxPage = (len(matched) + pageSize - 1) / pageSize
	}
	pageItems := make([]map[string]any, 0, end-start)
	for _, inst := range matched[start:end] {
		pageItems = append(pageItems, inst.Detail())
	}
	writeOK(w, map[string]any{
		"maxPage":  maxPage,
		"pageSize": pageSize,
		"page":     page,
		"total":    len(matched),
		"data":     pageItems,
	})
}

// handleInstanceDetail 获取实例详情。
// GET /api/instance?uuid&daemonId
func (d *Daemon) handleInstanceDetail(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, inst.Detail())
}

// handleInstanceCreate 创建实例。
// POST /api/instance?daemonId  body: InstanceConfig
func (d *Daemon) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	var cfg InstanceConfig
	if err := parseJSONBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	cfg.FillDefaults()
	abs, err := normalizeCwd(cfg.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Cwd = abs
	cfg.CreateDatetime = time.Now().UnixMilli()
	cfg.LastDatetime = cfg.CreateDatetime

	inst := NewInstance("", cfg)
	if err := d.Add(inst); err != nil {
		writeError(w, http.StatusInternalServerError, "保存实例失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"config":       inst.Config,
	})
}

// handleInstanceUpdate 更新实例配置。
// PUT /api/instance?uuid&daemonId  body: InstanceConfig
func (d *Daemon) handleInstanceUpdate(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var cfg InstanceConfig
	if err := parseJSONBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	cfg.FillDefaults()
	abs, err := normalizeCwd(cfg.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Cwd = abs
	if err := d.UpdateInstance(inst, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceDelete 删除实例。
// DELETE /api/instance?daemonId  body: {uuids: [...], deleteFile: bool}
func (d *Daemon) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Uuids      []string `json:"uuids"`
		DeleteFile bool     `json:"deleteFile"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	var deleted []string
	for _, uuid := range body.Uuids {
		if err := d.Remove(uuid, body.DeleteFile); err != nil {
			continue
		}
		deleted = append(deleted, uuid)
	}
	writeOK(w, deleted)
}

// handleInstanceStart 启动实例。
// GET /api/protected_instance/open?uuid&daemonId
func (d *Daemon) handleInstanceStart(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.startInstance(inst); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceStop 停止实例。
// GET /api/protected_instance/stop?uuid&daemonId
func (d *Daemon) handleInstanceStop(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.stopInstance(inst); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceRestart 重启实例。
// GET /api/protected_instance/restart?uuid&daemonId
// 重启语义：实例已停止时直接启动（与 MCSM 面板行为一致），
// 不因「实例未在运行」返回 500。
func (d *Daemon) handleInstanceRestart(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := d.startInstance(inst); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceKill 强制终止实例。
// GET /api/protected_instance/kill?uuid&daemonId
func (d *Daemon) handleInstanceKill(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst.SetStatus(StatusStopping)
	// 先解除进程引用再终止：防止退出监听 goroutine 误判为意外退出而触发 AutoRestart
	inst.mu.Lock()
	proc := inst.Proc
	inst.Proc = nil
	inst.mu.Unlock()
	if proc != nil && proc.IsRunning() {
		if err := proc.Kill(); err != nil {
			// Kill 失败：恢复引用，保留现场
			inst.mu.Lock()
			if inst.Proc == nil {
				inst.Proc = proc
			}
			inst.mu.Unlock()
			inst.SetStatus(StatusStopped)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	inst.SetStatus(StatusStopped)
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceCommand 发送命令。
// GET /api/protected_instance/command?uuid&daemonId&command
func (d *Daemon) handleInstanceCommand(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	command := r.URL.Query().Get("command")
	if command == "" {
		writeError(w, http.StatusBadRequest, "缺少 command 参数")
		return
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		writeError(w, http.StatusBadRequest, "实例未在运行")
		return
	}
	if err := proc.WriteCommand(command); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceOutputLog 获取实例输出日志。
// GET /api/protected_instance/outputlog?uuid&daemonId&size(1KB~2048KB)
func (d *Daemon) handleInstanceOutputLog(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	size, _ := strconv.Atoi(queryParam(r, "size"))
	if size < 0 {
		size = 0
	}
	if size > 2048 {
		size = 2048
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil {
		writeOK(w, "")
		return
	}
	writeOK(w, proc.Log.Tail(size))
}

// startInstance 启动实例（带状态流转与忙碌互斥）。
func (d *Daemon) startInstance(inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("实例不存在")
	}
	inst.mu.Lock()
	if inst.Busy {
		inst.mu.Unlock()
		return fmt.Errorf("实例正在执行其他操作")
	}
	if inst.Proc != nil && inst.Proc.IsRunning() {
		inst.mu.Unlock()
		return fmt.Errorf("实例已在运行")
	}
	inst.Busy = true
	inst.Status = StatusStarting
	// 在锁内取出启动所需配置：并发 PUT /api/instance 会整体替换 Config
	startCommand, cwd := inst.Config.StartCommand, inst.Config.Cwd
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.Busy = false
		inst.mu.Unlock()
	}()

	lc := d.logConfig()
	if lc != nil {
		lc.name = inst.InstanceUuid + ".log"
	}
	proc, err := startProcess(startCommand, cwd, lc)
	if err != nil {
		inst.SetStatus(StatusStopped)
		return fmt.Errorf("启动失败: %w", err)
	}

	inst.mu.Lock()
	inst.Proc = proc
	inst.Started++ // 与 Save 读取 Started 竞争，必须在锁内自增
	inst.mu.Unlock()
	inst.SetStatus(StatusRunning)
	_ = d.Save()

	// 先释放忙碌标记，再挂退出监听：进程秒退时（如 `exit 1`）done 可能在
	// Save 期间就已关闭，watcher 一注册会立即触发 autoRestart；若 Busy
	// 仍未清，下次启动会被误判为「实例正在执行其他操作」而丢弃，防抖链条中断。
	inst.mu.Lock()
	inst.Busy = false
	inst.mu.Unlock()

	// 监听进程退出：意外退出且启用 AutoRestart 时自动重启（带防抖）
	go func() {
		<-proc.done
		inst.mu.Lock()
		wasProc := inst.Proc == proc
		if wasProc {
			inst.Status = StatusStopped
		}
		autoRestart := inst.Config.EventTask.AutoRestart
		inst.mu.Unlock()
		if wasProc && autoRestart {
			d.autoRestart(inst)
		}
	}()
	return nil
}

// autoRestart 自动重启实例（防崩溃循环：10 秒窗口内最多 3 次）。
func (d *Daemon) autoRestart(inst *Instance) {
	inst.mu.Lock()
	now := time.Now()
	if now.Sub(inst.arWindowStart) > 10*time.Second {
		inst.arWindowStart = now
		inst.arAttempts = 0
	}
	inst.arAttempts++
	if inst.arAttempts > 3 {
		inst.arWindowStart = time.Time{}
		inst.arAttempts = 0
		alog.Printf("实例 %s 10 秒内自动重启超过 3 次，已停止自动重启（可能陷入崩溃循环）", inst.InstanceUuid)
		inst.mu.Unlock()
		return
	}
	inst.mu.Unlock()
	if err := d.startInstance(inst); err != nil {
		alog.Printf("自动重启实例 %s 失败: %v", inst.InstanceUuid, err)
	}
}

// StopAll 关停所有运行中的实例（用于守护进程优雅退出）。
// 并行执行：先按各自的停止命令优雅停止，超过 timeout 后强制终止，
// 避免守护进程退出后留下无人管理的孤儿进程。
func (d *Daemon) StopAll(timeout time.Duration) {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, inst := range insts {
		inst.mu.Lock()
		proc := inst.Proc
		// 提前解除引用：关停属于主动行为，不触发 AutoRestart
		inst.Proc = nil
		// 昵称必须在锁内取出：goroutine 里读 inst.Config 与并发 Update 竞争
		stopCmd, nickname := inst.Config.StopCommand, inst.Config.Nickname
		inst.mu.Unlock()
		if proc == nil || !proc.IsRunning() {
			continue
		}
		wg.Add(1)
		go func(inst *Instance, proc *Process, stopCmd, nickname string) {
			defer wg.Done()
			alog.Printf("正在停止实例 %s（%s）", inst.InstanceUuid, nickname)
			if err := proc.Stop(stopCmd, timeout); err != nil {
				alog.Printf("停止实例 %s 失败: %v", inst.InstanceUuid, err)
			}
			inst.SetStatus(StatusStopped)
		}(inst, proc, stopCmd, nickname)
	}
	wg.Wait()
	if err := d.Save(); err != nil {
		alog.Printf("关停时保存实例状态失败: %v", err)
	}
}

// errNotRunning 实例未在运行的哨兵错误；restart 据此区分「本就没运行」与真实故障。
var errNotRunning = errors.New("实例未在运行")

// stopInstance 停止实例。
func (d *Daemon) stopInstance(inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("实例不存在")
	}
	inst.mu.Lock()
	if inst.Busy {
		inst.mu.Unlock()
		return fmt.Errorf("实例正在执行其他操作")
	}
	if inst.Proc == nil || !inst.Proc.IsRunning() {
		inst.mu.Unlock()
		return errNotRunning
	}
	inst.Busy = true
	inst.Status = StatusStopping
	proc := inst.Proc
	inst.Proc = nil // 先解除引用再等待退出，防止误触发 AutoRestart
	stopCmd := inst.Config.StopCommand
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.Busy = false
		inst.mu.Unlock()
	}()

	err := proc.Stop(stopCmd, 30*time.Second)
	inst.SetStatus(StatusStopped)
	_ = d.Save()
	return err
}
