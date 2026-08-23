# irix-node 本地开服能力对齐（设计文档）

> 目标读者：`irix-node`（Go 守护进程）实现者 / IriX 客户端（Flutter）开发者。
>
> **目标**：让「节点上的实例管理」拥有与「本地实例管理」**完全一致**的开服能力 ——
> 用户在 IriX 中管理某个节点实例时，能获得与本地实例相同的完整体验：
> 创建/导入、启停控制、实时彩色控制台、配置编辑、插件/Mod 管理、文件管理、
> 备份恢复、回收站、核心安装、JDK 管理，直至内网穿透与 AI 辅助。
>
> 本文档是**能力对齐的增量设计**：已有接口（见 §2）不重复定义，只列出
> 「本地有、节点没有」的差距及对应的节点端 API / 客户端适配方案。
> 与 `docs/cluster-node-api.md`（多机模式节点级接口）、`docs/irix-node-container-api.md`
> （容器契约）配套阅读，三者构成 irix-node 的完整接口契约。

---

## 1. 本地开服能力盘点（现状）

以下为 IriX 桌面客户端「本地实例」已具备的全部能力，以及对应的客户端实现位置
（`irix-node` 需要对齐的目标清单）：

| # | 能力模块 | 本地实现（客户端） | 说明 |
|---|---------|------------------|------|
| 1 | 实例生命周期 | `AppState` / `ServerProcessManager` / `instance_store.dart` | 创建、导入目录、删除、启动/停止/重启/强制终止 |
| 2 | 实时控制台 | `instance_detail_screen.dart` + `server_process.dart` | stdout/stderr 实时流、stdin 命令、**ANSI 彩色渲染**（`ansi_color.dart`） |
| 3 | 日志持久化 | `LogPersistence` + Rust `logger` crate | 每实例日志文件、tail 读取（供 AI 上下文） |
| 4 | 配置编辑 | `ConfigService` / `ConfigEditorScreen` / `TextEditorDialog` | yml/yaml/properties/json/toml 解析与写回、语法高亮、**配置注解**（`data/config_descriptions.dart`） |
| 5 | 插件/Mod 管理 | `PluginsTab` + `JarMetadataService` | 扫描 plugins/mods 目录，解析 jar 元数据（`plugin.yml` / `paper-plugin.yml` / `fabric.mod.json` / `mods.toml`，含图标、描述），卡片展示，配置目录跳转 |
| 6 | 文件管理 | `FileManagerScreen` + Rust `file_ops` | 浏览/上传/下载/编辑/新建/移动/复制/重命名/压缩/解压 |
| 7 | 回收站 | `TrashStore` + `TrashView` + Rust `file_ops` | 删除进回收站、恢复、永久删除、清空（每实例 `xmc_trash/`） |
| 8 | 备份/恢复 | `BackupService`（Rust ZIP 并行）+ `BackupSettings` | 选择文件备份、压缩级别（每实例）、进度与取消、恢复 |
| 9 | 服务端核心安装 | `DownloadCoreScreen` + `MslApiService` | FastMirror / MSL 源下载核心 jar，下载后自动创建实例 |
| 10 | 市场安装 | `mod_detail_screen.dart` / `hangar_detail_screen.dart` | Modrinth / Hangar 下载（哈希校验）→ 装到实例 mods/ 或 plugins/（本地与节点均已支持，见 §2 注） |
| 11 | JDK 管理 | `JdkInstaller` | Java 版本检测、下载安装、版本目录管理、启动命令内引用 |
| 12 | 容器管理 | `lib/services/container/` + `ContainerEnvironmentPanel` | Docker 全功能（镜像/容器/网络/卷）——**节点侧已有** `/api/container/*` |
| 13 | 内网穿透 | `FrpcManager` + `frp_screen.dart` | OpenFrp / SakuraFrp / 自建 frps 隧道（frpc 二进制管理、配置下发） |
| 14 | AI 助手 | `AiChatPanel` + `knowledge_service.dart` | 读实例日志（tail）作为上下文、RAG 知识库问答、MCP 工具控制 |
| 15 | 资源监控 | 节点概览 + 集群监控（`cluster_monitor.dart`） | CPU/内存/网络历史采样（节点级已有；**实例级缺失**） |

## 2. irix-node 已有接口（无需新增，本档不再重复）

以下能力节点已通过同风格 API 暴露（见 `lib/services/node_api_client.dart`），
本地能力的子集已对齐：

- 资源信息：`GET /api/overview`
- 实例 CRUD / 启停 / 命令 / 输出轮询：`/api/instance`、`/api/protected_instance/*`
- 实例级文件：`/api/files/list|mkdir|touch|move|copy|compress|delete`、`PUT /api/files/`（读写）
- 实例插件/Mod 元数据：`GET /api/instance/plugins`（§4.4；客户端「远程实例 → 插件/Mod」Tab 已实现，节点不支持时回退文件列表）
- 直连传输：`POST /api/files/download|upload` → `GET /download/...`、`POST /upload/...`
- 容器环境：`/api/container/*`、`/api/image/*`、`/api/volume/*`、`/api/network/*`（Docker，见容器对接文档）
- Bastille（FreeBSD）：`/api/bastille/*`
- 节点级归档：`POST /api/container/archive`（压缩节点任意路径）/ `nodeArchiveDownload` / `nodeArchiveRestore`
- 规划中：`POST /api/instance/snapshot|restore`、`GET /api/instance/sync/list`（见 `cluster-node-api.md` §3.2）

> **市场安装（能力 #10）已对齐**：客户端通过「下载到临时目录 → 上传票据 →
> 直连上传到实例 mods/plugins」实现节点安装（`install_target_picker.dart`），
> 两种节点类型通用，无需新增节点 API。

## 3. 差距矩阵（本地有 / 节点缺）

| 能力 | 本地 | irix-node 现状 | 差距 | 方案优先级 |
|------|------|---------------|------|-----------|
| 实时控制台 | 进程流实时 | `outputlog` **轮询**（2s） | 延迟高、命令与日志分离、无 ANSI 保真 | **P0** §4.1 |
| 实例日志持久化 | Rust logger 文件 + tail | 无（仅内存环形） | 重启丢日志、AI 无上下文 | **P0** §4.1 |
| Java 检测/安装 | `JdkInstaller` | 无 | 节点上无法确认 java 版本/路径 | **P0** §4.2 |
| 服务端核心安装 | 客户端下载后建实例 | 无 | 核心需经客户端中转，大文件浪费带宽 | **P0** §4.2 |
| 导入目录建实例 | 扫描本地目录 | 无 | 节点上只能手工创建空实例 | **P0** §4.2 |
| 实例级指标 | 本机进程可读 | 无 | 无法看节点实例的 CPU/内存/TPS/玩家 | **P1** §4.3 |
| 插件/Mod 元数据 | jar 本地解析 | 无 | 远程插件页只有文件名，无图标/描述 | **P1** §4.4 |
| 备份/恢复 | 本地 ZIP | 无（归档 API 面向容器路径） | 实例备份需走「手动压缩+下载」 | **P1** §4.5 |
| 回收站 | `xmc_trash/` + SQLite | 删除即永久 | 误删不可恢复 | **P1** §4.6 |
| 内网穿透 | 本地 frpc | 无 | 隧道只能在客户端本机跑，无法为节点实例建隧道 | **P2** §4.7 |
| AI 上下文 | 日志 tail + RAG | 日志接口缺失 | AI 无法分析节点实例 | **P2** §4.8 |
| 配置注解 | 客户端内置 | 文件读写已有 | **无需新 API**：配置编辑走实例级文件 API 即可，注解数据保留在客户端 | 客户端适配 §5 |
| 容器管理 | 本机 Docker | 已有 `/api/container/*` | — | 已对齐 |

## 4. 新增 API 设计

通用约定与现有接口一致（见 `cluster-node-api.md` §1 与 `irix-node-container-api.md` §1）：
`{status, data, time}` 响应、`X-Requested-With: XMLHttpRequest`、`apikey` 查询参数、
错误时 `data` 为可读字符串。**所有耗时操作必须异步任务化**（返回 `jobId`，
客户端轮询进度），不允许同步阻塞节点 HTTP 请求（如大文件压缩/核心下载）。

### 4.1 P0 —— 实时控制台与日志（开服体验的核心差距）

#### 4.1.1 实时日志流（WebSocket）

```
WS  /api/instance/console/ws?uuid=<u>&daemonId=<d>&apikey=<key>
```

- **服务端 → 客户端**：文本帧，一行一条服务器原始输出（**保留 ANSI 转义序列**，
  颜色由客户端 `ansi_color.dart` 渲染；节点不得剥离）。
- **客户端 → 服务端**：文本帧为控制台命令（等效现有 `POST /api/protected_instance/command`）。
- 心跳：客户端每 30s 发送 `ping` 文本帧；节点 90s 未收到则断开。
- 断线重连：客户端自动重连并请求**补发断线期间的增量日志**（见 4.1.2，参数
  `since=<unix_ms>` 或行号）。

> 兼容性：不支持 WebSocket 的旧节点继续走 `outputlog` 轮询 + `command`（客户端
> 能力探测：`WS` 握手失败即回退，不报错）。

#### 4.1.2 实例日志持久化与查询

节点为每个实例维护日志文件（默认 `<data>/logs/<uuid>.log`），写入规则：
按行追加，**保留 ANSI**（供回放）；**轮转**：单文件超过 10 MB 或每 7 天轮转
（`<uuid>.log.1` … `.5`，最多保留 5 份），与本地 Rust logger 行为对齐。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/instance/logs?uuid=<u>&daemonId=<d>&tail=<n>&since=<unix_ms>` | 历史日志：`tail` 返回最后 N 行；`since` 返回该时间点之后的行（重连补发用）。响应 `data` 为字符串 |
| `DELETE` | `/api/instance/logs?uuid=<u>&daemonId=<d>` | 清空日志文件（客户端「清空日志」按钮） |

### 4.2 P0 —— 开服基础（Java / 核心 / 导入）

#### 4.2.1 Java 运行时检测

```
GET /api/runtime/java
```

```json
// 响应 data
{
  "default": { "path": "/usr/lib/jvm/java-21/bin/java", "version": "21.0.4", "vendor": "Eclipse Adoptium", "major": 21, "available": true },
  "all": [
    { "path": "/usr/lib/jvm/java-21/bin/java", "version": "21.0.4", "vendor": "Eclipse Adoptium", "major": 21, "available": true },
    { "path": "/usr/lib/jvm/java-17/bin/java", "version": "17.0.12", "vendor": "Eclipse Adoptium", "major": 17, "available": false }
  ]
}
```

- `available`：该路径可执行（`-version` 可运行）且版本解析成功。
- 客户端在「节点实例 → 设置」中展示 Java 列表，供填写启动命令时选择。

#### 4.2.2 节点端安装 JDK（任务化）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/runtime/java/install` | 请求体 `{ "major": 21 }`；节点端下载 Adoptium API 提供的 JDK（**节点直连下载**，客户端不中转字节），安装到 `<data>/jdk/jdk-21/`；返回 `{ "jobId": "j-1" }` |
| `GET` | `/api/runtime/java/install-progress?jobId=j-1` | 返回 `{ "status": "running\|done\|failed", "percent": 0.42, "message": "…", "path": "/data/jdk/jdk-21/bin/java" }` |
| `DELETE` | `/api/runtime/java?major=21` | 卸载指定版本 JDK |

> 客户端「节点实例 → 设置」：Java 检测列表 + 「安装 JDK 21」按钮（进度条）。

#### 4.2.3 节点端下载服务端核心（任务化）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/instance/download-core` | 请求体 `{ "uuid": "…", "daemonId": "…", "url": "https://…jar", "fileName": "server.jar", "sha512": "…" }`；**节点端**用直连 URL 下载到实例根目录（校验哈希，H-1 同款）；返回 `{ "jobId": "j-1" }` |
| `GET` | `/api/instance/download-core-progress?jobId=j-1` | `{ "status": "…", "percent": 0.5, "path": "<cwd>/server.jar" }` |

> 客户端「节点实例 → 设置」：填入核心 URL（或从市场/核心库复制直链）→ 节点端下载，
> 进度显示在客户端；完成后自动写入启动命令。

#### 4.2.4 导入目录创建实例

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/instance/import` | 请求体 `{ "daemonId": "…", "path": "/srv/mc/survival", "nickname": "生存服" }`。节点校验目录存在 → 扫描内容（存在 `server.jar`/`*.jar` 或 `eula.txt` 等特征即判定为可导入）→ 自动创建实例（cwd=该目录，启动命令留空由用户在配置页填写）；返回 `{ "instanceUuid": "…" }` |

> 客户端「节点实例列表」增加「导入目录」按钮，与本地「导入实例」入口对齐
> （本地实现见 `import_instance_screen.dart`）。

### 4.3 P1 —— 实例级指标

```
GET /api/instance/stats?uuid=<u>&daemonId=<d>
```

```json
// 响应 data（进程级，Linux 读 /proc/<pid>/stat + /proc/net，FreeBSD 用 rctl 等效）
{
  "pid": 1234,
  "cpuPercent": 12.5,
  "memoryMb": 2048,
  "networkDownloadBps": 102400,
  "networkUploadBps": 51200,
  "players": 3,
  "maxPlayers": 20,
  "tps": 19.8,
  "uptimeSec": 86400
}
```

- `players/maxPlayers/tps`：解析服务器输出中的周期性数据（`playerlist` 或
  spigot 心跳日志）；解析失败时省略该字段，客户端显示「—」。
- 客户端：远程实例详情「总览」头部展示实时指标（复用 `network_line_chart.dart` 组件）。

### 4.4 P1 —— 插件/Mod 元数据

```
GET /api/instance/plugins?uuid=<u>&daemonId=<d>
```

```json
// 响应 data（插件与 Mod 合并，type 区分）
{
  "items": [
    {
      "type": "plugin",
      "path": "/plugins/EssentialsX-2.20.1.jar",
      "fileName": "EssentialsX-2.20.1.jar",
      "size": 1048576,
      "name": "EssentialsX",
      "description": "Essential server commands",
      "version": "2.20.1",
      "iconBase64": "iVBORw0KGgo…",
      "configDir": "/plugins/Essentials"
    },
    { "type": "mod", "path": "/mods/1.20.1/sodium-0.5.8.jar", "fileName": "sodium-0.5.8.jar", "size": 2621440, "name": "Sodium", "description": "Rendering engine", "version": "0.5.8", "iconBase64": "…", "configDir": "/config/sodium" }
  ]
}
```

- 节点端解析 jar 内 `plugin.yml` / `paper-plugin.yml` / `fabric.mod.json` /
  `META-INF/mods.toml`（解析逻辑对齐客户端 `jar_metadata_service.dart`），
  **mods 目录递归**（版本子目录），`iconBase64` 为图标文件 base64（PNG）。
- `configDir`：按元数据 name / 文件名匹配的配置目录（可空）。
- 上传/删除/下载沿用现有文件 API（客户端远程插件 Tab 已实现），此 API 只补元数据。

### 4.5 P1 —— 实例备份 / 恢复（细化 cluster-node-api.md §3.2 快照到实例场景）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/instance/snapshot` | 请求体 `{ "uuid": "…", "daemonId": "…" }`；节点端把实例 cwd 打成 zip（**排除** `.irix-trash/` 与日志/临时文件），存入节点备份区 `<data>/backups/<uuid>/<ts>.zip`；返回 `{ "jobId": "j-1" }` |
| `GET` | `/api/instance/snapshot-progress?jobId=j-1` | `{ "status": "…", "percent": 0.6, "archivePath": "/data/backups/<uuid>/…zip" }` |
| `POST` | `/api/instance/restore` | 请求体 `{ "uuid": "…", "daemonId": "…", "archivePath": "/data/backups/<uuid>/…zip" }`；解压覆盖实例 cwd（**先自动停止实例**，恢复后保持停止）；返回 jobId |
| `GET` | `/api/instance/backups?uuid=<u>&daemonId=<d>` | 列出节点端备份：`{ "items": [ { "fileName": "2026-08-20-10-00-00.zip", "size": 10485760, "mtime": "…", "path": "/data/backups/<uuid>/…zip" } ] }` |
| `DELETE` | `/api/instance/backups?uuid=<u>&daemonId=<d>` | 请求体 `{ "paths": ["…"] }`，删除指定备份 |

- 客户端远程备份 Tab 升级：优先走 snapshot（进度条、断点由 jobId 恢复），
  snapshot 不可用（MCSM）时回退现有「compress → 下载」路径。
- 下载备份到本地：沿用 `downloadTicket` + 直连下载（票据 `addr` 指向节点）。

### 4.6 P1 —— 实例级回收站

与本地 `TrashStore` 语义对齐：删除 → 移入实例内 `<cwd>/.irix-trash/<id>-<name>`，
恢复 / 永久删除 / 清空。元数据（原始路径、删除时间）由**节点**维护
（`<data>/trash/<uuid>.json`），客户端只做展示。

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/files/trash` | 请求体 `{ "uuid": "…", "daemonId": "…", "targets": ["/world/region/r.0.0.mca"] }`；移动到回收站 |
| `GET` | `/api/files/trash/list?uuid=<u>&daemonId=<d>` | `{ "items": [ { "id": "…", "name": "…", "originalPath": "/world/region/r.0.0.mca", "trashPath": "/.irix-trash/<id>-…", "size": 123, "deletedAt": "…" } ] }` |
| `POST` | `/api/files/trash/restore` | 请求体 `{ "uuid": "…", "daemonId": "…", "ids": ["…"] }`（冲突时自动改名 `<name> (1)`） |
| `POST` | `/api/files/trash/empty` | 请求体同上；永久删除并清理元数据 |

> 客户端：远程文件管理器增加「删除到回收站」与回收站视图（对齐本地
> `trash_view.dart`）；节点不支持时（MCSM）回退为原删除确认。

### 4.7 P2 —— 节点端内网穿透（FRP）

隧道在**节点**上运行（frpc 由节点管理），客户端只下发配置与查看状态——
这样隧道出口与实例同机，本地开服的 FRP 体验（`frp_screen.dart`）可完整迁移到节点。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/frp/status` | 节点 frpc 二进制状态、版本、运行中隧道列表 |
| `POST` | `/api/frp/tunnels` | 请求体 `{ "name": "mc-生存服", "provider": "openfrp|sakura|self", "config": {…} }`（自建场景为完整 frpc toml）；节点写入配置并启动，返回 `{ "tunnelId": "…" }` |
| `POST` | `/api/frp/tunnels/{id}/start|stop` | 启停单隧道 |
| `DELETE` | `/api/frp/tunnels/{id}` | 删除隧道（停止 + 删配置） |
| `GET` | `/api/frp/tunnels/{id}/logs?tail=100` | 隧道运行日志（排障） |

> 客户端：多机模式下 FRP 页提供「本地隧道 / 节点隧道」分组（对齐
> `FrpcManager` 的 OpenFrp / SakuraFrp / 自建三种 provider）。

### 4.8 P2 —— AI 上下文与实例监控历史

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/instance/logs/query?uuid=<u>&daemonId=<d>&keyword=<s>&level=warn&windowMin=30&maxLines=200` | 结构化日志查询（供 AI 助手分析节点实例日志；未实现结构化解析时可先退化为 `tail` 全文） |
| `GET` | `/api/instance/metrics?uuid=<u>&daemonId=<d>&minutes=15` | 历史采样 `{ "samples": [ { "time": "…", "cpu": 0.1, "memoryMb": 2048, "downloadBps": 0, "uploadBps": 0 } ] }`（节点每 15s 采样一次，环形保留 60 条） |

---

## 5. 客户端适配点（Dart 侧改动清单）

| 文件 | 改动 |
|------|------|
| `lib/services/node_api_client.dart` | 新增 §4 全部 API 方法（`runtimeJava` / `installJdk` / `downloadCore` / `importInstance` / `instanceStats` / `pluginsMeta` / `snapshot` / `restore` / `backups` / `trash*` / `frpTunnels*` / `logsQuery` / `metrics` / WS 连接管理） |
| `lib/screens/remote_instance_detail_screen.dart` | 控制台改 WebSocket 实时流（保留轮询回退）；「总览」头部接入实例指标；新增「设置」Tab（Java 检测/安装、核心下载、导入） |
| `lib/screens/remote_plugins_backup_tabs.dart` | 插件区改用 §4.4 元数据（图标/描述）；备份区优先 §4.5 snapshot（回退现有路径） |
| `lib/screens/remote_file_manager_screen.dart` | 删除操作改为「进回收站」+ 回收站视图入口（§4.6） |
| `lib/screens/frp_screen.dart` | 多机模式下增加节点隧道分组（§4.7） |
| `lib/screens/ai_screen.dart` | 节点实例日志查询入口（§4.8） |
| `lib/utils/install_target_picker.dart` | 不变（市场安装已对齐） |

> 客户端能力探测原则：新 API 返回 404 / 非 200 时**自动降级**到旧路径并提示
> 「节点版本过旧」，不做硬失败。

## 6. 实现优先级与里程碑

| 里程碑 | 内容 | 验收 |
|--------|------|------|
| **M1（P0）开服基础** | §4.1 实时控制台 WS + 日志持久化/轮转；§4.2 Java 检测/安装、核心下载、导入目录 | 节点实例可完成「导入目录 → 装 JDK → 下核心 → 启动」全流程，控制台实时彩色输出，重启不丢日志 |
| **M2（P1）管理与数据** | §4.3 实例指标；§4.4 插件元数据；§4.5 备份/恢复；§4.6 回收站 | 节点实例管理界面与本地逐项对齐：插件卡片带图标、一键备份/恢复、删除可恢复 |
| **M3（P2）生态扩展** | §4.7 节点 FRP；§4.8 AI 日志/监控 | 多机模式 FRP 隧道、AI 可分析节点实例日志、实例级监控曲线 |

## 7. 验收标准（本地 ↔ 节点一致性清单）

在「同一台机器：本地实例 vs 同机 irix-node 实例」逐项对照：

1. **创建/导入**：本地「导入实例」与节点「导入目录」体验一致；核心安装流程等价。
2. **控制台**：输入命令、实时输出、ANSI 颜色、断线重连补日志，与本地无感知差异。
3. **配置**：远程配置页可编辑 server.properties 并写回，注解提示与本地一致。
4. **插件/Mod**：上传 jar 后卡片显示名称/图标/描述；删除、下载可用。
5. **文件**：浏览/上传/下载/编辑/压缩/解压一致；删除进回收站、可恢复、可清空。
6. **备份**：一键备份（进度/取消）、恢复（自动停服）、备份列表管理一致。
7. **FRP**：节点隧道与本地隧道同等可配、状态可见、日志可查。
8. **AI**：AI 助手能读取节点实例日志并给出分析。
9. **降级**：MCSM 节点上所有新能力均有优雅降级路径（提示或回退），不报错。

## 8. 与现有文档的关系

| 文档 | 关系 |
|------|------|
| `docs/cluster-node-api.md` | §3.2 的 `snapshot/restore` 为本档 §4.5 的上游设计；本档补充任务化进度与备份列表/删除 |
| `docs/irix-node-container-api.md` | 容器契约不变；本档不重复 |
| `docs/orchestration.md` | 编排迁移使用节点级归档（`/api/container/archive`），与本档实例级备份互补（归档面向容器路径，snapshot 面向实例） |
