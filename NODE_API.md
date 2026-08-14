# 节点 API 需求清单（多机管理模式）

> 本文档列出 **IriX 桌面应用（多机管理模式）实际调用**的节点侧 HTTP API 全集，
> 覆盖 MCSManager 面板节点与 IriX 本地节点（`irix-node`，Go 守护进程）——两者提供同一风格 API，
> 客户端共用一套实现（`lib/services/node_api_client.dart`）。
>
> 用途定位：
> - 为 `irix-node`（Go 守护进程）实现者 / 对接者提供**最小可实现面**（本文档内所有端点均为应用已调用的）。
> - 多机模式主页「节点资源总览」依赖的资源字段详见 §2（`GET /api/overview`）。
> - 多机模式**规划中但尚未实现**的集群级接口（节点级文件存储 / 增量同步 / 自组织）见 `docs/cluster-node-api.md`（P0–P2，仅 `irix-node`）。

---

## 1. 通用约定

| 项 | 约定 |
|----|------|
| 基础地址 | `http://<host>:<port>`（如 `http://127.0.0.1:12346` / `http://192.168.1.5:23333`） |
| 认证 | `apikey` 查询参数（本地节点可为空）；请求头 `X-Requested-With: XMLHttpRequest`（MCSM 必需，irix-node 建议兼容） |
| 请求体 | `application/json; charset=utf-8` |
| 响应体 | 统一 `{ "status": 200, "data": <payload>, "time": <unix_ms> }`；`status != 200` 时 `data` 为错误消息字符串 |
| 超时 | 应用侧默认 15s；连接失败 / 超时视为节点离线 |
| 分页 | `page` / `page_size` 查询参数 |

> 所有 HTTP 请求由应用内 Rust `http_client` FFI 发出（不经过 Dart `http` 包）。

---

## 2. 资源监控（主页「节点资源总览」核心依赖）

### `GET /api/overview`

多机模式监控循环每 15s 轮询一次所有节点，获取资源快照，聚合为统一总览表
（CPU / 内存 / 磁盘 并排对比 + 网络吞吐折线图）。

**`data.system` 字段**（应用侧解析 `lib/models/remote.dart` → `OverviewSystem`）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `type` | string | 系统类型（如 `Linux`），总览表「系统：」行主显示，缺失时回退 `platform` |
| `platform` | string | 平台名（如 `linux` / `win32` / `darwin`） |
| `hostname` | string | 主机名 |
| `release` | string | 内核版本 |
| `version` | string | 系统版本（如 `22.04`）；空则回退 `release` |
| `uptime` | number | 开机时长 |
| `totalmem` | int | 物理内存总量（字节） |
| `freemem` | int | 空闲内存（字节） |
| `cpuUsage` | number | CPU 使用率（0~1，如 `0.45` = 45%） |
| `memUsage` | number | 内存使用率（0~1） |
| `diskusage` | number | 磁盘使用率（0~1） |
| `disktotal` | int | 磁盘总容量（字节） |
| `diskused` | int | 磁盘已用（字节） |
| `networkDownload` | number | 下载速率（字节/秒） |
| `networkUpload` | number | 上传速率（字节/秒） |

**`data.remote`**（守护进程列表，`DaemonInfo`）：

| 字段 | 说明 |
|------|------|
| `uuid` / `ip` / `port` | 守护进程标识与地址 |
| `remarks` / `version` | 备注名 / 版本（`displayName` 优先 remarks） |
| `available` | 是否可用 |
| `instance.running` / `instance.total` | 运行中 / 总实例数 |
| `system` | 该守护进程自身的 `OverviewSystem`（同 system 字段表） |

> **MCSM 合并规则**：面板 `system` 可能不含磁盘 / 网络数据（数据实际位于 `remote[]` 各守护进程的 `system`），
> 应用在轮询时会用守护进程的 `system` 补齐缺失的 `diskusage/disktotal/diskused/networkDownload/networkUpload`。

**应用用途**：节点在线探测（`ping`）、资源总览表、内存压力检测与自动迁移决策、聚合网络吞吐图。

---

## 3. 实例管理

| 方法 | 路径 | 参数 | 应用用途 |
|------|------|------|----------|
| `GET` | `/api/service/remote_service_instances` | `daemonId`, `page`, `page_size`, `instance_name`, `status` | 实例列表（节点详情页） |
| `GET` | `/api/instance` | `uuid`, `daemonId` | 实例详情 / 崩溃检测（轮询状态变化） |
| `POST` | `/api/instance` | `daemonId`; body: 实例配置 | 创建集群实例（按资源分配），响应 `data.instanceUuid` |
| `PUT` | `/api/instance` | `uuid`, `daemonId`; body: 实例配置 | 更新实例配置 |
| `DELETE` | `/api/instance` | `daemonId`; body: `{uuids: [], deleteFile}` | 删除实例（迁移清理） |
| `GET` | `/api/protected_instance/open` | `uuid`, `daemonId` | 启动实例 |
| `GET` | `/api/protected_instance/stop` | `uuid`, `daemonId` | 停止实例（优雅停止 + 增量同步用） |
| `GET` | `/api/protected_instance/restart` | `uuid`, `daemonId` | 重启实例 |
| `GET` | `/api/protected_instance/kill` | `uuid`, `daemonId` | 强制终止 |
| `GET` | `/api/protected_instance/command` | `uuid`, `daemonId`, `command` | 向实例 stdin 发命令 |
| `GET` | `/api/protected_instance/outputlog` | `uuid`, `daemonId`, `size?` | 读取实例输出日志 |

---

## 4. 文件管理（实例级，迁移 / 远程文件管理器）

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/files/list` | `daemonId`, `uuid`, `target`, `page`, `page_size` | 文件列表 |
| `PUT` | `/api/files/` | `daemonId`, `uuid`; body: `{target}`（读）/ `{target, text}`（写） | 读 / 写文件内容 |
| `DELETE` | `/api/files` | `daemonId`, `uuid`; body: `{targets: []}` | 删除文件 / 目录 |
| `PUT` | `/api/files/move` | `daemonId`, `uuid`; body: `{targets: [[src, dst], ...]}` | 移动 / 重命名 |
| `POST` | `/api/files/copy` | `daemonId`, `uuid`; body: `{targets: [[src, dst], ...]}` | 复制 |
| `POST` | `/api/files/compress` | `daemonId`, `uuid`; body: `{type: 1, code, source, targets}` | 压缩（type=1） |
| `POST` | `/api/files/compress` | 同上，`type: 2`，`targets: [dest]` | 解压 |
| `POST` | `/api/files/mkdir` | `daemonId`, `uuid`; body: `{target}` | 新建目录 |
| `POST` | `/api/files/touch` | `daemonId`, `uuid`; body: `{target}` | 新建空文件 |
| `POST` | `/api/files/download` | `daemonId`, `uuid`, `file_name` | 申请下载票据 → `{password, addr}` |
| `POST` | `/api/files/upload` | `daemonId`, `uuid`, `upload_dir` | 申请上传票据 → `{password, addr, upload_dir}` |

**直连传输**（票据返回的 `addr` 指向节点自身，绕过面板代理）：

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/download/{password}/{fileName}` | 凭票据直连下载（大文件走 Rust downloader 断点续传） |
| `POST` | `/upload/{password}` | 凭票据直连上传（multipart，字段名 `file`） |

---

## 5. 用户管理（仅 MCSM 面板）

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/auth/search` | `page`, `page_size`, `userName?`, `role?` | 用户列表 |
| `POST` | `/api/auth` | body: `{username, password, permission}` | 创建用户（permission: 1=User, 10=Admin, -1=Banned） |
| `PUT` | `/api/auth` | body: `{uuid, config}` | 更新用户 |
| `DELETE` | `/api/auth` | body: `[uuid, ...]` | 删除用户 |

## 6. Docker 环境（仅 MCSM 面板，且按平台显示开关控制）

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/environment/image` | `daemonId` | 镜像列表 |
| `GET` | `/api/environment/containers` | `daemonId` | 容器列表 |
| `GET` | `/api/environment/network` | `daemonId` | 网络列表 |
| `POST` | `/api/environment/image` | `daemonId`; body: `{dockerFile, name, tag}` | 构建镜像 |
| `GET` | `/api/environment/progress` | `daemonId` | 构建进度（`{镜像名: -1失败/1构建中/2完成}`） |

---

## 6.1 容器环境（irix-node 全功能；客户端已实现，服务端按此对接）

> 客户端（`lib/services/node_api_client.dart` + `lib/services/container/node_container_backend.dart`）
> 已按以下契约实现调用方；`irix-node` 需实现这些端点，字段名以本表为准。
> 详细设计见 `docs/container-support.md` §3。

**能力探测**（UI 按 `runtime`/`platform` 决定展示 Docker 或 Bastille 管理）：

```
GET /api/container/info
// 响应 data: { runtime: "docker"|"bastille", platform: "linux"|"freebsd", version: "…", available: true }
```

**Docker（platform=linux）**：

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/container/ps` | `all=1` | 容器列表，条目：`{id, name, image, status, state, ports: [..], createdAt, restartPolicy}` |
| `POST` | `/api/container/create` | body: `{name, image, command?, workdir?, ports: [..], volumes: [..], env: {}, restartPolicy?, memoryLimitMb?, cpus?, diskLimitMb?}` | 创建容器（不启动） |
| `POST` | `/api/container/{id}/start` `stop` `restart` `kill` | | 容器操作 |
| `DELETE` | `/api/container/{id}` | `force=1` | 删除容器 |
| `GET` | `/api/container/{id}/logs` | `tail=N` | 日志尾部 |
| `POST` | `/api/container/{id}/exec` | body: `{command}` | 容器内执行命令 |
| `GET` | `/api/container/{id}/stats` | | `{cpuPercent, memoryBytes, memoryLimitBytes, netRxBytes, netTxBytes}` |
| `POST` | `/api/container/{id}/clone` | body: `{name}` | 克隆容器（commit+create 等效）→ `{id, name, image}` |
| `POST` | `/api/container/{id}/limits` | body: `{memoryMb?, cpus?}` | 动态调整资源限制（docker update） |
| `POST` | `/api/container/{id}/export` | | 导出容器文件系统为 tar → `{password, addr, fileName}` |
| `GET` | `/api/image/list` | | 镜像列表，条目：`{id, tags: [..], sizeBytes, createdAt}` |
| `POST` | `/api/image/pull` | body: `{name}` | 拉取镜像 |
| `POST` | `/api/image/build` | body: `{dockerfile, name, tag}` | 构建镜像 → `{jobId}` |
| `GET` | `/api/image/build-progress` | `jobId` | `{status: building\|done\|failed, log: [..], image: "name:tag"}` |
| `DELETE` | `/api/image/{name}` | | 删除镜像 |
| `POST` | `/api/image/import` | body: `{fileName, name}` | 从同步区 tar 导入为镜像 |
| `GET` | `/api/volume/list` / `DELETE /api/volume/{name}` | | 卷列表 / 删除 |
| `GET` | `/api/network/list` | | 网络列表，条目：`{name, driver, subnet?}` |

**Bastille（platform=freebsd）**：

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/bastille/releases` | | bootstrap 的发行版列表，条目：`{name, version, sizeBytes?, createdAt?}` |
| `POST` | `/api/bastille/bootstrap` | body: `{release}` | bootstrap 发行版 → `{jobId}` |
| `POST` | `/api/bastille/setup` | body: `{mode: pf\|vnet\|linux\|check, extIf?, tunIf?, addr?}` | 容器软件初始化（网络 / Linux Jail 功能）→ `{ok, detail?, checked?}` |
| `GET` | `/api/bastille/jails` | | jail 列表，条目：`{name, release, status, state?, ports: [..], createdAt?}` |
| `POST` | `/api/bastille/jails/create` | body: `{name, release, ip?, type: thin\|thick\|clone\|empty\|linux, vnet?, bridge?, mac?, volumes?: [{source, dest}], workdir?, memoryLimitMb?, cpus?, diskLimitMb?}` | 创建 jail（volumes 以 nullfs 挂载；workdir 设置 exec.start 工作目录；limits 为 rctl / ZFS 配额）→ `{name, warnings}` |
| `POST` | `/api/bastille/jails/{name}/start` `stop` `restart` | | jail 操作 |
| `POST` | `/api/bastille/jails/{name}/destroy` | `force=1` | 销毁 jail（force=1 附加 -a，可摧毁运行中的） |
| `POST` | `/api/bastille/jails/{name}/clone` | body: `{newName, ip?}` | 克隆 jail（可选改 IP） |
| `POST` | `/api/bastille/jails/{name}/export` | | 导出 jail 为归档 → `{path: 归档路径}` |
| `POST` | `/api/bastille/jails/import` | body: `{file, newName?, replace?}` | 从同步区归档导入 jail |
| `GET` | `/api/bastille/jails/{name}/console` | `tail=N` | 日志尾部 |
| `POST` | `/api/bastille/jails/{name}/cmd` | body: `{command}` | jail 内执行命令 |
| `GET` | `/api/bastille/jails/{name}/config` | | jail.conf 内容 |
| `GET` | `/api/bastille/jails/{name}/mounts` | | fstab 挂载列表 |
| `POST` / `DELETE` | `/api/bastille/jails/{name}/mounts` | body: `{source, dest}` / `{dest}` | 挂载 / 卸载 |
| `POST` | `/api/bastille/jails/{name}/limits` | body: `{memoryMb?, cpus?, diskMb?}` | 硬件资源限制（rctl memoryuse/cpuset、ZFS 配额） |
| `GET` | `/api/bastille/templates` | | 模板列表（project/template 格式） |
| `POST` | `/api/bastille/templates/apply` | body: `{jail, template, args: {KEY=VALUE}}` | 应用模板 |
| `POST` / `DELETE` | `/api/bastille/rdr` | body: `{jail, proto, hostPort, jailPort}` | 端口转发 / 删除转发 |
| `GET` | `/api/bastille/rdr` | `jail?` | 转发规则列表（可按 jail 过滤）→ `[{proto, hostPort, jailPort}]` |

**回退**：MCSM 面板无 `/api/container/info`，客户端自动回退到 §6 受限模式
（镜像构建 + 容器/网络只读列表），UI 标注「MCSM 受限模式」。

---

## 7. 多机模式功能 → 依赖端点映射

| 功能 | 依赖端点 |
|------|----------|
| 主页节点资源总览（CPU/内存/磁盘统一显示、网络图） | `GET /api/overview`（15s 轮询，MCSM 合并 daemon system 磁盘/网络） |
| 节点在线状态探测 | `GET /api/overview`（失败即离线，附错误信息） |
| 崩溃检测（running/starting → stopped 非用户停止 → 计数，≥3 次迁移） | `GET /api/instance` 轮询状态 |
| 内存压力自动迁移 | `GET /api/overview` 快照 + 实例创建/删除/启停/文件迁移 |
| 实例管理页（新建 / 启动 / 优雅停止+同步 / 迁移 / 详情） | §3 实例管理 + §4 文件管理 + `GET /api/overview`（取 daemonId） |
| 远程文件管理器 | §4 文件管理 + 直连下载 / 上传 |
| 节点详情页各标签页 | §2 概览 + §3 实例 + §4 文件 + §5 用户 + §6 Docker（按节点类型裁剪） |

---

## 8. 能力差异与缺口

| 能力 | irix-node | MCSM | 说明 |
|------|-----------|------|------|
| 本文档 §2–§4 全部端点 | ✅ | ✅ | 现有实现，双方均已支持（含 §2 磁盘/网络/version 字段、§4 上传票据 `upload_dir`） |
| §5 用户管理 / §6 Docker | ❌ | ✅ | 仅 MCSM 面板提供 |
| §6.1 容器环境（Docker / Bastille 全功能） | ✅ | ❌ | irix-node 已实现（CLI 包装：Linux 为 Docker、FreeBSD 为 Bastille），客户端全功能模式即生效 |
| 节点级文件存储 / 递归快照 / 集群自组织 | ✅ 基础版 | ❌ | `docs/cluster-node-api.md` P0–P2 已实现基础版（P2 为内存态协调，未持久化） |

> 结论：MCSM 节点是多机模式中的「阉割」节点 —— 可托管实例、可作为迁移目标（先建实例再上传），
> 但无法参与节点间直传与自组织；`irix-node` 已补齐 `docs/cluster-node-api.md` 的 P0–P2（基础版），
> 可实现节点间直传与基础自组织（心跳/事件/任务均为内存态，重启后需重新登记）。
