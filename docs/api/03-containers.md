# 容器 API 契约（Docker + Bastille）

> 数据源：`IriX-Node/container.go`、`container_docker.go`、`container_bastille.go`、`container_bastille_files.go` 及 `docs/irix-node-container-api.md`。
> 本文档面向前端（React 面板）开发，字段名与 Go struct 的 `json` tag **一字不差**；按本文档即可写出全部调用代码，无需再读后端源码。

---

## 0. 通用约定

| 项 | 约定 |
|----|------|
| 基础地址 | `http://<host>:<port>` |
| 认证 | `?apikey=<key>` 查询参数，或 `X-Api-Key` 请求头（二者取其一，`apikey` 查询参数优先） |
| 请求头 | 建议携带 `X-Requested-With: XMLHttpRequest`（兼容 MCSM 风格） |
| 请求体 | `application/json; charset=utf-8`（上传/下载类端点除外） |
| 字符集 | UTF-8 |

### 响应信封（成功与失败统一）

```json
{ "status": 200, "data": <payload>, "time": 1750000000000 }
```

- `status`：与真实 HTTP 状态码一致。成功恒为 `200`；错误时为 4xx/5xx（见下）。
- `data`：成功时为各端点定义的 payload；**失败时为一条中文错误消息字符串**。
- `time`：服务端 Unix 毫秒时间戳。

### 错误响应格式

```json
{ "status": 400, "data": "缺少 image 参数", "time": 1750000000000 }
```

HTTP 状态码同样等于 `status`。前端统一处理：`status !== 200` 时把 `data` 字符串展示给用户。

| HTTP 状态码 | 触发场景 |
|-------------|----------|
| `400` | 请求体格式错误、缺少必填参数、参数校验失败（如 jail 名纯数字）、路径越界/无效、长任务不存在或已过期 |
| `404` | 归档下载时目标归档文件不存在 |
| `500` | 底层 CLI（docker/bastille）执行失败、内部错误 |
| `501` | 当前平台不支持该容器能力（`available=false` 时调用操作端点） |

> 平台不支持时的统一错误消息：`当前平台不支持该容器能力`。

### 原始字节端点（不走 JSON 信封）

以下端点直接返回/接收二进制流，body 或响应为原始字节：

- `GET /api/container/archive?file=...` — 下载归档（响应 `application/octet-stream`）
- `POST /api/container/archive/upload` — multipart 上传归档（响应仍为 JSON：`{path}`）
- `GET /api/bastille/jails/{name}/files/download?path=...` — 下载 jail 内文件（二进制流）
- `POST /api/bastille/jails/{name}/files/upload` — multipart 上传文件到 jail（响应仍为 JSON：`{path}`）

---

## 1. 端点总表

### 能力探测

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/container/info` | 运行时/平台/版本/可用性探测 |

### Docker（Linux 节点）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/container/ps` | 容器列表（`?all=1` 含已停止） |
| POST | `/api/container/create` | 创建容器（不启动） |
| POST | `/api/container/{id}/start` | 启动 |
| POST | `/api/container/{id}/stop` | 停止 |
| POST | `/api/container/{id}/restart` | 重启 |
| POST | `/api/container/{id}/kill` | 强杀 |
| DELETE | `/api/container/{id}` | 删除（`?force=1`） |
| GET | `/api/container/{id}/logs` | 日志尾部（`?tail=N`） |
| POST | `/api/container/{id}/exec` | 容器内执行命令 |
| GET | `/api/container/{id}/stats` | 资源统计 |
| POST | `/api/container/{id}/clone` | 克隆容器 |
| POST | `/api/container/{id}/export` | 导出文件系统为 tar（返回下载票据） |
| POST | `/api/container/{id}/limits` | 动态调整资源限制 |
| GET | `/api/image/list` | 镜像列表 |
| POST | `/api/image/pull` | 拉取镜像（同步，最长 10 分钟） |
| POST | `/api/image/build` | 构建镜像（长任务） |
| GET | `/api/image/build-progress` | 构建进度轮询（`?jobId=`） |
| POST | `/api/image/import` | 从同步区归档导入镜像 |
| DELETE | `/api/image/{name}` | 删除镜像 |
| GET | `/api/volume/list` | 卷列表 |
| DELETE | `/api/volume/{name}` | 删除卷 |
| GET | `/api/network/list` | 网络列表 |

### 节点级归档（编排迁移用，任意宿主机路径）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/container/archive` | 压缩节点路径为 zip |
| GET | `/api/container/archive` | 下载归档（原始字节） |
| POST | `/api/container/archive/upload` | 上传归档（multipart） |
| POST | `/api/container/archive/restore` | 解压恢复归档 |

### Bastille（FreeBSD 节点）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/bastille/releases` | 已 bootstrap 发行版列表 |
| POST | `/api/bastille/bootstrap` | bootstrap 发行版（长任务） |
| POST | `/api/bastille/setup` | 环境初始化设置 |
| GET | `/api/bastille/jails` | jail 列表 |
| POST | `/api/bastille/jails/create` | 创建 jail |
| POST | `/api/bastille/jails/{name}/start` | 启动 jail |
| POST | `/api/bastille/jails/{name}/stop` | 停止 jail |
| POST | `/api/bastille/jails/{name}/restart` | 重启 jail |
| POST | `/api/bastille/jails/{name}/destroy` | 销毁 jail（`?force=1`） |
| POST | `/api/bastille/jails/{name}/clone` | 克隆 jail |
| POST | `/api/bastille/jails/{name}/export` | 导出 jail 为 txz |
| POST | `/api/bastille/jails/import` | 导入 jail |
| GET | `/api/bastille/jails/{name}/console` | 控制台日志尾部（`?tail=N`） |
| POST | `/api/bastille/jails/{name}/cmd` | jail 内执行命令 |
| POST | `/api/bastille/jails/{name}/pkg` | jail 内软件包管理 |
| GET | `/api/bastille/jails/{name}/config` | 读取 jail.conf 配置 |
| POST | `/api/bastille/jails/{name}/config` | 设置配置项 |
| DELETE | `/api/bastille/jails/{name}/config` | 删除配置项（`?key=`） |
| GET | `/api/bastille/jails/{name}/mounts` | 挂载列表 |
| POST | `/api/bastille/jails/{name}/mounts` | 添加挂载 |
| DELETE | `/api/bastille/jails/{name}/mounts` | 卸载挂载（`?dst=`） |
| POST | `/api/bastille/jails/{name}/limits` | 设置硬件资源限制 |
| GET | `/api/bastille/templates` | 模板列表 |
| POST | `/api/bastille/templates/apply` | 应用模板（长任务） |
| POST | `/api/bastille/rdr` | 添加端口转发 |
| DELETE | `/api/bastille/rdr` | 删除端口转发 |
| GET | `/api/bastille/rdr` | 端口转发列表（`?jail=`） |
| GET | `/api/bastille/jobs/{jobId}` | 长任务进度轮询 |
| POST | `/api/bastille/jails/{name}/run` | 启动 jail 内运行会话 |
| GET | `/api/bastille/jails/{name}/run/{session}` | 会话状态与增量日志 |
| POST | `/api/bastille/jails/{name}/run/{session}/stdin` | 写会话 stdin |
| POST | `/api/bastille/jails/{name}/run/{session}/stop` | 停止会话 |
| DELETE | `/api/bastille/jails/{name}/run/{session}` | 清理会话 |
| GET | `/api/bastille/jails/{name}/files` | 文件列表 |
| GET | `/api/bastille/jails/{name}/files/content` | 读取文本文件 |
| PUT | `/api/bastille/jails/{name}/files/content` | 写入文本文件 |
| DELETE | `/api/bastille/jails/{name}/files` | 删除文件/目录 |
| POST | `/api/bastille/jails/{name}/files/mkdir` | 新建目录 |
| POST | `/api/bastille/jails/{name}/files/touch` | 新建空文件 |
| POST | `/api/bastille/jails/{name}/files/upload` | 上传文件（multipart） |
| GET | `/api/bastille/jails/{name}/files/download` | 下载文件（二进制流） |

> 路由采用 Go 1.22 `METHOD /path/{param}` 模式；`{id}`、`{name}`、`{jobId}`、`{session}` 为路径参数（如 `{name}` 是 jail 名）。

---

## 2. 能力探测

### `GET /api/container/info`

无查询参数、无请求体。

响应 `data`：

```json
{
  "runtime": "docker",
  "platform": "linux",
  "version": "27.1.1",
  "available": true
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `runtime` | string | `docker` \| `bastille`（未检测到时为空串 `""`） |
| `platform` | string | 运行平台：`linux` \| `freebsd` \| `windows` 等 |
| `version` | string | 运行时版本（docker Server.Version / bastille version）；未检测到时为空串 |
| `available` | bool | 是否可用。false 时前端应整体置灰容器功能 |
| `error` | string? | 仅 `available=false` 时存在，中文不可用原因 |

不可用示例：

```json
{
  "runtime": "",
  "platform": "windows",
  "version": "",
  "available": false,
  "error": "未检测到可用的容器运行时（请确认已安装并可用：Linux 需要 docker CLI，FreeBSD 需要 bastille）"
}
```

---

## 3. Docker 端点

### 3.1 `GET /api/container/ps`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `all` | 否 | `1` 时包含已停止容器（`docker ps -a`）；否则仅运行中 |

响应 `data`：数组，条目字段：

```json
{
  "id": "a1b2c3d4e5f6",
  "name": "mc-server",
  "image": "itzg/minecraft-server:latest",
  "status": "Up 2 hours",
  "state": "running",
  "ports": ["0.0.0.0:25565->25565/tcp", ":::25565->25565/tcp"],
  "createdAt": "2026-08-14T04:34:56Z",
  "restartPolicy": "unless-stopped"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 容器 ID |
| `name` | string | 容器名（docker 原始输出，可能带前导 `/`，如 `/mc-server`） |
| `image` | string | 镜像名 |
| `status` | string | 人类可读状态（`Up 2 hours` / `Exited (0) 3 days ago`） |
| `state` | string | 短状态（`running` / `exited` / `created` 等） |
| `ports` | string[] | 端口映射数组，逗号分隔项逐个拆分（空为 `[]`） |
| `createdAt` | string | ISO-8601（UTC）；解析失败时原样透传 docker 时间 |
| `restartPolicy` | string | 重启策略（`no` / `always` / `unless-stopped` / `on-failure`）；批量 inspect 取回 |

### 3.2 `POST /api/container/create`

请求体（**`image` 必填，其余全部可选**）：

```json
{
  "name": "mc-server",
  "image": "itzg/minecraft-server:latest",
  "command": "java -jar server.jar nogui",
  "workdir": "/data",
  "ports": ["25565:25565"],
  "volumes": ["/data/mc:/data"],
  "env": { "EULA": "TRUE", "MEMORY": "2G" },
  "restartPolicy": "unless-stopped",
  "memoryLimitMb": 4096,
  "cpus": 4,
  "diskLimitMb": 20480
}
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | 空（docker 自动命名） | 容器名 |
| `image` | string | **是** | — | 镜像名 |
| `command` | string | 否 | 空 | 容器启动命令，经 `SplitCommand` 按空格/引号拆分后作为 `docker create <image> [args...]`（**不是 `sh -c`**，管道/重定向不生效） |
| `workdir` | string | 否 | 空 | 容器内工作目录（`-w`） |
| `ports` | string[] | 否 | 空 | 端口映射，每项原样传给 `-p` |
| `volumes` | string[] | 否 | 空 | 卷映射，每项原样传给 `-v` |
| `env` | object | 否 | 空 | 环境变量键值对，转 `-e K=V` |
| `restartPolicy` | string | 否 | 空 | 重启策略（`--restart`） |
| `memoryLimitMb` | int | 否 | 0 | `>0` 时加 `-m <N>m` 内存上限 |
| `cpus` | number | 否 | 0 | `>0` 时加 `--cpus <N>` |
| `diskLimitMb` | int | 否 | 0 | `>0` 时加 `--storage-opt size=<N>M` 可写层大小（需 overlay2 + quota） |

响应 `data`：

```json
{ "id": "a1b2c3d4e5f6...", "name": "mc-server" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 新容器完整 ID |
| `name` | string | 容器名（`name` 为空时为空串） |

### 3.3 生命周期操作

```
POST   /api/container/{id}/start
POST   /api/container/{id}/stop
POST   /api/container/{id}/restart
POST   /api/container/{id}/kill
DELETE /api/container/{id}?force=1
```

- 均无请求体。
- `DELETE` 查询参数：`force` = `1` 时加 `-f`（可删运行中容器）。
- 响应 `data`：`true`（布尔）。

### 3.4 `GET /api/container/{id}/logs`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `tail` | 否 | 日志尾部行数，正整数；缺省 `100`，非法值回退 `100` |

响应 `data`：**字符串**（日志文本，含换行）。

### 3.5 `POST /api/container/{id}/exec`

请求体：

```json
{ "command": "ls -la /data" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `command` | string | **是** | 命令，经 `sh -c` 执行（支持管道/重定向等 shell 语法） |

响应 `data`：**字符串**（命令 stdout+stderr 合并输出）。

### 3.6 `GET /api/container/{id}/stats`

无查询参数。响应 `data`：

```json
{
  "cpuPercent": 0.05,
  "memoryBytes": 1258291,
  "memoryLimitBytes": 8267812864,
  "netRxBytes": 1024,
  "netTxBytes": 3584
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `cpuPercent` | number | CPU 占用百分比数值（`0.05` = 0.05%；已去掉 `%`） |
| `memoryBytes` | number | 内存已用量（字节） |
| `memoryLimitBytes` | number | 内存上限（字节） |
| `netRxBytes` | number | 网络接收字节数 |
| `netTxBytes` | number | 网络发送字节数 |

### 3.7 `POST /api/container/{id}/clone`

请求体：

```json
{ "name": "mc-server-copy" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 否 | 新容器名（可为空，docker 自动命名） |

响应 `data`：

```json
{ "id": "b2c3d4e5f6a7", "name": "mc-server-copy", "image": "irix-clone-1750000000000" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 新容器 ID |
| `name` | string | 新容器名 |
| `image` | string | 临时提交镜像名（`irix-clone-<毫秒时间戳>`） |

### 3.8 `POST /api/container/{id}/export`

无请求体。导出容器文件系统为 tar 到同步区，返回下载票据。响应 `data`：

```json
{
  "password": "<下载票据密码>",
  "addr": "192.168.1.10:23333",
  "fileName": ".exports/a1b2c3d4e5f6-1750000000000.tar"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `password` | string | 下载票据密码 |
| `addr` | string | 节点对外地址（含端口） |
| `fileName` | string | 相对同步区根的路径（`/mirrors` 虚拟前缀下） |

下载方式：`GET /download/{password}/.exports/<fileName>`（直连通道，绕过 apikey）。

### 3.9 `POST /api/container/{id}/limits`

请求体（字段均可选，`>0` 才生效）：

```json
{ "memoryMb": 8192, "cpus": 6 }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `memoryMb` | int | 否 | 内存上限 MB（`-m`） |
| `cpus` | number | 否 | CPU 核数（`--cpus`） |

响应 `data`：`true`。

### 3.10 `GET /api/image/list`

无参数。响应 `data` 数组（同一 ID 的多个 tag 合并为一条）：

```json
[
  {
    "id": "sha256:abcd1234...",
    "tags": ["itzg/minecraft-server:latest", "itzg/minecraft-server:2026.1"],
    "sizeBytes": 858993459,
    "createdAt": "2026-08-01T00:00:00Z"
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 镜像 ID |
| `tags` | string[] | `repo:tag` 列表 |
| `sizeBytes` | number | 镜像大小（字节；解析失败为 0） |
| `createdAt` | string | ISO-8601 |

### 3.11 `POST /api/image/pull`

请求体：

```json
{ "name": "itzg/minecraft-server:latest" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | **是** | 镜像名 |

**同步等待**（最长 10 分钟）。响应 `data`：`true`。

### 3.12 `POST /api/image/build`（长任务）

请求体：

```json
{ "dockerfile": "FROM alpine:latest\n...", "name": "my-app", "tag": "v1" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `dockerfile` | string | 否 | Dockerfile 内容（写入构建上下文） |
| `name` | string | **是** | 镜像名 |
| `tag` | string | **是** | 镜像 tag（产出 `name:tag`） |

响应 `data`：

```json
{ "jobId": "3f2a...uuid..." }
```

### 3.13 `GET /api/image/build-progress`（构建进度轮询）

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `jobId` | **是** | 构建任务 ID（来自 build 响应） |

响应 `data`：

```json
{
  "status": "building",
  "log": ["Step 1/3 : FROM alpine:latest", "..."],
  "image": "my-app:v1"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | `building` \| `done` \| `failed` |
| `log` | string[] | 输出日志行（保留最近 500 行） |
| `image` | string | 目标镜像 `name:tag` |

- 轮询直到 `status != "building"`。
- 任务不存在/已过期 → `400`，`data` 为 `构建任务不存在或已过期`。

### 3.14 `POST /api/image/import`

请求体：

```json
{ "fileName": ".exports/xxx.tar", "name": "my-imported:latest" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `fileName` | string | **是** | 同步区归档相对路径（`/mirrors` 虚拟前缀下） |
| `name` | string | **是** | 导入后的镜像名 |

响应 `data`：`true`。

### 3.15 `DELETE /api/image/{name}`

`{name}` 为镜像名（路径参数）。无请求体。响应 `data`：`true`。

### 3.16 `GET /api/volume/list`

无参数。响应 `data` 数组：

```json
[
  { "name": "mc-data", "driver": "local", "mountpoint": "/var/lib/docker/volumes/mc-data/_data" }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 卷名 |
| `driver` | string | 卷驱动 |
| `mountpoint` | string | 宿主机挂载点 |

### 3.17 `DELETE /api/volume/{name}`

`{name}` 为卷名。响应 `data`：`true`。

### 3.18 `GET /api/network/list`

无参数。响应 `data` 数组：

```json
[
  { "name": "bridge", "driver": "bridge", "subnet": "172.17.0.0/16" }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 网络名 |
| `driver` | string | 网络驱动 |
| `subnet` | string | 子网（逐网 inspect 填充；失败时为空串） |

---

## 4. 节点级归档（编排迁移，任意宿主机路径）

> 与实例/jail 无关，操作节点上的**任意绝对路径**；`GET archive` 与 `POST upload` 为原始字节传输。

### 4.1 `POST /api/container/archive`

请求体：

```json
{ "path": "/data/mc/world", "archive": "world_s1_0.zip" }
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `path` | string | **是** | — | 待压缩的宿主机绝对路径（文件或目录） |
| `archive` | string | 否 | `<basename>_<时间戳>.zip` | 归档文件名（**只能是纯文件名**，不能含路径分隔符） |

响应 `data`：

```json
{ "path": "D:\\irix-node-data\\archives\\world_s1_0.zip" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `path` | string | 归档绝对路径 |

### 4.2 `GET /api/container/archive`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `file` | **是** | 归档文件名（纯文件名） |

**原始字节响应**（`Content-Type: application/octet-stream`，`Content-Disposition: attachment`）。文件不存在 → `404`。

### 4.3 `POST /api/container/archive/upload`

multipart 表单，字段名 `file`（原始字节）。响应 `data`：

```json
{ "path": "D:\\irix-node-data\\archives\\world_s1_0.zip" }
```

上传文件名同样只取纯文件名（拒绝路径分隔符与 `..`）。

### 4.4 `POST /api/container/archive/restore`

请求体：

```json
{ "file": "world_s1_0.zip", "destPath": "/data/mc/world" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `file` | string | **是** | 归档文件名（纯文件名，已上传或已创建的） |
| `destPath` | string | **是** | 解压目标目录（**必须绝对路径**，覆盖式恢复，防 zip-slip） |

响应 `data`：`true`。

---

## 5. Bastille 端点

### 5.1 `GET /api/bastille/releases`

无参数。响应 `data` 数组（已 bootstrap 的发行版）：

```json
[
  {
    "name": "14.2-RELEASE",
    "version": "14.2-RELEASE",
    "sizeBytes": 524288000,
    "createdAt": "2026-08-14 12:34:56"
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 发行版名 |
| `version` | string | 与 `name` 相同（客户端拼 `name:version` 标签） |
| `sizeBytes` | number | 发行版目录占用字节数（**前端大小显示的唯一来源，缺失即显示 0**） |
| `createdAt` | string | 目录修改时间，格式 `2006-01-02 15:04:05`（**不是 ISO-8601**） |

### 5.2 `POST /api/bastille/bootstrap`（长任务）

请求体：

```json
{ "release": "14.2-RELEASE" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `release` | string | **是** | 发行版名（自动剥离 `:` 后缀，如 `14.2-RELEASE:14.2-RELEASE` → `14.2-RELEASE`） |

响应 `data`：

```json
{ "jobId": "3f2a...uuid..." }
```

进度轮询见 §5.17 `GET /api/bastille/jobs/{jobId}`。

### 5.3 `POST /api/bastille/setup`

请求体：

```json
{ "mode": "firewall", "extIf": "em0", "tunIf": "", "addr": "" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `mode` | string | **是** | `default` \| `firewall` \| `vnet` \| `bridge` \| `shared` \| `linux` |
| `extIf` | string | 否 | 外网网卡（`firewall`/`vnet`/`shared` 用到） |
| `tunIf` | string | 否 | tun 网卡（`vnet` 用到，与 `extIf`/`addr` 一起传） |
| `addr` | string | 否 | 地址（`vnet` 用到） |

响应 `data`：

```json
{ "ok": true, "detail": "<命令输出摘要>" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `ok` | bool | 恒 `true`（成功时） |
| `detail` | string | 命令输出（去掉首尾空白） |

mode 映射：`default` → `bastille setup -y`；`firewall` → `setup -y firewall [extIf]`；`vnet` → `setup -y vnet [extIf tunIf addr]`；`bridge` → `setup -y bridge`；`shared` → `setup -y shared [extIf]`；`linux` → `setup -y linux`。

### 5.4 `GET /api/bastille/jails`

无参数。响应 `data` 数组：

```json
[
  {
    "name": "mc-jail",
    "release": "14.2-RELEASE",
    "status": "Up",
    "state": "running",
    "ports": ["tcp 25565 -> 25565"],
    "createdAt": ""
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | jail 名 |
| `release` | string | 发行版（从 jail.conf `osrelease` 或 root 符号链接推断，失败为空串） |
| `status` | string | `Up` \| `Down`（**前端以「含 up」判断运行态**） |
| `state` | string | `running` \| `stopped` |
| `ports` | string[] | rdr 规则摘要，格式 `"<proto> <hostPort> -> <jailPort>"` |
| `createdAt` | string | **恒为空串 `""`**（Bastille 未提供创建时间） |

### 5.5 `POST /api/bastille/jails/create`

请求体（**`name` 必填**；`type=empty` 时 `release`/`ip` 可省略）：

```json
{
  "name": "mc-jail",
  "release": "14.2-RELEASE",
  "ip": "192.168.1.50/24",
  "type": "thin",
  "vnet": "none",
  "interface": "",
  "bridge": "",
  "mac": false,
  "volumes": ["/data/mc:/data"],
  "workdir": "/data",
  "memoryLimitMb": 4096,
  "cpus": 4,
  "diskLimitMb": 20480
}
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | **是** | — | jail 名（**不能纯数字**，须含至少一个字母） |
| `release` | string | 条件必填 | 空 | 已 bootstrap 发行版；`type=empty` 时可为空 |
| `ip` | string | 条件必填 | 空 | jail IP；**VNET 模式必须含 `/掩码`**（如 `10.0.0.2/24`）；`type=empty` 时可省略 |
| `type` | string | 否 | `thin` | `thin` \| `thick` \| `clone` \| `empty` \| `linux` |
| `vnet` | any | 否 | `none` | **bool 或字符串**：`true`→`vnet`、`false`→`none`；字符串 `none` \| `vnet` \| `bridge`（**新契约推荐字符串**） |
| `interface` | string | 否 | 空 | 旧契约字段：VNET/bridge 网卡（`vnet` 为物理网卡、`bridge` 为桥接网卡） |
| `bridge` | string | 否 | 空 | 新契约字段：bridge 模式网卡名（提供时自动把 vnet 归一为 `bridge`） |
| `mac` | any | 否 | 空 | bool 或 MAC 字符串：`true` 仅加 `-M`；字符串写 `-M` 并改 jail.conf 静态 MAC（**仅 VNET**） |
| `volumes` | string[] | 否 | 空 | 挂载，每项格式 `"宿主机路径:jail内路径"` |
| `workdir` | string | 否 | 空 | jail 内工作目录（改写 exec.start 前置 `cd`） |
| `memoryLimitMb` | int | 否 | 0 | 内存上限 MB（rctl memoryuse） |
| `cpus` | int | 否 | 0 | CPU 核数（cpuset `0..N-1`；**注意是 int，Docker 是 float**） |
| `diskLimitMb` | int | 否 | 0 | 磁盘配额 MB（ZFS `quota`） |

约束：`linux` 与任何 VNET 模式互斥；VNET 时必须提供网卡且 IP 含掩码；`mac` 仅限 VNET。违反时返回 `400` 中文提示。

响应 `data`：

```json
{ "name": "mc-jail", "warnings": [] }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 创建的 jail 名 |
| `warnings` | string[] | 创建后配置（静态 MAC/挂载/workdir/limits）失败的告警；不影响创建结果 |

### 5.6 生命周期操作

```
POST /api/bastille/jails/{name}/start
POST /api/bastille/jails/{name}/stop
POST /api/bastille/jails/{name}/restart
POST /api/bastille/jails/{name}/destroy?force=1
```

- 均无请求体。
- `destroy` 查询参数 `force`：`1` 时附加 `-a`（可摧毁运行中 jail）；恒附加 `-y`。
- 响应 `data`：`true`。

### 5.7 `POST /api/bastille/jails/{name}/clone`

请求体：

```json
{ "newName": "mc-jail-2", "ip": "192.168.1.51/24" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `newName` | string | **是** | 新 jail 名 |
| `ip` | string | 否 | 新 IP |

响应 `data`：`true`。

### 5.8 `POST /api/bastille/jails/{name}/export`

无请求体。导出为 txz 到 `/usr/local/bastille/backups/`。响应 `data`：

```json
{ "path": "/usr/local/bastille/backups/mc-jail_20260814_120000.txz" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `path` | string | 归档绝对路径（可直接作为 import 的 `file`） |

### 5.9 `POST /api/bastille/jails/import`

请求体：

```json
{ "file": "/usr/local/bastille/backups/mc-jail_20260814_120000.txz", "release": "14.2-RELEASE", "force": false }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `file` | string | **是** | 归档路径（绝对路径须位于 backups 或同步区内；相对路径按同步区根解析） |
| `release` | string | 否 | 导入到指定发行版 |
| `force` | bool | 否 | `true` → `-f` 跳过校验和 |

响应 `data`：

```json
{ "name": "mc-jail" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 导入后的 jail 名 |

### 5.10 `GET /api/bastille/jails/{name}/console`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `tail` | 否 | 日志尾部行数，缺省 `100` |

响应 `data`：**字符串**（jail 控制台日志尾部）。

### 5.11 `POST /api/bastille/jails/{name}/cmd`

请求体：

```json
{ "command": "java -version" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `command` | string | **是** | 命令，经 `sh -c` 执行（shell 语义） |

响应 `data`：**字符串**（stdout+stderr 合并输出）。

### 5.12 `POST /api/bastille/jails/{name}/pkg`

请求体：

```json
{ "action": "install", "packages": ["openjdk17-jre", "ca_root_nss"] }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `action` | string | **是** | `install` / `delete` / `update` / `upgrade` / `autoremove`（其他 pkg 子命令亦可透传） |
| `packages` | string[] | 条件必填 | 包名列表；`install`/`delete` 时必填，其余可空 |

响应 `data`：**字符串**（`bastille pkg` 输出文本，服务端已附加 `-y`）。

### 5.13 配置编辑（jail.conf）

```
GET    /api/bastille/jails/{name}/config
POST   /api/bastille/jails/{name}/config
DELETE /api/bastille/jails/{name}/config?key=<key>
```

`GET` 响应 `data`：扁平键值对象（解析 `key = value;` 参数行，剥离外层引号）：

```json
{ "ip4.addr": "192.168.1.50/24", "hostname": "mc-jail", "exec.start": "/bin/sh /etc/rc" }
```

`POST` 请求体：

```json
{ "key": "ip4.addr", "value": "192.168.1.51/24" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `key` | string | **是** | 配置键（仅允许字母/数字/`.`/`_`/`-`） |
| `value` | string | 否 | 配置值 |

`POST`/`DELETE` 响应 `data`：`true`（DELETE 不存在的 key 幂等返回 200）。

### 5.14 挂载管理

```
GET    /api/bastille/jails/{name}/mounts
POST   /api/bastille/jails/{name}/mounts
DELETE /api/bastille/jails/{name}/mounts?dst=<jail内路径>
```

`GET` 响应 `data` 数组（合并 fstab 条目与当前实际挂载）：

```json
[
  { "src": "/data/mc-survival", "dst": "/data", "fstype": "nullfs", "options": "rw", "permanent": true },
  { "dst": "/proc", "fstype": "procfs", "options": "rw", "permanent": true }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `src` | string? | 宿主机源路径；procfs/devfs 时**省略此字段** |
| `dst` | string | jail 内目标路径 |
| `fstype` | string | `nullfs` \| `procfs` \| `devfs` |
| `options` | string? | 挂载选项（如 `rw`） |
| `permanent` | bool | `true` = 来自 fstab（启动自动挂载）；`false` = 当前即时挂载 |

`POST` 请求体：

```json
{ "src": "/data/mc-survival", "dst": "/data", "fstype": "nullfs", "options": "rw" }
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `src` | string | 条件必填 | 空 | 宿主机源路径；`nullfs` 必填，procfs/devfs 忽略 |
| `dst` | string | 条件必填 | 空 | jail 内目标路径（须以 `/` 开头） |
| `dest` | string | 否 | 空 | **旧契约字段**，`dst` 为空时兜底 |
| `fstype` | string | 否 | `nullfs` | `nullfs` \| `procfs` \| `devfs` |
| `options` | string | 否 | `rw` | 挂载选项 |

`DELETE`：优先 `?dst=` 查询参数；缺省时兼容 body `{ "dst" }` / `{ "dest" }`。响应 `data`：`true`。

### 5.15 `POST /api/bastille/jails/{name}/limits`

请求体（字段均可选，`>0` 才生效）：

```json
{ "memoryMb": 4096, "cpus": 4, "diskMb": 20480 }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `memoryMb` | int | 否 | 内存上限 MB（rctl memoryuse） |
| `cpus` | int | 否 | CPU 核数（cpuset `0..N-1`） |
| `diskMb` | int | 否 | 磁盘配额 MB（ZFS quota） |

响应 `data`：`true`。

### 5.16 `GET /api/bastille/templates`

无参数。响应 `data` 数组：

```json
[
  { "namespace": "default", "name": "minecraft" }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `namespace` | string | project 目录名 |
| `name` | string | 模板目录名 |

`POST /api/bastille/templates/apply` 请求体：

```json
{ "jail": "mc-jail", "template": "default/minecraft", "args": { "KEY": "VALUE" } }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `jail` | string | **是** | 目标 jail |
| `template` | string | **是** | 模板（`namespace/name` 或名称） |
| `args` | object | 否 | `KEY=VALUE` 环境变量传给模板脚本 |

响应 `data`：`{ "jobId": "..." }`。进度轮询见 §5.17。

### 5.17 `GET /api/bastille/jobs/{jobId}`（长任务进度轮询）

`{jobId}` 为路径参数（来自 bootstrap / templates/apply 响应）。

响应 `data`：

```json
{ "status": "building", "log": ["行1", "行2"] }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | `building` \| `done` \| `failed` |
| `log` | string[] | 输出日志行（保留最近 500 行） |

> 与 Docker `build-progress` 的差异：**Bastille 进度没有 `image` 字段**。
> 任务不存在/已过期 → `400`，`data` 为 `任务不存在或已过期`。

### 5.18 端口转发（rdr）

```
POST   /api/bastille/rdr
DELETE /api/bastille/rdr
GET    /api/bastille/rdr?jail=<name>
```

`POST`/`DELETE` 请求体相同：

```json
{ "jail": "mc-jail", "proto": "tcp", "hostPort": 25565, "jailPort": 25565 }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `jail` | string | **是** | jail 名 |
| `proto` | string | **是** | `tcp` \| `udp` |
| `hostPort` | int | **是** | 宿主端口（`>0`） |
| `jailPort` | int | **是** | jail 端口（`>0`） |

`POST`/`DELETE` 响应 `data`：`true`。

`GET` 响应 `data` 数组（`?jail=` 过滤单个 jail，缺省返回全部）：

```json
[
  { "jail": "mc-jail", "proto": "tcp", "hostPort": 25565, "jailPort": 25565 }
]
```

> rdr 依赖 PF 防火墙；未初始化时 `POST` 失败的错误消息会提示先调用 `POST /api/bastille/setup {"mode":"firewall"}`。

### 5.19 运行会话（jail 内后台长任务）

#### `POST /api/bastille/jails/{name}/run`

请求体：

```json
{ "command": "java -Xmx2G -jar server.jar nogui", "cwd": "/data", "watch": false }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `command` | string | **是** | 以 shell 语义执行（`sh -c` 包装） |
| `cwd` | string | 否 | jail 内工作目录（服务端前置 `cd <cwd> &&`） |
| `watch` | bool | 否 | 看门狗：进程退出后自动 `bastille stop` 该 jail |

响应 `data`：

```json
{ "sessionId": "s-1" }
```

#### `GET /api/bastille/jails/{name}/run/{session}`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `tail` | 否 | `since` 缺省时返回最后 N 行，缺省 `200` |
| `since` | 否 | 日志字节偏移，`>0` 时返回自该偏移后的**新增**内容 |

响应 `data`：

```json
{ "running": false, "exitCode": 0, "offset": 12345, "log": "…内容…" }
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `running` | bool | 进程是否仍在运行 |
| `exitCode` | int? | 退出码，**仅 `running=false` 时存在** |
| `offset` | int | 本次末尾日志字节偏移（客户端增量游标，下轮作 `since`） |
| `log` | string | 自 `since` 后的新增内容；`since` 缺省时返回最后 `tail` 行 |

> 节点重启后会话不在内存，服务端回退读磁盘日志：`running=false`、无 `exitCode`、`offset` 为日志字节数。

#### `POST /api/bastille/jails/{name}/run/{session}/stdin`

请求体：

```json
{ "input": "say hello\n" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `input` | string | 否 | 写入进程 stdin（**原样透传**，客户端自带换行） |

响应 `data`：`true`。

#### `POST /api/bastille/jails/{name}/run/{session}/stop`

无请求体。SIGTERM → 10s 超时 SIGKILL。响应 `data`：`true`。

#### `DELETE /api/bastille/jails/{name}/run/{session}`

无请求体。终止进程（运行中则 SIGKILL）+ 删除日志缓冲/磁盘日志。响应 `data`：`true`。

### 5.20 文件管理（jail 内路径）

> 所有 `path` 为 **jail 内路径**（如 `/data/config`）；`/` 开头表示 jail root 内绝对路径，相对路径按 root 解析；`..` 越界与符号链接指向 jail 外均被拒绝（400）。

#### `GET /api/bastille/jails/{name}/files`

| 查询参数 | 必填 | 默认值 | 含义 |
|----------|------|--------|------|
| `path` | 否 | 空（= root `/`） | jail 内目录路径 |
| `page` | 否 | 1 | 页码（**1 起**） |
| `page_size` | 否 | 100 | 每页条数 |

响应 `data`：

```json
{
  "items": [
    { "name": "config", "path": "/data/config", "isDir": true, "size": 0, "mtime": "2026-08-14 12:34:56" }
  ],
  "page": 0,
  "pageSize": 100,
  "total": 5,
  "absolutePath": "/data"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `items` | array | 目录条目（目录在前，按名称排序） |
| `items[].name` | string | 文件/目录名 |
| `items[].path` | string | jail 内绝对路径 |
| `items[].isDir` | bool | 是否目录 |
| `items[].size` | number | 字节大小（目录为 0） |
| `items[].mtime` | string | 修改时间，格式 `2006-01-02 15:04:05` |
| `page` | int | **0 基页码（请求 page-1）** |
| `pageSize` | int | 每页条数 |
| `total` | int | 目录内总条数 |
| `absolutePath` | string | 规范化后的 jail 内目录路径（如 `/data`） |

#### `GET /api/bastille/jails/{name}/files/content`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `path` | **是** | jail 内文本文件路径 |

响应 `data`：**字符串**（文件文本）。上限 8 MiB，超限返回 400 并提示改用下载接口。

#### `PUT /api/bastille/jails/{name}/files/content`

请求体：

```json
{ "path": "/data/config/server.properties", "content": "..." }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `path` | string | **是** | jail 内目标文件路径（覆盖写，父目录自动创建） |
| `content` | string | 否 | 文件内容 |

响应 `data`：`true`。

#### `DELETE /api/bastille/jails/{name}/files`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `path` | 否 | 待删除路径（文件/目录，递归，幂等）；**为空或根路径 `/` 时禁止删除** |

响应 `data`：`true`。

#### `POST /api/bastille/jails/{name}/files/mkdir`

请求体：`{ "path": "/data/logs" }`。递归创建。响应 `data`：`true`。

#### `POST /api/bastille/jails/{name}/files/touch`

请求体：`{ "path": "/data/logs/latest.log" }`。新建空文件（已存在则不覆盖）。响应 `data`：`true`。

#### `POST /api/bastille/jails/{name}/files/upload`

| 查询参数 | 必填 | 默认值 | 含义 |
|----------|------|--------|------|
| `path` | 否 | `/` | 目标目录（jail 内） |

multipart 表单，字段名 `file`。**只取文件名**、丢弃客户端路径部分。响应 `data`：

```json
{ "path": "/data/config/server.properties" }
```

#### `GET /api/bastille/jails/{name}/files/download`

| 查询参数 | 必填 | 含义 |
|----------|------|------|
| `path` | **是** | jail 内文件路径 |

**二进制流响应**（`Content-Disposition: attachment`）。

---

## 6. 长任务轮询模式总结

| 长任务 | 启动端点 | 启动响应 | 轮询端点 | 轮询 data |
|--------|----------|----------|----------|-----------|
| 镜像构建 | `POST /api/image/build` | `{jobId}` | `GET /api/image/build-progress?jobId=` | `{status, log[], image}` |
| Bastille bootstrap | `POST /api/bastille/bootstrap` | `{jobId}` | `GET /api/bastille/jobs/{jobId}` | `{status, log[]}` |
| Bastille 模板应用 | `POST /api/bastille/templates/apply` | `{jobId}` | `GET /api/bastille/jobs/{jobId}` | `{status, log[]}` |

- `status`：`building` \| `done` \| `failed`。
- `log`：字符串数组，保留最近 500 行。
- 前端以固定间隔轮询直至 `status != "building"`；任务已过期时返回 `400`（`data` 为中文提示）。
- `POST /api/image/pull`、`POST /api/image/import`、`POST /api/bastille/jails/{name}/export`、`POST /api/bastille/jails/import` 为**同步**端点（CLI 最长 10 分钟），直接等最终结果，无 jobId。

---

## 7. 关键差异速查（前端易踩坑点）

1. **`vnet` 类型**：Bastille create 的 `vnet` 既接受 bool 也接受字符串，字符串取值 `none|vnet|bridge`；新代码请传字符串。
2. **`cpus` 类型**：Docker create/limits 是 `number`（float64）；Bastille create/limits 是 `int`。
3. **创建后 rdr**：Bastille 的端口映射**不在 create body 里**，创建成功后前端需自行逐条调 `POST /api/bastille/rdr`。
4. **文本响应**：`logs`/`console`/`exec`/`cmd`/`pkg`/`files/content` 的 `data` 都是**纯字符串**，不是 `{output}`。
5. **时间格式不统一**：Docker 相关 `createdAt` 为 ISO-8601；Bastille releases `createdAt`、files `mtime` 为 `2006-01-02 15:04:05`；Bastille jails 的 `createdAt` 恒为空串。
6. **files 列表分页**：`page` 参数 1 起，但响应里的 `page` 是 **0 基**（请求 page=1 时响应 page=0）。
7. **错误信封**：`status` 字段与 HTTP 状态码一致（非旧版「恒 200」），前端按 `status !== 200` 判断失败。
8. **平台能力**：先调 `GET /api/container/info`，`available=false` 时不要调任何操作端点（会 501）。
