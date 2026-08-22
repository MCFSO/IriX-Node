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
| 基础地址 | `http://<host>:<port>`（如 `http://127.0.0.1:12346` / `http://192.168.1.5:23333`）；远程节点应启用 HTTPS |
| 认证 | 请求头 `X-Api-Key: <key>`（**首选**，H-6：密钥不进 URL，避免进入代理/访问日志）；`apikey` 查询参数仅为 MCSM 面板兼容保留；请求头 `X-Requested-With: XMLHttpRequest`（MCSM 必需，irix-node 建议兼容） |
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
| `POST` | `/api/container/create` | body: `{name, image, command?, ports: [..], volumes: [..], env: {}, restartPolicy?, memoryLimitMb?, cpus?}` | 创建容器（不启动） |
| `POST` | `/api/container/{id}/start` `stop` `restart` `kill` | | 容器操作 |
| `DELETE` | `/api/container/{id}` | `force=1` | 删除容器 |
| `GET` | `/api/container/{id}/logs` | `tail=N` | 日志尾部 |
| `POST` | `/api/container/{id}/exec` | body: `{command}` | 容器内执行命令 |
| `GET` | `/api/container/{id}/stats` | | `{cpuPercent, memoryBytes, memoryLimitBytes, netRxBytes, netTxBytes}` |
| `GET` | `/api/image/list` | | 镜像列表，条目：`{id, tags: [..], sizeBytes, createdAt}` |
| `POST` | `/api/image/pull` | body: `{name}` | 拉取镜像 |
| `POST` | `/api/image/build` | body: `{dockerfile, name, tag}` | 构建镜像 → `{jobId}` |
| `GET` | `/api/image/build-progress` | `jobId` | `{status: building\|done\|failed, log: [..], image: "name:tag"}` |
| `DELETE` | `/api/image/{name}` | | 删除镜像 |
| `GET` | `/api/volume/list` / `DELETE /api/volume/{name}` | | 卷列表 / 删除 |
| `GET` | `/api/network/list` | | 网络列表，条目：`{name, driver, subnet?}` |

**Bastille（platform=freebsd）**：

| 方法 | 路径 | 参数 | 说明 |
|------|------|------|------|
| `GET` | `/api/bastille/releases` | | bootstrap 的发行版列表，条目：`{name, version, sizeBytes?, createdAt?}` |
| `POST` | `/api/bastille/bootstrap` | body: `{release}` | bootstrap 发行版 → `{jobId}` |
| `GET` | `/api/bastille/jails` | | jail 列表，条目：`{name, release, status, state?, ports: [..], createdAt?}` |
| `POST` | `/api/bastille/jails/create` | body: `{name, release, ip?, type: thin\|thick\|clone\|empty\|linux, vnet?, bridge?, mac?}` | 创建 jail |
| `POST` | `/api/bastille/jails/{name}/start` `stop` `restart` `destroy` | | jail 操作 |
| `GET` | `/api/bastille/jails/{name}/console` | `tail=N` | 日志尾部 |
| `POST` | `/api/bastille/jails/{name}/cmd` | body: `{command}` | jail 内执行命令（data 为输出文本） |
| `POST` | `/api/bastille/jails/{name}/pkg` | body: `{action, packages}` | 软件包管理（`bastille pkg`，安装 Java 环境等） |
| `GET` | `/api/bastille/jails/{name}/mounts` | | 挂载列表（nullfs/procfs，见对接文档 §4.10） |
| `POST` | `/api/bastille/jails/{name}/mounts` | body: `{src?, dst, fstype, options?}` | 添加挂载（nullfs→`bastille mount`；procfs→fstab+挂载） |
| `DELETE` | `/api/bastille/jails/{name}/mounts` | `dst=` | 卸载并移除 fstab 条目 |
| `POST` | `/api/bastille/jails/{name}/run` | body: `{command, cwd?, watch?}` | 后台运行会话（MC 服务端等长任务；`watch` 进程退出即停 jail）→ `{sessionId}` |
| `GET` | `/api/bastille/jails/{name}/run/{session}` | `tail=N&since=<偏移>` | 会话状态 `{running, exitCode, offset, log}` |
| `POST` | `/api/bastille/jails/{name}/run/{session}/stdin` | body: `{input}` | 会话 stdin（控制台命令） |
| `POST` | `/api/bastille/jails/{name}/run/{session}/stop` | | 终止会话进程 |
| `DELETE` | `/api/bastille/jails/{name}/run/{session}` | | 清理会话 |
| `GET` | `/api/bastille/jails/{name}/config` | | jail.conf 配置（`bastille config`） |
| `POST` | `/api/bastille/jails/{name}/config` | body: `{key, value}` | 设置配置项 |
| `DELETE` | `/api/bastille/jails/{name}/config` | `key=` | 删除配置项 |
| `GET` | `/api/bastille/templates` | | 模板列表（project/template 格式） |
| `POST` | `/api/bastille/templates/apply` | body: `{jail, template, args: {KEY=VALUE}}` | 应用模板 |
| `POST` / `DELETE` | `/api/bastille/rdr` | body: `{jail, proto, hostPort, jailPort}` | 端口转发 / 删除转发 |

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
| 本文档 §2–§4 全部端点 | ✅ | ✅ | 现有实现，双方均已支持 |
| §5 用户管理 / §6 Docker | ❌ | ✅ | 仅 MCSM 面板提供 |
| 节点级文件存储 / 递归快照 / 集群自组织 | 规划中 | ❌ | 见 `docs/cluster-node-api.md`（P0–P2） |

> 结论：MCSM 节点是多机模式中的「阉割」节点 —— 可托管实例、可作为迁移目标（先建实例再上传），
> 但无法参与节点间直传与自组织；`irix-node` 补齐 `docs/cluster-node-api.md` 的 P0–P2 后可实现真正的节点间直传 + 自组织。

---

## 9. 加密保险库 Vault（irix-node 全功能）

可选功能（`-vault` 开启，强制要求 TLS）。完整设计见 `docs/vault-design.md`；
客户端对接要点：**证书在客户端解析**（P12/GPG → 标准 PEM），私钥永不上送，
解锁时对挑战签名（RSA PKCS#1 v1.5 + SHA-256 / ECDSA ASN.1 DER，签名消息 =
前缀 + 挑战字符串，base64 无填充）。

### 9.1 会话与状态

| 端点 | 说明 |
|------|------|
| `GET /api/vault/status` | `{enabled, initialized, locked, user?, expiresIn?, passwordExpired?}` |
| `POST /api/vault/challenge` | `{purpose: "unlock"\|"cert-bind"}` → `{challengeId, challenge}`；一次性，5 分钟有效 |
| `POST /api/vault/unlock` | `{user, password, totp, challengeId, signature, newPassword?}` → `{sessionToken, expiresIn}` |
| `POST /api/vault/lock` | 立即锁定（需 `X-Vault-Token` 头） |
| `POST /api/vault/recovery` | `{recoveryToken, user?}` → 5 分钟恢复会话（改密/重绑 TOTP/换绑证书） |

### 9.2 初始化与用户

| 端点 | 说明 |
|------|------|
| `POST /api/vault/init` | `{user, password}` → `{initToken, totpSecret, otpauthURI, recoveryToken}`（仅未初始化时；recoveryToken 仅显示一次） |
| `POST /api/vault/totp/verify` | `{code}`（`X-Vault-Token: initToken` 或恢复/解锁会话） |
| `POST /api/vault/totp/reset` | 重绑 TOTP（恢复/解锁会话） |
| `POST /api/vault/cert` | `{certPem, challengeId, signature}`（initToken / 解锁 / 恢复会话；SPKI 指纹绑定） |
| `POST /api/vault/password` | `{oldPassword?, newPassword}`（解锁会话需旧密码；恢复会话免） |
| `POST /api/vault/user/add` / `remove` / `GET /api/vault/users` | 多用户管理（禁删最后一个用户） |

### 9.3 数据面

| 端点 | 说明 |
|------|------|
| `POST /api/vault/migrate` / `GET /api/vault/migrate/status` | 两阶段迁移（instances.json + vaultFiles 文件树；幂等续跑，`{phase, done, total, bytes}`） |
| `POST /api/vault/backup` | 加密备份包（zip：vault.json + 索引 + 对象，不含密钥材料） |

**数据面门禁**：vault 启用后，未初始化 → `403 vault not initialized`；锁定 →
`403 vault locked`；迁移中 → `403 vault migrating`。解锁后数据面请求须携带
`X-Vault-Token` 头（禁止 query string 传令牌）。`/api/overview`、`/api/load`
豁免（overview 锁定态脱敏）。

**实例文件区**：`vaultFiles: true` 的实例启停物化加密（停止时整树加密入库，
启动前物化）；已停止状态下文件 API 走加密层（列表/读写/删除/移动/复制/
建目录/建文件），直连下载/上传与压缩/解压暂不支持（返回明确错误）。

