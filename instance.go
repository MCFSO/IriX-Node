// 实例相关 API 处理器与路由注册。
// 路由与响应格式完全对齐 MCSManager（见 apis/api_instance.md）。

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// writeJSON 输出 MCSM 风格的统一响应体 {status, data, time}。
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
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

	// 实例列表 / 详情 / 增删改
	mux.HandleFunc("GET /api/service/remote_service_instances", d.auth(d.handleInstanceList))
	mux.HandleFunc("GET /api/instance", d.auth(d.handleInstanceDetail))
	mux.HandleFunc("POST /api/instance", d.auth(d.handleInstanceCreate))
	mux.HandleFunc("PUT /api/instance", d.auth(d.handleInstanceUpdate))
	mux.HandleFunc("DELETE /api/instance", d.auth(d.handleInstanceDelete))

	// 实例操作
	mux.HandleFunc("GET /api/protected_instance/open", d.auth(d.handleInstanceStart))
	mux.HandleFunc("GET /api/protected_instance/stop", d.auth(d.handleInstanceStop))
	mux.HandleFunc("GET /api/protected_instance/restart", d.auth(d.handleInstanceRestart))
	mux.HandleFunc("GET /api/protected_instance/kill", d.auth(d.handleInstanceKill))
	mux.HandleFunc("GET /api/protected_instance/command", d.auth(d.handleInstanceCommand))
	mux.HandleFunc("GET /api/protected_instance/outputlog", d.auth(d.handleInstanceOutputLog))

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

	// 下载/上传直连通道
	mux.HandleFunc("GET /download/", d.handleDirectDownload)
	mux.HandleFunc("POST /upload/", d.handleDirectUpload)
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

// requireUuid 读取并校验 uuid 参数。
func (d *Daemon) requireUuid(r *http.Request) (string, error) {
	uuid := queryParam(r, "uuid")
	if uuid == "" {
		return "", errors.New("缺少 uuid 参数")
	}
	if d.Find(uuid) == nil {
		return "", errors.New("实例不存在")
	}
	return uuid, nil
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
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, d.Find(uuid).Detail())
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
	if strings.TrimSpace(cfg.Cwd) == "" {
		writeError(w, http.StatusBadRequest, "工作目录 (cwd) 不能为空")
		return
	}
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
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var cfg InstanceConfig
	if err := parseJSONBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if _, err := d.Update(uuid, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": uuid})
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
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.startInstance(uuid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": uuid})
}

// handleInstanceStop 停止实例。
// GET /api/protected_instance/stop?uuid&daemonId
func (d *Daemon) handleInstanceStop(w http.ResponseWriter, r *http.Request) {
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.stopInstance(uuid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": uuid})
}

// handleInstanceRestart 重启实例。
// GET /api/protected_instance/restart?uuid&daemonId
func (d *Daemon) handleInstanceRestart(w http.ResponseWriter, r *http.Request) {
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.stopInstance(uuid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := d.startInstance(uuid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"instanceUuid": uuid})
}

// handleInstanceKill 强制终止实例。
// GET /api/protected_instance/kill?uuid&daemonId
func (d *Daemon) handleInstanceKill(w http.ResponseWriter, r *http.Request) {
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst := d.Find(uuid)
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
	writeOK(w, map[string]any{"instanceUuid": uuid})
}

// handleInstanceCommand 发送命令。
// GET /api/protected_instance/command?uuid&daemonId&command
func (d *Daemon) handleInstanceCommand(w http.ResponseWriter, r *http.Request) {
	uuid, err := d.requireUuid(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	command := r.URL.Query().Get("command")
	if command == "" {
		writeError(w, http.StatusBadRequest, "缺少 command 参数")
		return
	}
	inst := d.Find(uuid)
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
	writeOK(w, map[string]any{"instanceUuid": uuid})
}

// handleInstanceOutputLog 获取实例输出日志。
// GET /api/protected_instance/outputlog?uuid&daemonId&size(1KB~2048KB)
func (d *Daemon) handleInstanceOutputLog(w http.ResponseWriter, r *http.Request) {
	uuid, err := d.requireUuid(r)
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
	inst := d.Find(uuid)
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
func (d *Daemon) startInstance(uuid string) error {
	inst := d.Find(uuid)
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
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.Busy = false
		inst.mu.Unlock()
	}()

	proc, err := startProcess(inst.Config.StartCommand, inst.Config.Cwd)
	if err != nil {
		inst.SetStatus(StatusStopped)
		return fmt.Errorf("启动失败: %w", err)
	}

	inst.mu.Lock()
	inst.Proc = proc
	inst.mu.Unlock()
	inst.Started++
	inst.SetStatus(StatusRunning)
	_ = d.Save()

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
		log.Printf("实例 %s 10 秒内自动重启超过 3 次，已停止自动重启（可能陷入崩溃循环）", inst.InstanceUuid)
		inst.mu.Unlock()
		return
	}
	inst.mu.Unlock()
	if err := d.startInstance(inst.InstanceUuid); err != nil {
		log.Printf("自动重启实例 %s 失败: %v", inst.InstanceUuid, err)
	}
}

// stopInstance 停止实例。
func (d *Daemon) stopInstance(uuid string) error {
	inst := d.Find(uuid)
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
		return fmt.Errorf("实例未在运行")
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
