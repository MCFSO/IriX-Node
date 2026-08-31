# 构建与运行

## 快速开始

```bash
go build -o irix-node .
./irix-node                  # 默认监听 127.0.0.1:12346，数据目录为当前目录
./irix-node -config /etc/irix-node/config.json   # 用配置文件启动（推荐，全部参数可写入）
./irix-node -port 23334 -data D:\irix-node-data -apikey secret
./irix-node -bind 0.0.0.0 -port 23333 -apikey secret   # 监听全部网卡（局域网可访问）
```

Windows 提供 `amd64` / `arm64` / `386` 三个架构的 Release 产物（`irix-node-windows-*.exe`），
32 位系统用 `windows-386`；Go 不支持 32 位 ARM 的 Windows（`windows/arm`），故无对应产物。

## 启动参数

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-config` | 配置文件路径（JSON，不存在则首次启动自动生成示例配置）；全部启动参数均可写入配置文件，字段见 `config.example.json` | `config.json` |
| `-bind` | 监听地址（IP 或主机名，如 `127.0.0.1` / `0.0.0.0` / `192.168.1.5` / `::`）；留空时依次读配置文件 `bind`、`IRIX_NODE_BIND_ALL` 环境变量（=1 则 `0.0.0.0`） | `127.0.0.1` |
| `-port` | HTTP 监听端口（1-65535） | `12346` |
| `-data` | 数据目录（实例配置 instances.json 与配对码 auth.hash 存放于此） | 当前目录 |
| `-apikey` | 固定 API 密钥；留空则启用配对码机制 | 空 |
| `-audit-log` | 将用户操作审计日志落盘到 `{data}/logs/audit.log` | 开启 |
| `-audit-log-max` | 审计日志单文件轮转上限（MB，超过后轮转为 `.1`） | `64` |
| `-transfer-allow-cidr` | 集群拉取（`POST /api/cluster/transfer`）放行的内网 CIDR 列表（逗号分隔）；默认拒绝全部 RFC1918 内网地址，集群 LAN 节点间直传需显式配置，如 `192.168.0.0/16,10.0.0.0/8` | 空 |
| `-accounts-driver` | 账户管理数据库驱动：`sqlite` / `mysql` / `postgres`（见 `docs/accounts-design.md`） | `sqlite` |
| `-accounts-dsn` | 账户管理数据库连接串（sqlite 为文件路径，空 = `{data}/accounts.db`） | 空 |
| `-redis-addr` | Redis 地址（空 = 不启用，账户会话与权限直接走数据库） | 空 |
| `-redis-password` | Redis 密码 | 空 |
| `-redis-db` | Redis 库号 | `0` |

## 账户管理

配对码登录即 **root 管理员**：首次用配对码登录时必须修改密码（此后配对码不再
用于登录）；管理员可创建/删除账户，并对**每个 API 端点**独立开关权限（按模块
分组，支持整组开关），默认 SQLite 存储、可选 MySQL/PostgreSQL（自带连接池）、
Redis 可选缓存会话与权限热数据。详见 `docs/accounts-design.md`。

## 配置文件

全部启动参数均可写入 JSON 配置文件（默认 `./config.json`，可用 `-config` 指定路径，
字段说明见 `config.example.json`）。**首次启动时若配置文件不存在，会自动落一份示例配置**
（内容与 `config.example.json` 一致，含字段注释；生成失败仅告警，不阻断启动）。
优先级：**命令行显式参数 > 配置文件 > 默认值**；
未写的字段回退默认值。监听地址（`bind`）也支持配置文件，留空时仍读 `IRIX_NODE_BIND_ALL`
环境变量。

```json
{
  "bind": "0.0.0.0",
  "port": 23333,
  "data": "/var/lib/irix-node",
  "apiKey": "secret",
  "instanceLog": true,
  "instanceLogMax": 64,
  "auditLog": true,
  "auditLogMax": 64,
  "loadTune": true,
  "transferAllowCidr": "192.168.0.0/16,10.0.0.0/8"
}
```

Linux systemd 安装（`scripts/install-systemd.sh`）会生成 `/etc/irix-node/config.json`
并以 `-config` 启动；修改后 `systemctl restart irix-node` 生效。
