# 实例核心 HTTP API 契约（IriX Node）

> 后端：`IriX-Node`（纯 Go 标准库，零依赖）。本文档面向 React 面板前端开发，
> 只依据 Go 源码推导，字段名与 JSON tag 一字不差。阅读本文件即可写出全部调用代码。

- 服务端路由注册位置：`IriX-Node/instance.go` 的 `RegisterRoutes`
- 数据模型：`IriX-Node/daemon.go`
- 状态常量：`StatusBusy=-1, StatusStopped=0, StatusStopping=1, StatusStarting=2, StatusRunning=3`

---

## 1. 通用约定

### 1.1 认证方式

每个 API 请求（除直连通道 `/download/`、`/upload/`）都需认证，任选其一：

| 方式 | 位置 | 说明 |
| --- | --- | --- |
| 查询参数 | `?apikey=<key>` | 最常用，WebSocket 也支持 |
| 请求头 | `X-Api-Key: <key>` | 与查询参数等效 |

服务端判定逻辑（`authOK`）：

1. 先取 `apikey` 查询参数，为空再取 `X-Api-Key` 头。
2. 若节点配置了 `-apikey`，则必须与该值完全相等。
3. 若未配置 `-apikey`，则校验配对码（首次启动生成、仅显示一次的 20 位码的 SHA-256 哈希）。

认证失败统一返回 **HTTP 403**，`data` 为错误字符串：

```json
{ "status": 403, "data": "API 密钥无效", "time": 1710000000000 }
```

### 1.2 响应包装格式（所有 JSON 接口统一）

```json
{
  "status": 200,
  "data": <任意值>,
  "time": 1710000000000
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | int | 与真实 HTTP 状态码一致（成功 200） |
| `data` | any | 业务数据；成功时为下方各端点定义的 JSON 结构 |
| `time` | int64 | 服务端响应时间，unix 毫秒 |

### 1.3 错误响应

`status != 200` 时，`data` 为**中文错误消息字符串**（不是对象）：

```json
{ "status": 400, "data": "实例不存在", "time": 1710000000000 }
```

常见状态码：

| 状态码 | 场景 |
| --- | --- |
| 400 | 参数缺失/非法、实例不存在、实例未运行、请求体格式错误、目录校验失败 |
| 403 | 认证失败（`API 密钥无效`） |
| 500 | 内部错误（保存失败、启动失败、终止进程失败、读日志失败等） |

> 注意：部分业务错误（如 `启动失败`、`实例正在执行其他操作`）返回 **500**，前端需以
> `data` 字符串内容为准提示用户，不能只按状态码判断。

### 1.4 时间戳与通用参数

- 所有时间戳均为 **unix 毫秒（int64）**：`createDatetime`、`lastDatetime`、`endTime`、
  `since`、响应包装的 `time` 等。
- `daemonId`：MCSM 兼容参数，本节点为单节点部署，**接受但忽略**，可传任意值（通常传 `""` 或节点 UUID）。

---

## 2. 数据模型

### 2.1 InstanceConfig（创建/编辑实例表单依据）

Go 结构：`daemon.go` 的 `InstanceConfig`。**注意**：本节点实现是 MCSM 字段的**子集**，
**不含** `TerminalOption`、`Docker`/容器相关字段——表单不要发送这些字段（会被忽略）。

| JSON 字段 | 类型 | 必填 | 默认值（FillDefaults） | 含义 |
| --- | --- | --- | --- | --- |
| `nickname` | string | 否 | `""` | 实例显示名（列表/概览按此名过滤） |
| `startCommand` | string | 否 | `""` | 启动命令（按空格/引号拆分为 argv，见 `SplitCommand`） |
| `stopCommand` | string | 否 | `"stop"` | 停止命令（停止时写入进程 stdin） |
| `cwd` | string | **是** | 无 | 工作目录；创建/更新/导入时经 `normalizeCwd` 校验并转为绝对路径；空、根目录、磁盘根、系统目录会被 400 拒绝 |
| `ie` | string | 否 | `"utf-8"` | 输入编码（MCSM 对齐，节点当前仅存储） |
| `oe` | string | 否 | `"utf-8"` | 输出编码（MCSM 对齐，节点当前仅存储） |
| `createDatetime` | int64 | 否 | 服务端写入 | 创建时间 unix 毫秒（服务端强制覆盖为当前时间） |
| `lastDatetime` | int64 | 否 | 服务端写入 | 最后操作时间 unix 毫秒 |
| `type` | string | 否 | `"universal"` | 实例类型 |
| `tag` | []string | 否 | `[]` | 标签数组 |
| `endTime` | int64 | 否 | `now+365天`（毫秒） | 到期时间 unix 毫秒 |
| `fileCode` | string | 否 | `"utf-8"` | 文件编码（MCSM 对齐，节点当前仅存储） |
| `processType` | string | 否 | `"universal"` | 进程类型（MCSM 对齐，节点当前仅存储） |
| `updateCommand` | string | 否 | `""` | 更新命令（MCSM 对齐，节点当前仅存储） |
| `actionCommandList` | []string | 否 | `[]` | 动作命令列表（MCSM 对齐，节点当前仅存储） |
| `crlf` | int | 否 | `2` | 换行模式（MCSM 约定 0=LF/1=CRLF/2=自动）；**注意：显式传 0 也会被覆盖为 2** |
| `eventTask` | object | 否 | 见下 | 事件任务配置 |
| `pingConfig` | object | 否 | 见下 | 状态探测配置（节点当前仅存储） |
| `vaultFiles` | bool | 否 | `false`（vault 开启且 `defaultFilesMode=materialize` 时为 `true`） | 文件区加密：true=停止时整树加密入库、启动前物化 |

#### `eventTask`（EventTask 嵌套）

| JSON 字段 | 类型 | 默认 | 含义 |
| --- | --- | --- | --- |
| `autoStart` | bool | `false` | 节点启动时自动启动该实例（main.go 消费） |
| `autoRestart` | bool | `false` | 意外退出自动重启（10 秒窗口内最多 3 次防抖） |
| `ignore` | bool | `false` | 忽略标记（MCSM 对齐，节点当前仅存储） |

#### `pingConfig`（PingConfig 嵌套）

| JSON 字段 | 类型 | 默认 | 含义 |
| --- | --- | --- | --- |
| `ip` | string | `""` | 探测目标 IP（节点当前仅存储） |
| `port` | int | `25565`（为 0 时） | 探测端口（节点当前仅存储） |
| `type` | int | `0` | 探测类型（节点当前仅存储） |

#### 完整 InstanceConfig 示例（服务端补齐默认后）

```json
{
  "nickname": "我的服务器",
  "startCommand": "java -Xmx2G -jar server.jar",
  "stopCommand": "stop",
  "cwd": "D:\\servers\\myserver",
  "ie": "utf-8",
  "oe": "utf-8",
  "createDatetime": 1710000000000,
  "lastDatetime": 1710000000000,
  "type": "universal",
  "tag": [],
  "endTime": 1741536000000,
  "fileCode": "utf-8",
  "processType": "universal",
  "updateCommand": "",
  "actionCommandList": [],
  "crlf": 2,
  "eventTask": { "autoStart": false, "autoRestart": false, "ignore": false },
  "pingConfig": { "ip": "", "port": 25565, "type": 0 },
  "vaultFiles": false
}
```

> **行为说明**：节点生命周期实际只消费 `nickname`、`startCommand`、`stopCommand`、`cwd`、
> `type`（默认）、`eventTask.autoStart`、`eventTask.autoRestart`、`vaultFiles`。
> 其余字段（`ie`/`oe`/`fileCode`/`processType`/`updateCommand`/`actionCommandList`/`crlf`/
> `tag`/`pingConfig`/`eventTask.ignore`）为 MCSM 对齐字段：**会原样持久化并在读取时回显，但节点不据此改变行为**。

### 2.2 InstanceDetail（实例列表条目 / 详情条目）

Go 来源：`daemon.go` 的 `Instance.Detail()`。列表每条与详情接口返回同一结构。

| JSON 字段 | 类型 | 含义 |
| --- | --- | --- |
| `config` | InstanceConfig | 实例完整配置（见 2.1） |
| `instanceUuid` | string | 实例 UUID（UUID v4 小写） |
| `started` | int | 累计成功启动次数（持久化） |
| `status` | int | 状态：-1 忙碌 / 0 已停止 / 1 停止中 / 2 启动中 / 3 运行中 |
| `space` | int64 | 工作目录占用字节（当前恒为 0，由文件管理器按需统计） |
| `info` | object | 服务端信息块（见下） |
| `processInfo` | object | 进程信息块（见下） |

`info` 对象（固定值/占位）：

```json
{
  "currentPlayers": -1,
  "maxPlayers": -1,
  "playersChart": [],
  "version": "",
  "fileLock": 0
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `currentPlayers` | int | 当前在线玩家，恒 -1（占位，实时玩家数见 stats 接口） |
| `maxPlayers` | int | 最大玩家，恒 -1（占位） |
| `playersChart` | []any | 恒空数组 |
| `version` | string | 恒空字符串 |
| `fileLock` | int | 恒 0 |

`processInfo` 对象（`Process.Info()`，MCSM 风格）：

```json
{ "cpu": 0, "memory": 0, "ppid": 0, "pid": 0, "ctime": 0, "elapsed": 0, "timestamp": 1710000000000 }
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `cpu` | int | CPU 占用，当前恒 0（占位） |
| `memory` | int | 内存占用，当前恒 0（占位） |
| `ppid` | int | 父进程 PID，当前恒 0 |
| `pid` | int | 进程 PID；未运行时 0，运行时为真实 PID |
| `ctime` | int64 | 进程启动时间 unix 毫秒（`Process.started`） |
| `elapsed` | int64 | 已运行秒数 |
| `timestamp` | int64 | 采样时间 unix 毫秒 |

#### 完整 InstanceDetail 示例

```json
{
  "config": { "nickname": "我的服务器", "startCommand": "java -jar server.jar", "stopCommand": "stop", "cwd": "D:\\servers\\myserver", "ie": "utf-8", "oe": "utf-8", "createDatetime": 1710000000000, "lastDatetime": 1710000000000, "type": "universal", "tag": [], "endTime": 1741536000000, "fileCode": "utf-8", "processType": "universal", "updateCommand": "", "actionCommandList": [], "crlf": 2, "eventTask": { "autoStart": false, "autoRestart": false, "ignore": false }, "pingConfig": { "ip": "", "port": 25565, "type": 0 }, "vaultFiles": false },
  "instanceUuid": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
  "started": 3,
  "status": 3,
  "space": 0,
  "info": { "currentPlayers": -1, "maxPlayers": -1, "playersChart": [], "version": "", "fileLock": 0 },
  "processInfo": { "cpu": 0, "memory": 0, "ppid": 0, "pid": 4242, "ctime": 1710000000000, "elapsed": 120, "timestamp": 1710000120000 }
}
```

---

## 3. 端点总表

| 方法 | 路径 | 说明 | 所在文件 |
| --- | --- | --- | --- |
| GET | `/api/overview` | 节点概览 | overview.go |
| GET | `/api/load` | 负载调谐状态 | loadtuner.go |
| GET | `/api/service/remote_service_instances` | 实例列表（分页/过滤） | instance.go |
| GET | `/api/instance` | 实例详情 | instance.go |
| POST | `/api/instance` | 创建实例 | instance.go |
| PUT | `/api/instance` | 更新实例配置（整体替换） | instance.go |
| DELETE | `/api/instance` | 删除实例（可批量） | instance.go |
| POST | `/api/instance/import` | 导入目录创建实例 | instance_import.go |
| GET | `/api/protected_instance/open` | 启动实例 | instance.go |
| GET | `/api/protected_instance/stop` | 停止实例 | instance.go |
| GET | `/api/protected_instance/restart` | 重启实例 | instance.go |
| GET | `/api/protected_instance/kill` | 强制终止实例 | instance.go |
| GET | `/api/protected_instance/command` | 下发控制台命令 | instance.go |
| GET | `/api/protected_instance/outputlog` | 获取输出日志（字节截尾） | instance.go |
| GET | `/api/instance/logs` | 历史日志（tail / since） | instance_logs.go |
| DELETE | `/api/instance/logs` | 清空日志 | instance_logs.go |
| GET | `/api/instance/logs/query` | 日志查询（关键词过滤，供 AI） | instance_metrics.go |
| GET | `/api/instance/stats` | 实例实时运行指标 | instance_stats.go |
| GET | `/api/instance/metrics` | 实例指标历史采样 | instance_metrics.go |
| GET | `/api/instance/plugins` | 插件/Mod 元数据 | plugins.go |
| GET | `/api/instance/console/ws` | 实时控制台 WebSocket | console_ws.go / websocket.go |
| GET | `?jobId=`（进度查询端点） | 异步任务进度轮询 | jobs.go |

---

## 4. 概览与负载

### 4.1 GET /api/overview

获取节点概览。无查询参数（仅认证）。

响应 `data` 结构：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `version` | string | 节点版本号（`Version` 常量，当前 `"1.1.0"`） |
| `specifiedDaemonVersion` | string | 同 `version`（MCSM 兼容） |
| `process` | object | 节点进程信息 |
| `record` | object | 统计记录（全部恒 0） |
| `system` | object | 系统信息 |
| `chart` | object | 图表占位（`system`/`request` 均为空数组） |
| `remoteCount` | object | 远端节点计数（`available`/`total` 均恒 1） |
| `remote` | array | 远端节点列表（本节点自身） |

`process`：

```json
{ "cpu": 0, "memory": 12345678, "cwd": "D:\\Irix-web\\IriX-Node\\data" }
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `cpu` | int | 恒 0 |
| `memory` | uint64 | 节点进程堆内存字节（`runtime.MemStats.Alloc`） |
| `cwd` | string | 节点数据目录 |

`system`：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `type` | string | 系统类型（如 `Windows`/`Linux`/`Darwin`） |
| `hostname` | string | 主机名 |
| `platform` | string | 平台（`windows`/`linux`/`darwin`/`freebsd` 等） |
| `release` | string | 系统发行版本 |
| `version` | string | 发行版版本号（如 `"22.04"`；空则前端回退 `release`） |
| `uptime` | float64 | 系统运行秒数 |
| `totalmem` | uint64 | 总内存字节 |
| `freemem` | uint64 | 可用内存字节 |
| `cpuUsage` | float64 | 整机 CPU 使用率（0-1；仅 Linux 采样，其他平台 0） |
| `memUsage` | float64 | 内存使用率（0-1） |
| `diskusage` | float64 | 磁盘使用率（0-1） |
| `disktotal` | uint64 | 数据目录所在盘总容量字节 |
| `diskused` | uint64 | 已用字节 |
| `networkDownload` | float64 | 下载速率字节/秒 |
| `networkUpload` | float64 | 上传速率字节/秒 |
| `processCpu` | int | 恒 0 |
| `processMem` | int | 恒 0 |
| `node` | string | Go 运行时版本（`runtime.Version()`） |
| `time` | int64 | 采样时间 unix 毫秒 |
| `cwd` | string | 节点数据目录 |

`remote` 数组每项：

```json
{
  "version": "1.1.0",
  "process": { "cpu": 0, "memory": 12345678, "cwd": "..." },
  "instance": { "running": 1, "total": 3 },
  "system": { /* 同上 system 对象 */ },
  "cpuMemChart": [],
  "uuid": "<节点 UUID>",
  "ip": "127.0.0.1",
  "port": 12346,
  "prefix": "",
  "available": true,
  "remarks": "本地节点"
}
```

响应示例：

```json
{
  "status": 200,
  "data": {
    "version": "1.1.0",
    "specifiedDaemonVersion": "1.1.0",
    "process": { "cpu": 0, "memory": 12345678, "cwd": "D:\\Irix-web\\IriX-Node\\data" },
    "record": { "logined": 0, "illegalAccess": 0, "banips": 0, "loginFailed": 0 },
    "system": { "type": "Windows", "hostname": "PC01", "platform": "windows", "release": "Windows 11", "version": "", "uptime": 86400.5, "totalmem": 17179869184, "freemem": 8589934592, "cpuUsage": 0, "memUsage": 0.5, "diskusage": 0.3, "disktotal": 1000204886016, "diskused": 300061465804, "networkDownload": 1024.5, "networkUpload": 512.2, "processCpu": 0, "processMem": 0, "node": "go1.22.0", "time": 1710000000000, "cwd": "D:\\Irix-web\\IriX-Node\\data" },
    "chart": { "system": [], "request": [] },
    "remoteCount": { "available": 1, "total": 1 },
    "remote": [ { "version": "1.1.0", "process": { "cpu": 0, "memory": 12345678, "cwd": "..." }, "instance": { "running": 1, "total": 3 }, "system": {}, "cpuMemChart": [], "uuid": "uuid", "ip": "127.0.0.1", "port": 12346, "prefix": "", "available": true, "remarks": "本地节点" } ]
  },
  "time": 1710000000000
}
```

### 4.2 GET /api/load

获取负载自适应调谐器当前状态。无查询参数。

响应 `data`：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `state` | string | `"idle"` / `"normal"` / `"busy"` |
| `since` | int64 | 进入当前状态的起始时间 unix 毫秒 |
| `gomaxprocs` | int | 当前 GOMAXPROCS |
| `gcPercent` | int | 当前 GOGC（`50`/`100`/`400`） |
| `cpuBusy` | float64 | 进程 CPU 占比（0-1，最近一次采样） |
| `goroutines` | int | 当前 goroutine 数 |
| `heapAlloc` | uint64 | 堆内存字节（最近一次采样） |
| `numCPU` | int | 机器逻辑核数 |

示例：

```json
{ "status": 200, "data": { "state": "normal", "since": 1710000000000, "gomaxprocs": 8, "gcPercent": 100, "cpuBusy": 0.02, "goroutines": 45, "heapAlloc": 20971520, "numCPU": 8 }, "time": 1710000000000 }
```

---

## 5. 实例 CRUD

### 5.1 GET /api/service/remote_service_instances —— 实例列表（分页）

查询参数：

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `page` | 否 | `1` | 页码（≤0 归为 1） |
| `page_size` | 否 | `100` | 每页条数（≤0 归为 100，无上限） |
| `instance_name` | 否 | 空 | 按昵称**子串**过滤（大小写敏感） |
| `status` | 否 | 空 | 按状态**精确字符串**过滤（`-1`/`0`/`1`/`2`/`3`） |
| `daemonId` | 否 | — | 兼容参数，忽略 |

响应 `data`：

```json
{
  "maxPage": 1,
  "pageSize": 100,
  "page": 1,
  "total": 3,
  "data": [ /* InstanceDetail 数组，见 2.2 */ ]
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `maxPage` | int | 最大页码（无匹配时为 1） |
| `pageSize` | int | 实际生效的每页条数 |
| `page` | int | 实际页码 |
| `total` | int | 过滤后总条数 |
| `data` | array\<InstanceDetail\> | 当前页条目，按 `createDatetime` 升序 |

### 5.2 GET /api/instance —— 实例详情

查询参数：

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | **是** | 实例 UUID |
| `daemonId` | 否 | 兼容参数，忽略 |

响应 `data`：`InstanceDetail`（见 2.2）。

错误：400 `缺少 uuid 参数` / `实例不存在`。

### 5.3 POST /api/instance —— 创建实例

查询参数：`daemonId`（兼容，忽略）。

请求体：`InstanceConfig`（见 2.1）。所有字段均可省略，服务端 `FillDefaults` 补齐默认值；
`cwd` 必须为非空合法目录，否则 400。`createDatetime`/`lastDatetime` 由服务端覆盖为当前时间。

响应 `data`：

```json
{
  "instanceUuid": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
  "config": { /* 补齐默认后的完整 InstanceConfig，见 2.1 */ }
}
```

错误：400 `请求体格式错误: ...` / `工作目录不能为空` / `工作目录不能位于系统目录内: ...`；
500 `保存实例失败: ...`。

### 5.4 PUT /api/instance —— 更新实例配置

查询参数：

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | **是** | 实例 UUID |
| `daemonId` | 否 | 兼容参数，忽略 |

请求体：`InstanceConfig`（**整体替换**，非 PATCH）。`cwd` 同样需非空合法。
`createDatetime` 保留原值，`lastDatetime` 由服务端设为当前时间。

响应 `data`：

```json
{ "instanceUuid": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" }
```

错误：400 `缺少 uuid 参数` / `实例不存在` / `请求体格式错误: ...` / 目录校验失败；
500 保存失败。

### 5.5 DELETE /api/instance —— 删除实例

查询参数：`daemonId`（兼容，忽略）。

请求体：

```json
{ "uuids": ["uuid1", "uuid2"], "deleteFile": false }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuids` | []string | 是 | 要删除的实例 UUID 列表 |
| `deleteFile` | bool | 否（默认 false） | true 时同时删除实例工作目录 |

> 若实例正在运行，删除前会先强制终止进程。删除单个失败会静默跳过（`continue`）。

响应 `data`：**成功删除的 UUID 字符串数组**（`[]string`）：

```json
{ "status": 200, "data": ["uuid1", "uuid2"], "time": 1710000000000 }
```

错误：400 `请求体格式错误: ...`。

### 5.6 POST /api/instance/import —— 导入目录创建实例

查询参数：无（仅认证）。

请求体：

```json
{ "daemonId": "", "path": "D:\\servers\\myserver", "nickname": "我的服务器" }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `daemonId` | string | 否 | 兼容参数，忽略 |
| `path` | string | **是** | 要导入的目录；须存在、为目录、未被其他实例占用、且含服务端特征 |
| `nickname` | string | 否 | 昵称；缺省时取目录名（`filepath.Base`） |

服务端特征扫描（`importableDir`，任一命中即可导入）：根目录存在任意 `*.jar`，或存在
`eula.txt` / `server.properties` / `bukkit.yml` / `spigot.yml` / `paper.yml` /
`purpur.yml` / `bungee.yml` / `velocity.toml` / `version.json`。

导入创建的实例：`cwd=该目录`、`type=universal`、`startCommand` 留空（由用户在配置页填写）。

响应 `data`：

```json
{ "instanceUuid": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" }
```

错误：400 `缺少 path 参数` / `目录不存在: ...` / `该目录已被实例使用，请勿重复导入` /
`目录中未发现服务端特征（根目录 *.jar、eula.txt、server.properties 等），无法导入`；
500 `保存实例失败: ...`。

---

## 6. 实例操作（启动/停止/重启/强杀/命令/输出日志）

这组路由均用 **GET** 方式（MCSM 风格），`uuid` 为必填查询参数，`daemonId` 为兼容忽略参数。

### 6.1 GET /api/protected_instance/open —— 启动

查询参数：`uuid`（必填）、`daemonId`（忽略）。

响应 `data`：

```json
{ "instanceUuid": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" }
```

错误：400 `缺少 uuid 参数` / `实例不存在`；500 `实例正在执行其他操作` / `实例已在运行` /
`启动失败: ...` / `工作目录为空` / `工作目录不存在: ...` / `文件区物化失败: ...`。

### 6.2 GET /api/protected_instance/stop —— 停止

查询参数：`uuid`（必填）、`daemonId`（忽略）。

语义：发送 `stopCommand` 到 stdin，30 秒超时后强制 Kill；随后状态置 0。
`vaultFiles=true` 的实例停止后整树加密回收。

响应 `data`：`{ "instanceUuid": "..." }`

错误：400 参数错误；500 `实例正在执行其他操作` / 停止失败。

### 6.3 GET /api/protected_instance/restart —— 重启

查询参数：`uuid`（必填）、`daemonId`（忽略）。

语义：已停止的实例直接启动（不报错）；运行中先停后启。

响应 `data`：`{ "instanceUuid": "..." }`

### 6.4 GET /api/protected_instance/kill —— 强制终止

查询参数：`uuid`（必填）、`daemonId`（忽略）。

语义：立即强杀进程（`Proc.Kill`，Windows 下杀进程树），不触发 AutoRestart；状态置 0。

响应 `data`：`{ "instanceUuid": "..." }`

错误：500 `终止进程失败: ...`。

### 6.5 GET /api/protected_instance/command —— 下发命令

查询参数：

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | **是** | 实例 UUID |
| `command` | **是** | 要发送的命令（写入 stdin，自动补 `\n`） |
| `daemonId` | 否 | 忽略 |

响应 `data`：`{ "instanceUuid": "..." }`

错误：400 `缺少 command 参数` / `实例未在运行`；500 写入失败。

### 6.6 GET /api/protected_instance/outputlog —— 输出日志（字节截尾）

查询参数：

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | **是** | — | 实例 UUID |
| `size` | 否 | 0 | 返回日志尾部多少 **KB**；范围 `[0, 2048]`，越界被钳制（负数→0，>2048→2048）；`0` 表示返回整个内存缓冲 |
| `daemonId` | 否 | — | 忽略 |

响应 `data`：**字符串**（进程 stdout+stderr 合并后的内存环形缓冲尾部，保留 ANSI；实例未运行时为空字符串 `""`）。

```json
{ "status": 200, "data": "Server started\nDone (1.2s)!\n", "time": 1710000000000 }
```

> 注意区分：`outputlog` 的 `size` 是**字节量（KB）**；`/api/instance/logs` 的 `tail` 是**行数**。

---

## 7. 实例日志

### 7.1 GET /api/instance/logs —— 历史日志（tail / since 断线补发）

查询参数：

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | **是** | — | 实例 UUID |
| `tail` | 否 | `1000` | 返回最后 N **行**；显式 `0` 表示全部；与 `since` 互斥 |
| `since` | 否 | 空 | unix 毫秒时间戳；存在时返回该时间点**之后**追加的行（断线重连补发），忽略 `tail` |
| `daemonId` | 否 | — | 忽略 |

数据来源：落盘日志文件（`{data}/logs/{uuid}.log` 及轮转 `.1`~`.5`）+ 运行中的带时间戳行缓冲。
`since` 查询在运行中走精确到行的缓冲，进程退出后回退到文件 mtime 过滤。

响应 `data`：**字符串**（多行日志，保留 ANSI 转义与行尾，末尾有 `\n`；无内容时 `""`）。

```json
{ "status": 200, "data": "[12:00:00 INFO]: Starting server...\n[12:00:01 INFO]: Done\n", "time": 1710000000000 }
```

错误：400 `缺少 uuid 参数` / `实例不存在`；500 `读取日志失败: ...`。

### 7.2 DELETE /api/instance/logs —— 清空日志

查询参数：`uuid`（必填）、`daemonId`（忽略）。

语义：运行中清空行缓冲 + 经 fileLogger 清空文件；已停止直接删除日志文件与轮转文件。

响应 `data`：布尔值 `true`。

```json
{ "status": 200, "data": true, "time": 1710000000000 }
```

错误：400 参数错误；500 `清空日志失败: ...`。

### 7.3 GET /api/instance/logs/query —— 日志查询（关键词过滤）

查询参数：

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | **是** | — | 实例 UUID |
| `keyword` | 否 | 空 | 关键词（大小写不敏感，命中行才返回） |
| `level` | 否 | — | **接受但忽略**（MCSM 兼容占位） |
| `windowMin` | 否 | — | **接受但忽略**（MCSM 兼容占位） |
| `maxLines` | 否 | `200` | 最多返回行数；`≤0` 归为 200，上限 2000 |
| `daemonId` | 否 | — | 忽略 |

实现为「扫描日志尾部 + 运行中行缓冲 → 关键词过滤 → 取最近 N 行 → 时间顺序返回」。

响应 `data`：

```json
{
  "items": ["[12:00:01 ERROR]: Oops", "[12:00:02 ERROR]: Again"],
  "total": 2
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `items` | []string | 匹配的日志行（时间正序） |
| `total` | int | 返回行数（= `len(items)`） |

---

## 8. 实例指标

### 8.1 GET /api/instance/stats —— 实时运行指标

查询参数：`uuid`（必填）、`daemonId`（忽略）。

响应 `data`：

```json
{
  "pid": 4242,
  "cpuPercent": 12.34,
  "memoryMb": 512,
  "networkDownloadBps": 1024,
  "networkUploadBps": 512,
  "uptimeSec": 120,
  "players": 3,
  "maxPlayers": 20,
  "tps": 19.98
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `pid` | int | 进程 PID；未运行时 0 |
| `cpuPercent` | float64 | 进程 CPU 使用率（归一化到单核 0~100，两位小数） |
| `memoryMb` | int64 | 进程内存（MB，工作集字节 >> 20） |
| `networkDownloadBps` | int64 | 节点**全局**网卡下载速率字节/秒（缓存采样） |
| `networkUploadBps` | int64 | 节点全局上传速率字节/秒 |
| `uptimeSec` | int64 | 进程已运行秒数 |
| `players` | int | **可选**。当前在线玩家数；未从输出解析出则省略该字段 |
| `maxPlayers` | int | **可选**。最大玩家数；未解析出则省略 |
| `tps` | float64 | **可选**。TPS；未解析出则省略（两位小数） |

> 未运行时：只有前 6 个字段且均为 0；`players`/`maxPlayers`/`tps` 不出现（前端显示「—」）。

### 8.2 GET /api/instance/metrics —— 指标历史采样

查询参数：

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | **是** | — | 实例 UUID |
| `minutes` | 否 | `15` | 回溯多少分钟；`≤0` 归为 15，上限 60 |
| `daemonId` | 否 | — | 忽略 |

节点每 15 秒对运行中实例采样一次，环形保留 60 条（约 15 分钟）。

响应 `data`：

```json
{
  "samples": [
    { "time": 1710000000000, "cpu": 12.34, "memoryMb": 512, "downloadBps": 1024, "uploadBps": 512 },
    { "time": 1710000015000, "cpu": 11.10, "memoryMb": 520, "downloadBps": 800, "uploadBps": 400 }
  ]
}
```

`samples` 每项（`metricSample`，JSON tag）：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `time` | int64 | 采样时间 unix 毫秒 |
| `cpu` | float64 | CPU 使用率（0~100，两位小数） |
| `memoryMb` | int64 | 内存 MB |
| `downloadBps` | int64 | 下载速率字节/秒（节点全局） |
| `uploadBps` | int64 | 上传速率字节/秒（节点全局） |

---

## 9. 插件 / Mod 元数据

### 9.1 GET /api/instance/plugins —— 插件列表

查询参数：`uuid`（必填）、`daemonId`（忽略）。

扫描实例 `cwd` 下 `plugins/`（仅一层）与 `mods/`（递归，含版本子目录）中的 `*.jar`，
解析 `plugin.yml` / `paper-plugin.yml` / `fabric.mod.json` / `META-INF/mods.toml` 元数据。
单次最多扫描 500 个 jar。

响应 `data`：

```json
{
  "items": [
    {
      "type": "plugin",
      "path": "/plugins/EssentialsX.jar",
      "fileName": "EssentialsX.jar",
      "size": 1234567,
      "name": "EssentialsX",
      "description": "The essential plugin suite",
      "version": "2.20.1",
      "iconBase64": "iVBORw0KGgo...",
      "configDir": "/plugins/Essentials"
    }
  ]
}
```

`items` 每项（`pluginItem`，JSON tag；`iconBase64`/`configDir` 为 `omitempty`，无值时不出现）：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `type` | string | `"plugin"` 或 `"mod"` |
| `path` | string | 相对 cwd 的路径（`/` 开头，`/` 分隔） |
| `fileName` | string | 文件名 |
| `size` | int64 | 字节 |
| `name` | string | 显示名 |
| `description` | string | 描述（可空） |
| `version` | string | 版本（可空） |
| `iconBase64` | string | 图标文件 base64（PNG），**可选** |
| `configDir` | string | 匹配到的配置目录路径，**可选** |

排序：`plugin` 在前；同类型按 `fileName` 升序。

错误：400 参数错误 / `实例工作目录为空`；500 `扫描 mods 目录失败: ...`。

---

## 10. WebSocket 实时控制台

### 10.1 连接与握手

- 路径：`GET /api/instance/console/ws`
- 查询参数：

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | **是** | 实例 UUID |
| `apikey` | **是**（或用头） | 认证密钥；也可用 `X-Api-Key` 头（与 REST 一致） |
| `daemonId` | 否 | 忽略 |
| `since` | 否 | unix 毫秒；断线重连时补发该时间点之后的日志 |

- 握手为标准 RFC 6455：客户端必须带 `Upgrade: websocket`、`Connection: Upgrade`、
  `Sec-WebSocket-Key`。服务端回 `101 Switching Protocols` 与 `Sec-WebSocket-Accept`。
- **无需** `Sec-WebSocket-Protocol` 子协议头（服务端不校验、不协商）。
- 认证失败（apikey 错）时**不会升级**，返回 HTTP 403（REST 风格错误体），
  客户端应将「非 101」视为握手失败并回退到 outputlog 轮询 + command。

### 10.2 帧约定（RFC 6455）

| 方向 | 帧 | 含义 |
| --- | --- | --- |
| 服务端 → 客户端 | 文本帧（opcode 0x1） | 一行一条服务器原始输出，**保留 ANSI 转义**（颜色由前端渲染，节点不剥离） |
| 客户端 → 服务端 | 文本帧 | 控制台命令（等同 command 接口，写入 stdin 补 `\n`） |
| 客户端 → 服务端 | 文本帧 `"ping"` | 心跳文本帧（被忽略，不下发到进程） |
| 客户端 → 服务端 | Ping 控制帧（0x9） | 服务端回 Pong（回显载荷） |
| 客户端 → 服务端 | Pong 控制帧（0xA） | 仅刷新活跃时间 |
| 任意方向 | Close 控制帧（0x8） | 关闭 |

服务端发送**不掩码**；客户端帧**必须掩码**（未掩码按协议错误断开）。
**不支持**：二进制帧、分片数据帧、单帧 > 1 MiB（均按协议错误断开）。

### 10.3 心跳与断线

- 客户端应每 30 秒发送任意帧（文本 `ping` 或 Ping 控制帧均可）。
- 服务端 90 秒未收到任何帧则发送 Close(1001, `"心跳超时"`) 并断开。
- 进程退出时，服务端先发文本帧 `[节点] 进程已退出，输出结束`，再 Close(1000, `"进程已退出"`)。
- 实例未运行时发命令，服务端回文本 `[节点] 实例未在运行，命令未发送`。
- 断线重连：带 `since=<unix_ms>` 重连，服务端先补发该时间点之后的增量日志
  （仅运行中且行缓冲可用时），再进入实时流。

### 10.4 消息时序

服务端先订阅输出、再补发 `since`、再进入读写循环，保证补发期间不丢行（少量重复可接受）。

---

## 11. 异步任务进度轮询（jobs.go）

耗时操作统一任务化：发起接口返回 `taskId`（响应 `data.taskId` 或 `data.jobId` 字段，见具体端点），
前端轮询进度查询端点。

### 11.1 进度查询端点（通用契约）

以下端点共用 `writeTaskStatus` 处理器：

| 端点 | 用途 |
| --- | --- |
| `GET /api/instance/download-core-progress` | 服务端核心下载进度 |
| `GET /api/runtime/java/install-progress` | JDK 安装进度 |

查询参数：

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `jobId` | **是** | 发起接口返回的任务 ID |

响应 `data`：

```json
{ "status": "running", "percent": 0.42, "message": "下载中… 42%", "path": "D:\\...\\server.jar" }
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | string | `"running"` 执行中 / `"done"` 完成 / `"failed"` 失败 |
| `percent` | float64 | 进度 0.0~1.0；`-1` 表示未知进度 |
| `message` | string | 人类可读进度消息（中文） |
| `path` | string | **可选**。产物路径（如下载完成的文件），无值时不出现 |

错误：400 `缺少 jobId 参数` / `任务不存在或已过期`。

任务保留策略：已完成/失败任务保留 2 小时；任务表上限 1024，超限淘汰最旧。

> 注：`GET /api/instance/snapshot-progress`（实例备份进度）用的是另一个处理器
> `writeSnapshotStatus`，不在 jobs.go，此处不展开；实例备份/恢复/下载核心的**发起**端点在
> 附录列出，其详细契约见对应源码文件。

---

## 12. 附录：完整路由速查（RegisterRoutes）

以下为 `RegisterRoutes` 中与实例相关的全部路由，标记 ✅ 的已在本文档详述，其余为相邻能力
（文件管理 / FRP / 容器 / 集群 / 保险库 / 备份恢复 / 运行时），不在「实例核心」范围。

| 方法与路径 | 说明 | 本文档 |
| --- | --- | --- |
| `GET /api/overview` | 概览 | ✅ §4.1 |
| `GET /api/load` | 负载调谐 | ✅ §4.2 |
| `GET /api/service/remote_service_instances` | 实例列表 | ✅ §5.1 |
| `GET /api/instance` | 详情 | ✅ §5.2 |
| `POST /api/instance` | 创建 | ✅ §5.3 |
| `PUT /api/instance` | 更新 | ✅ §5.4 |
| `DELETE /api/instance` | 删除 | ✅ §5.5 |
| `POST /api/instance/import` | 导入 | ✅ §5.6 |
| `GET /api/protected_instance/open` | 启动 | ✅ §6.1 |
| `GET /api/protected_instance/stop` | 停止 | ✅ §6.2 |
| `GET /api/protected_instance/restart` | 重启 | ✅ §6.3 |
| `GET /api/protected_instance/kill` | 强杀 | ✅ §6.4 |
| `GET /api/protected_instance/command` | 命令 | ✅ §6.5 |
| `GET /api/protected_instance/outputlog` | 输出日志 | ✅ §6.6 |
| `GET /api/instance/logs` | 历史日志 | ✅ §7.1 |
| `DELETE /api/instance/logs` | 清空日志 | ✅ §7.2 |
| `GET /api/instance/logs/query` | 日志查询 | ✅ §7.3 |
| `GET /api/instance/stats` | 实时指标 | ✅ §8.1 |
| `GET /api/instance/metrics` | 指标历史 | ✅ §8.2 |
| `GET /api/instance/plugins` | 插件元数据 | ✅ §9 |
| `GET /api/instance/console/ws` | 控制台 WS | ✅ §10 |
| `POST /api/instance/snapshot` | 创建备份（任务化） | 相邻（备份恢复） |
| `GET /api/instance/snapshot-progress` | 备份进度 | 相邻（`writeSnapshotStatus`） |
| `POST /api/instance/restore` | 恢复备份 | 相邻 |
| `GET /api/instance/backups` | 备份列表 | 相邻 |
| `DELETE /api/instance/backups` | 删除备份 | 相邻 |
| `POST /api/instance/backups/download` | 备份下载票据 | 相邻 |
| `POST /api/instance/download-core` | 下载核心（任务化） | 发起→`{jobId}`，轮询 ✅ §11 |
| `GET /api/instance/download-core-progress` | 核心下载进度 | ✅ §11 |
| `GET /api/runtime/java` | Java 运行时 | 相邻（运行时） |
| `POST /api/runtime/java/install` | JDK 安装（任务化） | 发起→`{jobId}`，轮询 ✅ §11 |
| `GET /api/runtime/java/install-progress` | JDK 安装进度 | ✅ §11 |
| `DELETE /api/runtime/java` | 卸载 JDK | 相邻 |
| `GET/PUT/DELETE /api/files/*` 等 | 文件管理 | 相邻（files.go） |
| `/api/frp/*`、`/api/container/*`、集群、保险库 | 其他能力 | 相邻 |
| `GET /download/`、`POST /upload/` | 票据直连（免 apikey） | 相邻（download.go） |
