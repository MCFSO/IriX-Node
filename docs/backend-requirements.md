# 后端需求文档（由 IriX-Node 维护者实现）

> 前端（Web 面板）只负责 `src/` 内的代码，后端（`IriX-Node/`）由你维护。
> 本文档列出前端需要的后端改动，按优先级排列。每一项都给出了完整契约与实现提示，
> 供你决定是否采纳、如何实现。
>
> **契约原则**：前端只按本文档的字段名/语义解析，任何偏差会直接表现为前端异常。

---

## P0：审计日志只读接口（前端「审计」页依赖）

前端侧边栏「审计」页需要查看节点审计日志。当前后端没有暴露审计文件的接口。

### 端点

```
GET /api/audit/log?tail=<行数>&since=<unix_ms>
```

- 认证：与其他 API 一致（`apikey` 查询参数或 `X-Api-Key` 请求头），走现有 `d.auth` 包装。
- 响应：统一信封 `{status, data, time}`；`data` 为**纯文本字符串**（审计行，`\n` 分隔）。
- 错误：`status != 200` 时 `data` 为中文错误消息（与现有 `writeError` 一致）。

### 参数语义

| 参数 | 说明 |
| --- | --- |
| `tail` | 返回最后 N 行。缺省 500；`0` = 全部（前端不调用 0，仅后端兜底）；建议上限 20000 防误用。 |
| `since` | Unix 毫秒。存在时返回该时间点之后新增的内容（前端 5 秒轮询增量用）。可按文件 mtime 近似过滤（允许多返回几行，不允许丢行）。 |

### 行为约定

1. `-audit-log=false`（`d.AuditLog == nil`）或日志文件尚不存在时：返回 `data: ""`（200），不报错。
2. 内容来源：`{data}/logs/audit.log` 与轮转文件 `.1`（旧→新拼接；实现可直接复用
   `instance_logs.go` 里的 `readLogTail(paths, n)` / `readLogSince(paths, sinceMs)`，
   传入 `[]string{审计文件路径 + ".1", 审计文件路径}`）。
3. 审计行本身已由 `auditMiddleware` 做 apikey 打码与控制字符转义，接口**不要再做任何改动**，
   原样输出即可（前端按行渲染）。
4. 该接口自身也会被审计中间件记录（与其他 API 一致），无需特殊处理。

### 参考实现（可直接采纳或修改；此前已按此实现并验证过：vet/build/test 全绿）

```go
// 路由注册（instance.go 的 RegisterRoutes，概览之后）：
mux.HandleFunc("GET /api/audit/log", d.auth(d.handleAuditLog))

// 处理器（audit.go 末尾追加）：
// handleAuditLog 读取审计日志（只读查看；内容已经过 apikey 打码与控制字符转义）。
// GET /api/audit/log?tail=<行数>&since=<unix_ms>
//   - tail：返回最后 N 行（默认 500；显式 0 表示全部，上限 20000）
//   - since：返回该时间点之后新增的内容（前端轮询增量用，按文件 mtime 近似过滤）
// -audit-log=false 或日志文件尚不存在时返回空字符串。
func (d *Daemon) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if d.AuditLog == nil {
		writeOK(w, "")
		return
	}
	// 旧→新：轮转归档在前，当前文件在后（readLogTail 按此顺序倒读）
	paths := []string{d.AuditLog.path + ".1", d.AuditLog.path}
	var (
		out string
		err error
	)
	if queryParam(r, "since") != "" {
		since := int64(atoiDefault(queryParam(r, "since"), 0))
		out, err = readLogSince(paths, since)
	} else {
		tail := atoiDefault(queryParam(r, "tail"), 500)
		if tail < 0 {
			tail = 0
		}
		if tail > 20000 {
			tail = 20000
		}
		out, err = readLogTail(paths, tail)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取审计日志失败: "+err.Error())
		return
	}
	writeOK(w, out)
}
```

### 前端联动

- 未实现时：审计页显示「当前节点版本过旧，不支持审计日志接口」提示卡片，其余功能不受影响。
- 实现后：审计页自动可用（tail 加载 / 5 秒 since 增量 / 导出），**无需改前端**。

---

## P1：集群同步区在相对 `-data` 路径下的双重拼接 bug

### 现象

节点以**相对路径**启动（如 `irix-node -data dev-node-data`）时：

- `GET /api/cluster/files/list?path=/mirrors` → 400
  `open dev-node-data\mirrors\dev-node-data\mirrors: The system cannot find the path specified.`
- `GET /api/cluster/sync/list?path=/mirrors` → 500 枚举失败（同因）

以绝对路径 `-data` 启动则一切正常。

### 定位

`cluster.go`：

```go
func (d *Daemon) clusterRoot() string {
	return filepath.Join(d.DataDir, "mirrors")   // DataDir 为相对路径时返回相对路径
}
```

`handleClusterFileList` 中：

```go
dir, err := d.clusterPath(queryParam(r, "path"))      // 同样可能是相对路径
items, total, abs, err := listDir(d.clusterRoot(), dir, ...)
//      listDir 内部：NormalizePath(cwd, target)
//      target 是相对路径时 → filepath.Join(cwd, target) → 根被拼接两次
```

### 修复建议（任选）

1. `clusterRoot()` 返回绝对路径：`filepath.Join` 前先 `filepath.Abs(d.DataDir)`；
   或
2. `handleClusterFileList` 调用 `listDir` 前把 `dir` 转成绝对路径。

顺带检查同文件内其他用 `clusterRoot()` 的路径（mkdir / delete / download / upload / sync/list），
统一改为绝对路径解析即可。

---

## 附：前端当前状态说明

- 前端审计页（`src/pages/Audit.jsx`）已就绪，只等 P0 接口；
- P1 修复前，集群页同步区浏览在「相对 -data」部署下会显示错误卡（前端已做空态兜底）；
- 前端不再修改 `IriX-Node/` 下的任何文件；后续新增需求一律补充到本文档。
