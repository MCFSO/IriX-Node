# 多平台部署

CI 和 Release 只编译下列可作为常驻节点运行、且默认 SQLite 账户存储可用的目标。
Release 文件名为 `irix-node-{系统}-{架构}`，Windows 产物带 `.exe`。

| 系统 | 架构 | Release 产物 |
| --- | --- | --- |
| Windows | amd64、arm64 | `irix-node-windows-amd64.exe`、`irix-node-windows-arm64.exe` |
| Linux | amd64、arm64、arm（GOARM=7） | `irix-node-linux-amd64`、`irix-node-linux-arm64`、`irix-node-linux-arm` |
| macOS | amd64、arm64 | `irix-node-darwin-amd64`、`irix-node-darwin-arm64` |
| FreeBSD | amd64、arm64 | `irix-node-freebsd-amd64`、`irix-node-freebsd-arm64` |
| OpenBSD | amd64、arm64 | `irix-node-openbsd-amd64`、`irix-node-openbsd-arm64` |
| NetBSD | amd64 | `irix-node-netbsd-amd64` |

以上二进制均以 `CGO_ENABLED=0` 编译。Linux 的 ARM 产物也可供具有兼容 Linux
用户态的 Termux 或 OpenHarmony 设备尝试使用；不再为相同的 Linux 二进制发布
Android、OpenHarmony 的重复命名产物。Android 原生（`GOOS=android`）也不再预编译。

例如，在 Linux ARM64 设备上运行：

```bash
chmod +x irix-node-linux-arm64
./irix-node-linux-arm64 -bind 127.0.0.1 -port 12346 -data ~/irix-data
```

Linux 可使用 Docker 容器能力，FreeBSD 可使用 Bastille；其余平台的容器能力由
`GET /api/container/info` 返回的 `available` 字段决定。NetBSD 的部分主机信息
目前使用兜底值。

## 其他平台自行编译

Windows/Linux/BSD 的 32 位或其他 CPU 架构，以及 Solaris、illumos、AIX、
DragonFly BSD、Plan 9 等目标不再进入 CI 构建矩阵，也不提供 Release 产物。
现有平台适配源码仍保留；需要这些目标时可自行设置 `GOOS`、`GOARCH` 并构建，
但它们未经过 CI 验证。部分目标不受默认 SQLite 驱动支持，需配置
`-accounts-driver postgres` 或 `mysql` 及相应的 `-accounts-dsn`。

WebAssembly 运行环境无法承载本项目的常驻 TCP 服务，因此不编译为节点产物。
iOS 与 32 位 Windows ARM 不在 Go 当前可用的本项目构建目标内。

## 配对码机制

不指定 `-apikey` 时启用配对码机制：

- 首次启动自动生成一个 20 位随机配对码，并在终端仅显示一次；
- 磁盘只保存配对码的 SHA-256 哈希（`{data}/auth.hash`）；
- API 请求携带 `?apikey=<配对码>` 查询参数或 `X-Api-Key: <配对码>` 请求头；
- 配对码丢失后可删除 `auth.hash`，重启时生成新配对码。

在 IriX 客户端中添加「节点」类型的节点时，地址填 `http://127.0.0.1:12346`。
