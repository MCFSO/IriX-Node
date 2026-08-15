# IriX Node Daemon

IriX 客户端「节点」类型中的本地节点守护进程，使用 Go 语言实现、零第三方依赖。

AI作品轻喷

它与 MCSM 面板提供同一风格的 HTTP API，因此 IriX 客户端可以用同一套代码同时管理
MCSM 节点与本节点。

## 构建与运行

```bash
go build -o irix-node .
./irix-node                  # 默认监听 127.0.0.1:12346，数据目录为当前目录
./irix-node -port 23334 -data D:\irix-node-data -apikey secret
./irix-node -bind 0.0.0.0 -port 23333 -apikey secret   # 监听全部网卡（局域网可访问）
```

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-bind` | 监听地址（IP 或主机名，如 `127.0.0.1` / `0.0.0.0` / `192.168.1.5` / `::`）；留空时读 `IRIX_NODE_BIND_ALL` 环境变量（=1 则 `0.0.0.0`） | `127.0.0.1` |
| `-port` | HTTP 监听端口（1-65535） | `12346` |
| `-data` | 数据目录（实例配置 instances.json 与配对码 auth.hash 存放于此） | 当前目录 |
| `-apikey` | 固定 API 密钥；留空则启用配对码机制 | 空 |
| `-audit-log` | 将用户操作审计日志落盘到 `{data}/logs/audit.log` | 开启 |
| `-audit-log-max` | 审计日志单文件轮转上限（MB，超过后轮转为 `.1`） | `64` |

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

> ⚠️ **Bastille PF 初始化注意事项**：`bastille setup firewall` 生成的
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
  instances.json   # 实例配置列表
  auth.hash        # 配对码 SHA-256 哈希（首次启动生成）
  logs/            # 实例日志 {uuid}.log（-instance-log 开启时）
                   # 审计日志 audit.log（-audit-log 开启时）
```

## 安全说明

- 未指定 `-apikey` 时启用配对码认证，所有 API 请求必须携带首次启动时显示的配对码。
- 文件操作被限制在实例工作目录内（`..` 越界会被拒绝）。
- 建议配合防火墙仅监听 127.0.0.1；如需局域网访问请设置 `-apikey`。
