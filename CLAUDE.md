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
| `internal/vaultcrypto/` | 保险库密码学原语：PBKDF2 派生、AES-GCM 包裹、TOTP、挑战签名校验、TLS |
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
- **首启引导**：`Daemon.ensureAdminAccount`（`auth.go`）在账户库无管理员账户时随机生成管理员用户名/密码并入库，凭据只在启动日志出现一次；共享账户库多节点并发启动时以「已有管理员即让位」避免重复创建。凭据丢失只能删除账户库重新生成（或直接操作账户库）。

## 测试

测试覆盖六个维度（并发安全、可靠性、高可用、HTTP 压测、长稳、安全）。测试分布在：

| 文件 | 聚焦 |
| --- | --- |
| `qa_test.go` | 核心功能测试：并发 CRUD、日志缓冲、票据、持久化、账户登录/会话认证、AutoRestart、路径安全、路由冒烟 |
| `qa_security_test.go` | 安全测试：路径越界、zip slip、上传文件名穿越、票据范围、body 限额、孤儿进程、slowloris 防御 |
| `qa_audit_test.go` | 审计日志测试：请求细节落盘、token/apikey 打码、请求体捕获与截断、关闭开关、查询打码/控制字符转义 |
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
