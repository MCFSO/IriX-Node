# AGENTS.md

## 项目概述

IriX 客户端「节点」类型的本地节点守护进程，纯 Go 标准库实现、**零第三方依赖**。
提供与 MCSManager 面板一致风格的 HTTP API，使 IriX 客户端可用同一套代码管理 MCSM 节点与本节点。

## 常用命令

```powershell
go build -o irix-node .       # 构建
go vet ./...                  # 静态检查（CI 必跑）
go test ./...                 # 测试（CI 必跑）
go run . -port 12346 -data <目录> [-apikey <key>]   # 本地运行
```

修改代码后必须通过 `go vet ./...` 与 `go build .`。

## 架构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：flag 解析、配对码初始化、路由注册、`NormalizePath`/`SplitCommand` 等通用工具 |
| `daemon.go` | 核心模型：`Daemon`/`Instance`/`InstanceConfig`、`instances.json` 持久化、增删改查 |
| `instance.go` | 实例相关 API 处理器、路由注册表 `RegisterRoutes`、认证包装器 `auth` |
| `process.go` | 进程管理：启动/停止/重启/强杀、环形日志缓冲 `LogBuffer`、stdin 命令下发 |
| `process_windows.go` / `process_other.go` | 按平台构建标签区分的 `sysProcAttr` |
| `files.go` | 文件管理 API：列表/读写/删除/移动/复制/压缩/解压/新建 |
| `download.go` | 带票据的下载/上传直连通道 `ticketStore` |
| `overview.go` | `GET /api/overview` 主机信息 |
| `sysinfo.go` + `sysinfo_{windows,linux,bsd,other}.go` | 跨平台系统信息采集（构建标签区分） |
| `auth.go` | 配对码机制：生成 20 位随机码、SHA-256 哈希持久化、恒定时间比较 |

## 约定

- **语言**：代码注释、README、错误消息一律使用中文。
- **依赖**：只允许标准库，禁止引入第三方模块（`go.mod` 无 require）。
- **风格**：`gofmt` 排版；导出符号带中文注释；类型为 `*Daemon` 的方法。
- **平台差异**：系统相关代码拆成 `_windows`/`_linux`/`_bsd`/`_other` 后缀文件，
  用 `//go:build` 标签区分，新平台能力必须补齐对应文件。
- **错误处理**：HTTP 处理器出错时用 `writeError` 返回；错误消息为中文。
- **并发**：`Daemon.mu` 保护实例列表，`Instance.mu` 保护单个实例；
  访问 `inst.Status`/`inst.Config` 前必须加锁。

## API 约定

- 响应体统一为 MCSM 风格 `{status, data, time}`（`writeJSON`）。
- 认证：请求携带 `?apikey=` 参数或 `X-Api-Key` 头；
  `-apikey` 为空时校验配对码哈希（`auth.hash`）。
- 路由用 Go 1.22+ 方法前缀模式注册（如 `"GET /api/instance"`），全部集中在 `RegisterRoutes`。
- 文件路径操作必须经过 `NormalizePath` 防止 `..` 越界。
- 实例状态常量：`StatusBusy=-1, StatusStopped=0, StatusStopping=1, StatusStarting=2, StatusRunning=3`。

## 数据目录

- `instances.json`：实例配置列表（`PersistedInstance`）。
- `auth.hash`：配对码 SHA-256 哈希（首次启动生成，仅显示一次；删除后重启可重置）。
