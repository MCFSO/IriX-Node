# 集群节点 API 需求文档

> 目标读者：`irix-node`（Go 守护进程）实现者 / 后续对接多机管理模式的开发者。
>
> 本文档列出「多机管理模式」在**节点侧**所需的 HTTP API。这些 API 用于弥补当前协调器实现的两处缺口：
> 1. **文件接口全部绑定实例 uuid** —— 无法在目标节点尚不存在实例时，把数据同步到该节点（「同步到其它所有节点」只能退化为「协调器本地镜像 + 迁移时下发」）。
> 2. **缺少节点间自组织能力** —— 目前监控 / 资源分配 / 崩溃统计 / 数据同步全部由 IriX 桌面应用轮询完成，节点之间无法直接通信协调。

实现下述 API 后，多机模式可升级为**真正的节点间直传 + 自组织**。

> ⚠️ **适用范围（MCSM 阉割）**：MCSManager 面板**不支持节点与节点互联**，因此本文档所列「节点级文件存储（P0）」与「集群协调 / 自组织（P2）」**仅适用于 `irix-node`（`NodeType.node`）**。MCSM 节点只能由协调器（桌面应用）通过其现有实例级 API 单向轮询管理，无法作为「被其它节点主动写入」的同步目标，也无法参与自组织。下表为能力差异矩阵（详见第 5 节）。

---

## 1. 约定

与现有节点 API（`lib/services/node_api_client.dart`）保持一致：

| 项 | 约定 |
|----|------|
| 基础地址 | `http://<host>:<port>` |
| 认证 | `apikey` 查询参数（本地节点可省略）；请求头 `X-Requested-With: XMLHttpRequest`（MCSM 必需，irix-node 建议兼容） |
| 请求体 | `application/json; charset=utf-8` |
| 响应体 | 统一 `{ "status": 200, "data": <payload>, "time": <unix_ms> }` |
| 错误 | `status != 200` 时 `data` 为错误消息字符串 |
| 时间 | Unix 秒或毫秒均可，字段名自说明（下文中 `mtime` 为文件修改时间字符串，`sha256` 为十六进制摘要） |
| 分页 | 沿用 `page` / `page_size` 查询参数 |

---

## 2. 现有可用接口（已实现，无需新增）

以下能力当前节点已通过同一风格 API 暴露，本文档**不再重复定义**：

- 资源信息：`GET /api/overview`（`system.cpuUsage` / `system.memUsage` / `system.totalmem` / `system.freemem` / `remote` 守护进程列表）
- 实例管理：`GET /api/service/remote_service_instances`、`GET /api/instance`、`POST /api/instance`、`PUT /api/instance`、`DELETE /api/instance`
- 实例操作：`GET /api/protected_instance/{open|stop|restart|kill}`、`command`、`outputlog`
- 实例级文件：`GET /api/files/list`、`PUT /api/files/`（读/写）、`DELETE /api/files`、`PUT /api/files/move`、`POST /api/files/copy`、`POST /api/files/compress`（含解压 type=2）、`POST /api/files/mkdir`、`POST /api/files/touch`
- 直连传输：`POST /api/files/download`（取下载票据）→ `GET /download/{password}/{fileName}`；`POST /api/files/upload`（取上传票据）→ `POST /upload/{password}`

---

## 3. 新增 API

按优先级分为三档：**P0（必须，弥补硬缺口）**、**P1（增量同步效率）**、**P2（自组织 / 去协调器）**。

### 3.1 P0 —— 节点级文件存储（仅 irix-node）

**用途**：让协调器（或对等节点）在不依赖具体实例的情况下，向某节点写入 / 读取一段「同步数据」。这是「同步到所有其它节点」的前提。

> MCSM 无此能力：它只暴露实例级文件接口，无法在没有实例时接收同步数据。因此对 MCSM 节点，「同步到该节点」只能通过「先在 MCSM 上创建实例 → 实例级上传」实现，且无法作为被动同步目标。

路径约定：节点级文件统一存放在节点本地一个**同步区**（staging root）下，路径形如 `/mirrors/<instanceId>/...`。`<instanceId>` 为集群实例的全局唯一 id（由协调器生成，贯穿所有节点）。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/cluster/files/list?path=/mirrors/x&page=1&page_size=100` | 列出同步区某目录下的文件 |
| `POST` | `/api/cluster/files/mkdir` | 创建目录 |
| `DELETE` | `/api/cluster/files` | 删除文件/目录 |
| `POST` | `/api/cluster/files/download` | 申请下载票据 |
| `POST` | `/api/cluster/files/upload` | 申请上传票据 |

直连传输复用现有 `GET /download/{password}/{fileName}` 与 `POST /upload/{password}`（票据的 `addr` 指向节点自身）。

**文件条目**（list 的 `items` 元素）—— 在现有 `RemoteFileEntry` 基础上增加 `sha256` 与 `mtime`：

```json
{
  "name": "session.lock",
  "size": 4,
  "mtime": "2026-08-13 10:00:00",
  "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "type": 1
}
```

> `type`: `0` = 目录，`1` = 文件。`sha256` 用于可靠的增量判定（比 `size+mtime` 更稳，避免 mtime 漂移漏同步）。

**请求 / 响应示例**

`POST /api/cluster/files/upload`
```json
// 请求体
{ "upload_dir": "/mirrors/i-abcd/world" }
// 响应 data
{ "password": "ticket-1", "addr": "http://192.168.1.5:12346", "upload_dir": "/mirrors/i-abcd/world" }
```

`GET /api/cluster/files/list?path=/mirrors/i-abcd/world`
```json
// 响应 data
{
  "items": [ { "name": "level.dat", "size": 1024, "mtime": "2026-08-13 09:00:00", "sha256": "…", "type": 1 } ],
  "total": 1,
  "absolutePath": "/mirrors/i-abcd/world"
}
```

---

### 3.2 P1 —— 增量同步原语（irix-node 全量；MCSM 部分）

**用途**：现在增量同步需要递归调用 `GET /api/files/list` 逐层枚举（N 次往返）。新增**单次递归快照**接口，一次性返回整个目录树的扁平文件清单（含 `sha256`/`mtime`），协调器据此与本地清单比对，只下载变化文件。

> MCSM 侧：`sync/list` 与 `snapshot/restore` 不可用，`sha256` 字段也不会有。对 MCSM 节点只能沿用现有 `GET /api/files/list`（逐层、`size+mtime` 判定）做增量，精确度略降。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/instance/sync/list?uuid=<u>&daemonId=<d>` | 递归枚举**实例**整个工作目录（单次） |
| `GET` | `/api/cluster/sync/list?path=/mirrors/x` | 递归枚举**同步区**某目录（单次） |

两者响应结构一致：

```json
{
  "items": [
    { "path": "/world/level.dat", "size": 1024, "mtime": "…", "sha256": "…", "type": 1 },
    { "path": "/world/region", "size": 0, "mtime": "…", "sha256": "", "type": 0 }
  ],
  "total": 2,
  "root": "/"
}
```

> `path` 为相对根目录的绝对路径（`/` 分隔）。目录项可省略 `sha256`。

**快照 / 恢复**（整目录搬运的便捷封装，替代「手动 compress→download→upload→unzip」的多步流程）：

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/instance/snapshot` | 服务端把实例工作目录打成归档，返回下载票据 |
| `POST` | `/api/instance/restore` | 服务端接收归档并解压到实例工作目录 |

```json
// POST /api/instance/snapshot  请求体
{ "uuid": "…", "daemonId": "…" }
// 响应 data（直接可下载）
{ "password": "ticket-snap", "addr": "http://…", "fileName": "snapshot.zip" }
```

---

### 3.3 P2 —— 集群协调 / 自组织（仅 irix-node）

**用途**：把「监控 / 分配 / 崩溃统计 / 同步」从桌面应用下放到节点，实现你最初设想的「节点之间自组织」。**这一档是可选的演进目标**——在 irix-node 实现前，协调器继续用轮询方式工作。

> MCSM 完全不可用：它无法与其它节点通信，只能作为被协调器单向管理的工作节点。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/cluster/status` | 集群状态：当前监控节点、对等节点列表、自身角色 |
| `POST` | `/api/cluster/heartbeat` | 向监控节点上报资源快照 + 运行实例 + 待处理事件 |
| `POST` | `/api/cluster/events` | 上报事件（实例崩溃 / 节点资源不足 / 同步完成） |
| `GET` | `/api/cluster/peers` | 获取已登记的对等节点列表 |
| `POST` | `/api/cluster/transfer` | 指示节点从对等节点**拉取**某实例数据（节点间直传，协调器不代理字节） |

**`GET /api/cluster/status`** 响应：

```json
{
  "monitorNodeId": "n-monitor",
  "role": "monitor",            // monitor | worker
  "peers": [ { "id": "n-1", "address": "http://192.168.1.6:12346", "available": true } ],
  "self": { "id": "n-2", "address": "http://192.168.1.5:12346" }
}
```

**`POST /api/cluster/heartbeat`** 请求体（worker → monitor，周期约 15s）：

```json
{
  "resource": { "cpuUsage": 0.4, "memUsage": 0.62, "totalmem": 17179869184, "freemem": 6528475136 },
  "instances": [ { "uuid": "…", "status": 3 } ],
  "events": [ { "type": "crash", "instanceUuid": "…", "count": 2 } ]
}
```

**`POST /api/cluster/events`** 事件类型：

| type | 含义 | 附加字段 |
|------|------|----------|
| `crash` | 实例非人为崩溃 | `instanceUuid`, `count` |
| `resource_pressure` | 节点资源不足 | `memUsage`, `threshold` |
| `sync_done` | 实例数据已同步到本节点 | `instanceId`, `bytes`, `files` |
| `migrated` | 实例已迁移至本节点 | `instanceId`, `fromNodeId` |

**`POST /api/cluster/transfer`**（节点间直传）：

```json
// 请求体：指示本节点从 source 拉取实例数据到本节点同步区
{
  "instanceId": "i-abcd",
  "source": { "address": "http://192.168.1.6:12346", "apikey": "", "uuid": "…", "daemonId": "…" },
  "dest": "/mirrors/i-abcd"
}
// 响应 data
{ "jobId": "job-1" }
```

> 轮询 `GET /api/cluster/transfer?jobId=job-1` 返回 `{ "status": "running|done|failed", "bytes": 12345 }`。

---

## 4. 与现有接口的关系

- **P0 节点级文件** 是 P1 增量同步、P2 直传的**底座**；实现时建议复用节点内部已有的文件读写 / 票据逻辑，仅把作用域从「实例 cwd」扩展到「节点同步区」。
- `sha256` / `mtime` 字段应**同时**补充到现有实例级 `GET /api/files/list` 的条目中，保证旧路径与快照路径的增量判定一致。
- 认证、票据、直连上传下载（`/download/*`、`/upload/*`）**无需新增**，直接复用。

## 5. 能力差异矩阵 + 落地优先级建议

### 5.1 能力差异矩阵（MCSM 阉割）

| 能力 | irix-node | MCSM | 说明 |
|------|-----------|------|------|
| 资源上报 `/api/overview` | ✅ | ✅ | 双方均可用，协调器轮询监控 |
| 实例 CRUD / 启停 / 实例级文件 | ✅ | ✅ | 双方均可用（现有接口） |
| 作为迁移目标（先建实例→上传） | ✅ | ✅ | MCSM 也能接收迁移，但需「先建实例再实例级上传」 |
| **节点级文件存储（P0）** | ✅ | ❌ | 无实例时无法写入；MCSM 不能作为被动同步目标 |
| **增量快照 `sha256`/`sync/list`（P1）** | ✅ | ❌ | MCSM 回退逐层 `size+mtime` |
| **集群协调 / 自组织（P2）** | ✅ | ❌ | MCSM 无法与其它节点通信 |

**结论**：MCSM 节点在多机模式下是「阉割」的——只能作为被协调器单向管理的工作节点（可托管实例、可被迁移目标），但无法参与节点间直传与自组织。「同步到其它所有节点」对 MCSM 而言，只能退化为协调器本地镜像（当前实现），或迁入时先建实例再上传。

### 5.2 落地优先级建议

| 档位 | 内容 | 适用节点 | 收益 |
|------|------|----------|------|
| **P0** | 3.1 节点级文件存储（含 `sha256`） | 仅 irix-node | 实现「同步到所有其它 irix-node 节点」，消除协调器本地镜像这一约束 |
| **P1** | 3.2 递归快照 + 快照/恢复 | 仅 irix-node | 增量同步从 O(目录数) 往返降到 O(1)，整目录迁移更稳 |
| **P2** | 3.3 集群协调 | 仅 irix-node | 实现节点间自组织，摆脱对桌面应用常驻的依赖（高可用 / 协调器可离线） |

> 说明：上述 P0–P2 已由 `irix-node` 实现基础版（见 `cluster.go`，同步区为 `{data}/mirrors`）。
> 当前边界：P2 的心跳 / 事件 / 对等节点均为内存态，守护进程重启后需重新登记；
> 快照与拉取任务的进度查询保留最近任务，不持久化。MCSM 侧受其「无法节点互联」限制，本文档不为其规划新增 API。
