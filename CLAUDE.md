# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

IriX 客户端「节点」类型的本地节点守护进程，Go 语言实现。
核心以标准库为主；账户管理引入少量经批准的第三方依赖（SQLite/MySQL/PostgreSQL
驱动、Redis 客户端与 x/crypto，见 `go.mod` 与 `docs/accounts-design.md`）。
提供与 MCSManager 面板一致风格的 HTTP API。

## 常用命令

```bash
go build -o bin/irix-node ./cmd/irix-node   # 构建
go vet ./...                  # 静态检查（CI 必跑）
go test ./...                 # 测试
go test -race -count=1 ./...  # 带竞态检测的测试（Windows 本地需 gcc）
go run ./cmd/irix-node -port 12346 -data <目录>   # 本地运行
go run ./cmd/irix-node -bind 0.0.0.0 -port 23333  # 监听全部网卡（局域网访问）
go run ./cmd/irix-node -port 12346 -data <目录> -instance-log=false  # 关闭实例日志落盘
go run ./cmd/irix-node -port 12346 -data <目录> -audit-log=false  # 关闭审计日志落盘（stderr 仍输出审计行）
```

修改代码后必须通过 `go vet ./...` 与 `go build ./cmd/irix-node`。

## 目录结构

| 路径 | 职责 |
| --- | --- |
| `cmd/irix-node/` | 程序入口与核心（`package main`）。Go 要求方法与其类型同包，`Daemon` 的 260+ 方法因此集中在此目录 |
| `internal/sysinfo/` | 系统与进程探测：主机指标、磁盘余量、数据目录选址、进程身份与资源占用（含各平台实现） |
| `internal/platform/` | 进程级 OS 适配：OpenBSD pledge/unveil 权限自限制、RLIMIT_NOFILE 提升、信号装配 |
| `internal/vaultcrypto/` | 保险库密码学原语：PBKDF2 派生、AES-GCM 包裹、TOTP、挑战签名校验、TLS（证书日期/链校验/续签使用节点时间） |
| `internal/timesys/` | 节点自有时间系统：Roughtime（RFC 10049 + 部署版）客户端与签名/Merkle 校验、漂移补偿、slew/step 时钟纪律、holdover 与状态持久化、多源 quorum |
| `docs/` | 设计与接口文档（`api/` 接口分册、`audit/` 审计报告） |
| `scripts/` | 部署脚本；`bin/` 为本地构建产物（已 gitignore） |

`internal/` 各包不反向依赖主包；`internal/platform` 需要日志时由主包 `platform.SetLogf(alog.Printf)` 注入。

## 架构

下表文件均位于 `cmd/irix-node/`。

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：flag 解析、首次启动管理员账户引导（`ensureAdminAccount`）、路由注册、`NormalizePath`/`SplitCommand`/`FormatSize` 通用工具、HTTP 中间件（body 限额、请求日志）、优雅关停 |
| `daemon.go` | 核心模型：`Daemon`/`Instance`/`InstanceConfig`/`PersistedInstance`、`instances.json` 持久化（原子写）、增删改查、`newUUID` |
| `instance.go` | 实例 API 处理器（CRUD + 启动/停止/重启/强杀/命令/日志）、路由注册表 `RegisterRoutes`、`auth` 认证包装器、`StopAll` 优雅关停、`autoRestart` 防抖 |
| `process.go` | 进程管理：`startProcess`、`Process` 结构、`LogBuffer` 环形日志缓冲、`stdinPipe`、`Stop`/`Kill`、`IsRunning`（通过 done channel 而非 Signal） |
| `logger.go` | 异步日志：全局 `alog`（访问/错误日志异步写 stderr，满则丢弃计数）+ `fileLogger`（实例日志异步落盘到 `{data}/logs/`，轮转 `.1`，Write 永不阻塞） |
| `process_windows.go` / `process_other.go` | 按 `//go:build` 标签区分的 `sysProcAttr`（Windows 隐藏控制台窗口） |
| `files.go` | 文件管理 API：列表/读写/删除/移动/复制/压缩(zip)/解压/新建目录/新建文件，所有操作限定在实例 cwd 内 |
| `audit.go` | 审计日志：每次 API 请求完整细节（时间/来源 IP/方法/路径+查询/状态码/耗时/请求体前缀），会话 token 与 apikey 打码、控制字符转义，异步落盘 `{data}/logs/audit.log` |
| `download.go` | 带票据的下载/上传直连通道：`ticketStore`（密码票据，10 分钟过期，上限 10000，定时清理） |
| `overview.go` | `GET /api/overview` 主机信息（MCSM 格式响应，含系统信息与远程节点列表） |
| `auth.go` | 首次启动引导：账户库无管理员时用 `crypto/rand` 生成管理员用户名（12 位）与密码（20 位），`bcrypt` 入库并仅在启动日志显示一次 |
| `accounts.go` | 账户管理数据面：SQL 存储（sqlite/mysql/postgres 驱动 + database/sql 连接池）、会话、bcrypt、Redis 热缓存与 30 秒冷却降级 |
| `accounts_handlers.go` | 账户管理 API：登录/登出/改密/账户 CRUD/权限开关；`authenticate`（Bearer / X-Auth-Token / ?token= 会话 token → 账户身份） |
| `perm_catalog.go` | 端点权限目录：`perm(组, 模式, 描述)` 织入、分组、`permAllowed` 逐端点判定 |
| `cors.go` | CORS 中间件：跨源响应头、预检 204 终结、错误响应同样带 CORS 头 |
| `defense.go` | 恒开 DDoS/CC 防御：封禁表、单 IP 令牌桶/并发、全节点匿名预算、单 User-Agent 并发与频率（超限即封禁该 UA）、状态快照与人工封禁持久化 |
| `defense_headers.go` | HTTP 头部规范校验：请求走私、Via/Forwarded/X-Forwarded-* 等代理链标记、重复单例头、方法覆盖、非法 Upgrade、超量头 |
| `defense_handlers.go` | 防御管理 API：`GET /api/defense/status`、`POST/DELETE /api/defense/ban` |
| `admin_access.go` | 管理员账户来源限制：仅环回 + RFC 1918（IPv6 为 ULA/链路本地），登录与会话逐请求校验 |
| `device_auth.go` | 设备绑定协议：Ed25519/P-256 挑战签名、一次性挑战（60s）、逐请求 HMAC-SHA256 证明、1024 位序号窗口 |
| `device_store.go` | 设备公钥与账户设备策略 SQL：`account_devices` 表、`accounts.device_policy`、`sessions.device_id/proof_key` |
| `device_handlers.go` | 设备管理 API：`/api/auth/device/challenge`、`/api/auth/devices*`、管理员设备与策略接口 |
| `time_system.go` | 时间系统编排：配置合并、安装全局时间源、TLS 证书签发/校验/自动续签、日志时间戳与 HTTP Date 接入 |
| `time_handlers.go` | 时间系统 API：`GET /api/time`（状态）、`POST /api/time/sync`（立即同步） |
| `compat.go` | 第三方 Daemon 兼容层：`compat` 配置校验与子路径挂载、来源白名单、MCSM 实例视图字段映射 |
| `compat_mcsm_engine.go` | MCSManager 兼容层传输引擎（自有实现）：engine.io v4 轮询 + WebSocket 升级 + socket.io v4 包 |
| `compat_mcsm.go` | MCSManager 兼容层事件分发：实例/信息/控制台/票据/计划任务事件与状态推送 |
| `compat_mcsm_file.go` | MCSManager 兼容层文件事件、直连下载/分片上传通道、URL 下载任务 |
| `compat_mslx.go` | MSLX 兼容层 REST：JWT（HS256）、`{code,message,data}` 信封、实例与设置接口、整数 id 映射 |
| `compat_mslx_files.go` | MSLX 兼容层文件管理接口（列表/读写/新建/删除/重命名/复制/移动/上传/下载） |
| `compat_mslx_hub.go` | MSLX 兼容层 SignalR JSON 协议最小实现（控制台 `ReceiveLog`、`ReceiveSystemStats`） |

## 第三方 Daemon 兼容（`compat` 块）

- `config.json` 的 `compat.mcsm` / `compat.mslx` 独立开关，**可同时开启**，各挂独立子路径
  （默认 `/mcsm`、`/mslx`）；未启用时不注册任何路由。协议见 `docs/compat-daemon.md`。
- **许可证边界（硬约束）**：只对齐线上协议与字段语义，**不得复制/翻译/引入**
  MCSManager（Apache-2.0）与 MSLX 的源码；传输层（socket.io、SignalR、JWT）
  必须基于标准库自研，不新增依赖；业务动作复用 `startInstance`/`stopInstance`、
  `tickets`、`safeResolve`、`sysinfo`、`timesys`。
- 兼容层不接入账户权限体系（MCSM 用节点密钥、MSLX 用 JWT），因此**不调用** `perm()`，
  也不在 `qa_perms_test.go` 反向扫描清单内（用 `mux.Handle(前缀+"/", ...)` 整体挂载）。
- 文件路径必须走 `safeResolve`/`NormalizePath`，不放宽既有边界检查。
- 变更兼容层须补 `qa_compat_*_test.go` 用例（含完整中间件链与 SignalR WebSocket 端到端）。

## 锁与并发

- **`Daemon.mu`** 保护 `Instances` 切片（增删遍历时持有）。
- **`Daemon.saveMu`** 串行化 `Save()` 写盘，防止并发持久化互相覆盖。
- **锁顺序**：`saveMu` **必须始终先于 `mu`** 获取，避免死锁。
- **`Instance.mu`** 保护单个实例的 `Status`/`Config`/`Proc`/`Busy`/自动重启窗口。访问这些字段前必须加锁。
- **`Instance.Busy`** 防止对同一实例并发执行启动/停止操作。

## 约定

- **语言**：代码注释、README、错误消息一律使用中文。
- **依赖**：依赖最小化——核心仍是标准库；仅数据库驱动（`go-sql-driver/mysql`、
  `jackc/pgx`、`modernc.org/sqlite`）、`redis/go-redis` 与 `golang.org/x/crypto`
  允许（`go.mod` 直接依赖即这些）。新增依赖须先说明理由。
- **时间**：一切**墙钟语义**（持久化时间戳、过期/封禁/审计/证书日期、API `time`
  字段）必须使用 `timesys.Now()/Since()/Until()`，禁止直接 `time.Now()`。
  例外仅限系统时间域与耗时测量：网络读写 deadline、`time.Since` 性能计时、
  进程启动时间/`fi.ModTime()`/uptime 等 OS 上报值、`crypto/tls` 内部票据轮换；
  这些位置保留 `time.Now()` 并加注释说明。详见 `docs/time-system.md`。
- **账户权限**：新路由注册时必须相邻调用 `perm(组名, 路由模式, 中文描述)`
  纳入端点权限目录（`qa_perms_test.go` 反向扫描源码校验漏标注）。
  规则见 `docs/accounts-design.md`。
- **风格**：`gofmt` 排版；导出符号带中文注释；类型为 `*Daemon`/`*Instance` 的方法。
- **平台差异**：系统相关代码拆成 `_windows`/`_linux`/`_bsd`/`_other` 后缀文件，用 `//go:build` 标签区分。新平台能力必须补齐对应文件（现主要位于 `internal/sysinfo`、`internal/platform` 与 `cmd/irix-node`）。
- **错误处理**：HTTP 处理器出错时用 `writeError` 返回；错误消息为中文。
- **响应格式**：统一 MCSM 风格 `{status, data, time}`（通过 `writeJSON`/`writeOK`）。

## API 约定

- 路由用 Go 1.22+ 方法前缀模式（如 `"GET /api/instance"`），全部集中在 `RegisterRoutes`。
- 认证：先 `POST /api/auth/login`（body `{username,password}`）换取会话 token（30 天有效，滑动续期），
  之后的请求携带 `X-Auth-Token: <token>` 头 / `Authorization: Bearer <token>` 头 / `?token=` 参数
  （WebSocket 必须用查询参数）。管理员（`is_admin`）不受端点权限开关限制，普通账户逐端点判定；
  但管理员账户默认只允许从环回或 RFC 1918 内网来源（IPv6 为 `fc00::/7`/`fe80::/10`）登录与访问，
  公网来源即使持有有效管理员会话也返回 403（`admin_access.go`；可用 `-admin-intranet-only=false`
  或 `adminIntranetOnly=false` 关闭，默认 true）。
  设备绑定会话（`deviceBound=true`）的每个请求还必须携带 `X-Device-Id` /
  `X-Session-Timestamp` / `X-Session-Seq` / `X-Session-Proof`（`docs/device-binding.md`）。
  另有直连票据通道（/download/ /upload/）免会话认证。见 `docs/accounts-design.md`。
- 已移除的旧机制：`-apikey` / `apiKey` 固定密钥与配对码（`auth.hash`）自 v1.2.0 起不再存在，
  `?apikey=` / `X-Api-Key` 已不再被接受。
- 文件路径必须经过 `NormalizePath` 防止 `..` 越界。`/` 前缀的路径代表实例 cwd 根（跨平台一致）。
- 实例状态常量：`StatusBusy=-1, StatusStopped=0, StatusStopping=1, StatusStarting=2, StatusRunning=3`。
- API 请求体上限 16 MiB（`/upload/` 直连通道除外）。

## 数据目录

```
{data}/
  instances.json   # 实例配置列表（原子写：先写 .tmp 再 rename）
  accounts.db      # 账户库（sqlite 默认；-accounts-driver 可换 mysql/postgres）
                   # 含 account_devices / sessions.device_id/proof_key / accounts.device_policy
  defense.json     # 人工封禁的 IP / User-Agent（自动封禁只在运行期）
  timesys.json     # 时间系统状态（频率校正/上次同步/保持锚点，0600 原子写）
  compat-secrets.json   # 兼容层自动生成的凭据（mcsmKey/mslxPassword/mslxJwtSecret，0600）
                        # 配置字段留空时以此为准，并会定点回写配置文件（enabled=false 也生成）
  compat-mslx-ids.json  # MSLX 兼容层实例 uuid ↔ 整数 id 映射
  logs/            # 实例日志落盘（-instance-log 开启时）：{uuid}.log + 轮转 {uuid}.log.1
                   # 审计日志（-audit-log 开启时）：audit.log + 轮转 audit.log.1
```

损坏的 `instances.json` 会被自动备份为 `instances.json.corrupt-<时间戳>` 后按空列表启动。

## 关键设计细节

- **优雅关停**：收到 SIGINT/SIGTERM → `srv.Shutdown(15s)` 停止接受新请求 → `StopAll(30s)` 并行停止所有子进程（先发 stop 命令，超时后 Kill）→ 退出。`StopAll` 中先将 `inst.Proc = nil` 防止误触发 AutoRestart。
- **自动重启防抖**：10 秒窗口内最多 3 次，防止崩溃循环。
- **进程存活检测**：通过 `done` channel（`cmd.Wait()` 返回时关闭），不能用 `Signal(0)`（Windows 不支持）。
- **异步日志**：所有 `log.Printf` 一律走全局 `alog`（有界缓冲，满则丢弃计数，`main` 退出前 `alog.Close()` 排空）。实例 stdout/stderr 在写内存环形缓冲的同时镜像异步落盘（`fileLogger`：非阻塞 `Write`、`bufio` + 大小轮转、磁盘追不上丢弃——磁盘慢绝不能阻塞游戏进程的 stdout 管道）。`done` 在输出复制结束（3s 超时兜底）且日志 flush 后才关闭，保证退出即完整落盘。
- **票据系统**：下载/上传通过票据密码（10 分钟过期、10000 上限）直连，绕过 API 认证但受限于实例 cwd。
- **审计日志**：`auditMiddleware` 记录每次 API 请求的完整细节（时间、来源 IP、方法、路径与查询参数、状态码、耗时、请求体前 2KB），查询串里的会话 `token`（及历史 `apikey`）一律打码防明文落盘，控制字符转义防伪造日志行；落盘复用 `fileLogger`（有界队列 + 大小轮转，磁盘慢不阻塞请求）。`/download/`、`/upload/` 直连通道同样在审计范围内。
- **DDoS/CC 防御（恒开、不可关闭）**：`defenseMiddleware` 位于 `trackRequests` 之后、审计之前（`http_server.go`），依次执行封禁表 → 头部规范（`defense_headers.go`）→ 频率/并发（全节点匿名预算 + 单 IP 令牌桶与并发上限；单 UA 并发与频率超限直接封禁该 UA）。自动封禁到期自动解除、重复触发时长翻倍（上限 24 小时）；人工封禁持久化 `{data}/defense.json`。环回来源豁免频率限制与自动封禁，但硬性头部违规与人工/配置的 UA 封禁仍生效。阈值由 `config.json` 的 `defense` 块调整，**没有 enabled 开关**（写 false 会被忽略并告警）。
- **管理员内网限制**：`auth` 与登录入口都调用 `adminIntranetAllowed`（`admin_access.go`），管理员来源默认必须是环回或 RFC 1918（IPv6 ULA/链路本地）；公网来源即使密码正确也不签发会话、已有会话也 403。可用 `-admin-intranet-only=false` / `adminIntranetOnly=false` 显式关闭（默认 true）。普通账户不受限；跨公网集群调用请使用非管理员账户并只授予集群端点权限。
- **设备绑定（v1.4.0+）**：客户端生成 Ed25519/P-256 设备密钥（推荐 TPM/系统密钥库），用一次性挑战签名登录；`accountSession.DeviceID/ProofKey` 绑定会话后，每个请求校验 `X-Device-Id` / `X-Session-Timestamp` / `X-Session-Seq` / `X-Session-Proof`（HMAC-SHA256 + 1024 位序号窗口，热路径无 DB）。账户 `devicePolicy=optional|required`（默认 optional，required 强制 TLS 且至少一台设备）；策略变更/撤销设备/删账户都会撤销会话。协议与客户端交接见 `docs/device-binding.md`。
- **首启引导**：`Daemon.ensureAdminAccount`（`auth.go`）在账户库无管理员账户时随机生成管理员用户名/密码并入库，凭据只在启动日志出现一次；共享账户库多节点并发启动时以「已有管理员即让位」避免重复创建。凭据丢失只能删除账户库重新生成（或直接操作账户库）。
- **时间系统（v1.5.0+）**：`internal/timesys` 以 Roughtime 为唯一外部时间源（RFC 10049 + 已部署 legacy 格式），校验 Ed25519 签名与 Merkle 包含证明后得到带半径的时间区间；多源按区间相交取最大一致组（`minAgreement`）剔除虚假源；受纪律时钟锚定单调时钟，对 (单调时刻, 真值) 做最小二乘估计晶振 ppm 并平滑校正，小误差按 `maxSlewPPM` 缓变、大误差 step；不确定度随离线时长按漂移上界增长，状态落盘 `{data}/timesys.json` 供重启 holdover。启动时先等首轮同步（`time-boot-wait-ms`）再初始化账户/TLS；TLS 证书 NotBefore/NotAfter 按节点时间签发并按不确定度回拨 NotBefore，`tls.Config.Time` 与出站 HTTPS 链校验都指向 `timesys.Now()`，自签证书在节点时间失效时自动重签并热替换。`GET /api/time`、`POST /api/time/sync` 暴露状态与手动同步，见 `docs/time-system.md`。

## 测试

测试覆盖六个维度（并发安全、可靠性、高可用、HTTP 压测、长稳、安全）。测试分布在：

| 文件 | 聚焦 |
| --- | --- |
| `qa_test.go` | 核心功能测试：并发 CRUD、日志缓冲、票据、持久化、账户登录/会话认证、AutoRestart、路径安全、路由冒烟 |
| `qa_security_test.go` | 安全测试：路径越界、zip slip、上传文件名穿越、票据范围、body 限额、孤儿进程、slowloris 防御 |
| `qa_audit_test.go` | 审计日志测试：请求细节落盘、token/apikey 打码、请求体捕获与截断、关闭开关、查询打码/控制字符转义 |
| `qa_defense_test.go` | DDoS/CC 防御与管理员内网限制：头部规范判定、Via 拦截与累计封禁、UA 并发/频率封禁、IP 限流与环回豁免、人工封禁持久化、防御接口、RFC 1918 来源校验 |
| `qa_device_test.go` | 设备绑定：Ed25519/P-256 注册与挑战登录、逐请求 HMAC/时间戳/序号防重放、required 策略、设备撤销/最后一台保护/管理员降级、旧库补列迁移、证明性能基准 |
| `qa_time_test.go` | 时间系统集成：/api/time 状态与手动同步、响应 time 字段/HTTP Date、会话过期跟随节点时间、TLS 证书签发/自动续签/manual 过期拒绝、配置合并 |
| `qa_compat_mcsm_test.go` | MCSManager 兼容层：engine.io 握手/心跳、socket.io 认证、实例增删启停杀、文件事件、通行证绑定下载、前缀/白名单解析、完整中间件链 |
| `qa_compat_mslx_test.go` | MSLX 兼容层：JWT 登录与伪造拒绝、实例列表/动作、文件全套操作、Hub 协商、501/404、两层同时开启 |
| `qa_compat_mslx_ws_test.go` | MSLX SignalR WebSocket 端到端：握手、JoinGroup、ReceiveLog 推送、SendCommand 应答、系统监视 |
| `qa_perf_test.go` | 性能测试：万实例加载、大目录列表、大日志截取 |
| `qa_scale_test.go` | 规模测试：十万实例、日志缓冲内存占用、Tail 开销 |

测试辅助函数（在 `qa_test.go` 中定义，所有测试文件共享）：
- `newTestDaemon(t)` — 创建临时数据目录的守护进程，并初始化账户系统、注入固定测试管理员会话（`qaTestToken()`，可用环境变量 `IRIX_QA_TOKEN` 覆盖）
- `newTestServer(d)` — 启动 httptest 服务器（注册路由并确保测试会话就绪）
- `doReq(t, url)` — 发起 GET 请求（`?token=<token>` 已含在 URL 查询串中）
- `sampleUUID(i)` / `sampleInst(i, cwd)` — 构造测试实例

运行单个测试：`go test -race -run TestName ./...`

## CI

GitHub Actions（`.github/workflows/ci.yml`）：
- `gofmt -l .` 格式检查
- `go vet ./...` 静态分析
- `go test ./...` + `go test -race -count=1 ./...`
- `go build`
- 交叉编译矩阵：windows/linux/darwin/freebsd/openbsd × amd64/arm64（`CGO_ENABLED=0`）
