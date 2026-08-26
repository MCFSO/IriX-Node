# AGENTS.md

## 项目概述

IriX 客户端「节点」类型的本地节点守护进程，Go 语言实现。
核心以标准库为主；账户管理引入少量经批准的第三方依赖（SQLite/MySQL/PostgreSQL
驱动、Redis 客户端与 x/crypto，见 `go.mod` 与 `docs/accounts-design.md`）。
提供与 MCSManager 面板一致风格的 HTTP API，使 IriX 客户端可用同一套代码管理 MCSM 节点与本节点。

## 常用命令

```powershell
go build -o irix-node .       # 构建
go vet ./...                  # 静态检查（CI 必跑）
go test ./...                 # 测试（CI 必跑）
go run . -port 12346 -data <目录> [-apikey <key>]   # 本地运行
go run . -config config.json  # 用配置文件启动（全部启动参数可写入）
go run . -bind 0.0.0.0 -port 23333 -apikey <key>    # 监听全部网卡（局域网访问）
```

修改代码后必须通过 `go vet ./...` 与 `go build .`。

## 架构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：flag 解析、配置文件加载与合并、配对码初始化、路由注册、`NormalizePath`/`SplitCommand` 等通用工具 |
| `config.go` | 配置文件（`config.json`）加载：`Config` 结构、`loadConfigFile`、`ensureConfigFile`（首次启动无配置时自动生成示例）、`nodeOptions` 命令行/配置文件合并（优先级：命令行显式参数 > 配置文件 > 默认值） |
| `daemon.go` | 核心模型：`Daemon`/`Instance`/`InstanceConfig`、`instances.json` 持久化、增删改查 |
| `instance.go` | 实例相关 API 处理器、路由注册表 `RegisterRoutes`、认证包装器 `auth` |
| `process.go` | 进程管理：启动/停止/重启/强杀、环形日志缓冲 `LogBuffer`、stdin 命令下发 |
| `process_windows.go` / `process_other.go` | 按平台构建标签区分的 `sysProcAttr` |
| `files.go` | 文件管理 API：列表/读写/删除/移动/复制/压缩/解压/新建 |
| `download.go` | 带票据的下载/上传直连通道 `ticketStore` |
| `overview.go` | `GET /api/overview` 主机信息 |
| `sysinfo.go` + `sysinfo_{windows,linux,bsd,other}.go` | 跨平台系统信息采集（构建标签区分） |
| `auth.go` | 配对码机制：生成 20 位随机码、SHA-256 哈希持久化、恒定时间比较 |
| `accounts.go` | 账户管理数据面：SQL 存储（sqlite/mysql/postgres，连接池）、会话、bcrypt、Redis 热缓存与降级 |
| `accounts_handlers.go` | 账户管理 API：登录/登出/改密/账户 CRUD/权限开关、`authenticate` 身份认证 |
| `perm_catalog.go` | 端点权限目录：`perm()` 织入、分组、`permAllowed` 逐端点判定 |
| `cors.go` | CORS 中间件：跨源响应头与预检终结（错误响应同样带 CORS 头） |

## 约定

- **语言**：代码注释、README、错误消息一律使用中文。
- **依赖**：依赖最小化——核心仍是标准库；仅数据库驱动（`go-sql-driver/mysql`、
  `jackc/pgx`、`modernc.org/sqlite`）、`redis/go-redis` 与 `golang.org/x/crypto`
  允许（`go.mod` 直接依赖即这些）。新增依赖须先说明理由；系统相关能力仍应优先标准库。
- **账户权限**：新路由注册时必须相邻调用 `perm(组名, 路由模式, 中文描述)`
  纳入端点权限目录（`qa_perms_test.go` 会反向扫描源码校验漏标注）。
  规则见 `docs/accounts-design.md`。
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

## 配置文件

- `config.json`：启动参数配置（JSON，可用 `-config` 指定路径；不存在时首次启动
  自动生成一份示例配置，生成失败仅告警不阻断启动）。
  字段见 `config.example.json`；优先级为 **命令行显式参数 > 配置文件 > 默认值**。
  布尔/整数字段用指针区分「未设置」（回退默认值）与「显式 false/0」。
- Linux systemd 安装脚本生成 `/etc/irix-node/config.json` 并以 `-config` 启动。
