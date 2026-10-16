# IriX-Node 红队审计报告：集群 / FRP / WebSocket / 网络层

审计范围：`cluster.go`、`frp.go`、`websocket.go`、`console_ws.go`、`http_server.go`、`cors.go`、`audit.go`、`download.go`，辅以 `main.go`、`instance.go`、`accounts_handlers.go`、`perm_catalog.go`、`vault.go`、`files.go`、`docs/cluster-node-api.md`。
结论基于源码逐行确认；每条均附 `文件:行号` 与原文片段。

## 1. 严重

无。

（说明：本层最重的两条 —— 控制台 WS 越权下发 stdin 命令、FRP 上传任意二进制并执行 —— 前置条件均需一个已认证账户持有对应端点权限，故按「高」定级并写明升级链。审计中未发现无需认证即可利用的问题。）

## 2. 高

### [高] 控制台 WebSocket 可越权下发 stdin 命令（权限判定与 `command` 端点脱钩）+ 无速率限制

**位置**：`console_ws.go:144-158`、`instance.go:92-93`、`instance.go:218-231`

**证据**（`console_ws.go:144-158`）：

```go
		case wsText:
			msg := string(payload)
			if msg == "ping" {
				continue // 心跳文本帧
			}
			inst.mu.Lock()
			p := inst.Proc
			inst.mu.Unlock()
			if p == nil || !p.IsRunning() {
				_ = conn.writeText("[节点] 实例未在运行，命令未发送")
				continue
			}
			if err := p.WriteCommand(msg); err != nil {
```

**证据**（同一能力在 REST 侧是**独立开关的独立端点**）：`instance.go:92-93`

```go
	perm("实例", "GET /api/protected_instance/command", "向实例发送命令")
	mux.HandleFunc("GET /api/protected_instance/command", d.auth(d.handleInstanceCommand))
```

**证据**（WS 只被包裹在入口 `d.auth`，handler 内不再做端点级判定）：`instance.go:126-127`

```go
	perm("实例", "GET /api/instance/console/ws", "实时控制台")
	mux.HandleFunc("GET /api/instance/console/ws", d.auth(d.handleConsoleWS))
```

**前置条件**：账户拥有 `GET /api/instance/console/ws`（控制台）权限（管理员或任意被开启该开关的账户），且目标实例正在运行。`instances` 无属主模型，任何账户对任何 uuid 一视同仁。

**攻击输入**：`GET /api/instance/console/ws?uuid=<受害实例>&token=<token>` 升级成功后，发送文本帧（内容即 stdin 一行）：`stop` / `op <玩家> deop` / `whitelist off` / 若服务端有执行型指令（如 `/exec`、`/run`、插件命令）则可进一步在宿主机执行命令。载荷无需掩码之外的任何额外条件。

**影响**：把「只读控制台」权限升级为「向任意实例下发任意命令」。若目标实例是 Minecraft Java 服务端且装有可执行系统命令的插件/Mod（RCON、`/script`、`bukkit:execute` 等），构成节点侧远程命令执行；即使无插件，也足以破坏游戏状态（关服、掉 OP、改白名单）。同时 `readFrame` 无速率限制（`websocket.go:26` 仅限 1 MiB/帧），单个持有控制台权限的账户可在 `p.mu` 保护的 stdin 管道上无限灌入命令（`process.go:505-519`），造成实例侧资源耗尽。

**修复**：`console_ws.go` 在 `case wsText` 分支内按 `GET /api/protected_instance/command` 的权限键再判定一次（复用 `d.permAllowed(identityOf(r).username, "GET /api/protected_instance/command")`，`id.isAdmin` 旁路），无权则回文本帧拒绝而非静默丢弃；同时给文本帧加令牌桶限速（例如 5 条/秒、突发 20）与单条长度上限。

### [高] `POST /api/frp/binary` 上传任意文件到 `{data}/frp/` 并立即执行 → 节点级 RCE + 数据面篡改

**位置**：`frp.go:450-504`、`frp.go:65-80`、`frp.go:213`

**证据**（上传：文件名硬编码 `frpc`/`frpc.exe`，内容零校验、非 Windows 直接 `chmod 0755`）：`frp.go:466-499`

```go
	if err := os.MkdirAll(d.frpDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	name := "frpc"
	if runtime.GOOS == "windows" {
		name = "frpc.exe"
	}
	dest := filepath.Join(d.frpDir(), name)
	tmp := dest + ".upload"
	out, err := os.Create(tmp)
```

```go
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o755)
	}
```

**证据**（执行：随后任意隧道 start 即以该文件为可执行体）：`frp.go:195`、`frp.go:213`

```go
	binary := d.frpcBinaryPath()
 ...
	proc, err := startProcess(binary+" -c "+quotePath(cfgPath), d.frpDir(), nil)
```

**前置条件**：账户拥有 `POST /api/frp/binary`（FRP 权限组）权限；`{data}/frp/frpc(.exe)` 是其运行所必需的、通常由管理员放置的合法二进制。

**攻击输入**：multipart 上传一个伪装成 frpc 的可执行体（Linux ELF / Windows PE 均可），再 `POST /api/frp/tunnels` 创建任一条隧道（创建即启动，`frp.go:339`）即触发执行。

**影响**：（1）以守护进程权限执行任意代码（节点 RCE）；（2）同时覆盖掉合法 frpc 二进制，破坏既有隧道（完整性/可用性）。审计日志只记路径 `POST /api/frp/binary` 与请求体前缀（`audit.go:238-241`），不含文件哈希，事后难以取证。

**修复**：上传后校验必需指纹/签名（发布方公钥或管理员预置 SHA-256 白名单）后方可落位，不匹配立即删除并告警；或改为「仅允许管理员 + 需要显式 `-allow-frp-binary-upload`」；上传与 `Rename` 之间做哈希校验防 TOCTOU。

### [高] WebSocket 升级路径无 `Origin` 校验：任何网页可发起控制台连接（CSWSH）

**位置**：`websocket.go:54-88`、`cors.go:22-47`

**证据**（`upgradeWS` 只校验 `Sec-WebSocket-Key` / `Upgrade` / `Connection`，从读 `Origin`）：`websocket.go:54-64`

```go
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "缺少 Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("缺少 Sec-WebSocket-Key")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "非 WebSocket 升级请求", http.StatusBadRequest)
		return nil, errors.New("非 WebSocket 升级请求")
	}
```

**证据**（CORS 中间件对 WS 升级请求只是加头放行，不构成浏览器侧拦截）：`cors.go:24-31`、`cors.go:46`

```go
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			// 非浏览器请求（curl / 服务间调用）：无 Origin 可回显，放行全部来源
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
```

**前置条件**：受害者浏览器所在页面已持有（或可拼出）`?token=`。认证走 `?token=` 查询参数（`accounts_handlers.go:49-58`），浏览器 WS 无法自定义请求头，因此 token 必然出现在 URL 中，会随 Referer、浏览器历史、代理/CDN 日志、桌面客户端日志、截图外泄。WS 不受同源策略保护，唯一防御就是服务端校验 `Origin` —— 此处完全缺失。

**攻击输入**：恶意页面执行 `new WebSocket("ws://127.0.0.1:12346/api/instance/console/ws?uuid=<uuid>&token=<泄漏的token>")`，握手必然成功（无任何 Origin 判定），随后 `ws.send("stop")` 即可停服；结合上一条可下发任意命令。

**影响**：从「token 泄漏」到「跨站直接操控节点控制台」的利用链；节点默认仅监听 127.0.0.1，但受害者浏览器恰好能访问 127.0.0.1，DNS rebinding 与非本机部署（`-bind 0.0.0.0`）进一步扩大面。

**修复**：`upgradeWS` 增加 Origin 白名单校验（取 `r.Header.Get("Origin")`，与配置的面板来源比对；不匹配则 403 且不升级）；更彻底的做法是不在 URL 放长期会话 token，改为一次性、短时效（≤30s）、绑定 uuid 的 WS 票据。

### [高] CORS 反射任意 Origin + `Access-Control-Allow-Credentials: true`（跨源可读全部 API 响应）

**位置**：`cors.go:24-34`

**证据**：

```go
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			// 非浏览器请求（curl / 服务间调用）：无 Origin 可回显，放行全部来源
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		// 浏览器读取下载文件名等自定义响应头需要显式暴露
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
```

**前置条件**：无需认证即可完成预检（`cors.go:37-45` 直接 204 终结，不进入路由与保险库门禁）。

**攻击输入**：任意站点 `fetch("http://127.0.0.1:12346/api/cluster/status", {credentials:"include"})`。响应头回显该站点 Origin 且允许凭据，浏览器放行读取。

**影响**：把「跨源」当作已授权。当前认证材料是 `Authorization` / `X-Auth-Token` 头或 `?token=`，浏览器不会自动附带，因此**不能直接凭 cookie 打穿认证**；但一旦宿主把 token 放进 Cookie/`localStorage` 并被脚本读取，或 `?token=` 出现在被恶意页面可控的 URL 中，即可全量读取 `/api/cluster/*`、`/api/frp/*`、`/api/instance/sync/list` 等响应（含对等节点地址、隧道配置与 token、节点绝对路径）。`Access-Control-Expose-Headers: Content-Disposition` 还会把 `/download/` 票据通道的文件名暴露给任意站点。

**修复**：改为 Origin 白名单（配置项，默认仅 127.0.0.1/localhost 的面板端口）；非白名单来源不回 `Access-Control-Allow-Origin`，绝不回显；仅在确需凭据时才回 `Allow-Credentials`；预检继续免认证但必须过白名单。

## 3. 中

### [中] 审计日志明文落盘集群会话 token 与 FRP 认证 token

**位置**：`audit.go:169-173`、`audit.go:198-206`、`audit.go:227-241`、`cluster.go:598-609`、`frp.go:305-314`

**证据**（打码字段表只覆盖保险库字段，不含 `token`/`auth.token`）：`audit.go:169-173`

```go
var vaultMaskFields = []string{
	`"password"`, `"newPassword"`, `"oldPassword"`,
	`"totp"`, `"signature"`, `"recoveryToken"`, `"sessionToken"`,
}
```

**证据**（请求体前 2KB 原文进审计行，仅对上述字段替换）：`audit.go:227-233`

```go
		body := ""
		if ab != nil {
			body = redactBody(string(ab.kept), strings.HasPrefix(r.URL.Path, "/api/vault/"))
			if ab.truncated {
				body += "…(截断)"
			}
		}
```

**证据**（被记录的字段名就是 `token` / `config.token`）：`cluster.go:604` `Token string \`json:"token"\``；`frp.go:309` `Config   map[string]any \`json:"config"\``

**前置条件**：账户拥有 `GET /api/audit/log`（审计组）权限，或能读到 `{data}/logs/audit.log`（含 `.1` 归档与 `{data}/backup/audit/`）。

**攻击输入**：`POST /api/cluster/transfer {"source":{"token":"<30 天有效的会话 token>"}}`、`POST /api/frp/tunnels {"config":{"token":"<FRP 服务端认证 token>"}}`，随后 `GET /api/audit/log?tail=20000`。

**影响**：token 打码只覆盖查询串（`redactQuery`）与保险库字段，请求体里的**跨节点会话 token**（最长 30 天有效）与 **FRP 服务端 auth token** 明文落盘；拿到审计日志（低权账户即可申请该权限）后可横向登录其他节点。属于凭据二次泄露面。

**修复**：`vaultMaskFields` 增加 `"token"`、`"auth.token"`、`"password"`（FRP/cluster 之外的通用敏感键）；`redactBody` 对 `token` 类键做不区分大小写的键名匹配；`/api/frp/tunnels` 与 `/api/cluster/transfer` 路径强制走 mask（不限于 `/api/vault/`）；响应体不回显 `config.token`（`frp.go:269-277` 目前原样回显）。

### [中] 审计打码可被 `;` 分隔的查询串绕过（token 明文落盘）

**位置**：`audit.go:112-126`、`accounts_handlers.go:57`

**证据**（打码按 `&` 切分）：`audit.go:116-125`

```go
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		lower := strings.ToLower(p)
		if strings.HasPrefix(lower, "token=") {
			parts[i] = "token=***"
		} else if strings.HasPrefix(lower, "apikey=") {
			parts[i] = "apikey=***"
		}
	}
	return "?" + strings.Join(parts, "&")
```

**证据**（认证侧走 `url.Query()`，与上面按 `&` 手工切分的语义不一致）：`accounts_handlers.go:57`

```go
	return strings.TrimSpace(r.URL.Query().Get("token"))
```

**实测**（本机 `go test` 验证 `net/url` 行为）：

```
raw="token=SECRET"        Query.Get(token)="SECRET"  redacted="?token=***"
raw="a=1;token=SECRET"    Query.Get(token)=""        redacted="?a=1;token=SECRET"   ← 未打码
raw="a=1;b=2&token=SECRET" Query.Get(token)="SECRET" redacted="?a=1;b=2&token=***"
```

**前置条件**：攻击者能在目标账户的请求 URL 中控制查询串（诱导点击/被污染的客户端配置），或自身需要伪造一条「token 明文」的审计记录。

**攻击输入**：`GET /api/overview?a=1;token=<真实会话 token>` —— 认证会因 `token` 落在含 `;` 的字段而被 `net/url` 丢弃（该请求本身 403），但审计中间件对**所有**请求（含 403）都写日志，`RawQuery` 原样落盘，于是 token 以明文进入 `audit.log`。

**影响**：审计日志的凭据脱敏承诺（`audit.go` 头注释）在该情形下失效；日志读者可获得明文会话 token。相比上一条，此处需要攻击者已经知道 token（用于污染日志），因此主要是「日志可信度 / 取证污染」问题，与上一条叠加后扩大面。

**修复**：`redactQuery` 改为基于 `url.ParseQuery` 的键值遍历（对解析失败的 raw 串同样做正则兜底替换 `(?i)(^|[&;])token=[^&;]*`）；或统一改名为 `access_token` 并只在该单一键名上脱敏。

### [中] FRP 隧道可把节点本机任意 `localIP:localPort` 服务经内网穿透暴露到公网

**位置**：`frp.go:131-187`（`buildFRPConfig`）、`frp.go:178-179`、`frp.go:13-20`（文档承认「原样下发」）

**证据**：

```go
	fmt.Fprintf(&sb, "localIP = %q\n", firstNonEmpty(get("localIP"), "127.0.0.1"))
	fmt.Fprintf(&sb, "localPort = %d\n", localPort)
```

**证据**（`self` 类型完全原样下发，不受任何字段白名单约束）：`frp.go:132-138`

```go
	if t.Provider == "self" {
		raw, _ := t.Config["toml"].(string)
		if strings.TrimSpace(raw) == "" {
			return "", errors.New("self 类型需要 config.toml（完整 frpc 配置）")
		}
		return raw, nil
	}
```

**前置条件**：账户拥有 `POST /api/frp/tunnels` 权限（FRP 组）。

**攻击输入**：`provider="self"`，`config.toml` 中 `[[proxies]] localIP="127.0.0.1" localPort=12346 remotePort=<任意>`，或直接指定 `localIP`/`localPort` 指向节点 API（默认 12346）、Redis（6379）、MySQL（3306）、`{data}` 上的其他本机监听端口。

**影响**：把一个持有 FRP 权限的低权账户升级为「把节点 API 暴露到攻击者控制的公网 frps」，绕过 `-bind 127.0.0.1` 的本地暴露约束；隧道一旦建立，节点 API 的会话 token 即可被无限次尝试（`POST /api/auth/login` 的限速按来源 IP，经 frps 后全部来自 frps 的地址）。节点侧对 `localIP`/`localPort`/`remotePort` 无端口白名单、无 deny 列表、无提示。

**修复**：对 `localIP` 加白名单（仅 `127.0.0.1` 且禁止 `0.0.0.0`/`::`），对 `localPort` 加黑名单（节点自身监听端口 `d.Port`、账户库/Redis 端口、`privileged ports`），并在创建隧道时记录告警审计行；`provider=self` 至少做一次「代理指向本机 API 端口」的静态检查。

### [中] 集群下载/上传票据按目录范围生效 → 可越界读取同步区任意文件并借符号链接逃出同步区

**位置**：`cluster.go:256-283`、`cluster.go:285-318`、`download.go:223-253`

**证据**（集群下载票据 `file` 传空串，即目录范围；`cwd` 为同步区根）：`cluster.go:275-277`

```go
	// 集群票据为目录范围票据（file 为空）：下载路径按同步区根解析，
	// 兼容 /mirrors/... 虚拟前缀（见 handleDirectDownload）。
	password := tickets.CreateDownload("cluster", d.clusterRoot(), "")
```

**证据**（`file == ""` 时跳过「仅绑定文件」校验，路径由客户端任意指定）：`download.go:240-252`

```go
	filePath, err := NormalizePath(t.cwd, seg[1])
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	// 单文件票据：只允许下载申请时绑定的文件，杜绝同目录越权读取
	// 与 http.ServeFile 的目录列表浏览（请求目录时同样在此被拒）。
	if t.file != "" && !sameFilePath(filePath, t.file) {
		http.Error(w, "下载票据仅限绑定文件", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(filePath)))
	http.ServeFile(w, r, filePath)
```

**前置条件**：账户拥有 `POST /api/cluster/files/download` 权限（集群组）。票据有效期 10 分钟（`download.go:92`）。

**攻击输入**：申请票据时 `path=/mirrors/x/any.txt`（只为通过 `os.Stat` 存在性检查，`cluster.go:271-274`），随后 `GET /download/<password>/../../../../etc/passwd`（或 `/download/<password>/<同步区任意相对路径>`）。`NormalizePath` 只做**词法**约束（`main.go:516-541` 无 `EvalSymlinks`），若同步区内存在指向区外的符号链接（可由同权限的 `POST /upload/`、`POST /api/cluster/files/upload` 直连上传配合 zip/前端产物植入），`http.ServeFile` 会跟随该链接读到同步区外的文件。

**影响**：一是「申请一个文件、下载任意文件」的授权范围失真（票据意图是单文件）；二是 `NormalizePath` 的词法边界对符号链接无效，越界读取的最终边界依赖目录中不出现符号链接，属于脆弱的隐含假设。

**修复**：`handleClusterDownload` 改为绑定单文件（`tickets.CreateDownload("cluster", d.clusterRoot(), path)`），仅在 `runTransfer` 这类内部流程使用目录范围票据并额外携带一次性 job 标识；`handleDirectDownload` 在 `os.Open` 前对 `filePath` 与 `t.cwd` 同时做 `filepath.EvalSymlinks` 后重新校验 `pathWithin`。

### [中] 心跳接口无界追加对等节点，无 TTL/上限（内存放大 + 线性退化）

**位置**：`cluster.go:375-403`、`cluster.go:423-429`

**证据**（只要 body 带新的 `id` 就 append，无任何去重上限、过期或签名校验）：

```go
	body["time"] = time.Now().UnixMilli()
	d.clusterMu.Lock()
	d.clusterHeartbeat = body
	if id, _ := body["id"].(string); id != "" {
		addr, _ := body["address"].(string)
		found := false
		for _, p := range d.clusterPeers {
			if p["id"] == id {
				p["address"] = addr
				p["available"] = true
				found = true
				break
			}
		}
		if !found {
			d.clusterPeers = append(d.clusterPeers, map[string]any{
				"id": id, "address": addr, "available": true,
			})
		}
	}
```

**前置条件**：账户拥有 `POST /api/cluster/heartbeat`（集群组）。

**攻击输入**：循环发送 `{"id":"n-<递增>","address":"http://任意","resource":{...}}`。每条约 100 字节常驻内存且永不回收；同时 `handleClusterStatus`（`cluster.go:358-370`）与 `handleClusterPeers` 会序列化整个列表，`handleClusterHeartbeat` 每次做 O(n) 扫描 → n 次请求后 O(n²)。

**影响**：单个低权账户可让常驻内存线性增长（百万次请求 ≈ 百 MB 级），并使集群状态接口随规模退化；`d.clusterHeartbeat` 整体保存请求体（上限 16 MiB，见下条），可放大为单条 16 MiB 的常驻内存。

**修复**：`clusterPeers` 改为 `map[id]*peer` 并加容量上限（如 256）与 `lastSeen` TTL（如 5 分钟）淘汰；心跳体只保留白名单字段（resource/instances/events）而非整个 `map[string]any`。

### [中] 未设置 `ReadTimeout` / `WriteTimeout`。`/upload/` 无整体时限 → slowloris 与慢速写占用

**位置**：`http_server.go:12-19`、`main.go:481-488`

**证据**：

```go
	return &http.Server{
		Addr:    addr,
		Handler: d.trackRequests(d.auditMiddleware(corsMiddleware(d.vaultGate(limitAPIBody(mux))))),
		// 只限制请求头与空闲连接，保留大文件流式上传/下载的能力。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
```

**证据**（body 限额只覆盖 `/api/`，`/upload/` 完全裸奔；`ReadHeaderTimeout` 只保护请求头阶段）：`main.go:483-485`

```go
		if r.Body != nil && strings.HasPrefix(r.URL.Path, "/api/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
		}
```

**前置条件**：无需认证 —— `/upload/{password}` 通道的票据校验在读取 body **之前**（`download.go:258-273`），但 `ParseMultipartForm` 之前的连接可由任意客户端占用；`/api/` 路径的慢速 POST 也无需有效 token 即可占住连接（认证在读完 body 后才发生，中间件链为 audit → cors → vaultGate → limitAPIBody → mux → auth）。

**攻击输入**：大量 `POST /api/instance` 连接发送 `Content-Length: 16777216` 后每 30 秒发 1 字节（`ReadHeaderTimeout` 已在头阶段结束后失效，无 `ReadTimeout`），或 `POST /upload/x` 发送畸形 multipart 边界后挂起。

**影响**：连接与 goroutine 被长期占用；配合 `raiseFDLimit`（`main.go:384-388`）把 fd 上限拉满，攻击者可用极少带宽耗尽 fd/内存，造成拒绝服务。`qa_security_test.go:1131-1132` 明确断言「不应设置 ReadTimeout/WriteTimeout」，说明这是**有意的设计取舍**，但 `/upload/` 缺少独立的整体时限（如 `http.TimeoutHandler` 或按路径的 body 读超时）属于未吸收的残余风险。

**修复**：对 `/upload/`、`/download/` 使用独立的 `http.TimeoutHandler` 或自实现「body 读空闲超时」（如 `conn.SetReadDeadline` 在每次成功读后顺延），保留大文件能力的同时消除挂起连接；对 `/api/` 设置 `ReadTimeout`（如 60s）。

## 4. 低

### [低] `GET /api/instance/sync/list` 与 `GET /api/cluster/sync/list` 单次递归枚举 + 全文件 SHA-256，无上限、无限速

**位置**：`cluster.go:320-354`、`cluster.go:66-77`、`cluster.go:89-125`

**证据**：

```go
func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
```

```go
	entries, err := walkEntries(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "枚举失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"items": entries,
		"total": len(entries),
		"root":  "/",
	})
```

**前置条件**：账户拥有对应 sync/list 权限（集群组或实例组）。`handleInstanceSyncList` 是**另一个权限端点**，仅按实例 uuid 取 cwd，不再校验文件读权限（`cluster.go:333-340`）。

**攻击输入**：对 cwd 内含数十万小文件（或 GB 级单文件）的实例并发多次调用该端点；整个 `entries` 切片与 JSON 序列化结果全部驻留内存。

**影响**：单请求即可产生持续磁盘 IO（每文件一次全量读）与数百 MB 级内存峰值，N 次并发可打满 IO/内存。另外该端点绕过「文件」权限组，仅凭集群/实例端点开关即可拿到实例完整文件树（含 sha256/mtime/size）——对无文件权限的账户构成信息泄露。

**修复**：给 `walkEntries` 加条目数上限（超限返回 413/部分结果 + 截断标记）与累计字节上限；对 `/sync/list` 单独限速（如每账户 1 次/秒）并纳入「文件」权限组判定。

### [低] 集群事件/心跳请求体上限 16 MiB 且事件列表不裁剪单条大小

**位置**：`cluster.go:407-421`、`main.go:477`

**证据**：

```go
	d.clusterEvents = append(d.clusterEvents, body)
	if len(d.clusterEvents) > maxClusterEvents {
		d.clusterEvents = d.clusterEvents[len(d.clusterEvents)-maxClusterEvents:]
	}
```

**前置条件**：账户拥有 `POST /api/cluster/events`。

**攻击输入**：连续 100 次发送约 16 MiB 的 `POST /api/cluster/events`（`maxAPIBodyBytes = 16 << 20`，`main.go:477`）。

**影响**：`maxClusterEvents = 100` 只限制条数，不限制单条大小 —— 最坏情况 100 × 16 MiB ≈ 1.6 GB 常驻内存，且这些内容从不被任何接口返回（无 events 读接口），纯粹是内存注入。

**修复**：对入库事件只保留白名单字段并限制单条序列化后大小（如 32 KiB），超限丢弃并记审计。

### [低] `clusterPeers` 中 `address` 完全由上报方控制，`status`/`peers` 原样回显给其他协助方

**位置**：`cluster.go:384-399`、`cluster.go:356-370`、`cluster.go:423-429`

**证据**（`addr` 直取请求体、不校验协议与格式，也不做 SSRF 校验——与 `transfer` 的严格校验形成反差）：

```go
	if id, _ := body["id"].(string); id != "" {
		addr, _ := body["address"].(string)
```

**前置条件**：账户拥有 `POST /api/cluster/heartbeat`。

**攻击输入**：`{"id":"real-peer-id","address":"http://169.254.169.254/"}` —— 由于 `id` 匹配即覆盖 `address`（`cluster.go:388-393`），可把其他节点/协调器看到的某个真实 peer 的地址改写到任意目标（含云元数据地址）。

**影响**：`GET /api/cluster/peers` 的消费者（协调器、桌面客户端、其他节点的 UI）可能据此发起请求或展示被污染的地址 → 客户端侧 SSRF 与错误路由；也会让 `cluster/transfer` 的「人工选择来源」路径指向攻击者选定的地址（`transfer` 自身仍有 IP 校验，故影响局限于客户端与显示层）。

**修复**：`address` 落库前做与 `validateTransferTarget` 同等或更严的格式校验（仅 http/https、拒绝保留地址段、拒绝非预期端口）；`id` 与来源身份绑定（见下条）。

### [低] 集群节点间无独立凭据：节点必须持有对端账户会话 token，且 token 直接拼进 URL

**位置**：`cluster.go:596-598`、`cluster.go:690-693`、`docs/cluster-node-api.md:202-204`

**证据**：

```go
// POST /api/cluster/transfer body: {instanceId, source: {address, token, uuid, daemonId}, dest}
// source.token 为来源节点的账户会话 token（来源为 MCSM 节点时填其 apikey）。
```

```go
	auth := ""
	if token != "" {
		auth = "?token=" + url.QueryEscape(token)
	}
```

**前置条件**：账户拥有 `POST /api/cluster/transfer`。

**攻击输入**：无独立集群密钥可伪造；但 `source.token` 以查询参数形式出现在节点出站请求的 URL 中（会进入对端节点的访问日志/审计、HTTP 代理日志、`Referer`）。

**影响**：横向移动凭据是「长期（30 天）、高权（需能读实例快照与备份下载票据）」的对端账户 token，没有节点身份绑定、没有到期轮换、没有逐节点作用域；任何拿到该 token 的一方可直接以该账户身份调用对端全部 API。`POST /api/cluster/heartbeat` 也没有任何来源签名校验，任何持有集群心跳权限的账户都能以任意 `id` 冒充对等节点（`cluster.go:384-399`）。

**修复**：引入节点间共享密钥 / 双向 TLS 或按节点签发的短期集群凭据，与用户会话 token 分离；`source.token` 改走请求头（`X-Auth-Token`）而非 URL；心跳与事件接口校验节点身份（签名 + nonce + 时间窗）。

### [低] 票据可重放 10 分钟且全局上限 10000 对任何认证账户开放

**位置**：`download.go:53-54`、`download.go:85-94`、`download.go:97-106`

**证据**：

```go
// maxTickets 票据上限，防止恶意刷票据耗尽内存。
const maxTickets = 10000
```

```go
	password := newUUID()
	ts.tickets[password] = &transferTicket{
		uuid:    uuid,
		kind:    kind,
		cwd:     cwd,
		dir:     dir,
		file:    file,
		expires: time.Now().Add(10 * time.Minute),
	}
```

**前置条件**：账户拥有 `POST /api/files/download`、`POST /api/files/upload` 或集群票据端点之一。

**攻击输入**：循环申请票据直到 10000 上限（`Get` 不删除、不标记已用，同一票据在 10 分钟内可无限次重放）。票据密码为 `newUUID()`（`daemon.go:632-647`，122 bit 熵 + `crypto/rand`），**不可枚举/爆破**，这一点无问题。

**影响**：票据是全局共享的单一存储，任一认证账户刷满 10000 条后，其他账户（含管理员）在旧票据过期前无法再申请下载/上传票据（`download.go:81-83` 返回空串 → 503），构成经认证的可用性影响。票据本身可重放属可接受设计（10 分钟窗口、绑定 cwd/文件、审计已打码路径）。

**修复**：按账户/来源实例分别限额（如每账户 64 条）；票据按「读取一次性」或「N 次使用」计数；上限满时优先淘汰最旧票据而非直接拒绝。

### [低] 传输任务表只增不减，且任务状态无属主校验

**位置**：`cluster.go:632-639`、`cluster.go:644-656`

**证据**：

```go
	jobID := newUUID()
	job := &transferJob{status: "running"}
	d.clusterMu.Lock()
	d.transfers[jobID] = job
	d.clusterMu.Unlock()
```

**证据**（`jobID` 由调用方提供，命中即返回，不校验请求者是否创建该任务）：

```go
	jobID := queryParam(r, "jobId")
	d.clusterMu.Lock()
	job := d.transfers[jobID]
	d.clusterMu.Unlock()
```

**前置条件**：账户拥有 `POST/GET /api/cluster/transfer`。

**攻击输入**：反复创建 transfer 任务（每个任务还会拉起 goroutine 并请求远端，见 `cluster.go:659`）；`jobID` 为 UUID 不可枚举，故越权读取他人任务实际不可行。

**影响**：`d.transfers` 永不清理 → 长时间运行后 map 与 `transferJob` 永久驻留（每个任务还携带 error 对象）；这是确定的内存缓慢增长（泄漏），而非可即时放大的攻击。

**修复**：任务完成后保留 TTL（如 30 分钟）并由清理协程删除；给 `transferJob` 加创建者用户名，状态查询校验属主或管理员。

### [低] 集群状态接口返回节点 UUID 与同步区绝对路径

**位置**：`cluster.go:361-369`、`cluster.go:203-207`、`cluster.go:163`

**证据**：

```go
	writeOK(w, map[string]any{
		"monitorNodeId": d.clusterMonitor,
		"role":          d.clusterRole,
		"peers":         d.clusterPeers,
		"self": map[string]any{
			"id":      d.UUID,
			"address": "http://" + d.publicAddr(),
		},
	})
```

```go
		"absolutePath": "/mirrors" + abs, // 补回虚拟前缀，供客户端直接作 path 参数复用
```

**证据**（`listDir` 返回的本就是绝对路径）：`files.go:163`

```go
	abs := strings.ReplaceAll(strings.TrimPrefix(dir, cwd), "\\", "/")
```

**前置条件**：任一持有集群组端点权限的账户。

**影响**：泄露部署方主机名/内网 IP:端口（`publicAddr()` 在 `-bind 0.0.0.0` 时返回真实网卡 IP，`download.go:327-342`）与节点 UUID，为后续横向扫描与其他集群接口（`heartbeat` 用 `id` 覆盖 `address`）提供输入。属信息收集便利，非直接越权。

**修复**：`publicAddr` 对集群/客户端只返回与请求 `Host` 一致的地址；节点 UUID 只在管理员可见接口返回；`absolutePath` 只回 `/` 前缀的虚拟路径。

## 5. 待确认

- **WebSocket 单账户连接数上限**：`registerConsole`/`consoles`（`http_server.go:41-55`）只做关停登记，未按账户或来源 IP 限流。每连接固定消耗 1 读循环 + 1 写循环 + 1 心跳 goroutine（`console_ws.go:96`、`console_ws.go:119`）。持有控制台权限的账户能打开多少连接仅受 fd 上限（`main.go:384-388` 已主动拉满）约束；未实测大规模连接下的实际内存/吞吐拐点，故不列为确定漏洞。
- **`-transfer-allow-cidr` 放行后的一致性**：`validateTransferTarget` 在 `d.transferAllowLoopback` 为真时跳过**全部**校验（含协议白名单，`cluster.go:456-458`），`transferDialContext` 同样跳过（`cluster.go:576-581`）。该标志仅在测试中置位（`qa_cluster_test.go:440`、`qa_core_test.go:90`），生产无 flag 可开启，故不列为漏洞；但若后续暴露为配置项，将立即成为完整 SSRF。建议在 `validateTransferTarget` 中把协议校验提到 `transferAllowLoopback` 判断之前。
- **符号链接逃逸的实际可利用性**：`NormalizePath`（`main.go:516-541`）为纯词法校验，`clusterPath`/`listDir`/`unzip`/`http.ServeFile` 全链路均未做 `EvalSymlinks`。已确认 `archive/zip` 解压不会创建符号链接（`files.go:810-838` 以 `os.Create` 逐个写入普通文件），因此需要另一条能植入符号链接的路径（本地用户、其他服务提供的文件、`POST /upload/` 直连上传后由外部解压）。未在代码中找到不经操作系统配合即可植入符号链接的节点 API，故降级为「待确认 + 加固建议」。
- **FRP TOML 配置注入**：`buildFRPConfig` 采用 Go `%q` 转义（`frp.go:164-185`），新建行需要注入裸 `\n`，而 `%q` 会把 CR/LF 转义为 `\n` 序列（TOML 在字符串内解析，不换行）；数值字段经 `num()` 只接受 float64/int（`frp.go:146-154`），字符串无法进入端口位置。**未发现可确认的注入**。唯一不确定点是 TOML 对 Go `%q` 特有转义（`\a`、`\v`、`\x..`）的容忍度：不兼容时 frpc 启动即解析失败（fail-closed），但若某些解析器宽松处理则需重新评估。建议改用 TOML 专用编码器或至少过滤控制字符。
- **`/api/audit/log?tail=20000` 的内存峰值**：`readLogTail`（`instance_logs.go:61`）与 `readLogSince` 会读取 `audit.log` 与 `audit.log.1`。审计文件单文件上限 64 MiB（`main.go:266`），因此最坏单次读取量级为百 MB 级；未实测并发放大倍数，故不单列条目。

## 6. 加固建议

1. **WS 通道的授权模型与 REST 对齐**：把 `console_ws.go` 的每条文本帧当作一次「命令下发」请求，逐条走 `permAllowed`（`console_ws.go:144-158`），并把 `Origin` 白名单校验放进 `upgradeWS`（`websocket.go:54-64`）；控制台改用一次性短时票据而非长期会话 token。
2. **CORS 改白名单**：`cors.go:24-31` 不再回显任意 `Origin`；仅在白名单内回 `Allow-Credentials`；`Access-Control-Expose-Headers` 收敛到确有需要的端点（建议不下发到 `/download/`）。
3. **敏感字段脱敏改为键名驱动**：`vaultMaskFields`（`audit.go:169-173`）增加 `token`、`auth.token`、`secret`、`key`；`redactQuery`（`audit.go:112-126`）改为 `url.ParseQuery` 遍历 + 正则兜底（覆盖 `;` 分隔与畸形串）；`apiPost` 类响应中的 `config.token` 不回显（`frp.go:269-277`）。
4. **为 FRP 建立能力边界**：`localIP`/`localPort` 白名单与节点自身端口黑名单；`provider=self` 增加静态检查；`POST /api/frp/binary` 增加哈希白名单与管理员限制（`frp.go:450-504`）。
5. **票据范围与生命周期**：集群下载票据绑定单文件（`cluster.go:277`）；`handleDirectDownload` 在 `os.Open` 前对目标与 `t.cwd` 做 `EvalSymlinks` 后重新 `pathWithin`（`download.go:240-252`）；按账户分票据配额。
6. **网络层资源边界**：`/upload/`、`/sync/list`、`/api/audit/log` 三处加独立超时/条目上限/限速（`http_server.go:12-19`、`cluster.go:320-354`、`audit.go:251-280`）；`clusterPeers` 加容量与 TTL（`cluster.go:384-399`）；`clusterEvents` 加单条大小上限（`cluster.go:407-421`）。
7. **节点间凭据独立化**：`source.token` 改请求头传递并改用按节点签发的短期凭据（`cluster.go:690-693`）；`heartbeat`/`events` 增加节点签名校验；`peers` 里的 `address` 落库前做保留地址段校验（`cluster.go:384-399`）。
8. **`transferAllowLoopback` 的协议校验前置**：把 `cluster.go:445-447` 的 http/https 判断移到 `cluster.go:456-458` 的 loopback 短路之前，避免该调试开关在将来成为配置项时被当作 SSRF 开关。
