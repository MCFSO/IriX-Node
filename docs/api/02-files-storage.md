# 文件管理与存储 API 契约

> 适用后端：`IriX-Node`（纯 Go 标准库守护进程）
> 本文档只覆盖「文件管理与存储」相关端点，供 React 面板前端直接调用。所有字段名、路径、参数均与 Go 源码中的 JSON tag / 路由注册一字不差。

---

## 1. 通用约定

### 1.1 基础地址

- 节点守护进程监听地址（示例）：`http://127.0.0.1:23333`
- 下文所有路径都相对于该地址拼接。

### 1.2 认证

- `/api/` 开头的接口需要认证，二选一：
  - 查询参数 `?apikey=<key>`
  - 请求头 `X-Api-Key: <key>`
- `-apikey` 为空时，节点校验配对码（首次启动生成的 20 位随机码，哈希持久化）。
- **例外**：直连通道 `GET /download/...` 与 `POST /upload/...` 不经过 API 认证，票据密码本身就是凭证。

### 1.3 统一响应封装

所有 `/api/` 接口（成功与失败）都返回 MCSM 风格的三字段 JSON：

```json
{
  "status": 200,
  "data": <任意 JSON>,
  "time": 1710000000000
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | int | 同时是「JSON 字段」与「真实 HTTP 状态码」。成功固定为 `200` |
| `data` | any | 业务载荷，类型随端点不同 |
| `time` | int64 | 响应时间，Unix 毫秒 |

### 1.4 错误响应

错误响应**同样**使用上述封装，`data` 是一个中文字符串错误消息，`status` 为该错误的 HTTP 状态码：

```json
{
  "status": 400,
  "data": "实例不存在: xxx",
  "time": 1710000000000
}
```

常见状态码语义：

| status | 场景 |
| --- | --- |
| `400` | 参数缺失/非法、JSON 解析失败、实例不存在、路径越界、目录按文本读取、文件过大等 |
| `403` | 票据无效/过期/越权（直连通道） |
| `404` | 资源不存在（如卸载未安装的 JDK） |
| `500` | 服务端 IO 失败 |
| `503` | 票据已满（超过 10000 张） |

> **注意**：`GET /download/...`、`POST /upload/...` 直连通道的错误是**纯文本**（`http.Error`），不是上述 JSON 封装。

### 1.5 请求体限制

- `/api/` 请求体上限 **16 MiB**（超出被中间件拒绝）。
- `/upload/{password}` 直连上传**不受 16 MiB 限制**：multipart 内存阈值 32 MiB，超出部分由标准库落临时文件，内存占用恒定。

### 1.6 路径规范（`target` / `file_name` / `targets` 内的路径）

所有文件路径都经过 `NormalizePath` 归一化，规则：

- 以 `/` 开头 → 表示实例工作目录（cwd）根，例如 `/server.properties`。
- 普通相对路径 → 相对 cwd，例如 `plugins/config.yml`。
- 任何 `..` 越界都会被拒绝（`400 路径越界: ...`）。
- 空字符串 `target` → 归一化为 cwd 本身。

所有文件操作**严格限定在实例 cwd 内**。

### 1.7 实例标识 `uuid`

- 绝大多数文件接口用查询参数 `?uuid=<实例UUID>` 定位实例（进而得到 cwd）。
- 回收站 / 备份 / 快照 / 恢复 / 核心下载接口把 `uuid` 放在**请求体**里。
- `uuid` 不存在或实例 cwd 为空 → `400 实例不存在: <uuid>` 或 `400 实例工作目录为空`。

### 1.8 `daemonId` 字段

MCSM 兼容的冗余字段：多数接口在查询串或请求体里都接受 `daemonId`，但**后端不读取、不校验**，前端可按需携带或省略。

### 1.9 异步任务进度结构（通用）

耗时操作（快照 / 恢复 / 核心下载 / JDK 安装）统一任务化：

1. 发起接口返回 `{ "jobId": "<uuid>" }`；
2. 前端轮询对应的进度接口，参数 `?jobId=<jobId>`；
3. 进度响应 `data` 结构如下（`snapshot()` 快照）：

```json
{
  "status": "running",
  "percent": 0.5,
  "message": "压缩中…",
  "path": "D:\\...\\server.jar"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | string | `"running"`（执行中）\| `"done"`（完成）\| `"failed"`（失败） |
| `percent` | number | `0.0 ~ 1.0`；`-1` 表示未知进度（创建瞬间/失败） |
| `message` | string | 中文进度消息；失败时为错误原因 |
| `path` | string | **可选**，产物路径（如下载完成的文件、安装后的 `bin/java`）；为空时不出现 |

> 任务上限 1024（超出淘汰最旧），已完成/失败任务保留 2 小时后清理。`jobId` 不存在 → `400 任务不存在或已过期`。

---

## 2. 端点总表

| 方法 | 路径（Go 1.22 路由模式） | 说明 |
| --- | --- | --- |
| `GET` | `/api/files/list` | 文件列表 |
| `PUT` | `/api/files/` | 读写文件内容（末尾斜杠必须） |
| `DELETE` | `/api/files` | 永久删除文件/目录 |
| `PUT` | `/api/files/move` | 移动 / 重命名 |
| `POST` | `/api/files/copy` | 复制 |
| `POST` | `/api/files/compress` | 压缩 / 解压 |
| `POST` | `/api/files/mkdir` | 新建目录 |
| `POST` | `/api/files/touch` | 新建空文件 |
| `POST` | `/api/files/download` | 申请下载票据 |
| `POST` | `/api/files/upload` | 申请上传票据 |
| `GET` | `/download/{password}/{path...}` | 直连下载（无认证） |
| `POST` | `/upload/{password}` | 直连上传 multipart（无认证） |
| `POST` | `/api/files/trash` | 移入回收站 |
| `GET` | `/api/files/trash/list` | 回收站列表 |
| `POST` | `/api/files/trash/restore` | 从回收站恢复 |
| `POST` | `/api/files/trash/empty` | 永久删除回收站内容 |
| `POST` | `/api/instance/snapshot` | 创建实例快照（备份） |
| `GET` | `/api/instance/snapshot-progress` | 快照 / 恢复进度 |
| `POST` | `/api/instance/restore` | 从备份恢复 |
| `GET` | `/api/instance/backups` | 备份列表 |
| `DELETE` | `/api/instance/backups` | 删除备份 |
| `POST` | `/api/instance/backups/download` | 申请备份下载票据 |
| `POST` | `/api/instance/download-core` | 服务端下载核心 jar |
| `GET` | `/api/instance/download-core-progress` | 核心下载进度 |
| `GET` | `/api/runtime/java` | 检测 Java 运行时 |
| `POST` | `/api/runtime/java/install` | 安装 JDK |
| `GET` | `/api/runtime/java/install-progress` | JDK 安装进度 |
| `DELETE` | `/api/runtime/java` | 卸载 JDK |

---

## 3. 文件列表与读写

### 3.1 `GET /api/files/list` — 文件列表

路由模式：`GET /api/files/list`

#### 查询参数

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | ✅ | — | 实例 UUID |
| `daemonId` | ❌ | — | 忽略 |
| `target` | ❌ | cwd 根 | 要列出的目录（相对 cwd；`/` 前缀 = cwd 根） |
| `page` | ❌ | `1` | 页码，**1 起始** |
| `page_size` | ❌ | `100` | 每页条数 |

#### 响应 `data`

```json
{
  "items": [
    {
      "name": "server.properties",
      "size": 1024,
      "time": "Mon Jan 02 2006 15:04:05 GMT+0800 (中国标准时间)",
      "mtime": "2006-01-02 15:04:05",
      "mode": 420,
      "type": 1,
      "sha256": "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
    },
    {
      "name": "plugins",
      "size": 0,
      "time": "Mon Jan 02 2006 15:04:05 GMT+0800 (中国标准时间)",
      "mtime": "2006-01-02 15:04:05",
      "mode": 493,
      "type": 0,
      "sha256": ""
    }
  ],
  "page": 0,
  "pageSize": 100,
  "total": 2,
  "absolutePath": "/"
}
```

#### 顶层字段

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `items` | array | 当前页条目数组 |
| `page` | int | **0 起始**的回传页码（= 请求 `page - 1`） |
| `pageSize` | int | 每页条数（回传 `page_size`） |
| `total` | int | 分页前的总条目数 |
| `absolutePath` | string | 当前目录相对 cwd 的路径（`/` 前缀，`/` 分隔）；根目录为 `"/"` |

#### 文件条目字段（`items[i]`）

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `name` | string | 文件/目录名（basename） |
| `size` | int64 | 大小（字节） |
| `time` | string | 展示用修改时间，格式 `Mon Jan 02 2006 15:04:05 GMT+0800 (中国标准时间)` |
| `mtime` | string | 增量同步用修改时间，格式 `2006-01-02 15:04:05` |
| `mode` | int | 权限位（`int(info.Mode().Perm())`，例如 `0644` → `420`，`0755` → `493`） |
| `type` | int | `0` = 目录，`1` = 文件 |
| `sha256` | string | 文件内容 SHA-256（小写十六进制）；**目录为空字符串** `""` |

#### 排序

目录（`type=0`）在前，文件（`type=1`）在后；同类内按 `name` 字典序升序。

#### 错误

- `400 实例不存在: <uuid>` / `400 实例工作目录为空`
- `400 路径越界: <target>`

> 隐藏文件：本实现**没有**隐藏文件过滤参数，`os.ReadDir` 会返回 `.` 开头文件（包括回收站目录 `.irix-trash`），前端如需隐藏需自行过滤 `name` 以 `.` 开头的条目。

---

### 3.2 `PUT /api/files/` — 读取 / 写入文件内容

路由模式：`PUT /api/files/`（**末尾斜杠必须**；`PUT /api/files/move` 是独立路由，由 Go 1.22 更具体的模式优先匹配）。

#### 查询参数

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | ✅ | 实例 UUID |
| `daemonId` | ❌ | 忽略 |

#### 请求体

```json
{
  "target": "server.properties",
  "text": "motd=hello\n"
}
```

| 字段 | 类型 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- | --- |
| `target` | string | ✅ | — | 目标文件路径（相对 cwd） |
| `text` | string \| null | ❌ | — | 文件内容（见下方模式说明） |

#### 读取 / 写入判定（`text` 用指针区分）

| 请求体 | 行为 | 响应 `data` |
| --- | --- | --- |
| 只有 `target`（**不含** `text` 字段） | **读取**：返回文件内容 | `string`（文件文本内容） |
| 含 `text`（可为空串 `""`） | **写入**：把 `text` 写入文件，父目录自动创建（`0755`），文件权限 `0644` | `true` |

读取响应示例：

```json
{ "status": 200, "data": "motd=hello\n", "time": 1710000000000 }
```

写入响应示例：

```json
{ "status": 200, "data": true, "time": 1710000000000 }
```

#### 二进制处理与限制（重要）

- 这是**文本接口**：文件整体读入内存 → 转成 Go `string` → JSON 编码。
- 单文件读取上限 **8 MiB**（`maxTextReadBytes = 8 << 20`）。
- 超过 8 MiB → `400 文件过大（...），文本读取上限为 ...，请改用下载接口`。
- 目标是目录 → `400 目标为目录，无法按文本读取`。
- **二进制 / 大文件不要走此接口**，请用 §5 的票据直连下载 / 上传（流式、内存恒定）。

#### 错误

- `400 读取请求体失败: ...` / `400 请求体格式错误: ...`
- `400 实例不存在: <uuid>` / `400 实例工作目录为空`
- `400 路径越界: <target>`
- `400 读取文件失败: ...`（文件不存在等）
- `400 目标为目录，无法按文本读取`
- `400 文件过大（...），文本读取上限为 8.0 MB，请改用下载接口`
- `500`（写入/创建失败）

---

## 4. 文件操作

> 以下接口除回收站外，都用查询参数 `?uuid=` 定位实例；请求体结构如下。均返回 `data: true` 表示成功。

### 4.1 `DELETE /api/files` — 永久删除

路由模式：`DELETE /api/files`

查询参数：`uuid`（✅）、`daemonId`（❌）。

请求体：

```json
{ "targets": ["a.txt", "world/"] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `targets` | string[] | ✅ | 待删除路径列表（相对 cwd） |

响应：`data: true`。

说明：`os.RemoveAll` 递归删除，**不可恢复**（如需回收站请用 §6 的 `POST /api/files/trash`）。任一目标失败即整体报错。

### 4.2 `PUT /api/files/move` — 移动 / 重命名

路由模式：`PUT /api/files/move`

查询参数：`uuid`（✅）。

请求体：

```json
{ "targets": [["old/server.properties", "new/server.properties"]] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `targets` | string[][] | ✅ | `[源, 目标]` 二元组数组 |

响应：`data: true`。

说明：`os.Rename`。长度 ≠ 2 的组被跳过；首个错误即中止并返回 `500 移动失败: ...`。

### 4.3 `POST /api/files/copy` — 复制

路由模式：`POST /api/files/copy`

查询参数：`uuid`（✅）。

请求体：

```json
{ "targets": [["world", "world-backup"]] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `targets` | string[][] | ✅ | `[源, 目标]` 二元组数组 |

响应：`data: true`。

说明：递归复制（目录整树复制）。首个错误即中止并返回 `500 复制失败: ...`。

### 4.4 `POST /api/files/compress` — 压缩 / 解压

路由模式：`POST /api/files/compress`

查询参数：`uuid`（✅）。

请求体：

```json
{
  "type": 1,
  "code": "utf-8",
  "source": "backup.zip",
  "targets": ["world/", "server.properties"]
}
```

| 字段 | 类型 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- | --- |
| `type` | int | ✅ | — | `1` = 压缩，`2` = 解压。其它值 → `400 type 仅支持 1=压缩, 2=解压` |
| `code` | string | ❌ | — | 编码标识；**当前实现接受但忽略**（zip 固定 UTF-8） |
| `source` | string | ✅ | — | 压缩：产物 zip 路径；解压：待解压 zip 路径（均相对 cwd） |
| `targets` | string[] | 压缩必填 / 解压可选 | 解压为空 = zip 同目录 | 压缩：要打包的文件/目录列表；解压：取 `targets[0]` 作为解压目标目录 |

#### `type` 语义

| `type` | 语义 | `source` 含义 | `targets` 含义 |
| --- | --- | --- | --- |
| `1` | 压缩 | 目标 zip 文件路径 | 要打包的文件/目录（目录递归，zip 内保留相对 cwd 的路径） |
| `2` | 解压 | 待解压 zip 路径 | 可选，`targets[0]` = 解压目标目录；缺省解压到 zip 所在目录 |

响应：`data: true`。

错误：

- `400 type 仅支持 1=压缩, 2=解压`（type 非法）
- `400 路径越界: <target>`（source/target 越界）
- `500 压缩失败: ...`
- `500 解压失败: ...`（含 zip slip 防御：`解压失败: 解压路径越界: <name>`）

> vaultFiles（加密文件区）且实例停止态时，压缩/解压**不支持**，返回 `400 该实例文件区已加密且未运行，压缩/解压暂不支持（请先启动实例，或改用文件读写接口）`。

### 4.5 `POST /api/files/mkdir` — 新建目录

路由模式：`POST /api/files/mkdir`

查询参数：`uuid`（✅）。

请求体：

```json
{ "target": "plugins/sub" }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `target` | string | ✅ | 目录路径（相对 cwd） |

响应：`data: true`。递归创建（`MkdirAll 0755`）。

### 4.6 `POST /api/files/touch` — 新建空文件

路由模式：`POST /api/files/touch`

查询参数：`uuid`（✅）。

请求体：

```json
{ "target": "new.txt" }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `target` | string | ✅ | 文件路径（相对 cwd） |

响应：`data: true`。父目录自动创建，文件权限 `0644`。

---

## 5. 下载 / 上传票据直连通道

### 5.1 流程总览

1. 前端先向节点申请票据（受 API 认证保护），拿到 `{password, addr}`；
2. 用 `addr` 拼出直连地址，用 `password` 做票据，直接 `GET /download/{password}/...` 或 `POST /upload/{password}` 传输；
3. 直连通道**不带 apikey**，票据密码即凭证。

票据属性：

- 有效期 **10 分钟**；
- 全局上限 **10000** 张，满则 `503 下载/上传票据已满，请稍后重试`；
- 每分钟清理过期票据；
- 下载票据**绑定单个文件**（只能下载申请时那个文件）；上传票据**绑定目标目录**；
- 下载 / 上传票据**类型严格区分**，不能混用。

### 5.2 `POST /api/files/download` — 申请下载票据

路由模式：`POST /api/files/download`

#### 查询参数

| 参数 | 必填 | 含义 |
| --- | --- | --- |
| `uuid` | ✅ | 实例 UUID |
| `file_name` | ✅ | 要下载的文件路径（相对 cwd） |
| `daemonId` | ❌ | 忽略 |

无请求体。

#### 响应 `data`

```json
{
  "password": "3f2a1b9e-...-...",
  "addr": "127.0.0.1:23333"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `password` | string | 票据密码（UUID v4），用于直连 URL |
| `addr` | string | 直连下载用的 `host:port` |

#### 错误

- `400 缺少 file_name 参数`
- `400 文件不存在: <file_name>`（不存在或是目录）
- `503 下载票据已满，请稍后重试`

### 5.3 `GET /download/{password}/{path...}` — 直连下载

路由模式：`GET /download/`（子路径模式）。

URL 形态：`GET /download/{password}/{fileName}`（`{fileName}` 可含子路径，如 `sub/dir/a.txt`）。

- `{password}`：来自申请接口的 `password`。
- `{path...}`：相对票据 cwd 的文件路径。对实例下载票据，即相对实例 cwd 的路径；对集群票据会先剥离 `mirrors/` 前缀。

#### 成功响应

- HTTP `200`，响应体为文件字节流（`http.ServeFile` 流式传输）；
- 响应头 `Content-Disposition: attachment; filename="<basename>"`。

#### 错误（纯文本，非 JSON 封装）

| status | 文本 |
| --- | --- |
| `400` | `无效的下载链接`（路径段不完整） |
| `403` | `下载票据无效或已过期` |
| `403` | `下载票据仅限绑定文件`（单文件票据下载了别的路径） |
| `403` | `路径越界` |
| `404` | 文件不存在（`http.ServeFile` 默认输出） |

### 5.4 `POST /api/files/upload` — 申请上传票据

路由模式：`POST /api/files/upload`

#### 查询参数

| 参数 | 必填 | 默认 | 含义 |
| --- | --- | --- | --- |
| `uuid` | ✅ | — | 实例 UUID |
| `upload_dir` | ❌ | `/` | 上传目标目录（相对 cwd；`/` = cwd 根） |
| `daemonId` | ❌ | — | 忽略 |

无请求体。

#### 响应 `data`

```json
{
  "password": "3f2a1b9e-...-...",
  "addr": "127.0.0.1:23333",
  "upload_dir": "/"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `password` | string | 票据密码 |
| `addr` | string | 直连上传用的 `host:port` |
| `upload_dir` | string | 回传的实际目标目录 |

### 5.5 `POST /upload/{password}` — 直连上传（multipart）

路由模式：`POST /upload/`（子路径模式）。

URL 形态：`POST /upload/{password}`

#### 请求

- `Content-Type: multipart/form-data`
- 表单字段名：**`file`**（唯一字段，值为文件内容）
- 客户端上传的文件名会取 basename（丢弃路径部分），写入 `票据目录 + basename`。

#### 成功响应

- HTTP `200`，响应体为纯文本 `OK`。

#### 错误（纯文本）

| status | 文本 |
| --- | --- |
| `403` | `上传票据无效或已过期`（票据不存在 / 非上传票据 / 无目标目录） |
| `400` | `解析上传表单失败: ...`（非 multipart、边界损坏、超限等） |
| `400` | `缺少 file 字段: ...` |
| `400` | `文件名无效`（文件名解析后为空） |
| `403` | `路径越界` |
| `500` | `创建文件失败: ...` / `写入失败: ...` |

---

## 6. 回收站（实例级）

回收站目录位于实例内 `<cwd>/.irix-trash/`，元数据持久化在 `{data}/trash/<uuid>.json`。删除进回收站时文件被 `rename` 为 `<cwd>/.irix-trash/<id前8位>-<原名>`。

> 这些接口的 `uuid` 在**请求体**里（不是查询参数）。

### 6.1 `POST /api/files/trash` — 移入回收站

路由模式：`POST /api/files/trash`

请求体：

```json
{ "uuid": "实例UUID", "daemonId": "x", "targets": ["a.txt", "world/"] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |
| `targets` | string[] | ✅ | 待删除路径（非空） |

响应：`data: true`。

错误：

- `400 缺少 targets 参数`
- `400 回收站内的内容不能再次删除`
- `400 目标不存在: <t>`

### 6.2 `GET /api/files/trash/list` — 回收站列表

路由模式：`GET /api/files/trash/list`

查询参数：`uuid`（✅）、`daemonId`（❌）。

响应 `data`：

```json
{
  "items": [
    {
      "id": "a1b2c3d4",
      "name": "server.properties",
      "originalPath": "/server.properties",
      "trashPath": "/.irix-trash/a1b2c3d4-server.properties",
      "size": 1024,
      "deletedAt": 1710000000000
    }
  ]
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `id` | string | 条目 id（UUID 前 8 位） |
| `name` | string | 原始 basename |
| `originalPath` | string | 原始路径（相对 cwd，`/` 前缀，`/` 分隔） |
| `trashPath` | string | 回收站内路径（相对 cwd，`/` 前缀） |
| `size` | int64 | 大小（字节；目录为递归总大小） |
| `deletedAt` | int64 | 删除时间，Unix 毫秒 |

排序：新 → 旧（`deletedAt` 降序）。

### 6.3 `POST /api/files/trash/restore` — 恢复

路由模式：`POST /api/files/trash/restore`

请求体：

```json
{ "uuid": "实例UUID", "daemonId": "x", "ids": ["a1b2c3d4"] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |
| `ids` | string[] | ✅ | 要恢复的条目 id（非空） |

响应 `data`：

```json
{ "a1b2c3d4": "/server.properties" }
```

- `data` 是对象：`id → 实际恢复路径`（相对 cwd，`/` 前缀）。
- 目标已存在时自动改名：`name (1).ext`、`name (2).ext` …

错误：`400 缺少 ids 参数`、`400 回收站条目不存在: <id>`、`400 回收站内容已不存在（可能被手动删除）: <name>`。

### 6.4 `POST /api/files/trash/empty` — 永久删除

路由模式：`POST /api/files/trash/empty`

请求体：

```json
{ "uuid": "实例UUID", "daemonId": "x", "ids": ["a1b2c3d4"] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |
| `ids` | string[] | ❌ | 要永久删除的条目 id；**空 / 缺省 = 全部清空** |

响应：`data: true`。

---

## 7. 实例备份 / 恢复 / 核心下载

### 7.1 `POST /api/instance/snapshot` — 创建快照（备份）

路由模式：`POST /api/instance/snapshot`

请求体：

```json
{ "uuid": "实例UUID", "daemonId": "x" }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |

响应 `data`：

```json
{ "jobId": "3f2a1b9e-..." }
```

备份行为：实例 cwd 打成 zip，存入 `{data}/backups/<uuid>/<时间戳>.zip`（时间戳格式 `2006-01-02-15-04-05`）。排除 `.irix-trash/`、`.git/`、`*.log`、`*.tmp`、含 `.part-` 的文件。

### 7.2 `GET /api/instance/snapshot-progress` — 快照 / 恢复进度

路由模式：`GET /api/instance/snapshot-progress`

查询参数：`jobId`（✅）。

响应 `data`（任务快照，并把 `path` 改名为 `archivePath`）：

```json
{
  "status": "running",
  "percent": 0.5,
  "message": "压缩中…",
  "archivePath": "D:\\data\\backups\\<uuid>\\2026-08-20-10-00-00.zip"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | string | `running` \| `done` \| `failed` |
| `percent` | number | `0.0~1.0`；`-1` 未知 |
| `message` | string | 中文进度 |
| `archivePath` | string | 可选；快照完成时为归档 zip 的**绝对路径**（恢复任务没有该字段） |

快照进度阶段：`0.01` 统计文件 → `0.05 ~ 0.95` 压缩（按字节 `0.05 + 0.9*done/total`）→ `1.0` 完成（message=`备份完成`）。

错误：`400 缺少 jobId 参数`、`400 任务不存在或已过期`。

### 7.3 `POST /api/instance/restore` — 从备份恢复

路由模式：`POST /api/instance/restore`

请求体：

```json
{
  "uuid": "实例UUID",
  "daemonId": "x",
  "archivePath": "D:\\data\\backups\\<uuid>\\2026-08-20-10-00-00.zip"
}
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |
| `archivePath` | string | ✅ | 备份 zip 的**绝对路径**（必须在实例备份区内） |

响应 `data`：`{ "jobId": "3f2a1b9e-..." }`

行为：先自动停止实例（未运行则跳过）→ 解压覆盖 cwd → 恢复后**保持停止状态**。进度同样轮询 §7.2（`0.05` 解压 → `1.0` 完成，message=`恢复完成，实例保持停止`）。

错误：`400 实例不存在`、`400 备份路径无效`、`400 备份文件不在本实例备份区`、`400 备份文件不存在: <path>`。

### 7.4 `GET /api/instance/backups` — 备份列表

路由模式：`GET /api/instance/backups`

查询参数：`uuid`（✅）、`daemonId`（❌）。

响应 `data`：

```json
{
  "items": [
    {
      "fileName": "2026-08-20-10-00-00.zip",
      "size": 1234567,
      "mtime": "Mon Jan 02 2006 15:04:05 GMT+0800 (中国标准时间)",
      "path": "D:\\data\\backups\\<uuid>\\2026-08-20-10-00-00.zip"
    }
  ]
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `fileName` | string | 备份文件名（含 `.zip`） |
| `size` | int64 | 字节 |
| `mtime` | string | 展示用修改时间 |
| `path` | string | **绝对路径**（恢复 / 下载 / 删除都使用它） |

排序：新 → 旧。目录不存在时返回 `items: []`。只列 `.zip`。

### 7.5 `DELETE /api/instance/backups` — 删除备份

路由模式：`DELETE /api/instance/backups`

查询参数：`uuid`（✅）。

请求体：

```json
{ "paths": ["D:\\data\\backups\\<uuid>\\2026-08-20-10-00-00.zip"] }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `paths` | string[] | ✅ | 要删除的备份绝对路径（必须在实例备份区内） |

响应：`data: true`。

### 7.6 `POST /api/instance/backups/download` — 申请备份下载票据

路由模式：`POST /api/instance/backups/download`

查询参数：`uuid`（可选，见下）。

请求体：

```json
{ "uuid": "实例UUID", "path": "D:\\data\\backups\\<uuid>\\2026-08-20-10-00-00.zip" }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | 查询或请求体二选一 | 查询参数优先；均缺 → `400 缺少 uuid 参数` |
| `path` | string | ✅ | 备份文件绝对路径（必须在实例备份区内） |

响应 `data`：

```json
{ "password": "3f2a1b9e-...", "addr": "127.0.0.1:23333" }
```

随后直连下载：`GET http://<addr>/download/<password>/<fileName>`（`fileName` 用 `path` 的 basename）。

### 7.7 `POST /api/instance/download-core` — 服务端下载核心

路由模式：`POST /api/instance/download-core`

请求体：

```json
{
  "uuid": "实例UUID",
  "daemonId": "x",
  "url": "https://example.com/server.jar",
  "fileName": "server.jar",
  "sha512": "abcd..."
}
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `uuid` | string | ✅ | 实例 UUID（请求体） |
| `daemonId` | string | ❌ | 忽略 |
| `url` | string | ✅ | 直连下载 URL（仅 `http`/`https`） |
| `fileName` | string | ✅ | 保存文件名（只取 basename，丢弃路径） |
| `sha512` | string | ❌ | 期望的 SHA-512（十六进制，大小写不敏感）；缺省跳过校验 |

响应 `data`：`{ "jobId": "3f2a1b9e-..." }`

行为：下载到实例 cwd 下 `<fileName>`，先写 `.part-<taskID>` 临时文件，流式算 sha512，校验通过后 `rename` 就位。文件上限 8 GiB。

### 7.8 `GET /api/instance/download-core-progress` — 核心下载进度

路由模式：`GET /api/instance/download-core-progress`

查询参数：`jobId`（✅）。

响应 `data`（任务快照，字段名为 `path`，非 `archivePath`）：

```json
{
  "status": "running",
  "percent": 0.45,
  "message": "下载中 90.0 MB / 200.0 MB",
  "path": "D:\\...\\server.jar"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | string | `running` \| `done` \| `failed` |
| `percent` | number | `0.0~1.0`；`-1` 未知 |
| `message` | string | 中文进度 |
| `path` | string | 可选；完成时为最终文件绝对路径 |

进度阶段：`0.01` 开始 → `0~0.9` 下载（按字节 `0.9*written/total`）→ `0.93` 校验 sha512（或 `0.95` 跳过）→ `1.0` 完成。

---

## 8. Java 运行时与 JDK

### 8.1 `GET /api/runtime/java` — 检测 Java 运行时

路由模式：`GET /api/runtime/java`

无参数。

响应 `data`：

```json
{
  "default": {
    "path": "D:\\data\\jdk\\jdk-21\\bin\\java.exe",
    "version": "21.0.4",
    "vendor": "Eclipse Adoptium (Temurin)",
    "major": 21,
    "available": true
  },
  "all": [
    {
      "path": "D:\\data\\jdk\\jdk-21\\bin\\java.exe",
      "version": "21.0.4",
      "vendor": "Eclipse Adoptium (Temurin)",
      "major": 21,
      "available": true
    }
  ]
}
```

| 顶层字段 | 类型 | 含义 |
| --- | --- | --- |
| `default` | object \| null | 可用版本号最高的运行时；无可用时为 `null` |
| `all` | array | 全部运行时（排序：可用优先 → 大版本号降序 → 路径升序） |

`javaRuntime` 条目字段：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `path` | string | `java` 可执行文件绝对路径 |
| `version` | string | 从 `java -version` 解析的版本串（如 `21.0.4`；不可用时为空） |
| `vendor` | string | 厂商展示名（如 `Eclipse Adoptium (Temurin)`、`Oracle`、`OpenJDK`；无法识别为 `未知`） |
| `major` | int | 大版本号（`21.0.4 → 21`，旧式 `1.8.0 → 8`） |
| `available` | bool | `false` = 路径存在但 `java -version` 执行失败 |

### 8.2 `POST /api/runtime/java/install` — 安装 JDK

路由模式：`POST /api/runtime/java/install`

请求体：

```json
{ "major": 21 }
```

| 字段 | 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `major` | int | ✅ | 要安装的 Java 大版本（支持 `8 ~ 30`，超出 → `400 大版本号无效（支持 8~30）: <n>`） |

响应 `data`：`{ "jobId": "3f2a1b9e-..." }`

行为：节点直连 Adoptium API 下载对应大版本 JDK，解压安装到 `{data}/jdk/jdk-<major>/`。同大版本安装互斥。

### 8.3 `GET /api/runtime/java/install-progress` — JDK 安装进度

路由模式：`GET /api/runtime/java/install-progress`

查询参数：`jobId`（✅）。

响应 `data`（任务快照）：

```json
{
  "status": "running",
  "percent": 0.42,
  "message": "下载中 84.0 MB / 200.0 MB",
  "path": "D:\\data\\jdk\\jdk-21\\bin\\java.exe"
}
```

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `status` | string | `running` \| `done` \| `failed` |
| `percent` | number | `0.0~1.0`；`-1` 未知 |
| `message` | string | 中文进度 |
| `path` | string | 可选；完成时为安装后 `bin/java` 的绝对路径 |

进度阶段：

| percent | 阶段 |
| --- | --- |
| `0.01` | 查询 Adoptium 下载信息 |
| `0.02` | 已获取下载信息 |
| `0.05 ~ 0.90` | 下载（按字节 `0.05 + 0.85*written/total`） |
| `0.90 ~ 0.95` | 解压（按文件数） |
| `1.0` | 完成（message 形如 `JDK 21 安装完成（21.0.4）`） |

### 8.4 `DELETE /api/runtime/java` — 卸载 JDK

路由模式：`DELETE /api/runtime/java`

查询参数：`major`（✅，整数，`>0`）。

响应：`data: true`。

错误：

- `400 缺少 major 参数`
- `404 该版本 JDK 未安装`
- `500 卸载失败: ...`

---

## 9. 前端调用要点速查

1. **列表分页**：请求 `page` 从 1 开始，响应 `page` 是 0 起始（`page - 1`），翻页用 `total` 与 `pageSize` 计算总页数。
2. **读/写文件**：`PUT /api/files/`（末尾斜杠必须）。`text` 字段「缺省 = 读、存在 = 写」；超过 8 MiB 的文件必须走票据直连。
3. **二进制传输**：`/api/files/` 是文本通道，大文件/二进制一律走 §5 票据直连（下载流式、上传 multipart 流式落盘）。
4. **上传 multipart 字段名固定为 `file`**。
5. **压缩 `type`**：`1` = 压缩（`source` 是产物 zip、`targets` 是待打包列表）；`2` = 解压（`source` 是 zip、`targets[0]` 可选为解压目标目录）。
6. **异步任务**：`snapshot` / `restore` / `download-core` / `java/install` 都返回 `jobId`，轮询对应 progress 接口直到 `status` 为 `done` 或 `failed`。
7. **快照进度字段是 `archivePath`**；核心下载与 JDK 安装进度字段是 `path`。
