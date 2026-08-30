# IriX Node Daemon

IriX 客户端「节点」类型中的本地节点守护进程，使用 Go 语言实现。
核心仍以标准库为主；账户管理引入少量第三方依赖（SQLite/MySQL/PostgreSQL
驱动、Redis 客户端与 x/crypto，见 `go.mod` 与 `docs/accounts-design.md`）。

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

Windows 提供 `amd64` / `arm64` / `386` 三个架构的 Release 产物（`irix-node-windows-*.exe`），
32 位系统用 `windows-386`；Go 不支持 32 位 ARM 的 Windows（`windows/arm`），故无对应产物。

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

### 账户管理

配对码登录即 **root 管理员**：首次用配对码登录时必须修改密码（此后配对码不再
用于登录）；管理员可创建/删除账户，并对**每个 API 端点**独立开关权限（按模块
分组，支持整组开关），默认 SQLite 存储、可选 MySQL/PostgreSQL（自带连接池）、
Redis 可选缓存会话与权限热数据。详见 `docs/accounts-design.md`。

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

### 在 ARM / x86 / PowerPC / s390x / MIPS Linux 上运行

树莓派、Orange Pi、NanoPi、瑞芯微 RK / 全志系等 ARM 开发板，奔腾/赛扬年代的
**32 位 x86** 老机器，**POWER 系** 服务器（IBM POWER8/9、Talos II、OpenPOWER），
**IBM Z（z/Architecture，s390x）** 大型机，以及 **MIPS** 设备（路由器、老
Loongson 等），都只是普通的 `linux` 目标——与 x86 服务器用同一套 `linux`
编译产物。按 `uname -m` 选择：

| `uname -m` 输出 | 设备 | Release 产物 |
| --- | --- | --- |
| `aarch64` | 64 位 ARM 开发板（树莓派 4/5、RK3566、Orange Pi 3 等） | `irix-node-linux-arm64` |
| `armv7l` | 32 位 ARM 老开发板（树莓派 3、Orange Pi Zero 等） | `irix-node-linux-arm` |
| `i686` / `i386` | 32 位 x86 老机器（奔腾/赛扬，跑轻量 Linux） | `irix-node-linux-386` |
| `ppc64le` | 64 位小端 POWER（IBM POWER8/9、Talos II、OpenPOWER） | `irix-node-linux-ppc64le` |
| `s390x` | IBM Z / 大型机（z/Architecture，如 z15 / LinuxONE） | `irix-node-linux-s390x` |
| `mips` | 32 位大端 MIPS（老式网络设备） | `irix-node-linux-mips` |
| `mipsel` | 32 位小端 MIPS（MT7620/7621 等路由器） | `irix-node-linux-mipsle` |
| `mips64` | 64 位大端 MIPS（Cavium Octeon 等） | `irix-node-linux-mips64` |
| `mips64el` | 64 位小端 MIPS（老 Loongson 3A 等） | `irix-node-linux-mips64le` |

```bash
wget https://github.com/<你的仓库>/releases/latest/download/irix-node-linux-arm64
chmod +x irix-node-linux-arm64
./irix-node-linux-arm64 -bind 127.0.0.1 -port 12346 -data ~/irix-data
```

> 注意：`irix-node-linux-arm*` 与 `irix-node-Android-*` / `irix-node-OpenHarmony-*`
> 底层同为 `linux/arm(64)` 编译（二进制可互通），但命名区分用途：标准 Linux
> 开发板请用 `linux-arm*`；Termux 用 `Android-*`；鸿蒙用 `OpenHarmony-*`。
>
> **MIPS 全系账户存储不支持 SQLite**：Go 的 SQLite 驱动 `modernc.org/sqlite`
> 未覆盖任何 MIPS 变体，因此 MIPS 产物与 Solaris/illumos 同款——启动时必须
> 指定 `-accounts-driver postgres`（或 mysql）并配置 `-accounts-dsn`，其余
> 功能（实例/文件/集群等）与普通 Linux 完全一致。32 位 MIPS 产物为
> **softfloat** 编译（`GOMIPS=softfloat`），可运行在无 FPU 的路由器 SoC 上。
>
> **PowerPC 大端（`ppc64`）暂不支持**：旧 IBM pSeries / 老 Mac G5 等大端 PowerPC
> 因 Go 的 SQLite 驱动 `modernc.org/sqlite` 仅覆盖小端 `ppc64le` 而无法编译；
> 这类机器若可改用 PostgreSQL/MySQL（与 Solaris/illumos 同款隔离方案）可解锁，
> 或升级到小端 POWER 硬件。
>
> 关于 **真·MS-DOS（16 位实模式）**：Go 运行时要求 32 位保护模式与 MMU，且本程序是
> 监听 TCP 端口的常驻服务，纯 DOS 无此能力，**无法兼容**。DOS 时代的老机器只要能跑
> 轻量 32 位 Linux，即可用上表的 `irix-node-linux-386`。
>
> 关于 **Xtensa（ESP32 系列）**：Go 编译器不支持 Xtensa 架构（`unsupported
> GOOS/GOARCH`），且 ESP32 级 Xtensa 芯片跑的是 FreeRTOS 而非 Linux，无法运行
> 常驻 HTTP 服务，**无法兼容**。乐鑫系里仅 RISC-V 架构（ESP32-C/D/P 系列）理论上
> 可跑 Linux 的型号才有可能，属另一话题。

### 在 Android（Termux）上运行

IriX Node 为静态链接的纯 Go 二进制，无需 NDK 或交叉工具链，可直接在 Termux 中运行。
SQLite 驱动 `modernc.org/sqlite` 纯 Go 实现（无 CGO），账户管理开箱即用。

**方式一：下载预编译二进制**

从 Release 页面按设备架构选择（Termux 里执行 `uname -m` 查看）：

| `uname -m` 输出 | 设备 | Release 产物 |
| --- | --- | --- |
| `aarch64` | 绝大多数 64 位手机（armv8a） | `irix-node-Android-armv8a` |
| `armv7l` | 32 位老设备（armv7a） | `irix-node-Android-armv7a` |

```bash
# 64 位设备示例
pkg install wget         # 首次需装 wget
wget https://github.com/<你的仓库>/releases/latest/download/irix-node-Android-armv8a
chmod +x irix-node-Android-armv8a
./irix-node-Android-armv8a -bind 127.0.0.1 -port 12346 -data ~/irix-data
```

**方式二：在 Termux 中直接编译**

Termux 自带完整 Go 工具链，可本机编译（无需交叉编译）：

```bash
pkg install golang git
git clone https://github.com/<你的仓库>.git
cd IriX-Node
go build -trimpath -ldflags "-s -w" -o irix-node .
./irix-node -bind 127.0.0.1 -port 12346 -data ~/irix-data
```

**注意事项**

- Termux 不支持 systemd，需常驻时用 `termux-wake-lock` 防止后台被杀，或配合
  `Termux:Boot` 开机自启。
- 默认监听 `127.0.0.1`，仅本机访问；如需局域网访问用 `-bind 0.0.0.0` 并设置
  `-apikey`，同时注意 Android 仅对前台应用开放部分端口。
- 数据目录建议放在 `$HOME` 下（如 `~/irix-data`），避免写入受外置存储权限限制的路径。

### 在 OpenHarmony（鸿蒙原生 Linux 用户态）上运行

OpenHarmony 标准系统内核为 Linux，因此 IriX Node 复用 `linux/arm64`（或 `linux/arm`）
的**静态链接**二进制即可运行——Go 工具链目前没有 `ohos` 目标，无法单独交叉编译鸿蒙
专用二进制。SQLite 驱动 `modernc.org/sqlite` 纯 Go 实现（无 CGO），账户管理开箱即用。
节点会读取 `/etc/os-release` 自动识别 `OpenHarmony`，`GET /api/overview` 的 `type`/
`platform` 字段返回 `OpenHarmony`（而非普通 `linux`）。

**重要约束**

- **必须是 `CGO_ENABLED=0` 的静态二进制**。OpenHarmony 标准系统使用 musl 系 C 库，
  动态链接 glibc 的二进制可能启动失败；Release 产物均为静态链接，直接可用。
- 需要设备具备**终端与 Root/Shell 权限**（如 oh 命令、`hdc shell` 进入的设备侧
  shell，或带终端的开源移植）。纯 ArkTS 应用沙箱（无终端、无进程运行权限）无法以
  进程形态运行本守护进程。
- 容器能力（Docker/Bastille）在 OpenHarmony 上不可用，探测返回 `available=false`，
  客户端自动隐藏容器 UI，不影响其余功能。

**获取与启动**

从 Release 页面按 `uname -m` 选择产物（与 Android 同源，但命名区分鸿蒙环境）：

| `uname -m` 输出 | 设备 | Release 产物 |
| --- | --- | --- |
| `aarch64` | 64 位设备（armv8a） | `irix-node-OpenHarmony-armv8a` |
| `armv7l` | 32 位设备（armv7a） | `irix-node-OpenHarmony-armv7a` |

```bash
# 以设备侧 shell 为例（需有写权限的目录）
wget https://github.com/<你的仓库>/releases/latest/download/irix-node-OpenHarmony-armv8a
chmod +x irix-node-OpenHarmony-armv8a
./irix-node-OpenHarmony-armv8a -bind 127.0.0.1 -port 12346 -data /data/local/irix-data
```

- 常驻：OpenHarmony 无 systemd，可借助后台 shell / `nohup` 或在开机脚本中拉起；
  注意系统对后台进程的资源回收策略。
- 数据目录建议使用设备侧可写路径（如 `/data/local/irix-data`），避免沙箱受限目录。
- 局域网访问同理用 `-bind 0.0.0.0` 并设置 `-apikey`。

### 在 Solaris / illumos 上运行

IriX Node 支持 Oracle Solaris（`solaris/amd64`）与开源 Solaris 系 illumos
（SmartOS / OmniOS / Tribblix 等，`illumos/amd64`）。Release 产物：

| 平台 | Release 产物 |
| --- | --- |
| Oracle Solaris（amd64） | `irix-node-Solaris-amd64` |
| illumos（amd64） | `irix-node-Illumos-amd64` |

**关键约束：账户存储不能使用 SQLite**

Go 的纯 Go SQLite 驱动 `modernc.org/sqlite`（本项目默认账户存储）**未覆盖
solaris/illumos 平台**，无法编译。因此该驱动的引入已被 `build tag` 隔离
（见 `accounts_sqlite.go` / `accounts_nosqlite.go`）：solaris/illumos 下
SQLite 选项直接返回错误，必须改用 **PostgreSQL 或 MySQL**：

```bash
./irix-node-Solaris-amd64 \
  -accounts-driver postgres \
  -accounts-dsn "postgres://user:pass@127.0.0.1:5432/irix?sslmode=disable" \
  -bind 127.0.0.1 -port 12346 -data /var/irix-node
```

- 未配置 `-accounts-driver postgres` 且用了默认的 `sqlite` 时，启动会直接报错
  提示改用 postgres/mysql，不会静默失败。
- MySQL / PostgreSQL 驱动（`go-sql-driver/mysql`、`jackc/pgx`）为纯 Go，
  在 solaris/illumos 下可正常交叉编译，账户管理其余功能（会话、权限、Redis 缓存）
  与 Linux 完全一致。
- 容器能力（Docker/Bastille）在 solaris/illumos 上不可用，探测返回 `available=false`，
  客户端自动隐藏容器 UI。
- 主机信息（`GET /api/overview`）在 solaris/illumos 下部分字段（内存/磁盘/网络）
  走兜底零值或基础探测，不影响节点基本功能。

### 在 NetBSD 上运行

NetBSD 是标准类 Unix 平台，Release 提供 `netbsd/amd64` 产物（`irix-node-netbsd-amd64`），
账户管理默认 SQLite 即可使用，无需切换驱动：

```bash
./irix-node-netbsd-amd64 -bind 127.0.0.1 -port 12346 -data /var/irix-node
```

**约束**

- 受 Go 的 SQLite 驱动 `modernc.org/sqlite` 覆盖限制，目前仅 `netbsd/amd64`
  可编译（`netbsd/386`、`netbsd/arm`、`netbsd/arm64` 的 SQLite 驱动未覆盖，
  需改用 PostgreSQL/MySQL，与 Solaris/illumos 同款隔离方案）；后续上游驱动补齐
  对应架构后会自动解锁。
- 主机信息采集（`osUptime`/`osMem` 等）在 NetBSD 下走兜底逻辑，`GET /api/overview`
  的 `type`/`platform` 返回 `netbsd`，内存/磁盘/网络部分字段可能为兜底零值，
  不影响节点基本功能；如需精确主机信息，后续可补 `sysinfo_netbsd.go`。
- 容器能力（Docker/Bastille）在 NetBSD 上不可用，探测返回 `available=false`，
  客户端自动隐藏容器 UI。

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
