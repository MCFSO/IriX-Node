# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

IriX 客户端「节点」类型的本地节点守护进程。纯 Go 标准库实现、**零第三方依赖** (`go.mod` 无 `require`)。提供与 MCSManager 面板一致风格的 HTTP API。

## 常用命令

```bash
go build -o irix-node .       # 构建
go vet ./...                  # 静态检查（CI 必跑）
go test ./...                 # 测试
go test -race -count=1 ./...  # 带竞态检测的测试（Windows 本地需 gcc）
go run . -port 12346 -data <目录> [-apikey <key>]   # 本地运行
go run . -port 12346 -data <目录> -instance-log=false  # 关闭实例日志落盘
go run . -port 12346 -data <目录> -audit-log=false  # 关闭审计日志落盘（stderr 仍输出审计行）
```

修改代码后必须通过 `go vet ./...` 与 `go build .`。

## 架构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：flag 解析、配对码初始化、路由注册、`NormalizePath`/`SplitCommand`/`FormatSize` 通用工具、HTTP 中间件（body 限额、请求日志）、优雅关停 |
| `daemon.go` | 核心模型：`Daemon`/`Instance`/`InstanceConfig`/`PersistedInstance`、`instances.json` 持久化（原子写）、增删改查、`newUUID` |
| `instance.go` | 实例 API 处理器（CRUD + 启动/停止/重启/强杀/命令/日志）、路由注册表 `RegisterRoutes`、`auth` 认证包装器、`StopAll` 优雅关停、`autoRestart` 防抖 |
| `process.go` | 进程管理：`startProcess`、`Process` 结构、`LogBuffer` 环形日志缓冲、`stdinPipe`、`Stop`/`Kill`、`IsRunning`（通过 done channel 而非 Signal） |
| `logger.go` | 异步日志：全局 `alog`（访问/错误日志异步写 stderr，满则丢弃计数）+ `fileLogger`（实例日志异步落盘到 `{data}/logs/`，轮转 `.1`，Write 永不阻塞） |
| `process_windows.go` / `process_other.go` | 按 `//go:build` 标签区分的 `sysProcAttr`（Windows 隐藏控制台窗口） |
| `files.go` | 文件管理 API：列表/读写/删除/移动/复制/压缩(zip)/解压/新建目录/新建文件，所有操作限定在实例 cwd 内 |
| `audit.go` | 审计日志：每次 API 请求完整细节（时间/来源 IP/方法/路径+查询/状态码/耗时/请求体前缀），apikey 打码、控制字符转义，异步落盘 `{data}/logs/audit.log` |
| `download.go` | 带票据的下载/上传直连通道：`ticketStore`（密码票据，10 分钟过期，上限 10000，定时清理） |
| `overview.go` | `GET /api/overview` 主机信息（MCSM 格式响应，含系统信息与远程节点列表） |
| `auth.go` | 配对码机制：20 位随机码生成（`crypto/rand`，无偏差）、SHA-256 哈希持久化、恒定时间比较 |
| `sysinfo.go` + `sysinfo_{windows,linux,bsd,other}.go` | 跨平台系统信息采集（构建标签区分）：运行时间、内存、CPU |

## 锁与并发

- **`Daemon.mu`** 保护 `Instances` 切片（增删遍历时持有）。
- **`Daemon.saveMu`** 串行化 `Save()` 写盘，防止并发持久化互相覆盖。
- **锁顺序**：`saveMu` **必须始终先于 `mu`** 获取，避免死锁。
- **`Instance.mu`** 保护单个实例的 `Status`/`Config`/`Proc`/`Busy`/自动重启窗口。访问这些字段前必须加锁。
- **`Instance.Busy`** 防止对同一实例并发执行启动/停止操作。

## 约定

- **语言**：代码注释、README、错误消息一律使用中文。
- **依赖**：只允许标准库，禁止引入第三方模块。
- **风格**：`gofmt` 排版；导出符号带中文注释；类型为 `*Daemon`/`*Instance` 的方法。
- **平台差异**：系统相关代码拆成 `_windows`/`_linux`/`_bsd`/`_other` 后缀文件，用 `//go:build` 标签区分。新平台能力必须补齐对应文件。
- **错误处理**：HTTP 处理器出错时用 `writeError` 返回；错误消息为中文。
- **响应格式**：统一 MCSM 风格 `{status, data, time}`（通过 `writeJSON`/`writeOK`）。

## API 约定

- 路由用 Go 1.22+ 方法前缀模式（如 `"GET /api/instance"`），全部集中在 `RegisterRoutes`。
- 认证：请求携带 `?apikey=` 参数或 `X-Api-Key` 头；`-apikey` 为空时校验配对码哈希。
- 文件路径必须经过 `NormalizePath` 防止 `..` 越界。`/` 前缀的路径代表实例 cwd 根（跨平台一致）。
- 实例状态常量：`StatusBusy=-1, StatusStopped=0, StatusStopping=1, StatusStarting=2, StatusRunning=3`。
- API 请求体上限 16 MiB（`/upload/` 直连通道除外）。

## 数据目录

```
{data}/
  instances.json   # 实例配置列表（原子写：先写 .tmp 再 rename）
  auth.hash        # 配对码 SHA-256 哈希（首次启动生成，仅显示一次）
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
- **审计日志**：`auditMiddleware` 记录每次 API 请求的完整细节（时间、来源 IP、方法、路径与查询参数、状态码、耗时、请求体前 2KB），`apikey` 一律打码防明文落盘，控制字符转义防伪造日志行；落盘复用 `fileLogger`（有界队列 + 大小轮转，磁盘慢不阻塞请求）。`/download/`、`/upload/` 直连通道同样在审计范围内。

## 测试

测试覆盖六个维度（并发安全、可靠性、高可用、HTTP 压测、长稳、安全）。测试分布在：

| 文件 | 聚焦 |
| --- | --- |
| `qa_test.go` | 核心功能测试：并发 CRUD、日志缓冲、票据、持久化、配对认证、AutoRestart、路径安全、路由冒烟 |
| `qa_security_test.go` | 安全测试：路径越界、zip slip、上传文件名穿越、票据范围、body 限额、孤儿进程、slowloris 防御 |
| `qa_audit_test.go` | 审计日志测试：请求细节落盘、apikey 打码、请求体捕获与截断、关闭开关、查询打码/控制字符转义 |
| `qa_perf_test.go` | 性能测试：万实例加载、大目录列表、大日志截取 |
| `qa_scale_test.go` | 规模测试：十万实例、日志缓冲内存占用、Tail 开销 |

测试辅助函数（在 `qa_test.go` 中定义，所有测试文件共享）：
- `newTestDaemon(t)` — 创建临时数据目录的守护进程（`apikey="test-key"`）
- `newTestServer(d)` — 启动 httptest 服务器
- `doReq(t, url)` — 发起带 apikey 的 GET 请求
- `sampleUUID(i)` / `sampleInst(i, cwd)` — 构造测试实例

运行单个测试：`go test -race -run TestName ./...`

## CI

GitHub Actions（`.github/workflows/ci.yml`）：
- `gofmt -l .` 格式检查
- `go vet ./...` 静态分析
- `go test ./...` + `go test -race -count=1 ./...`
- `go build`
- 交叉编译矩阵：windows/linux/darwin/freebsd/openbsd × amd64/arm64（`CGO_ENABLED=0`）
