# IriX Node Daemon

IriX 客户端「节点」类型中的本地节点守护进程，使用 Go 语言实现、零第三方依赖。

AI作品轻喷

它与 MCSM 面板提供同一风格的 HTTP API，因此 IriX 客户端可以用同一套代码同时管理
MCSM 节点与本节点。

## 构建与运行

```bash
go build -o irix-node .
./irix-node                  # 默认监听 127.0.0.1:12346，数据目录为当前目录
./irix-node -config /etc/irix-node/config.json   # 用配置文件启动（推荐，全部参数可写入）
./irix-node -port 23334 -data D:\irix-node-data -apikey secret
./irix-node -bind 0.0.0.0 -port 23333 -apikey secret   # 监听全部网卡（局域网可访问）
```

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

### 配置文件

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


不指定 `-apikey` 时启用**配对码机制**：

- 首次启动自动生成一个 20 位随机配对码，并在终端**仅显示这一次**；
- 磁盘只保存配对码的 SHA-256 哈希（`{data}/auth.hash`），后续启动不会再次显示；
- 之后所有 API 请求都必须携带该配对码：`?apikey=<配对码>` 查询参数或 `X-Api-Key: <配对码>` 请求头；
- 配对码丢失后无法找回，只能删除 `auth.hash` 重新启动以生成新的配对码。

在 IriX 客户端中添加「节点」类型的节点时，地址填 `http://127.0.0.1:12346`。

## 功能

- **概览**：`GET /api/overview` 返回主机信息、内存、实例统计。
- **实例管理**：列表 / 详情 / 创建 / 更新 / 删除，启动 / 停止 / 重启 / 强制终止，
  命令下发（标准输入），输出日志（环形缓冲）。
- **文件管理**：列表 / 读写 / 删除 / 移动 / 复制 / 压缩 / 解压 / 新建目录 /
  新建文件，以及带票据的下载 / 上传直连通道。
- **审计日志**：每次 API 请求（时间、来源 IP、方法、路径与参数、状态码、耗时、
  请求体）落盘到 `{data}/logs/audit.log`，`apikey` 自动打码；下载/上传直连通道
  同样在审计范围内。
- 实例配置与状态持久化在 `{data}/instances.json`。

## 容器环境（Docker / Bastille）

- **Linux 节点**：`docker` CLI 可用时暴露 Docker 全功能（容器 / 镜像 / 卷 / 网络，
  含构建长任务与克隆 / 限额），端点见 `NODE_API.md` §6.1。
- **FreeBSD 节点**：暴露 Bastille 全功能（jail 创建 / 启停 / 克隆 / 导入导出 /
  rdr 端口转发 / setup 环境初始化），服务端包装 `bastille` CLI。
- 能力探测 `GET /api/container/info`：CLI 缺失时 `available=false`，客户端自动隐藏容器 UI。
- 客户端契约细节见 `docs/container-support.md`（字段级契约，实现须逐条对齐）。

>  **Bastille PF 初始化注意事项**：`bastille setup firewall` 生成的
> `/etc/pf.conf` 把 `block in all` 放在 `pass in proto tcp port ssh` **之前**，
> 而 PF 是 last-match 语义——直接 `service pf start` 会立刻切断 SSH 等一切入站连接。
> 启用 PF 前必须把管理流量放行（SSH、ICMP）移到 `block in all` 之前并加 `quick`：
>
> ```
> pass in quick proto tcp from any to any port ssh flags S/SA keep state
> pass in quick inet proto icmp from any to any icmp-type echoreq keep state
> block in all
> ```
>
> 未初始化 PF 时 rdr 端口转发会失败（`pfctl: /dev/pf: No such file or directory`）：
> 需先 `POST /api/bastille/setup {"mode":"firewall"}` 写入 pf.conf，再手动
> `service pf start`（bastille setup 只写配置、不启动服务）。

## 数据目录结构

```
{data}/
  instances.json   # 实例配置列表（保险库模式下加密存储，见下）
  auth.hash        # 配对码 SHA-256 哈希（首次启动生成）
  vault/           # 加密保险库（-vault 开启时）
    vault.json     # 用户/密钥包裹/恢复令牌哈希/迁移标记
    objects/       # 密文对象（文件名随机化，无明文痕迹）
    index.json.enc # 加密索引（文件名/大小/时间不泄露）
  tls/             # 自签 TLS 证书（-tls-mode auto 时）
  backup/audit/    # 审计日志轮转归档（防覆盖丢失）
  logs/            # 实例日志 {uuid}.log（-instance-log 开启时）
                   # 审计日志 audit.log（-audit-log 开启时）
```

## 加密保险库（Vault）

可选功能（默认关闭，由用户显式开启）：用「TOTP + 密码 + 客户端证书（P12/GPG 在
客户端转换为标准 PEM）」三重认证保护节点数据。设计文档：`docs/vault-design.md`。

```bash
irix-node -tls-mode auto -vault       # 开启 TLS（自签）与加密保险库
irix-node -tls-mode manual -tls-cert x.pem -tls-key x.key -vault   # 正式证书
```

- 开启后 `instances.json` 与 `vaultFiles=true` 实例的文件区以 AES-256-GCM 加密
  存储；未解锁（`POST /api/vault/unlock`）时数据面 API 返回 403。
- 每次解锁需 TOTP 验证码 + 账号密码 + 证书私钥对挑战签名（私钥不出设备）。
- **开启 vault 强制要求 TLS**（`tls-mode=off` 时拒绝启动）；TLS 本身默认关闭，
  由用户显式开启（等保二级部署请开启）。
- 实例文件区加密粒度：`vaultFiles` 字段（默认 false 保持明文；true = 启停物化
  加密），可用 `-vault-default-files-mode materialize` 让新实例默认开启。
- 忘记密码/丢失恢复令牌 = 数据永久不可恢复（初始化时一次性显示恢复令牌，请
  物理保管）。

## 安全说明

- 未指定 `-apikey` 时启用配对码认证，所有 API 请求必须携带首次启动时显示的配对码。
- 文件操作被限制在实例工作目录内（`..` 越界会被拒绝）。
- 实例 `cwd` 拒绝文件系统/磁盘根、系统目录与 Windows 用户 Profile 目录
  （`\Users` 整树，仅豁免 `%TEMP%`）。
- 集群拉取（节点间直传）默认拒绝环回、链路本地与本机地址（防认证后 SSRF），
  并默认拒绝 RFC1918 内网地址；LAN 直传需显式配置 `-transfer-allow-cidr`，
  该配置即信任边界——仅放行自己管辖的网段。
- 建议配合防火墙仅监听 127.0.0.1；如需局域网访问请设置 `-apikey`。
