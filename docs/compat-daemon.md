# 第三方 Daemon 协议兼容（compat 块）

本节点可以作为**现成第三方面板的 Daemon** 使用：面板不需要任何改造，只要把
"节点地址"指向本节点开启的兼容子路径，即可用同一套面板管理本节点上的实例。

目前提供两层，**可以同时开启**，各自占用**独立监听端口**，互不影响，也不与节点
自身 API 端口（默认 12346）冲突：

| 兼容层 | 对应面板 | 默认端口 | 协议形态 |
| --- | --- | --- | --- |
| `compat.mcsm` | MCSManager 面板（按"节点"接入） | `24444` | socket.io v4（engine.io v4）+ 直连文件通道 |
| `compat.mslx` | MSLX WebPanel | `7891` | REST `/api/...`（JWT）+ SignalR 控制台 |

> 为什么必须是独立端口：MCSManager 面板的「节点」配置只有**地址**和**端口**两栏，
> 没有路径前缀输入框，所以兼容层不能挂在子路径下。

## 1. 许可与实现边界（重要）

兼容层的**底层处理逻辑全部是本节点自有代码**：

- 只对齐"线上协议与字段语义"（事件名、字段名、响应信封、路由形态）；
- **不引入、不复制、不翻译** MCSManager（Apache-2.0）与 MSLX 的源代码；
- 传输层（engine.io/socket.io、SignalR JSON 协议、WebSocket、JWT）全部基于
  Go 标准库从协议文本自行实现，未新增任何第三方依赖；
- 所有业务动作都落到本节点已有能力上（`startInstance`/`stopInstance`、
  票据通道、`NormalizePath`/`safeResolve`、`sysinfo`、`timesys`）。

因此本项目不会因兼容层而承担上述项目的许可证义务，也不存在许可证冲突。

## 2. 配置

```json
{
  "compat": {
    "mcsm": {
      "enabled": true,
      "bind": "",
      "port": 24444,
      "key": "",
      "allowPanelIps": ["127.0.0.1", "192.168.0.0/16"]
    },
    "mslx": {
      "enabled": true,
      "bind": "",
      "port": 7891,
      "username": "admin",
      "password": "",
      "jwtSecret": "",
      "allowPanelIps": ["192.168.0.0/16"]
    }
  }
}
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `mcsm.enabled` / `mslx.enabled` | 两个开关**互相独立**，可同时为 `true`；默认 `false`（需显式开启） |
| `port` | 该兼容层的**独立监听端口**。默认 `24444`（MCSM）/ `7891`（MSLX）；写 `0` = 由系统分配空闲端口；端口被占用时该层只告警不启动，不影响节点主功能 |
| `bind` | 该兼容层的监听地址。留空 = 沿用节点 `bind`（节点绑 `0.0.0.0` 时兼容层也对外）；只给本机面板用可填 `127.0.0.1` |
| `mcsm.key` | 面板"节点密钥"（等价 MCSManager 的 apiKey）。**留空即自动生成**（见 §2.1） |
| `mslx.username` / `mslx.password` | WebPanel 登录账号；密码**留空即自动生成**（见 §2.1） |
| `mslx.jwtSecret` | JWT 签名密钥；留空即自动生成，写入 `{data}/compat-secrets.json` |
| `mslx.linkKey` | MSLX「设置 → 节点管理 → 添加节点」里填的**配对密钥**；留空即自动生成（写回配置 + `{data}/compat-secrets.json`） |
| `mslx.slaveMode` | 子节点模式，默认 `true`。MSLX 只在子节点模式下接受主节点的链接请求；本节点就是以「子节点」身份被面板纳管 |
| `allowPanelIps` | 允许的面板来源（裸 IP 或 CIDR，数组）；留空 = 不限制来源 |
| `mcsm.version` | 上报给面板的节点版本号。留空 = 本节点版本；面板用 `hasVersionUpdate()` 比较**主次版本号**，与面板自带 daemon 版本不一致就把节点标红「有更新」——想显示为最新就填面板所带的 daemon 版本（如 `4.5.0`） |
| `prefix` | **已废弃**：面板不支持路径前缀，兼容层已改为独立端口；填了只在启动日志提示，不生效 |

### 2.1 凭据自动生成（无需手填）

**密钥与密码不需要自己编**：留空即可，节点首次启动会随机生成并处理妥当。

- 生成时机：每次启动都会解析一遍凭据——**即使两层都是 `enabled=false` 也会生成**，
  所以配置文件里"永远有东西"，想启用时只改 `enabled` 即可。
- 回写配置文件：只把 `compat.mcsm.key` / `compat.mslx.password` 里**仍为空**的字段
  用定点文本替换填上，不重排、不改动注释、不覆盖你已填的值，也不改文件权限。
- 持久化副本：三项凭据（含 `jwtSecret`）同时写入 `{data}/compat-secrets.json`
  （0600 原子写），因此重启后凭据不变，面板不必重配，已签发的 MSLX token 也不会失效。
- 首次生成的配置文件（`ensureConfigFile`）本身就带随机值，不是空的。
- 启动日志会打印可直接使用的地址与凭据；未启用时也会打印打码后的凭据提示：

```
兼容层凭据：已自动生成 compat.mcsm.key（面板节点密钥）
MCSManager Daemon 兼容层已启用（面板「节点」填 地址=<本机IP> 端口=24444，密钥 <key>）
MSLX Daemon 兼容层已启用（WebPanel 后端地址 http://<本机IP>:7891，账号 admin，密码 <pass>）
```

优先级：**配置文件里显式填写的值 > `{data}/compat-secrets.json` > 现场随机生成**。
想更换凭据：删掉配置文件里对应字段的值（置为 `""`）并删除 `{data}/compat-secrets.json`
后重启，或直接自己填一个新值。

启动日志会明确打印启用状态与面板应填写的地址：

```
MCSManager Daemon 兼容层监听于 0.0.0.0:24444
MSLX 兼容层监听于 0.0.0.0:7891
```

监听端口被占用、来源白名单条目非法时只打印告警并**跳过该层**，不影响节点主功能
启动。凭据缺失**不再**导致跳过——那是自动生成的。

### 跨源（CORS）注意

面板是在**浏览器**里直接请求子节点的，属于跨源请求，预检会要求
`X-Node-Id` / `X-User-Token`（MSLX）或 `X-Api-Key`（MCSM）这些自定义头。
节点的 CORS 中间件已把这三个头加入允许清单（`corsAllowedHeaders`），
并且兼容层**不再**自己写 `Access-Control-Allow-Origin: *`——统一由中间件按
来源白名单下发，避免任意站点读取兼容层响应。若面板与节点不同源，务必让面板
的来源落在白名单内（环回 / 同主机名 / `corsOrigins`）。

### 安全须知

兼容层凭据等价于节点最高权限：可创建/删除实例、下发任意命令、读写实例工作目录
文件。因此：

- 务必使用长随机串，且不要与账户密码 / 其他服务复用；
- 公网暴露时建议同时设置 `allowPanelIps`（只放行面板出口 IP）；
- 兼容层端口默认是明文 HTTP；经公网暴露时建议前面套 TLS 反向代理（面板地址填
  `https`/`wss` 对应的域名即可），或只监听内网地址（`bind` 填内网 IP）；
- 兼容层流量同样经过节点的审计日志与 DDoS/CC 防御中间件（环回来源豁免限频）。

## 3. MCSManager 兼容层（默认端口 24444）

### 3.1 面板侧接入方式

MCSManager 面板 →「节点」→「新建节点」（面板只有地址、端口、密钥三栏）：

| 字段 | 填写 |
| --- | --- |
| 地址 | `<本机IP>`（例如 `192.168.1.5`；面板与节点同机可填 `127.0.0.1`） |
| 端口 | `24444`（与 `compat.mcsm.port` 一致） |
| 密钥 | `compat.mcsm.key` 的值（启动日志里也会打印） |

面板随后会：连接 `ws://<地址>:<端口>/socket.io` → 发 `auth` 事件校验密钥 →
拉 `info/overview` 与 `instance/*`。

### 3.2 传输层

- **engine.io v4**：HTTP 轮询（`GET` 长轮询取包 / `POST` 送包，`\x1e` 分隔多包）
  + WebSocket 升级（`2probe`/`3probe`/`5` 握手序列）。心跳由**服务端**负责：
  每 25s 下发 `2`（ping），客户端回 `3`（pong）；超过 `pingInterval+pingTimeout`
  没有任何 pong 即断开。纯 WebSocket 建连（`transports:["websocket"]`，面板的
  控制台流就是这种）同样走上这条心跳，不会出现「每 45s 断一次」的反复重连。
- **事件异步处理**：每个会话一个串行工作协程执行业务事件，读循环只解析帧与
  心跳（与 MCSM daemon 在 Node 事件循环里异步监听一致）。慢操作（大目录列表、
  压缩、下载）不会堵住同一会话的 ping/pong 与后续请求。
- **请求信封**：`{uuid, data}`，daemon 只处理其中的 `data`；但面板的
  **控制台流事件**（`stream/auth`、`stream/input`、`stream/write`、`stream/resize`、
  `stream/detail`）只发 `{data: {...}}`（没有 uuid 字段）。服务端按「有没有 `data` 键」
  拆包，两种形状都能正确取到内层载荷。
- **socket.io v4**：`0` 连接（服务端回 `40{"sid":...}`）、`1` 断开、`2` 事件、`3` 应答。
- 并发会话上限 256；轮询待发队列有界（满则丢包，绝不拖慢节点）。

### 3.3 事件（`emit(event, {uuid, data})` → 同名事件回 `{uuid, status, event, data}`）

已实现：

| 事件 | 说明 |
| --- | --- |
| `auth` | 节点密钥校验（载荷为裸字符串），成功后会话才可访问业务事件 |
| `info/overview` | 节点概览（版本、实例数、主机指标、`cpuMemChart` 系统曲线、配置块）。`cpuMemChart` 与 MCSM `VisualDataSubsystem` 同形：200 个 `{cpu,mem}` 整数百分比，每 3 秒一个采样点 |
| `info/setting` | 面板下发节点设置，本节点接受但忽略（无对应开关） |
| `instance/select` | 分页 + 关键字/状态/标签过滤的实例列表 |
| `instance/overview` / `instance/section` | 全部 / 指定 uuid 集合的实例概览 |
| `instance/detail` | 单实例详情（含 `processInfo`） |
| `instance/new` / `instance/update` / `instance/delete` | 实例增删改（`cwd` 会做绝对化与禁区校验，创建时补齐目录） |
| `instance/open` / `stop` / `restart` / `kill` | 批量实例操作（逐个执行并逐个回包） |
| `instance/command` | 下发标准输入命令 |
| `instance/forward` | 订阅/取消实例输出（推送 `instance/stdout`） |
| `instance/outputlog` | 控制台历史日志（内存环形缓冲） |
| `instance/process_config/list` / `instance/process_config/file` | 配置文件存在性检查与读/写（properties/json/txt 文本子集） |
| `passport/register` | 注册一次性通行证（下载/上传/控制台） |
| `stream/auth` / `stream/detail` / `stream/input` / `stream/write` / `stream/resize` | 控制台流通道 |
| `file/list` / `status` / `touch` / `mkdir` / `delete` / `copy` / `move` / `compress` | 文件管理 |
| `file/edit` | **无 `text` = 读取**（回包为文件内容字符串，上限 5 MiB，与 MCSM `MAX_EDIT_SIZE` 一致）；**有 `text`（含空串）= 写入**（回包 `true`）。面板「打开编辑器」发的就是不带 `text` 的 `file/edit` |
| `file/download_from_url` / `file/download_from_url_stop` | 从链接下载（仅 https，复用节点既有出站校验） |
| `java_manager/list` | 已安装的 Java 运行时（只读） |
| `upgrade/info` | 版本信息（明确不支持远程自更新） |
| `schedule/list` / `register` / `delete` | 计划任务：列表为空、注册明确拒绝 |
| `instance/asynchronous` 等 | 快速安装/更新类异步任务明确拒绝 |

服务端主动推送：`instance/stdout`、`instance/opened`、`instance/stopped`、
`instance/failure`（状态监视每 2s 比对一次实例状态）。

**明确不支持**（面板会看到中文错误提示，而不是静默成功）：Docker 环境管理、
Java 运行时下载/删除、远程自更新、MCSM 计划任务、快速安装、文件 chmod、
主机文件系统浏览。

### 3.4 HTTP 直连通道

| 路由 | 说明 |
| --- | --- |
| `GET /download/<通行证>/<文件名>` | 直连下载（通行证只允许下载注册时绑定的那个文件） |
| `POST /upload/<通行证>` | 直连上传（multipart，字段名 `file`） |
| `POST /upload-new/<通行证>?filename=&size=` | 分片上传会话（返回 `{data:{id}}`） |
| `POST /upload-piece/<id>?offset=` | 写入分片，覆盖完整后自动落盘（`unzip=true` 时解压） |
| `GET /` | 占位说明页（显示当前监听地址与密钥位置） |

## 4. MSLX 兼容层（默认端口 7891）

### 4.0 子节点链接（面板「节点管理 → 添加节点」）

MSLX 的节点纳管不是普通 REST 登录，而是**主节点与子节点的配对链接**（Protocol V1）。
本节点扮演「子节点」：

| 步骤 | 谁发起 | 接口 | 说明 |
| --- | --- | --- | --- |
| 1 | 主节点 | `GET {节点}/api/node/protocol` | 读协议号，必须是 `protocolVersion: 1` |
| 2 | 主节点 | `POST {节点}/api/node/accept-link` | 带 `linkKey`、`pairingId`、`masterUrl`、`masterId` |
| 3 | 子节点（本节点） | `GET {主节点}/api/node/protocol?pairingId=…` | 反向确认主节点认可本次配对（5 秒超时） |
| 4 | 本节点返回 | `{code:200,message:"链接成功",data:{nodeId,commsKey}}` | 关系落盘 `{data}/compat-mslx-node.json`（0600） |
| 5 | 本节点 | `POST {主节点}/api/node/report-stats` | 每 2 秒上报 `{cpu,memTotal,memUsed,memUsage,timestamp}`，头 `x-api-key: commsKey`；主节点回 `410` 表示关系撤销 → 自动清除本地链接。链接成功后**立即上报一次**，避免面板先闪一下离线 |
| 6 | 面板 | `GET {节点}/api/status`（头 `x-node-id`） | **面板判定子节点在线就靠这个探活**（3 秒超时）。因此该接口匿名可访问，但匿名只返回公开信息（版本 / nodeId / 链接状态）；主机指标与实例统计仅对通过本节点账户或主节点核验的调用返回 |
| 7 | 面板 | 其余 `{节点}/api/...` + `x-node-id` + 主节点签发的节点令牌 | 本节点把令牌回源到主节点 `POST {主节点}/api/node/verify-token`（头 `x-api-key: commsKey`）核对，通过后按该身份放行（结果缓存 30 秒） |

另有 `POST /api/node/update-link`（主节点同步地址变更）与 `POST /api/node/remove-link`
（幂等解除）；`GET /api/node/status`（需登录）可查看当前链接状态。

面板填写方式：**节点地址**填 `http://<本机IP>:7891`，**配对密钥**填 `compat.mslx.linkKey`
（启动日志会打印，也是配置文件里的值），**主节点地址**留空即面板自身地址。

### 4.1 WebPanel 侧接入方式

WebPanel 后端地址填 `http://<本机IP>:7891`（与 `compat.mslx.port` 一致），用 `compat.mslx.username` /
`compat.mslx.password` 登录。登录响应：

```json
{"code":200,"message":"登录成功","data":{"token":"<JWT>","expiresIn":86400,"user":{...}}}
```

后续请求携带 `x-user-token: <JWT>`（或 `access_token` / `token` 查询参数；
`Authorization: Bearer` 亦可用）。JWT 为 HS256、有效期 1 天；签名/过期校验全部
基于节点时间（`timesys`）。

### 4.2 响应信封

统一 `{code, message, data}`（camelCase）。成功 `code=200`；失败时 HTTP 状态与
`code` 一致（401/403/404/501…）。未实现接口返回 `501` 与中文说明。

### 4.3 已实现接口

| 路由 | 说明 |
| --- | --- |
| `GET /api/ping` | 存活探测（匿名） |
| `POST /api/auth/login` | 登录签发 JWT（匿名；5 分钟内 10 次失败封禁来源 60 分钟） |
| `GET /api/user/me`、`POST /api/user/logout` | 当前账号 / 退出（无服务端会话状态） |
| `POST /api/files/upload/{init,chunk/{id},status/{id},finish/{id},delete/{id}}` | 面板上传核心/整合包的分片通道：init 发 `uploadId`，chunk 为 multipart（`index`/`file`/可选 `crc32c`，逐片校验），finish 合并到 `{data}/compat-mslx-uploads/{id}.bin` 并按 `totalSize` 校验。**不校验 blake3**（避免引入新依赖），响应里 `blake3: null` |
| `POST /api/instance/createServer` | 面板「创建服务端」的内置接口（**不是插件**）。本节点同步建实例 + 后台准备目录/核心/eula，返回 `{serverId}`；`coreUrl` 走节点既有 SSRF 防护下载并可按 sha256 校验；`coreFileKey`/`packageFileKey` 取上面分片上传的结果；`packageUrl`/`packageLocalPath` 支持 zip 解压与 Modrinth `.mrpack`（按 `modrinth.index.json` 下载 + 应用 overrides，防目录穿越）；`mcdr=true` 时按需 `pip install mcdreforged`、生成 `config.yml` 并把启动命令改为 `<python> -m mcdreforged` |
| `POST /api/instance/delete` | 删除实例（运行中拒绝，`deleteFiles` 决定是否删目录） |
| `/api/instance/tasks/{list,list/{id},create,update,delete/{id}}` | 面板「定时任务」：5 段 cron（分 时 日 月 周，支持 `*`/`a`/`a-b`/`*/n`/`a-b/n`/列表/月份与星期英文名，日与周同时限制取「或」）；类型 `command`（发控制台指令）、`start`/`stop`/`restart`、`backup`（实例快照）、`shell`（在实例目录执行命令，Windows 走临时 .bat 避免引号转义）；任务表持久化 `{data}/compat-mslx-tasks.json`，调度循环 20 秒一跳、按分钟去重、`runWhenOffline=false` 时实例未运行则跳过 |
| `instance/asynchronous`、`instance/query_asynchronous`、`instance/stop_asynchronous`（面板 `/api/protected_instance/asynchronous`） | 面板「服务端模板市场」安装：下载模板包 → 解压到实例目录 → 读包内 `mcsmanager-config.json`（或面板传的 `setupInfo`）→ 建实例；任务状态 0/1/-1，`detail.downloadProgress` 带进度；`install_instance` 在已有实例内解压、`update` 执行实例的 `updateCommand`；下载走节点既有 SSRF 守卫 |
| `schedule/register`、`schedule/list`、`schedule/delete`（面板 `/api/protected_schedule`） | MCSManager 面板「计划任务」：`type` 1=间隔（`time` 秒，最小 3，`count=-1` 无限）/ 2=cron（**6 段含秒**，同 node-schedule）/ 3=指定时间（面板同样拼 **6 段 cron**：`秒 分 时 日 月 *`，命中一次即执行；手工调用也兼容 `2006-01-02 15:04:05`）；动作 `delay`/`command`/`stop`/`start`/`restart`/`kill`，单个任务最多 10 个动作、同实例最多 8 个任务、同名视为更新；任务表持久化 `{data}/compat-mcsm-schedules.json`，秒级调度，执行后按 `count` 递减并在归零时自动删除 |
| `GET/POST/DELETE /api/tasks` | 面板任务中心：创建任务以 `state=2 完成` 呈现，供面板停止轮询；支持取消/删除/清空已完成 |
| `GET /api/plugins/list`（含 `/api/plugins`） | **返回空列表**（200）。MSLX 面板打开「服务端列表」前会先拉它注册插件页面，失败会整页报「目标节点插件加载失败」并挡住实例操作。本节点不提供 .NET 插件运行时，空列表即事实 |
| `POST /api/plugins/{action,install,upload,…}` | 明确 501 + 中文说明（不静默成功、也不假装装上了） |
| `GET /api/status` | 节点运行状态。**匿名可访问**（面板探活用）：匿名只返回版本 / nodeId / 链接状态；认证或主节点核验后返回主机指标与实例统计 |
| `GET /api/node/protocol` | 协议标识（匿名，供面板识别） |
| `GET /api/update/info` | 版本信息（不支持自更新） |
| `GET /api/instance/list` | 实例列表（`id/name/basePath/status/statusText/…`） |
| `GET /api/instance/info?id=` | 实例详情 |
| `POST /api/instance/action` | `{id, action}`：`start`/`stop`/`restart`/`forceExit`/`agreeEula` |
| `GET|POST /api/instance/settings/general/{id}` | 实例设置读取/保存（`name`/`startCommand`/`stopCommand`/`basePath`/`autoStart`…） |
| `GET /api/instance/backups/all` | 备份列表（始终为空） |
| `GET /api/files/instance/{id}/lists` | 目录列表（`pageSize=0` 为全量；支持 `search/sort/order`） |
| `GET|POST /api/files/instance/{id}/content` | 读取/写入文本文件（≤8MB） |
| `POST /api/files/instance/{id}/directory` | 新建目录 `{path, name}` |
| `POST /api/files/instance/{id}/delete` | 删除 `{paths:[...]}` |
| `POST /api/files/instance/{id}/rename` | 重命名 `{oldPath, newPath}` |
| `POST /api/files/instance/{id}/copy` / `move` | 批量复制/移动 `{sourcePaths, targetPath}` |
| `POST /api/files/instance/{id}/upload` | 一次性上传（multipart `file`） |
| `GET /api/files/instance/{id}/download?path=` | 下载（`inline=true` 内联） |
| `GET /api/settings` | 节点设置（只读） |
| `GET /api/java/list` | 已安装运行时（只读） |

**未实现**（返回 501 + 中文说明）：Docker 管理、插件、FRP、资源搜索、计划任务、
玩家管理、实例创建/删除/导出、主机文件系统浏览、节点主从互联、面板发起备份。

### 4.4 SignalR 通道（`/api/hubs/...`）

只实现本场景必需的 JSON 协议子集（0x1e 记录分隔、`type` 1/3/6）：

| Hub | 方法 | 服务端推送 |
| --- | --- | --- |
| `instanceControlHub` | `JoinGroup`、`LeaveGroup`、`SendCommand`、`JoinPtyGroup`、`LeavePtyGroup`、`SendPtyInput`、`ResizePty`、`GetPtyStatus`、`ResumeConsole` | `ReceiveLog`、`ReceiveStatus`、`PtyStatus`、`CommandResult` |
| `system` | `JoinMonitor`、`LeaveMonitor` | `ReceiveSystemStats`（`{local, slaves}`） |

连接流程：`POST /api/hubs/{hub}/negotiate?negotiateVersion=1`（凭 JWT）拿到
`connectionToken` → `GET /api/hubs/{hub}?id=<token>` 升级 WebSocket → 发送
`{"protocol":"json","version":1}\x1e` → 服务端回 `{}\x1e`。

本节点无 PTY，`PtyStatus.isPty` 恒为 `false`、`enablePty` 恒为 `false`，
`SendPtyInput` 按行写入标准输入。

### 4.5 实例整数 id

MSLX 用递增整数表示实例。映射持久化在 `{data}/compat-mslx-ids.json`
（`{"next":N,"ids":{"<uuid>":id}}`），因此重启后 id 保持不变。

### 异步模型（为什么改）

MCSM 兼容层的传输层全部自己实现，早期的「会话串行队列 + 满则同步执行」在
高频场景下会把读循环拖死（一个慢的 `file/list` 会让同会话的 ping/pong 与后续
请求一起排队）。现在分三层异步：

| 层 | 机制 | 效果 |
| --- | --- | --- |
| 任务队列 | 环形缓冲（初始 256，按需翻倍到 8192），入队 O(1)、满则丢最旧并计数，**永不阻塞、永不内联执行** | 单协程入队 ≈28 ns/op，18 协程并发 ≈81 ns/op（改造前并发入队 14.7 µs/op，因为队满要 copy 上千元素） |
| 通道分离 | **有序通道**：有状态事件（auth、instance/action、instance/new、stream/*），单协程保序；**并发通道**：只读/幂等事件（file/list、file/edit、info/* 等），4 个协程并行、多槽唤醒、逐个出队 | 慢的有状态事件不再挡住只读请求与状态推送 |
| WS 出站 | 每连接一个写协程 + 有界出站队列（2048 帧），同一批多帧合并成一个 WS 文本帧（engine.io 允许帧内以 `\x1e` 分隔多包）；慢客户端只丢自己的帧 | syscall 从「一包一次」降到「一批一次」，慢客户端不阻塞业务协程 |

> 面板会把「没有值」的可选参数序列化成字符串 `"undefined"`（实测 `file/list` 的
> `fileName` 恒为 `"undefined"`）。把它当关键字过滤会导致**文件管理里目录永远是空的**，
> 兼容层对这类值按空处理。

轮询 GET 另加 300 ms 兜底：`notify` 只有 1 个槽位，多等待者可能漏唤醒，
兜底轮询既不空等 25 s 也不增加可见延迟。

MSLX 的 SignalR 通道按同一套思路改造：

| 层 | 机制 |
| --- | --- |
| 调用分派 | 读循环只解析记录并入队（非阻塞队列），由每连接一个工作协程按到达顺序执行 `handleInvocation`；`JoinGroup`→`SendCommand` 的顺序语义不变 |
| 出站 | 每连接一个出站队列，同一时刻的多条记录合并成一个 WS 文本帧；**每条记录都以 `0x1e` 结尾**（SignalR JSON 协议的分帧方式，join 与 end 都是分隔符——只给 end 会把两条记录粘在一起） |
| 控制台游标 | 面板 v1.7.6+ 用带游标的控制台：`ReceiveLog(text, {streamId,sequence})` 与 `ResumeConsole(instanceId, logCursor, ptyCursor, pty, cols, rows)`。**不带游标的 `ReceiveLog` 会被前端直接丢弃**；`ResumeConsole.log` 必须是 `{streamId,sequence,gap,frames[]}` 对象（早先返回字符串会让前端抛 `Cannot read properties of undefined (reading 'length')`）。每个实例维护一个输出流（2000 帧环形、进程重启换 `streamId`），回放算法与 MSLX 的 `ConsoleOutputBuffer.Replay` 一致 |
| 系统监视 | 全局采样器每 3 秒采样一次并广播给所有在监视的连接（原来每个连接各自 ticker + 各自 sysinfo，N 个标签页就采 N 次）；连接加入时先推一份，面板不用等第一个周期 |

## 5. 兼容层的范围与边界（已确定）

**目标**：让现成的第三方面板（MCSManager / MSLX）能像管自己家的 daemon 一样管本节点，
协议、字段、时序对齐，业务实现全部是本节点自己的代码。

**覆盖到了**：节点接入与探活、实例的增删改查与启停强杀、实时控制台（含 Ctrl+C）、
文件管理（含读取/编辑/上传/下载）、按端口/地址的节点纳管与配对（LinkKey）、
状态与曲线上报、任务中心、分片上传、整合包（zip / Modrinth mrpack）部署、
MCDReforged 部署、SignalR 控制台与系统监视。

**明确不做的**：

| 不做的部分 | 原因 | 表现 |
| --- | --- | --- |
| MSLX 第三方插件运行时（.NET DLL 宿主） | 需要复刻一个 .NET 插件宿主（加载 DLL、SDK、前端扩展注入），与「兼容层」不是一个量级 | `GET /api/plugins/list` 返回空列表（面板按「未安装插件」正常渲染）；插件写操作返回 501 + 中文说明 |
| 面板侧的「社区插件市场」 | 同上，且属对方商业/社区生态 | 不请求、不代理 |
| MCSM 的 Docker 实例、远程自更新 | 本节点不提供容器运行时与自更新 | 对应事件返回明确错误（不静默成功） |
| MSLX 的 Docker / FRP / 定时任务等面板模块 | 本节点无对应能力 | 返回 501 + 中文说明 |

原则：**能做的一律按对方协议真做**；做不了的明确报错并说明原因，绝不「假装成功」。

## 6. 数据目录新增文件

| 文件 | 说明 |
| --- | --- |
| `{data}/compat-secrets.json` | 自动生成的兼容层凭据（`mcsmKey` / `mslxPassword` / `mslxJwtSecret`，0600 原子写）。配置里留空时以此为准；旧版本的 `{data}/compat-mslx-secret` 仍会被读取兼容 |
| `{data}/compat-mslx-ids.json` | 实例 uuid ↔ 整数 id 映射 |

## 7. 测试

| 文件 | 覆盖 |
| --- | --- |
| `qa_compat_mcsm_test.go` | engine.io 握手/心跳、socket.io 连接、密钥认证与拒绝、实例增删启停杀、文件事件与越界拒绝、直连下载通行证绑定、未启用时不开监听、来源白名单与 UA 封禁（独立端口上的中间件生效）、两层端口隔离、监听地址组装 |
| `qa_compat_mslx_test.go` | 登录/JWT（含伪造令牌与 `access_token` 传参）、实例列表/详情/动作、文件全套操作、Hub 协商、未实现接口 501、未启用 404、**两层同时开启** |
| `qa_compat_mslx_ws_test.go` | SignalR WebSocket 端到端：握手、`JoinGroup`、`ReceiveLog` 实时推送、`SendCommand` 应答、系统监视推送 |

单独运行：

```bash
go test ./cmd/irix-node -run TestCompat -count=1
```

## 8. 用真实面板/daemon 验收

自造用例只能证明「协议解析对」，证明不了「真程序能用」。这里用两边的**官方发行版**
跑端到端验收，结果与复现步骤见 `docs/compat-e2e-verification.md`，脚本在
`scripts/e2e/`（`mcsm-panel.mjs`、`mslx-master.mjs`）。

真机跑出来的问题往往在「形状细节」上，例如：
请求包只有 `{uuid}` 没有 `data` 时不能丢 uuid；engine.io 的 WS 传输一帧只能一个包
（`\x1e` 多包只适用于轮询）；面板不等 `auth` 回包就连发请求；停止要立刻回包不能
同步等进程退出。这些都已在代码里修掉并补了回归用例
（`cmd/irix-node/qa_compat_mcsm_regress_test.go`）。

## 9. 排错

| 现象 | 排查 |
| --- | --- |
| 面板连不上 / 握手失败 | 确认面板端口填的是 `compat.mcsm.port`（默认 24444）而不是节点主端口 12346；确认 `bind` 可被面板访问、防火墙放行该端口 |
| 面板提示密钥错误 | 启动日志会打印 `来源 … 密钥错误，认证失败`；当前实际密钥见启动日志或 `{data}/compat-secrets.json` |
| 兼容层未启用 / 未监听 | 启动日志会有 `警告: … 兼容层未启动：监听 <addr> 失败（…）；端口可能被占用`——改 `port` 或释放端口即可；`allowPanelIps` 条目非法也会跳过该层 |
| 想换密钥/密码 | 把配置里对应字段置为 `""` 并删除 `{data}/compat-secrets.json`，重启后自动生成新值；或直接填自己的值 |
| 401 / 403 | MSLX 侧 token 过期（1 天）需重新登录；`allowPanelIps` 不包含面板出口 IP；登录失败过多进入封禁期 |
| 面板功能点击报"未实现" | 该功能在本文档第 3.3 / 4.3 节的"明确不支持"清单内，属预期行为 |
