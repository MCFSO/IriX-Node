# IriX Node Daemon

IriX 客户端「节点」类型中的本地节点守护进程，使用 Go 语言实现。
核心仍以标准库为主；账户管理引入少量第三方依赖（SQLite/MySQL/PostgreSQL
驱动、Redis 客户端与 x/crypto，见 `go.mod` 与 `docs/accounts-design.md`）。

AI作品轻喷

它与 MCSM 面板提供同一风格的 HTTP API，因此 IriX 客户端可以用同一套代码同时管理
MCSM 节点与本节点。

## 文档导航

| 主题 | 说明 |
| --- | --- |
| [构建与运行](docs/getting-started.md) | 编译命令、启动参数表、JSON 配置文件、账户管理概述 |
| [多平台部署](docs/platform-deploy.md) | Windows、Linux、macOS、FreeBSD、OpenBSD、NetBSD 的预编译产物，自行编译其他平台，以及首次启动的账户引导 |
| [功能与数据目录](docs/features.md) | 能力概览、`{data}` 目录结构 |
| [容器环境](docs/container.md) | Docker（Linux）/ Bastille（FreeBSD）支持与 PF 注意事项 |
| [高并发压测调优](docs/perf-tuning.md) | 百万级连接的 OS 资源上限与代码侧优化 |
| [加密保险库](docs/vault.md) | TOTP+密码+证书三重保护的数据加密存储 |
| [安全说明](docs/security.md) | 认证、路径越界防护、SSRF 防护与部署建议 |
| [架构设计](docs/architecture.md) | 分层边界、请求生命周期、持久化一致性与优雅关停 |

### 设计文档（`docs/`）

- `docs/accounts-design.md` — 账户与权限模型（端点级权限开关、连接池、Redis 缓存）
- `docs/vault-design.md` — 加密保险库设计（密钥包裹、迁移、物化加密）
- `docs/container-support.md` — 容器能力客户端契约（字段级）
- `docs/cluster-node-api.md` — 集群节点 API（LAN 直传、协调）
- `docs/compliance-l2.md` — 等保二级合规要点
- `docs/irix-node-local-parity.md` — 与 MCSM 节点的本地一致性说明
- `docs/backend-requirements.md` — 后端需求背景
- `docs/NODE_API.md` / `docs/api/` — HTTP API 接口文档

## 项目结构

```
cmd/irix-node/        程序入口与核心（HTTP API、实例与进程管理、账户、保险库、集群等）
internal/sysinfo/     系统与进程探测（主机指标、磁盘余量、数据目录选址、进程身份）
internal/platform/    进程级 OS 适配（OpenBSD pledge/unveil、RLIMIT_NOFILE、信号装配）
internal/vaultcrypto/ 保险库密码学原语（PBKDF2、AES-GCM、TOTP、证书校验与 TLS）
docs/                 设计与接口文档（api/ 为分册，audit/ 为审计报告）
scripts/              部署脚本（systemd 安装）
bin/                  本地构建产物（已被 .gitignore 忽略）
```

`internal/` 下的包不反向依赖主包，可独立编译与测试。

`cmd/irix-node/` 内源码按职责命名：`accounts_*.go` 账户、`container_*.go` 容器、
`vault_*.go` 保险库、`process_*.go` 进程、`cluster.go`/`frp.go` 集群、`qa_*_test.go` 测试。
带平台后缀（`_windows`/`_linux`/`_bsd`…）的文件按构建标签区分平台实现。

## 快速开始

```bash
go build -o bin/irix-node ./cmd/irix-node
./bin/irix-node                  # 默认监听 127.0.0.1:12346，数据目录为当前目录
./bin/irix-node -config /etc/irix-node/config.json   # 用配置文件启动（推荐，全部参数可写入）
./bin/irix-node -bind 0.0.0.0 -port 23333   # 监听全部网卡（局域网可访问）
```

详细参数与多平台运行说明见上方文档导航。**首次启动**时节点会在启动日志中打印一组
随机生成的管理员用户名与密码（仅此一次，请立即记录），随后用它登录换取会话 token：

```bash
curl -X POST http://127.0.0.1:12346/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"<用户名>","password":"<密码>"}'      # 返回 data.token
curl "http://127.0.0.1:12346/api/overview?token=<token>"
```

在 IriX 客户端中添加「节点」时，地址填 `http://127.0.0.1:12346`，用户名与密码填上面
打印的凭据（详见[多平台部署](docs/platform-deploy.md)）。
