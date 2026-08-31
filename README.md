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
| [多平台部署](docs/platform-deploy.md) | ARM/x86/POWER/s390x/MIPS Linux、Android(Termux)、OpenHarmony、Solaris/illumos、FreeBSD/OpenBSD/NetBSD，以及配对码机制 |
| [功能与数据目录](docs/features.md) | 能力概览、`{data}` 目录结构 |
| [容器环境](docs/container.md) | Docker（Linux）/ Bastille（FreeBSD）支持与 PF 注意事项 |
| [高并发压测调优](docs/perf-tuning.md) | 百万级连接的 OS 资源上限与代码侧优化 |
| [加密保险库](docs/vault.md) | TOTP+密码+证书三重保护的数据加密存储 |
| [安全说明](docs/security.md) | 认证、路径越界防护、SSRF 防护与部署建议 |

### 设计文档（`docs/`）

- `docs/accounts-design.md` — 账户与权限模型（端点级权限开关、连接池、Redis 缓存）
- `docs/vault-design.md` — 加密保险库设计（密钥包裹、迁移、物化加密）
- `docs/container-support.md` — 容器能力客户端契约（字段级）
- `docs/cluster-node-api.md` — 集群节点 API（LAN 直传、协调）
- `docs/compliance-l2.md` — 等保二级合规要点
- `docs/irix-node-local-parity.md` — 与 MCSM 节点的本地一致性说明
- `docs/backend-requirements.md` — 后端需求背景
- `NODE_API.md` / `docs/api/` — HTTP API 接口文档

## 快速开始

```bash
go build -o irix-node .
./irix-node                  # 默认监听 127.0.0.1:12346，数据目录为当前目录
./irix-node -config /etc/irix-node/config.json   # 用配置文件启动（推荐，全部参数可写入）
./irix-node -bind 0.0.0.0 -port 23333 -apikey secret   # 监听全部网卡（局域网可访问）
```

详细参数与多平台运行说明见上方文档导航。在 IriX 客户端中添加「节点」时，地址填
`http://127.0.0.1:12346`（未设 `-apikey` 时首次启动会生成配对码，见
[多平台部署 · 配对码机制](docs/platform-deploy.md#配对码机制)）。
