# 多平台部署

各平台运行说明。除特别标注外，均与 x86 服务器用同一套编译产物，按 `uname -m` 选择即可。

- [在 ARM / x86 / PowerPC / s390x / MIPS Linux 上运行](#在-arm--x86--powerpc--s390x--mips-linux-上运行)
- [在 Android（Termux）上运行](#在-androidth上运行)
- [在 OpenHarmony（鸿蒙原生 Linux 用户态）上运行](#在-openharmony鸿蒙原生-linux-用户态上运行)
- [在 Solaris / illumos 上运行](#在-solaris--illumos-上运行)
- [在 FreeBSD / OpenBSD / NetBSD 上运行（多架构）](#在-freebsd--openbsd--netbsd-上运行多架构)
- [配对码机制](#配对码机制)

## 在 ARM / x86 / PowerPC / s390x / MIPS Linux 上运行

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

## 在 Android（Termux）上运行

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

## 在 OpenHarmony（鸿蒙原生 Linux 用户态）上运行

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

## 在 Solaris / illumos 上运行

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

## 在 FreeBSD / OpenBSD / NetBSD 上运行（多架构）

三个 BSD 系系统全部按 Go 支持的架构提供产物。受 Go 的 SQLite 驱动
`modernc.org/sqlite` 覆盖限制，部分架构的账户存储需改用 PostgreSQL/MySQL
（与 Solaris/illumos 同款隔离方案），其余功能完全一致：

| 系统 / 架构 | Release 产物 | 账户存储 |
| --- | --- | --- |
| freebsd/amd64、arm64、386、arm(GOARM=7) | `irix-node-freebsd-*` | SQLite 可用 |
| openbsd/amd64、arm64 | `irix-node-openbsd-amd64` / `-arm64` | SQLite 可用 |
| openbsd/386、arm(GOARM=7)、ppc64、riscv64 | `irix-node-openbsd-*` | 需 postgres/mysql |
| netbsd/amd64 | `irix-node-netbsd-amd64` | SQLite 可用 |
| netbsd/386、arm(GOARM=7)、arm64 | `irix-node-netbsd-*` | 需 postgres/mysql |
| solaris/amd64、illumos/amd64 | `irix-node-Solaris-amd64` / `-Illumos-amd64` | 需 postgres/mysql |

```bash
./irix-node-netbsd-amd64 -bind 127.0.0.1 -port 12346 -data /var/irix-node
# 需 postgres 的架构示例（openbsd/386、netbsd/arm64 等）
./irix-node-openbsd-386 -accounts-driver postgres \
  -accounts-dsn "postgres://user:pass@127.0.0.1:5432/irix?sslmode=disable" \
  -bind 127.0.0.1 -port 12346 -data /var/irix-node
```

**约束**

- 需 postgres 的架构若未配置 `-accounts-driver`，启动会直接报错提示，不会静默失败。
- 主机信息采集（`osUptime`/`osMem` 等）在 NetBSD 下走兜底逻辑，`GET /api/overview`
  的 `type`/`platform` 返回 `netbsd`，内存/磁盘/网络部分字段可能为兜底零值，
  不影响节点基本功能；如需精确主机信息，后续可补 `sysinfo_netbsd.go`。
- 容器能力（Docker/Bastille）在 BSD 系上仅 FreeBSD 提供（Bastille），OpenBSD/NetBSD
  探测返回 `available=false`，客户端自动隐藏容器 UI。

## 配对码机制

不指定 `-apikey` 时启用**配对码机制**：

- 首次启动自动生成一个 20 位随机配对码，并在终端**仅显示这一次**；
- 磁盘只保存配对码的 SHA-256 哈希（`{data}/auth.hash`），后续启动不会再次显示；
- 之后所有 API 请求都必须携带该配对码：`?apikey=<配对码>` 查询参数或 `X-Api-Key: <配对码>` 请求头；
- 配对码丢失后无法找回，只能删除 `auth.hash` 重新启动以生成新的配对码。

在 IriX 客户端中添加「节点」类型的节点时，地址填 `http://127.0.0.1:12346`。
