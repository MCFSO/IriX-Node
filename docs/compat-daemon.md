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
| `allowPanelIps` | 允许的面板来源（裸 IP 或 CIDR，数组）；留空 = 不限制来源 |
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
  + WebSocket 升级（`2probe`/`3probe`/`5` 握手序列）；服务端每 25s 下发 `2`（ping），
  客户端回 `3`（pong）；20s 未回 pong 视为断线。
- **socket.io v4**：`0` 连接（服务端回 `40{"sid":...}`）、`1` 断开、`2` 事件、`3` 应答。
- 并发会话上限 256；轮询待发队列有界（满则丢包，绝不拖慢节点）。

### 3.3 事件（`emit(event, {uuid, data})` → 同名事件回 `{uuid, status, event, data}`）

已实现：

| 事件 | 说明 |
| --- | --- |
| `auth` | 节点密钥校验（载荷为裸字符串），成功后会话才可访问业务事件 |
| `info/overview` | 节点概览（版本、实例数、主机指标、配置块） |
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
| `file/list` / `status` / `touch` / `mkdir` / `delete` / `edit` / `copy` / `move` / `compress` | 文件管理 |
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
| `GET /api/status` | 节点运行状态与主机指标 |
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

## 5. 数据目录新增文件

| 文件 | 说明 |
| --- | --- |
| `{data}/compat-secrets.json` | 自动生成的兼容层凭据（`mcsmKey` / `mslxPassword` / `mslxJwtSecret`，0600 原子写）。配置里留空时以此为准；旧版本的 `{data}/compat-mslx-secret` 仍会被读取兼容 |
| `{data}/compat-mslx-ids.json` | 实例 uuid ↔ 整数 id 映射 |

## 6. 测试

| 文件 | 覆盖 |
| --- | --- |
| `qa_compat_mcsm_test.go` | engine.io 握手/心跳、socket.io 连接、密钥认证与拒绝、实例增删启停杀、文件事件与越界拒绝、直连下载通行证绑定、未启用时不开监听、来源白名单与 UA 封禁（独立端口上的中间件生效）、两层端口隔离、监听地址组装 |
| `qa_compat_mslx_test.go` | 登录/JWT（含伪造令牌与 `access_token` 传参）、实例列表/详情/动作、文件全套操作、Hub 协商、未实现接口 501、未启用 404、**两层同时开启** |
| `qa_compat_mslx_ws_test.go` | SignalR WebSocket 端到端：握手、`JoinGroup`、`ReceiveLog` 实时推送、`SendCommand` 应答、系统监视推送 |

单独运行：

```bash
go test ./cmd/irix-node -run TestCompat -count=1
```

## 7. 排错

| 现象 | 排查 |
| --- | --- |
| 面板连不上 / 握手失败 | 确认面板端口填的是 `compat.mcsm.port`（默认 24444）而不是节点主端口 12346；确认 `bind` 可被面板访问、防火墙放行该端口 |
| 面板提示密钥错误 | 启动日志会打印 `来源 … 密钥错误，认证失败`；当前实际密钥见启动日志或 `{data}/compat-secrets.json` |
| 兼容层未启用 / 未监听 | 启动日志会有 `警告: … 兼容层未启动：监听 <addr> 失败（…）；端口可能被占用`——改 `port` 或释放端口即可；`allowPanelIps` 条目非法也会跳过该层 |
| 想换密钥/密码 | 把配置里对应字段置为 `""` 并删除 `{data}/compat-secrets.json`，重启后自动生成新值；或直接填自己的值 |
| 401 / 403 | MSLX 侧 token 过期（1 天）需重新登录；`allowPanelIps` 不包含面板出口 IP；登录失败过多进入封禁期 |
| 面板功能点击报"未实现" | 该功能在本文档第 3.3 / 4.3 节的"明确不支持"清单内，属预期行为 |
