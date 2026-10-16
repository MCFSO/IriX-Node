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

// RegisterRoutes 注册全部路由。除直连通道与登录入口外，
// 每个端点同时通过 perm() 织入权限目录（组名 + 端点 + 中文描述），
// 供账户管理的「每一步开关」使用（docs/accounts-design.md）。
func (d *Daemon) RegisterRoutes(mux *http.ServeMux) {
	// 概览
	perm("概览", "GET /api/overview", "查看节点概览")
	mux.HandleFunc("GET /api/overview", d.auth(d.handleOverview))
	perm("概览", "GET /api/load", "查看负载调谐状态")
	mux.HandleFunc("GET /api/load", d.auth(d.handleLoad))

	// 审计日志只读接口（docs/backend-requirements.md P0）
	perm("审计", "GET /api/audit/log", "查看审计日志")
	mux.HandleFunc("GET /api/audit/log", d.auth(d.handleAuditLog))

	// 实例列表 / 详情 / 增删改
	perm("实例", "GET /api/service/remote_service_instances", "查看实例列表")
	mux.HandleFunc("GET /api/service/remote_service_instances", d.auth(d.handleInstanceList))
	perm("实例", "GET /api/instance", "查看实例详情")
	mux.HandleFunc("GET /api/instance", d.auth(d.handleInstanceDetail))
	perm("实例", "POST /api/instance", "创建实例")
	mux.HandleFunc("POST /api/instance", d.auth(d.handleInstanceCreate))
	perm("实例", "PUT /api/instance", "修改实例配置")
	mux.HandleFunc("PUT /api/instance", d.auth(d.handleInstanceUpdate))
	perm("实例", "DELETE /api/instance", "删除实例")
	mux.HandleFunc("DELETE /api/instance", d.auth(d.handleInstanceDelete))

	// 导入目录创建实例（docs/irix-node-local-parity.md §4.2.4）
	perm("实例", "POST /api/instance/import", "导入实例")
	mux.HandleFunc("POST /api/instance/import", d.auth(d.handleInstanceImport))

	// 实例操作
	perm("实例", "GET /api/protected_instance/open", "启动实例")
	mux.HandleFunc("GET /api/protected_instance/open", d.auth(d.handleInstanceStart))
	perm("实例", "GET /api/protected_instance/stop", "停止实例")
	mux.HandleFunc("GET /api/protected_instance/stop", d.auth(d.handleInstanceStop))
	perm("实例", "GET /api/protected_instance/restart", "重启实例")
	mux.HandleFunc("GET /api/protected_instance/restart", d.auth(d.handleInstanceRestart))
	perm("实例", "GET /api/protected_instance/kill", "强制终止实例")
	mux.HandleFunc("GET /api/protected_instance/kill", d.auth(d.handleInstanceKill))
	perm("实例", "GET /api/protected_instance/command", "向实例发送命令")
	mux.HandleFunc("GET /api/protected_instance/command", d.auth(d.handleInstanceCommand))
	perm("实例", "GET /api/protected_instance/outputlog", "查看实例输出日志")
	mux.HandleFunc("GET /api/protected_instance/outputlog", d.auth(d.handleInstanceOutputLog))

	// 实例日志（docs/irix-node-local-parity.md §4.1.2）
	perm("实例", "GET /api/instance/logs", "查看实例历史日志")
	mux.HandleFunc("GET /api/instance/logs", d.auth(d.handleInstanceLogs))
	perm("实例", "DELETE /api/instance/logs", "清空实例日志")
	mux.HandleFunc("DELETE /api/instance/logs", d.auth(d.handleInstanceLogsClear))

	// 实例级指标（docs/irix-node-local-parity.md §4.3）
	perm("实例", "GET /api/instance/stats", "查看实例实时指标")
	mux.HandleFunc("GET /api/instance/stats", d.auth(d.handleInstanceStats))

	// 插件/Mod 元数据（docs/irix-node-local-parity.md §4.4）
	perm("实例", "GET /api/instance/plugins", "查看实例插件元数据")
	mux.HandleFunc("GET /api/instance/plugins", d.auth(d.handleInstancePlugins))

	// 实例备份/恢复（docs/irix-node-local-parity.md §4.5，任务化）
	perm("实例", "POST /api/instance/snapshot", "创建实例快照")
	mux.HandleFunc("POST /api/instance/snapshot", d.auth(d.handleInstanceSnapshot))
	perm("实例", "GET /api/instance/snapshot-progress", "查看快照进度")
	mux.HandleFunc("GET /api/instance/snapshot-progress", d.auth(d.writeSnapshotStatus))
	perm("实例", "POST /api/instance/restore", "恢复实例快照")
	mux.HandleFunc("POST /api/instance/restore", d.auth(d.handleInstanceRestore))
	perm("实例", "GET /api/instance/backups", "查看实例备份列表")
	mux.HandleFunc("GET /api/instance/backups", d.auth(d.handleBackupsList))
	perm("实例", "DELETE /api/instance/backups", "删除实例备份")
	mux.HandleFunc("DELETE /api/instance/backups", d.auth(d.handleBackupsDelete))
	perm("实例", "POST /api/instance/backups/download", "下载实例备份")
	mux.HandleFunc("POST /api/instance/backups/download", d.auth(d.handleBackupDownloadTicket))

	// 实时控制台 WebSocket（docs/irix-node-local-parity.md §4.1.1）
	perm("实例", "GET /api/instance/console/ws", "实时控制台")
	mux.HandleFunc("GET /api/instance/console/ws", d.auth(d.handleConsoleWS))

	// 核心下载（docs/irix-node-local-parity.md §4.2.3，任务化）
	perm("实例", "POST /api/instance/download-core", "下载核心")
	mux.HandleFunc("POST /api/instance/download-core", d.auth(d.handleDownloadCore))
	perm("实例", "GET /api/instance/download-core-progress", "查看核心下载进度")
	mux.HandleFunc("GET /api/instance/download-core-progress", d.auth(d.writeTaskStatus))
	// Java 运行时（docs/irix-node-local-parity.md §4.2.1）
	perm("运行时", "GET /api/runtime/java", "查看 Java 运行时")
	mux.HandleFunc("GET /api/runtime/java", d.auth(d.handleRuntimeJava))
	// JDK 安装/卸载（docs/irix-node-local-parity.md §4.2.2，任务化）
	perm("运行时", "POST /api/runtime/java/install", "安装 JDK")
	mux.HandleFunc("POST /api/runtime/java/install", d.auth(d.handleInstallJava))
	perm("运行时", "GET /api/runtime/java/install-progress", "查看 JDK 安装进度")
	mux.HandleFunc("GET /api/runtime/java/install-progress", d.auth(d.writeTaskStatus))
	perm("运行时", "DELETE /api/runtime/java", "卸载 JDK")
	mux.HandleFunc("DELETE /api/runtime/java", d.auth(d.handleUninstallJava))

	// 文件管理
	perm("文件", "GET /api/files/list", "列出文件")
	mux.HandleFunc("GET /api/files/list", d.auth(d.handleFileList))
	perm("文件", "PUT /api/files/", "读写文件内容")
	mux.HandleFunc("PUT /api/files/", d.auth(d.handleFileReadWrite))
	perm("文件", "DELETE /api/files", "删除文件")
	mux.HandleFunc("DELETE /api/files", d.auth(d.handleFileDelete))
	perm("文件", "PUT /api/files/move", "移动文件")
	mux.HandleFunc("PUT /api/files/move", d.auth(d.handleFileMove))
	perm("文件", "POST /api/files/copy", "复制文件")
	mux.HandleFunc("POST /api/files/copy", d.auth(d.handleFileCopy))
	perm("文件", "POST /api/files/compress", "压缩文件")
	mux.HandleFunc("POST /api/files/compress", d.auth(d.handleFileCompress))
	perm("文件", "POST /api/files/mkdir", "新建目录")
	mux.HandleFunc("POST /api/files/mkdir", d.auth(d.handleFileMkdir))
	perm("文件", "POST /api/files/touch", "新建文件")
	mux.HandleFunc("POST /api/files/touch", d.auth(d.handleFileTouch))
	perm("文件", "POST /api/files/download", "申请下载票据")
	mux.HandleFunc("POST /api/files/download", d.auth(d.handleFileDownloadTicket))
	perm("文件", "POST /api/files/upload", "申请上传票据")
	mux.HandleFunc("POST /api/files/upload", d.auth(d.handleFileUploadTicket))

	// 实例级回收站（docs/irix-node-local-parity.md §4.6）
	perm("回收站", "POST /api/files/trash", "移入回收站")
	mux.HandleFunc("POST /api/files/trash", d.auth(d.handleFileTrash))
	perm("回收站", "GET /api/files/trash/list", "查看回收站")
	mux.HandleFunc("GET /api/files/trash/list", d.auth(d.handleTrashList))
	perm("回收站", "POST /api/files/trash/restore", "恢复回收站条目")
	mux.HandleFunc("POST /api/files/trash/restore", d.auth(d.handleTrashRestore))
	perm("回收站", "POST /api/files/trash/empty", "清空回收站")
	mux.HandleFunc("POST /api/files/trash/empty", d.auth(d.handleTrashEmpty))

	// 节点端内网穿透 FRP（docs/irix-node-local-parity.md §4.7）
	perm("FRP", "GET /api/frp/status", "查看 FRP 状态")
	mux.HandleFunc("GET /api/frp/status", d.auth(d.handleFRPStatus))
	perm("FRP", "POST /api/frp/tunnels", "创建 FRP 隧道")
	mux.HandleFunc("POST /api/frp/tunnels", d.auth(d.handleFRPCreate))
	perm("FRP", "POST /api/frp/tunnels/{id}/start", "启动 FRP 隧道")
	mux.HandleFunc("POST /api/frp/tunnels/{id}/start", d.auth(d.handleFRPStart))
	perm("FRP", "POST /api/frp/tunnels/{id}/stop", "停止 FRP 隧道")
	mux.HandleFunc("POST /api/frp/tunnels/{id}/stop", d.auth(d.handleFRPStop))
	perm("FRP", "DELETE /api/frp/tunnels/{id}", "删除 FRP 隧道")
	mux.HandleFunc("DELETE /api/frp/tunnels/{id}", d.auth(d.handleFRPDelete))
	perm("FRP", "GET /api/frp/tunnels/{id}/logs", "查看隧道日志")
	mux.HandleFunc("GET /api/frp/tunnels/{id}/logs", d.auth(d.handleFRPLogs))
	perm("FRP", "POST /api/frp/binary", "上传 FRP 客户端")
	mux.HandleFunc("POST /api/frp/binary", d.auth(d.handleFRPUploadBinary))

	// AI 日志查询与监控历史（docs/irix-node-local-parity.md §4.8）
	perm("AI 与指标", "GET /api/instance/logs/query", "AI 日志查询")
	mux.HandleFunc("GET /api/instance/logs/query", d.auth(d.handleLogsQuery))
	perm("AI 与指标", "GET /api/instance/metrics", "查看实例监控历史")
	mux.HandleFunc("GET /api/instance/metrics", d.auth(d.handleInstanceMetrics))

	// 下载/上传直连通道（票据绕过 API 认证，不在权限目录内）
	mux.HandleFunc("GET /download/", d.handleDirectDownload)
	mux.HandleFunc("POST /upload/", d.handleDirectUpload)

	// 账户管理（docs/accounts-design.md）
	d.registerAccountRoutes(mux)

	// 容器环境（Docker / Bastille，docs/NODE_API.md §6.1）
	d.registerContainerRoutes(mux)

	// 集群节点 API（P0-P2，docs/cluster-node-api.md）
	d.registerClusterRoutes(mux)

	// 加密保险库（M3，docs/vault-design.md §10）
	d.registerVaultRoutes(mux)
}

// auth 包装器：校验账户会话，并按端点权限开关放行。
// 管理员（is_admin 账户会话）不受端点开关限制。
func (d *Daemon) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := d.authenticate(r)
		if id == nil {
			writeError(w, http.StatusForbidden, "未认证或会话已过期（请先登录 POST /api/auth/login）")
			return
		}
		if !id.isAdmin && !permIdentityExempt(r.Pattern) && !d.permAllowed(id.username, r.Pattern) {
			writeError(w, http.StatusForbidden, "权限不足: 当前账户无权访问 "+r.Pattern)
			return
		}
		// 认证过的响应一律禁止缓存（红队审计 中-2）：避免会话数据落到
		// 中间代理/浏览器缓存中；万一客户端误把 token 放进 URL，也能
		// 避免响应被共享缓存复用。
		w.Header().Set("Cache-Control", "no-store")
		next(w, withIdentity(r, id))
	}
}

// permIdentityExempt 账户自身端点（查看自身/权限目录/自己改密/登出）：
// 任何已登录账户均可用，不参与端点开关；管理端点（账户增删查、权限开关、
// 管理员重置）在 handler 内再校验管理员身份。
func permIdentityExempt(pattern string) bool {
	switch pattern {
	case "GET /api/accounts/me", "GET /api/accounts/catalog",
		"PUT /api/accounts/password", "POST /api/auth/logout":
		return true
	}
	return false
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

// errInstanceForbidden 表示实例存在但当前账户无权访问（跨租户 IDOR，
// 红队审计 高-3）；调用方据此返回 403 而非 400。
var errInstanceForbidden = errors.New("无权访问该实例")

// requireInstanceAccess 解析实例并校验**实例级授权**（红队审计 高-3）。
//
// 此前所有实例端点只按 uuid 查实例、从不校验属主，导致只要拿到「文件/控制台/
// 启停」任一端点开关，就能对**任意**实例生效（IDOR）。授权规则：
//   - 管理员旁路；
//   - 实例属主为该账户 → 放行；
//   - 属主为空（公共实例，含历史数据）→ 放行，保持既有部署可用；
//   - 其余 → errInstanceForbidden。
//
// 账户系统未启用（identity == nil）时不引入属主约束（此时端点已由认证层拦截）。
func (d *Daemon) requireInstanceAccess(r *http.Request) (*Instance, error) {
	inst, err := d.requireInstance(r)
	if err != nil {
		return nil, err
	}
	id := identityOf(r)
	if id == nil || id.isAdmin {
		return inst, nil
	}
	owner := inst.OwnerOf()
	if owner == "" || owner == id.username {
		return inst, nil
	}
	return nil, fmt.Errorf("%w（属主: %s）", errInstanceForbidden, owner)
}

// writeInstanceAccessError 输出实例解析错误：无权访问 → 403，其余（缺 uuid /
// 实例不存在）→ 400，保持既有客户端契约不变。
func writeInstanceAccessError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInstanceForbidden) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// canAccessInstance 判定当前账户是否可访问某实例（不写响应）。
// 供批量端点（删除多实例、列表过滤）复用。
func (d *Daemon) canAccessInstance(r *http.Request, inst *Instance) bool {
	id := identityOf(r)
	if id == nil || id.isAdmin {
		return true
	}
	owner := inst.OwnerOf()
	return owner == "" || owner == id.username
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
		// 实例级授权（红队审计 高-3）：非管理员只看到「自己拥有」与「无属主
		// 的公共实例」，避免跨租户泄露实例清单/名称/状态（管理员旁路）。
		if !d.canAccessInstance(r, inst) {
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
		return
	}
	writeOK(w, inst.Detail())
}

// handleInstanceCreate 创建实例。
// POST /api/instance?daemonId  body: InstanceConfig（可附加 owner 字段，仅管理员）
//
// 属主（红队审计 高-3）：非管理员创建的实例自动归属自己，无法为他人建实例；
// 管理员可在 body 中显式指定 owner（空串 = 公共实例）。
func (d *Daemon) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstanceConfig
		Owner *string `json:"owner"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	cfg := body.InstanceConfig
	cfg.FillDefaults()
	d.applyVaultDefault(&cfg)
	abs, err := d.normalizeInstanceCwd(cfg.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Cwd = abs
	cfg.CreateDatetime = time.Now().UnixMilli()
	cfg.LastDatetime = cfg.CreateDatetime

	inst := NewInstance("", cfg)
	owner, err := d.resolveInstanceOwner(r, body.Owner)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	inst.Owner = owner
	if err := d.Add(inst); err != nil {
		writeError(w, http.StatusInternalServerError, "保存实例失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"config":       inst.Config,
		"owner":        inst.OwnerOf(),
	})
}

// resolveInstanceOwner 计算新实例的属主（红队审计 高-3）：
//   - 请求未指定 owner：归属发起账户（非管理员即可获得自己创建的实例）；
//   - 请求指定 owner：仅管理员可指定（可指定为空串 = 公共实例）；
//     非管理员指定他人属主一律拒绝，防止把实例「塞给别人」。
func (d *Daemon) resolveInstanceOwner(r *http.Request, requested *string) (string, error) {
	id := identityOf(r)
	if id == nil {
		return "", nil
	}
	if requested == nil {
		if id.isAdmin {
			return "", nil // 管理员未指定 → 公共实例（与历史行为一致）
		}
		return id.username, nil
	}
	if !id.isAdmin {
		return "", errors.New("仅管理员可以指定实例属主")
	}
	owner := strings.TrimSpace(*requested)
	if owner == "" {
		return "", nil
	}
	if !validUsername(owner) {
		return "", fmt.Errorf("属主用户名不合法: %s", owner)
	}
	return owner, nil
}

// handleInstanceUpdate 更新实例配置。
// PUT /api/instance?uuid&daemonId  body: InstanceConfig（可附加 owner 字段，仅管理员）
//
// 属主变更（红队审计 高-3）仅管理员可做；非管理员提交 owner 字段将被拒绝，
// 避免把实例「过户」给自己或他人来绕过实例级授权。
func (d *Daemon) handleInstanceUpdate(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
		return
	}
	var body struct {
		InstanceConfig
		Owner *string `json:"owner"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	cfg := body.InstanceConfig
	cfg.FillDefaults()
	abs, err := d.normalizeInstanceCwd(cfg.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Cwd = abs

	ownerChanged := false
	newOwner := ""
	if body.Owner != nil {
		id := identityOf(r)
		if id != nil && !id.isAdmin {
			writeError(w, http.StatusForbidden, "仅管理员可以变更实例属主")
			return
		}
		newOwner = strings.TrimSpace(*body.Owner)
		if newOwner != "" && !validUsername(newOwner) {
			writeError(w, http.StatusBadRequest, "属主用户名不合法: "+newOwner)
			return
		}
		ownerChanged = newOwner != inst.OwnerOf()
	}

	if err := d.UpdateInstance(inst, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ownerChanged {
		inst.SetOwner(newOwner)
		d.markDirty()
	}
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid, "owner": inst.OwnerOf()})
}

// handleInstanceDelete 删除实例。
// DELETE /api/instance?daemonId  body: {uuids: [...], deleteFile: bool}
//
// 实例级授权（红队审计 高-3）：任一 uuid 不属于当前账户（且非公共实例、
// 调用者非管理员）即整体拒绝，不做部分删除。
func (d *Daemon) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Uuids      []string `json:"uuids"`
		DeleteFile bool     `json:"deleteFile"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	// 先整体鉴权：避免「删除 succeed 一部分、拒绝一部分」导致客户端状态不明。
	for _, uuid := range body.Uuids {
		inst := d.Find(uuid)
		if inst == nil {
			continue // 不存在视为已删除（幂等），由下方 Remove 返回错误后跳过
		}
		if !d.canAccessInstance(r, inst) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("无权删除实例 %s（属主: %s）", uuid, inst.OwnerOf()))
			return
		}
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
			inst.SetStatus(StatusRunning)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := d.forgetProcessFor(inst.InstanceUuid, proc); err != nil {
			writeError(w, http.StatusInternalServerError, "清理进程记录失败: "+err.Error())
			return
		}
	}
	inst.SetStatus(StatusStopped)
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// handleInstanceCommand 发送命令。
// GET /api/protected_instance/command?uuid&daemonId&command
func (d *Daemon) handleInstanceCommand(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
	inst, err := d.requireInstanceAccess(r)
	if err != nil {
		writeInstanceAccessError(w, err)
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
	if !d.processStarts.enter() {
		return fmt.Errorf("节点正在关停，无法启动实例")
	}
	defer d.processStarts.leave()
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
	// （VaultFiles 也必须在此一并取出——此前在解锁后读 inst.Config.VaultFiles，
	// 与并发更新构成数据竞争，违反 AGENTS.md「访问 Config 前必须加锁」）
	startCommand, cwd := inst.Config.StartCommand, inst.Config.Cwd
	vaultFiles := inst.Config.VaultFiles
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
	// vaultFiles 物化（D9）：崩溃残留回收（幂等）→ 余量预检 → 解密物化到工作目录
	if vaultFiles {
		if err := d.vaultMaterialize(inst, cwd); err != nil {
			inst.SetStatus(StatusStopped)
			return fmt.Errorf("文件区物化失败: %w", err)
		}
	}
	proc, err := startProcess(startCommand, cwd, lc)
	if err != nil {
		inst.SetStatus(StatusStopped)
		return fmt.Errorf("启动失败: %w", err)
	}
	record, err := d.recordProcess(inst.InstanceUuid, proc)
	if err != nil && proc.IsRunning() {
		_ = proc.Kill()
		inst.SetStatus(StatusStopped)
		return fmt.Errorf("保存进程身份失败，已终止进程: %w", err)
	}
	proc.record = record

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
	d.processWatchers.Add(1)
	go func() {
		defer recoverLog("实例进程看门狗")
		defer d.processWatchers.Done()
		select {
		case <-proc.done:
		case <-d.workers.ctx.Done():
			return
		}
		if err := d.forgetProcess(record); err != nil {
			alog.Printf("清理实例 %s 进程记录失败: %v", inst.InstanceUuid, err)
		}
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
	// 保险库锁定态禁止自动重启（docs/vault-design.md §7.3）
	if d.vault != nil && d.vault.enabled && !d.vault.unlockedSafe() {
		d.auditLogf("vault.autoRestart.blocked 实例 %s 自动重启被拒绝（保险库锁定）", inst.InstanceUuid)
		return
	}
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
	if err := d.stopAll(timeout); err != nil {
		alog.Printf("关停实例存在失败步骤: %v", err)
	}
}

// stopAll 将停止和持久化错误交给运行时汇总，保证其余实例仍然得到清理。
func (d *Daemon) stopAll(timeout time.Duration) error {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()

	var wg sync.WaitGroup
	stopErrors := make(chan error, len(insts))
	type recycler struct {
		inst *Instance
		cwd  string
	}
	var toRecycle []recycler
	for _, inst := range insts {
		inst.mu.Lock()
		proc := inst.Proc
		// 提前解除引用：关停属于主动行为，不触发 AutoRestart
		inst.Proc = nil
		// 昵称必须在锁内取出：goroutine 里读 inst.Config 与并发 Update 竞争
		stopCmd, nickname, cwd := inst.Config.StopCommand, inst.Config.Nickname, inst.Config.Cwd
		vaultFiles := inst.Config.VaultFiles
		inst.mu.Unlock()
		if proc == nil || !proc.IsRunning() {
			continue
		}
		wg.Add(1)
		go func(inst *Instance, proc *Process, stopCmd, nickname string) {
			defer recoverLog("实例停止")
			defer wg.Done()
			alog.Printf("正在停止实例 %s（%s）", inst.InstanceUuid, nickname)
			if err := proc.Stop(stopCmd, timeout); err != nil {
				stopErrors <- fmt.Errorf("停止实例 %s 失败: %w", inst.InstanceUuid, err)
			} else if err := d.forgetProcessFor(inst.InstanceUuid, proc); err != nil {
				stopErrors <- fmt.Errorf("清理实例 %s 进程记录失败: %w", inst.InstanceUuid, err)
			}
			inst.SetStatus(StatusStopped)
		}(inst, proc, stopCmd, nickname)
		if vaultFiles {
			toRecycle = append(toRecycle, recycler{inst, cwd})
		}
	}
	wg.Wait()
	close(stopErrors)
	var errs []error
	for err := range stopErrors {
		errs = append(errs, err)
	}
	// vaultFiles 回收（D9）：优雅关停时整树加密入库（串行，避免并发回收竞态）
	for _, rc := range toRecycle {
		if err := d.vaultRecycle(rc.inst, rc.cwd); err != nil {
			errs = append(errs, fmt.Errorf("实例 %s 文件树回收失败（解锁后自动重试）: %w", rc.inst.InstanceUuid, err))
		}
	}
	if err := d.Save(); err != nil {
		// 保险库锁定态：已拒绝明文落盘（符合「实例列表始终加密」承诺，
		// 审计 高-7）。加密存储的索引在 lock 时已 flush，此处没有待落盘内容，
		// 因此不作为关停错误上报。
		if errors.Is(err, errVaultLockedPlaintext) {
			alog.Printf("保险库锁定：关停时跳过 instances.json 明文落盘（配置保持在上次加密快照）")
		} else {
			errs = append(errs, fmt.Errorf("关停时保存实例状态失败: %w", err))
		}
	}
	return errors.Join(errs...)
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
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.Busy = false
		inst.mu.Unlock()
	}()

	err := proc.Stop(stopCmd, 30*time.Second)
	if err != nil && proc.IsRunning() {
		inst.mu.Lock()
		inst.Proc = proc
		inst.Status = StatusRunning
		inst.mu.Unlock()
		return err
	}
	if err == nil {
		if forgetErr := d.forgetProcessFor(inst.InstanceUuid, proc); forgetErr != nil {
			err = forgetErr
		}
	}
	inst.SetStatus(StatusStopped)
	_ = d.Save()
	// vaultFiles 回收（D9）：停止后整树加密入库并删除明文
	if inst.Config.VaultFiles {
		if rerr := d.vaultRecycle(inst, cwd); rerr != nil {
			alog.Printf("实例 %s 文件树回收失败（解锁后或下次启动时重试）: %v", inst.InstanceUuid, rerr)
		}
	}
	return err
}
