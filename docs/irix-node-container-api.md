# irix-node 容器 API 对接文档（Docker + Bastille）

> 目标读者：`irix-node`（Go 守护进程）后端实现者。
> 对接对象：IriX 桌面客户端（Flutter），分支 `4-dockerbastille容器化支持`，
> 客户端容器层代码在 `lib/services/container/` 与 `lib/services/node_api_client.dart`。
>
> 本文档是**客户端与服务端的字段级契约**：客户端只按本文档解析响应，
> 任何字段缺失/命名不符都会直接表现为前端异常（如「发行版大小显示 0」、
> 「无法创建 Jail」）。实现时请以本文档为准逐条核对。

---

## 0. 当前已知问题（请后端优先排查）

| # | 现象 | 客户端侧表现 | 排查点 |
|---|------|-------------|--------|
| 1 | bootstrap 后发行版列表**不显示大小** | 显示 `0 B` | `GET /api/bastille/releases` 响应是否包含 `sizeBytes` 字段（见 §4.1）。客户端只认 `sizeBytes`（数字，字节）。 |
| 2 | **无法创建 Jail** | 面板报错 | ① `POST /api/bastille/jails/create` 的 body 契约（见 §4.2），注意 `vnet` 是字符串 `none|vnet|bridge` 而不是 bool；② 客户端创建成功后**会自动调用 rdr 应用端口**（body 中的 `ports` 是客户端自行调 rdr，不在 create body 里）——若 `/api/bastille/rdr` 端点未实现或 PF 未初始化，客户端会把「Jail 已创建」误报为「创建失败」。 |

---

## 1. 通用约定（与现有 irix-node API 一致）

| 项 | 约定 |
|----|------|
| 基础地址 | `http://<host>:<port>` |
| 认证 | `apikey` 查询参数（本地节点可省略） |
| 请求头 | `X-Requested-With: XMLHttpRequest`（MCSM 必需，irix-node 建议兼容） |
| 请求体 | `application/json; charset=utf-8` |
| 响应体 | 统一 `{ "status": 200, "data": <payload>, "time": <unix_ms> }` |
| 错误 | `status != 200` 时 `data` 为错误消息字符串；HTTP 层错误（4xx/5xx）客户端会转成 `HTTP <code>: <body>` 展示 |
| 字符集 | UTF-8 |

客户端所有请求都带 `apikey` 查询参数（配置了密钥时），响应 `status != 200` 即抛异常并把 `data` 字符串显示给用户。**错误信息请写清楚可读的中文/英文原因**，不要只返回 exit code。

---

## 2. 能力探测

```
GET /api/container/info
```

响应 `data`（Bastille 节点示例）：

```json
{
  "runtime": "bastille",
  "platform": "freebsd",
  "version": "0.13.20250126",
  "available": true
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `runtime` | string | `docker` \| `bastille` |
| `platform` | string | `linux` \| `freebsd` 等（用于节点列表显示能力标签） |
| `version` | string? | 运行时版本（面板右上角显示 `vX.Y.Z`） |
| `available` | bool | false 时客户端整体展示不可用状态 |
| `error` | string? | 不可用原因（可选，展示给用户） |

---

## 3. Docker 端点（Linux 节点）

> 客户端 Docker 侧为「创建时全参数 + 列表/操作」，无热更新端口能力。

### 3.1 列表

```
GET /api/container/ps?all=1
GET /api/image/list
GET /api/volume/list
GET /api/network/list
```

容器条目字段（客户端解析键名，**必须逐字一致**）：

```json
{
  "id": "a1b2c3d4e5f6",
  "name": "mc-server",
  "image": "itzg/minecraft-server:latest",
  "status": "Up 2 hours",
  "state": "running",
  "ports": ["0.0.0.0:25565->25565/tcp"],
  "createdAt": "2026-01-01T00:00:00Z",
  "restartPolicy": "unless-stopped"
}
```

镜像条目：`{ "id", "tags": ["name:tag", ...], "sizeBytes": <int>, "createdAt": "..." }`。
卷条目：`{ "name", "driver", "mountpoint" }`。网络条目：`{ "name", "driver", "subnet" }`。

### 3.2 创建与操作

```
POST   /api/container/create      body: 见下
POST   /api/container/{id}/start | stop | restart | kill
DELETE /api/container/{id}?force=1
POST   /api/container/{id}/clone      body: { "name": "<新名称>" }
POST   /api/container/{id}/limits     body: { "memoryMb"?: int, "cpus"?: int }   // docker update
GET    /api/container/{id}/logs?tail=N          → data 为纯文本日志
POST   /api/container/{id}/exec      body: { "command": "..." }
GET    /api/container/{id}/stats      → { "cpuPercent": double, "memoryBytes": int,
                                           "memoryLimitBytes": int, "netRxBytes": int,
                                           "netTxBytes": int }
```

create body（客户端 `DockerCliBackend.createContainer` / `NodeDockerBackend` 组装，全部可选除 name/image）：

```json
{
  "name": "mc-server",
  "image": "itzg/minecraft-server:latest",
  "command": "java -jar server.jar nogui",
  "ports": ["25565:25565"],
  "volumes": ["/data/mc:/data"],
  "env": { "EULA": "TRUE", "MEMORY": "2G" },
  "restartPolicy": "unless-stopped",
  "memoryLimitMb": 4096,
  "cpus": 4,
  "diskLimitMb": 20480,
  "workdir": "/data"
}
```

命令映射：`docker create --name <name> [-p 每个端口] [-v 每个卷] [-e K=V ...]
[--restart <策略>] [-m <N>m] [--cpus <N>] [--storage-opt size=<N>m] [-w <workdir>] <image> [command...]`。

### 3.3 镜像构建（长任务）

```
POST /api/image/pull        body: { "name": "itzg/minecraft-server:latest" }
POST /api/image/build       body: { "dockerfile": "...", "name": "...", "tag": "..." }
                            → data: { "jobId": "<任务id>" }
GET  /api/image/build-progress?jobId=<id>
                            → data: { "status": "building|done|failed",
                                      "log": ["行1", "行2", ...],
                                      "image": "name:tag" }
DELETE /api/image/{name}
DELETE /api/volume/{name}
```

---

## 4. Bastille 端点（FreeBSD 节点）

> 命令语法以官方文档 latest 为准（bastille.readthedocs.io / docs.bastillebsd.org）。
> 服务端通过 `Process.run('bastille', ...)` 包装；**所有会交互确认的命令必须附加 `-y`**。

### 4.1 发行版列表（问题 #1 所在）

```
GET /api/bastille/releases
```

响应 `data` 为数组，**条目必须包含 `sizeBytes`**（客户端仅认这个键；缺省时面板显示 `0 B`）：

```json
[
  {
    "name": "14.2-RELEASE",
    "version": "14.2-RELEASE",
    "sizeBytes": 524288000,
    "createdAt": "2026-01-01T00:00:00Z"
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 发行版名（客户端 `ImageInfo.id`） |
| `version` | string? | 客户端拼标签 `name:version`，缺省显示 `name:RELEASE` |
| `sizeBytes` | **int（必填）** | 发行版目录占用字节数，可 `du -sm` 换算；**这是当前面板唯一显示的大小来源** |
| `createdAt` | string? | ISO-8601 时间，可选 |

### 4.2 创建 Jail（问题 #2 所在）

```
POST /api/bastille/jails/create
```

body（客户端 `NodeBastilleBackend.createContainer` 组装的**完整实际字段**，字段类型务必核对）：

```json
{
  "name": "mc-jail",
  "release": "14.2-RELEASE",
  "ip": "192.168.1.50/24",
  "type": "thin",
  "vnet": "none",
  "interface": null,
  "volumes": [],
  "workdir": null,
  "memoryLimitMb": null,
  "cpus": null,
  "diskLimitMb": null
}
```

| 字段 | 类型 | 取值 | 服务端命令映射 |
|------|------|------|---------------|
| `name` | string | Jail 名（客户端已校验 `^(?=.*[a-zA-Z])[a-zA-Z0-9_-]+$`，**不能为纯数字**——纯数字会被 jail(8) 当作 jid 解析） | 位置参数 NAME |
| `release` | string | 已 bootstrap 的发行版；**`type=empty` 时可为空串** | 位置参数 RELEASE |
| `ip` | string | 显式必填（empty 除外）；VNET 时必须含 `/掩码` | 位置参数 IP |
| `type` | string | `thin`(默认) \| `thick` \| `clone` \| `empty` \| `linux` | thin 无标志；`-T`/`-C`/`-E`/`-L` |
| `vnet` | **string** | `none` \| `vnet` \| `bridge`（**不是 bool！**） | `none` 无标志；`vnet`→`-V`；`bridge`→`-B` |
| `interface` | string? | VNET 时必填：`vnet` 模式为物理网卡、`bridge` 模式为桥接网卡 | 位置参数 INTERFACE |
| `volumes` | string[] | 数据目录挂载 `"宿主机路径:jail内路径"` | 创建后逐条 `bastille mount <name> <host> <jailpath> nullfs` |
| `workdir` | string? | 容器内工作目录（如 `/data`） | 设置 jail `exec.start` 的 cwd（cd 到该目录执行启动命令） |
| `memoryLimitMb` | int? | 内存上限（MB） | `bastille limits <name> add memoryuse <N>M` |
| `cpus` | int? | CPU 核数（服务端换算为 cpuset 列表 `0..N-1`） | `bastille limits <name> cpu 0..N-1` |
| `diskLimitMb` | int? | 磁盘上限（MB） | ZFS：`zfs set quota=<N>M`（jail 数据集）；UFS 可忽略并返回提示 |

约束（官方文档依据）：

- `thin/thick/clone/empty/linux` 互斥；`linux` 与任何 VNET 模式互斥（客户端已挡，服务端可再校验）。
- VNET 时 IP 必须含子网掩码（官方：非 VNET 可省略，VNET 强制）。
- `bastille create -E NAME` 仅需名称（客户端在 empty 时 release/ip 可能为空，服务端不要因此 400）。

**⚠️ 客户端创建后会自动应用端口转发**：面板「端口映射」字段（如 `25565:25565`）不在 create body 里，而是 create 成功后客户端逐条调用 `POST /api/bastille/rdr`。如果 rdr 端点缺失或 PF 未初始化，客户端会报「创建失败」——**但 Jail 其实已经建好**。请服务端保证 rdr 端点存在；PF 未配置时返回带说明的错误消息。

### 4.3 生命周期

```
POST /api/bastille/jails/{name}/start | stop | restart
POST /api/bastille/jails/{name}/destroy?force=1
    → bastille destroy -y [-a] <name>
      force=1（客户端删除时恒传）→ 附加 -a（可摧毁运行中的 jail）；-y 恒附加
POST /api/bastille/jails/{name}/clone     body: { "newName": "...", "ip": "192.168.1.51/24" }
    → bastille clone <name> <newName> [ip]
GET  /api/bastille/jails/{name}/console?tail=N   → data 为纯文本日志尾部
POST /api/bastille/jails/{name}/cmd       body: { "command": "..." }  → bastille cmd / jexec（data 为输出文本，见 §4.13）
GET  /api/bastille/jails/{name}/config    → jail.conf 属性（见 §4.12）
GET  /api/bastille/jails/{name}/mounts    → 挂载列表（见 §4.10）
```

jail 列表 `GET /api/bastille/jails` 条目字段（客户端解析键名）：

```json
{
  "name": "mc-jail",
  "release": "14.2-RELEASE",
  "status": "Up",
  "state": "running",
  "ports": ["tcp 25565 -> 25565"],
  "createdAt": "2026-01-01T00:00:00Z"
}
```

> 客户端用 `status` 判断运行态（含 `up` 即视为运行）；`release` 用于展示「镜像/发行版」列。

### 4.4 Bootstrap / 模板（长任务）

```
POST /api/bastille/bootstrap      body: { "release": "14.2-RELEASE" }
    → 可同步执行（bastille bootstrap 本身耗时）；若异步，返回 data: { "jobId": "..." }
GET  /api/bastille/templates      → [{ "namespace", "name", ... }]
POST /api/bastille/templates/apply body: { "jail", "template", "args": {"KEY": "VALUE"} }
```

### 4.5 端口转发（rdr）

```
POST   /api/bastille/rdr     body: { "jail": "mc-jail", "proto": "tcp",
                                     "hostPort": 25565, "jailPort": 25565 }
    → bastille rdr <jail> tcp|udp <hostPort> <jailPort>
DELETE /api/bastille/rdr     body: 同上
    → 官方 CLI 无单条删除：读 `bastille rdr <jail> list` → `bastille rdr <jail> clear` → 重放其余规则
GET    /api/bastille/rdr?jail=<name>?
    → data 数组，条目：{ "jail": "mc-jail", "proto": "tcp", "hostPort": 25565, "jailPort": 25565 }
```

### 4.6 导入 / 导出

```
POST /api/bastille/jails/{name}/export
    → bastille export --txz <name> /usr/local/bastille/backups/<name>_<ts>.txz
    → data: { "path": "/usr/local/bastille/backups/mc-jail_20260101_120000.txz" }
POST /api/bastille/jails/import     body: { "file": "/path/archive.txz",
                                             "release": "14.2-RELEASE",   // 可选
                                             "force": false }              // 可选，-f 跳过校验和
    → bastille import [-f] <file> [release]
    → data: { "name": "<导入后的 jail 名>" }
```

### 4.7 环境初始化（bastille setup）

```
POST /api/bastille/setup     body: { "mode": "firewall", "extIf": "em0", "tunIf": null, "addr": null }
    → data: { "ok": true, "detail": "<命令输出摘要>" }
```

| mode | 服务端命令 | 参数 |
|------|-----------|------|
| `default` | `bastille setup -y`（自动 loopback+firewall+storage） | 无 |
| `firewall` | `bastille setup firewall` | `extIf` 外网网卡（可选） |
| `vnet` | `bastille setup vnet` | `extIf`/`tunIf`/`addr`（部分版本交互式，可 `-y` + stdin 注入或提示用户手动执行） |
| `bridge` | `bastille setup bridge` | 无 |
| `shared` | `bastille setup shared` | `extIf` 网卡 |
| `linux` | `bastille setup linux`（加载内核模块 + 安装 debootstrap） | 无 |

### 4.8 节点级归档（编排系统跨物理机迁移存档用）

> 客户端编排引擎（xmc_orchestrator）的迁移状态机执行「压缩 → 传输 → 恢复」时
> 调用以下节点级端点。**与实例无关**（不需要 uuid）：操作任意宿主机路径。
> 存档传输以桌面客户端为中继（源节点下载 → 目标节点上传）。

```
POST /api/container/archive          body: { "path": "/data/mc/world", "archive"?: "world_s1_0.zip" }
                                     → data: { "path": "/usr/local/bastille/backups/world_s1_0.zip" }
                                     （服务端在节点上压缩 path 为 zip；archive 缺省自动命名）
GET  /api/container/archive?file=<归档名>
                                     → 原始二进制（非 JSON 信封，直接返回文件字节）
POST /api/container/archive/upload   multipart 字段 "file"（原始字节）→ data: { "path": "<保存路径>" }
POST /api/container/archive/restore  body: { "file": "<归档名>", "destPath": "/data/mc/world" }
                                     （服务端解压归档到 destPath，覆盖式恢复）
```

实现提示：压缩/解压可用系统 `zip`/`tar`（FreeBSD 自带），或复用 zip 库；
`GET archive` 与 `POST upload` 为原始字节传输，不走统一 JSON 信封。

---

## 4.9 软件包管理（bastille pkg）

> 用途：为 jail 安装 Java 运行环境等软件包（客户端「Jail 详情 → 软件包」Tab）。

```
POST /api/bastille/jails/{name}/pkg
```

body：

```json
{ "action": "install", "packages": ["openjdk17-jre", "ca_root_nss"] }
```

| 字段 | 类型 | 取值 |
|------|------|------|
| `action` | string | `install` / `delete` / `update` / `upgrade` / `autoremove`（其他 pkg 子命令亦可透传） |
| `packages` | string[] | 包名列表（install/delete 必填，update/upgrade/autoremove 可空） |

服务端命令映射：`bastille pkg <name> <action> [-y] [pkgs...]`（**必须附加 `-y`**）。
响应 `data` 为命令输出文本（可为字符串，也可为 `{ "output": "..." }`；
pkg 安装耗时较长，客户端超时已放大到 10 分钟）。

---

## 4.10 挂载管理（bastille mount / fstab）

> 用途：将节点上的实例目录挂载进 jail（默认 `/data`），以及挂载 procfs
> （部分 Java 版本 / JVM 特性需要 `/proc`）。客户端「Jail 详情 → 挂载」Tab。

```
GET    /api/bastille/jails/{name}/mounts
POST   /api/bastille/jails/{name}/mounts
DELETE /api/bastille/jails/{name}/mounts?dst=<jail内路径>
```

`GET` 响应 `data` 数组，条目：

```json
{ "src": "/data/mc-survival", "dst": "/data", "fstype": "nullfs", "options": "rw", "permanent": true }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `src` | string? | 宿主机源路径（procfs/devfs 为 null） |
| `dst` | string | jail 内目标路径 |
| `fstype` | string | `nullfs` \| `procfs` \| `devfs` |
| `options` | string? | 挂载选项（如 `rw`） |
| `permanent` | bool | 是否写入 fstab（jail 启动时自动挂载） |

`POST` body：

```json
{ "src": "/data/mc-survival", "dst": "/data", "fstype": "nullfs", "options": "rw" }
```

服务端映射：

- `fstype=nullfs`：`bastille mount <name> <src> <dst>`；
- `fstype=procfs`：向 fstab（`/usr/local/bastille/jails/<name>/fstab`）追加
  `proc <dst> procfs <options> 0 0`（`<dst>` 为 jail 内路径，如 `/proc`），
  并立即挂载（thin jail 下为宿主 `<jailroot>/<dst>`）；
- `fstype=devfs` 同理追加 fstab 并挂载。

`DELETE`：`bastille umount <name> <dst>` 并从 fstab 移除对应条目（找不到条目时
仅卸载，不报错）。

> 注：官方 `bastille mount` 仅支持 nullfs；procfs/devfs 走 fstab 方案
> （fstab 条目会在 `bastille start` 时自动挂载）。`GET` 列表应合并
> fstab 条目与当前 `mount` 输出，`permanent` 表示条目来自 fstab。

### 4.10.1 挂载路径语义与排障

- **`src` 是宿主机路径，`dst` 是 jail 内绝对路径**（如 `/data`）。二者视角不同：
  `src=/data/instances/test50` + `dst=/data` 表示把宿主机目录挂到 jail 内的
  `/data`，在 jail 里 `ls /data` 才能看到内容。若把 `dst` 写成宿主绝对路径或
  实例 cwd 之外的路径，文件会挂进 jail 内一个「没人去读」的目录，表现为
  "挂上但看不到文件"。
- **fstab 持久化**：`bastille mount`（nullfs）与 procfs/devfs 都会把条目写入
  jail 的 fstab（`/usr/local/bastille/jails/<name>/fstab`）。**重启 jail
  （`bastille restart`/`start`）会按 fstab 自动重新挂载**，条目不丢。
  `GET /mounts` 同时合并 fstab 条目（permanent=true）与当前 `mount` 输出
  （permanent=false），二者 dst 统一归一化为 jail 内路径返回。
- **卸载后挂载点目录会被清掉**：`bastille umount` 会移除 jail 内挂载点目录，
  再次 `POST /mounts` 时服务端会先 `MkdirAll` 重建挂载点再挂载（兜底），
  不应再出现"卸载后永远挂不上"。若卸载报错（如设备忙），服务端会如实返回，
  不再静默吞掉导致 fstab 已删但挂载残留的半残状态。
- **不要绕过 IriX 直接 `bastille mount`**：命令行直接挂的条目虽也写 fstab，
  但 `GET /mounts` 与 `DELETE /mounts` 的 dst 匹配已同时兼容 jail 内路径与
  宿主绝对路径两种写法；统一走 IriX API 可避免 fstab 与列表/卸载状态不一致。

---

## 4.11 运行会话（在 jail 内运行长任务进程，如 MC 服务端）

> 用途：客户端「Jail 详情 → 运行」Tab —— 将实例挂载进 jail（默认 `/data`）后，
> 在 jail 内启动服务端进程并轮询输出 / 下发 stdin；「进程退出即停止 Jail」
> 看门狗开关经 `watch` 下发。

```
POST   /api/bastille/jails/{name}/run                 body: { "command", "cwd"?, "watch"? }
GET    /api/bastille/jails/{name}/run/{session}?tail=N&since=<字节偏移>
POST   /api/bastille/jails/{name}/run/{session}/stdin body: { "input" }
POST   /api/bastille/jails/{name}/run/{session}/stop
DELETE /api/bastille/jails/{name}/run/{session}
```

`POST run` body 与命令映射：

| 字段 | 类型 | 说明 |
|------|------|------|
| `command` | string | 以 shell 语义执行（服务端 `sh -c` 包装），如 `java -Xmx2G -jar server.jar nogui` |
| `cwd` | string? | 容器内工作目录（默认 jail 根），服务端执行 `sh -c "cd <cwd> && exec <command>"` |
| `watch` | bool? | **看门狗**：进程退出后服务端自动执行 `bastille stop <name>`（客户端也会自行兜底检测） |

响应 `data`：`{ "sessionId": "s-1" }`。

**服务端要求**：会话进程必须在**后台**运行（不得阻塞 HTTP 请求），stdout/stderr
写入会话环形缓冲（建议同时落盘 `<bastille>/run/<name>/<session>.log` 便于重启恢复）。
进程退出后会话保留一段时间（如 30 分钟）供客户端读取最终状态，之后可清理。

`GET run/{session}` 响应 `data`：

```json
{ "running": false, "exitCode": 0, "offset": 12345, "log": "…新增内容…" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `running` | bool | 进程是否仍在运行 |
| `exitCode` | int? | 已退出时的退出码 |
| `offset` | int | 本次返回末尾的日志字节偏移（客户端增量游标） |
| `log` | string | 自 `since` 偏移之后的**新增**内容（`since` 缺省时返回最后 `tail` 行，默认 200） |

`POST stdin`：`input` 原样写入进程 stdin（客户端会自带换行）。
`POST stop`：终止进程（SIGTERM → 超时 SIGKILL）。
`DELETE`：清理会话缓冲 / 日志文件。

---

## 4.12 配置编辑（bastille config / jail.conf）

> 用途：客户端「Jail 详情 → 设置」Tab —— 编辑已创建 jail 的设置
> （IP、hostname、exec.start、autostart、allow.mount 等）。

```
GET    /api/bastille/jails/{name}/config
POST   /api/bastille/jails/{name}/config        body: { "key": "ip4.addr", "value": "192.168.1.51/24" }
DELETE /api/bastille/jails/{name}/config?key=<key>
```

- `GET` 响应 `data` 为扁平对象：`{ "ip4.addr": "192.168.1.50/24", "hostname": "…", … }`
  （解析 jail.conf 的 `key = value;` 参数；无法解析的整段保持原样可省略）。
- `POST`：`bastille config <name> <key> <value>`（非运行中参数可同时写 jail.conf）。
- `DELETE`：从 jail.conf 移除该参数（`bastille config <name> <key>` 无值形式或
  直接改写 jail.conf，实现自选）；不存在的 key 返回 200 即可。

> 客户端预置常用键提示：`ip4.addr` / `ip6.addr` / `hostname` / `exec.start` /
> `exec.stop` / `exec.consolelog` / `autostart` / `allow.mount` /
> `allow.mount.procfs` / `vnet` / `interface` / `securelevel`。

---

## 4.13 命令输出（cmd 返回文本）

> 现有 `POST /api/bastille/jails/{name}/cmd`（§4.3）的返回契约补充：`data`
> 应返回命令输出文本（可为字符串，或 `{ "output": "..." }`），供客户端
> 「检测 Java」（`java -version`）与「控制台」Tab 一键命令使用。
> 命令以 shell 语义执行（服务端 `sh -c` 包装）。

---

## 5. 自测清单（curl 示例，假设 apikey=KEY）

```bash
# 1. 能力探测
curl "http://<node>:<port>/api/container/info?apikey=KEY"

# 2. 发行版列表 —— 必须含 sizeBytes（问题 #1）
curl "http://<node>:<port>/api/bastille/releases?apikey=KEY"

# 3. 创建 Jail —— 用客户端真实 body 验证（问题 #2）
curl -X POST "http://<node>:<port>/api/bastille/jails/create?apikey=KEY" \
  -H 'Content-Type: application/json' -H 'X-Requested-With: XMLHttpRequest' \
  -d '{"name":"mc-test","release":"14.2-RELEASE","ip":"192.168.1.50/24","type":"thin","vnet":"none","interface":null,"volumes":[],"workdir":null,"memoryLimitMb":null,"cpus":null,"diskLimitMb":null}'

# 4. rdr（创建后客户端会自动调用）
curl -X POST "http://<node>:<port>/api/bastille/rdr?apikey=KEY" \
  -H 'Content-Type: application/json' -H 'X-Requested-With: XMLHttpRequest' \
  -d '{"jail":"mc-test","proto":"tcp","hostPort":25565,"jailPort":25565}'
```

预期：全部返回 `status: 200`；任何一步非 200，前端对应功能即报错。

---

## 6. 客户端侧已确认的容错行为（后端无需处理，仅知悉）

- 发行版 `sizeBytes` 缺失时面板显示 `0 B`（**计划改为显示 `—`**，等后端确认字段后落地）。
- 创建 Jail 时 rdr 失败会中断整个创建流程（**计划改为「创建成功后 rdr 失败仅提示、不中断」**，等后端确认 rdr 可用性后落地）。
- 客户端不会调用本文档以外的 Bastille 命令；`kill` 对 Bastille 回退为 `stop`；Docker 不支持热端口管理（create 时指定）。

## 7. 变更记录

| 日期 | 变更 |
|------|------|
| 2026-01 | 初版：Docker + Bastille 全端点契约（分支 `4-dockerbastille容器化支持`，commit `a2cef12` 起） |
