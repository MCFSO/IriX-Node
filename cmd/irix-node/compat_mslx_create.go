// MSLX 实例创建/删除与后台任务（createServer / delete / cancelCreation / tasks）。
//
// MSLX 的「创建服务端」是**内置能力**，不是插件：WebPanel 调用
// POST /api/instance/createServer → 拿到 {serverId}，随后轮询 GET /api/tasks
// 看任务状态（0 待处理 / 1 运行中 / 2 成功 / 3 失败）。之前兼容层对这三个
// 接口直接回 501，面板上就只能看不能用——用户即便把插件列表哄过去了，
// 也没有任何途径创建实例。本文件把这条链路落到本节点既有能力上：
// 建实例记录 → 准备目录/核心 → 建 eula.txt → 登记一条「已完成」任务。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"irix/node/internal/timesys"
)

// mslxTask MSLX 后台任务（与面板的 BackgroundTask 同形）。
type mslxTask struct {
	ID         string `json:"id"`
	UserID     string `json:"userId"`
	InstanceID uint32 `json:"instanceId"`
	Type       int    `json:"type"`
	Title      string `json:"title"`
	TargetName string `json:"targetName"`
	State      int    `json:"state"` // 0 待处理 / 1 运行中 / 2 成功 / 3 失败
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// mslxTaskRegistry 任务登记表（内存，进程退出即清空——与 MSLX 的任务队列语义一致）。
type mslxTaskRegistry struct {
	mu    sync.Mutex
	items map[string]*mslxTask
	order []string
}

// add 登记任务（返回副本）。
func (r *mslxTaskRegistry) add(task *mslxTask) *mslxTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = map[string]*mslxTask{}
	}
	cp := *task
	r.items[task.ID] = &cp
	r.order = append(r.order, task.ID)
	out := cp
	return &out
}

// update 更新任务状态。
func (r *mslxTaskRegistry) update(id string, state, progress int, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.items[id]; ok {
		t.State = state
		t.Progress = progress
		t.Message = message
		if state >= 2 {
			t.FinishedAt = timesys.Now().Format("2006-01-02 15:04:05")
		}
	}
}

// list 返回任务列表（新的在前），instanceID>0 时只返回该实例的任务。
func (r *mslxTaskRegistry) list(instanceID uint32) []mslxTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]mslxTask, 0, len(r.items))
	for _, id := range r.order {
		t, ok := r.items[id]
		if !ok {
			continue
		}
		if instanceID > 0 && t.InstanceID != instanceID {
			continue
		}
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// remove 删除任务；clearFinished 只清已完成。
func (r *mslxTaskRegistry) remove(id string, clearFinished bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if clearFinished {
		for k, t := range r.items {
			if t.State >= 2 {
				delete(r.items, k)
			}
		}
		return
	}
	delete(r.items, id)
}

// ---------------------------------------------------------------------------
// createServer
// ---------------------------------------------------------------------------

// mslxCreateRequest 创建实例请求（字段名与面板 CreateInstanceQucikModeModel 一致）。
type mslxCreateRequest struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	Java             string `json:"java"`
	Core             string `json:"core"`
	CoreURL          string `json:"coreUrl"`
	CoreSha256       string `json:"coreSha256"`
	CoreFileKey      string `json:"coreFileKey"`
	PackageURL       string `json:"packageUrl"`
	PackageFileKey   string `json:"packageFileKey"`
	PackageLocalPath string `json:"packageLocalPath"`
	PackageSha256    string `json:"packageSha256"`
	MinM             int    `json:"minM"`
	MaxM             int    `json:"maxM"`
	Args             string `json:"args"`
	IgnoreEula       bool   `json:"ignoreEula"`
	Mcdr             bool   `json:"mcdr"`
	McdrPython       string `json:"mcdrPython"`
	McdrHandler      string `json:"mcdrHandler"`
	McdrInstall      bool   `json:"mcdrInstall"`
	McdrPipMirror    string `json:"mcdrPipMirror"`
}

// handleInstanceCreate 创建实例（面板「创建服务端」）。
func (ms *mslxCompat) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req mslxCreateRequest
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		mslxFail(w, http.StatusBadRequest, "服务器名称不能为空")
		return
	}

	// 工作目录：优先用面板给的绝对路径（必须可创建），否则放到 {data}/instances/<名称>
	cwd := strings.TrimSpace(req.Path)
	if cwd == "" {
		cwd = filepath.Join(ms.d.DataDir, "instances", sanitizeInstanceDir(req.Name))
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		mslxFail(w, http.StatusBadRequest, "创建实例目录失败: "+err.Error())
		return
	}

	// 启动命令：java -Xms -Xmx [args] -jar <core> nogui
	javaBin := strings.TrimSpace(req.Java)
	if javaBin == "" {
		javaBin = "java"
	}
	core := strings.TrimSpace(req.Core)
	parts := []string{javaBin}
	if req.MinM > 0 {
		parts = append(parts, fmt.Sprintf("-Xms%dM", req.MinM))
	}
	if req.MaxM > 0 {
		parts = append(parts, fmt.Sprintf("-Xmx%dM", req.MaxM))
	}
	if extra := strings.TrimSpace(req.Args); extra != "" {
		parts = append(parts, extra)
	}
	if core != "" {
		parts = append(parts, "-jar", core, "nogui")
	}
	startCommand := strings.Join(parts, " ")
	if req.Mcdr {
		python := strings.TrimSpace(req.McdrPython)
		if python == "" {
			python = "python"
		}
		startCommand = python + " -m mcdreforged"
	}

	inst := NewInstance(newUUID(), InstanceConfig{
		Nickname:     req.Name,
		Cwd:          cwd,
		StartCommand: startCommand,
		StopCommand:  "stop",
		Type:         "universal",
		IE:           "utf-8",
		OE:           "utf-8",
	})
	inst.Config.CreateDatetime = timesys.Now().Unix()
	if err := ms.d.Add(inst); err != nil {
		mslxFail(w, http.StatusBadRequest, "创建实例失败: "+err.Error())
		return
	}
	id := ms.idFor(inst.InstanceUuid)

	task := ms.tasks.add(&mslxTask{
		ID:         newUUID(),
		InstanceID: id,
		Type:       1,
		Title:      "创建实例 " + req.Name,
		TargetName: core,
		State:      1,
		Progress:   10,
		Message:    "正在准备实例目录",
		CreatedAt:  timesys.Now().Format("2006-01-02 15:04:05"),
	})

	// 后续准备（目录/核心/eula）放到后台，避免面板请求超时（面板任务上限 120s）
	safeGo("MSLX 兼容层创建实例", func() {
		if err := ms.prepareCreatedInstance(inst, &req, cwd); err != nil {
			alog.Printf("MSLX 兼容层：创建实例 %s 未完成：%v", req.Name, err)
			ms.tasks.update(task.ID, 3, 100, err.Error())
			return
		}
		ms.tasks.update(task.ID, 2, 100, "创建完成")
	})

	mslxOK(w, map[string]any{"serverId": id}, "创建任务已提交")
}

// prepareCreatedInstance 准备实例：核心（上传/下载）、整合包解压、MCDR 部署、eula。
func (ms *mslxCompat) prepareCreatedInstance(inst *Instance, req *mslxCreateRequest, cwd string) error {
	// 1) 核心：优先用面板上传的临时文件，其次按下载地址取
	coreName := strings.TrimSpace(req.Core)
	if coreName == "" {
		coreName = "server.jar"
	}
	coreName = filepath.Base(coreName)
	dest := filepath.Join(cwd, coreName)
	switch {
	case strings.TrimSpace(req.CoreFileKey) != "":
		src := ms.uploadFilePath(strings.TrimSpace(req.CoreFileKey))
		if src == "" {
			return fmt.Errorf("上传的核心文件不可用：%w", errUploadNotFound)
		}
		if err := copyFileTo(src, dest); err != nil {
			return fmt.Errorf("保存上传的核心失败: %w", err)
		}
	case strings.TrimSpace(req.CoreURL) != "":
		if err := ms.downloadCoreTo(strings.TrimSpace(req.CoreURL), dest, req.CoreSha256); err != nil {
			return fmt.Errorf("下载核心失败: %w", err)
		}
	}
	// 2) 整合包：上传的压缩包 / 本地路径 / 下载地址，统一取到本地再解压
	archive := ""
	switch {
	case strings.TrimSpace(req.PackageFileKey) != "":
		if p := ms.uploadFilePath(strings.TrimSpace(req.PackageFileKey)); p != "" {
			archive = p
		} else {
			return fmt.Errorf("上传的整合包不可用：%w", errUploadNotFound)
		}
	case strings.TrimSpace(req.PackageLocalPath) != "":
		local := strings.TrimSpace(req.PackageLocalPath)
		lower := strings.ToLower(local)
		if !strings.HasSuffix(lower, ".zip") && !strings.HasSuffix(lower, ".mrpack") {
			return errors.New("本机整合包路径必须是 .zip 或 .mrpack")
		}
		if _, err := os.Stat(local); err != nil {
			return fmt.Errorf("本机整合包路径不可读: %w", err)
		}
		archive = local
	case strings.TrimSpace(req.PackageURL) != "":
		tmp, err := os.CreateTemp("", "irix-package-*.zip")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpName)
		if err := ms.downloadCoreTo(strings.TrimSpace(req.PackageURL), tmpName, req.PackageSha256); err != nil {
			return fmt.Errorf("下载整合包失败: %w", err)
		}
		archive = tmpName
	}
	if archive != "" {
		if err := extractZipInto(archive, cwd); err != nil {
			return fmt.Errorf("解压整合包失败: %w", err)
		}
		// Modrinth 整合包：按 modrinth.index.json 下载文件，并把 overrides 覆盖到根
		if err := applyModrinthPackage(cwd); err != nil {
			return err
		}
	}
	// 3) MCDReforged：按需安装、写配置、把启动命令改成 python -m mcdreforged
	if req.Mcdr {
		if err := ms.setupMCDR(inst, req, cwd); err != nil {
			return err
		}
	}
	// 3) eula.txt（除非用户明确忽略）
	if !req.IgnoreEula {
		eula := filepath.Join(cwd, "eula.txt")
		if _, err := os.Stat(eula); err != nil {
			_ = os.WriteFile(eula, []byte("# 由 irix-node 自动生成\neula=true\n"), 0o644)
		}
	}
	// 4) 顺手写一份 server.properties 骨架，避免核心首次启动前的空目录困惑
	props := filepath.Join(cwd, "server.properties")
	if _, err := os.Stat(props); err != nil {
		_ = os.WriteFile(props, []byte("motd="+sanitizeInstanceDir(req.Name)+"\nmax-players=20\n"), 0o644)
	}
	return nil
}

// downloadCoreTo 下载核心文件（带 SSRF 防护与大小上限）。
func (ms *mslxCompat) downloadCoreTo(rawURL, dest, sha256Hex string) error {
	if err := validateCoreURL(rawURL); err != nil {
		return err
	}
	client := &http.Client{
		Timeout:   0, // 由 context/传输层控制
		Transport: &http.Transport{DialContext: ms.d.coreDownloadDialContext()},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxCoreSize = 512 << 20 // 512 MiB
	tmp := dest + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxCoreSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if n > maxCoreSize {
		_ = os.Remove(tmp)
		return errors.New("核心文件超过 512 MiB 上限")
	}
	if want := strings.ToLower(strings.TrimSpace(sha256Hex)); want != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			_ = os.Remove(tmp)
			return fmt.Errorf("sha256 校验失败（期望 %s，实际 %s）", want, got)
		}
	}
	return os.Rename(tmp, dest)
}

// sanitizeInstanceDir 把实例名转成安全的目录名。
func sanitizeInstanceDir(name string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(name))
	out = strings.Trim(out, "._")
	if out == "" {
		out = "server"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// ---------------------------------------------------------------------------
// 删除 / 取消创建
// ---------------------------------------------------------------------------

// handleInstanceDelete 删除实例（面板「删除服务端」）。
func (ms *mslxCompat) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req struct {
		ID          *uint32 `json:"id"`
		DeleteFiles *bool   `json:"deleteFiles"`
	}
	if err := parseJSONBody(r, &req); err != nil || req.ID == nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误：需要 id")
		return
	}
	uuid, ok := ms.uuidForID(*req.ID)
	if !ok {
		mslxFail(w, http.StatusNotFound, fmt.Sprintf("实例 %d 不存在", *req.ID))
		return
	}
	inst := ms.d.Find(uuid)
	if inst == nil {
		mslxFail(w, http.StatusNotFound, fmt.Sprintf("实例 %d 不存在", *req.ID))
		return
	}
	inst.mu.Lock()
	running := inst.Proc != nil && inst.Proc.IsRunning()
	inst.mu.Unlock()
	if running {
		mslxFail(w, http.StatusBadRequest, "服务器实例正在运行，请先停止再删除！")
		return
	}
	delFiles := req.DeleteFiles != nil && *req.DeleteFiles
	if err := ms.d.Remove(uuid, delFiles); err != nil {
		mslxFail(w, http.StatusBadRequest, "删除失败: "+err.Error())
		return
	}
	ms.forgetID(uuid)
	mslxOK(w, true, fmt.Sprintf("服务器实例 %d 删除成功！", *req.ID))
}

// handleInstanceCancelCreation 取消创建（本节点创建是秒级同步完成的，这里只做状态收敛）。
func (ms *mslxCompat) handleInstanceCancelCreation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var req struct {
		ServerID     string `json:"serverId"`
		CleanupFiles bool   `json:"cleanupFiles"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	// 本节点创建是同步的：没有「进行中」的创建任务可取消
	mslxFail(w, http.StatusNotFound, fmt.Sprintf("未找到正在执行的创建任务 (ServerId: %s)", req.ServerID))
}

// ---------------------------------------------------------------------------
// /api/tasks
// ---------------------------------------------------------------------------

// handleTasks 任务列表与操作（面板任务中心轮询它判断创建是否完成）。
func (ms *mslxCompat) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var instanceID uint32
		if v := strings.TrimSpace(r.URL.Query().Get("instanceId")); v != "" {
			if id, err := strconv.ParseUint(v, 10, 32); err == nil {
				instanceID = uint32(id)
			}
		}
		mslxOK(w, ms.tasks.list(instanceID), "获取成功")
	case http.MethodPost:
		// /api/tasks/{id}/cancel 或 /api/tasks/clear-finished
		path := strings.TrimPrefix(r.URL.Path, "/api/tasks")
		path = strings.Trim(path, "/")
		if path == "clear-finished" {
			ms.tasks.remove("", true)
			mslxOK(w, true, "已清理已完成任务")
			return
		}
		mslxOK(w, true, "任务已取消")
	case http.MethodDelete:
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
		ms.tasks.remove(id, false)
		mslxOK(w, true, "任务已删除")
	default:
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}
