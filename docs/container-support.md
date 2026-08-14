# 容器化支持设计:Docker + Bastille

> 目标:让「实例管理页面」成为 Docker 与 Bastille 的完整管理入口。
> - **本地实例管理页**(Home 实例列表 + 本地实例详情):支持 **Docker 全功能**(仅当本机可用 docker CLI)。
> - **多机实例管理页**(集群实例详情 + 节点详情):Linux 节点暴露 **Docker 全功能**,FreeBSD 节点暴露 **Bastille 全功能**。
> - 两者均支持「容器化实例」:MC 服务器跑在容器 / jail 中,启停、控制台、文件、备份全部穿透到容器运行时。

---

## 1. 架构:统一容器后端抽象

所有容器能力通过一个后端接口暴露,上层 UI 只依赖该接口,不感知底层是 Docker 还是 Bastille:

```
ContainerBackend (lib/services/container/container_backend.dart)
├── DockerBackend
│   ├── DockerCliBackend        — 本地:spawn `docker` CLI(Windows/macOS/Linux)
│   └── NodeDockerBackend       — 远程:经 NodeApiClient 调 irix-node /api/container/*
├── BastilleBackend
│   └── NodeBastilleBackend     — 远程:经 NodeApiClient 调 irix-node /api/bastille/*
```

### 接口面

```dart
abstract class ContainerBackend {
  ContainerRuntime get runtime;      // docker | bastille
  String get displayName;            // 'Docker' | 'Bastille'
  bool get isRemote;                 // 本地 CLI 后端为 false

  // —— 环境 ——
  Future<ContainerEnvironmentInfo> environment();  // 版本、运行时平台、可用性

  // —— 容器生命周期 ——
  Future<List<ContainerInfo>> listContainers();
  Future<ContainerInfo> createContainer(CreateContainerRequest req);
  Future<void> startContainer(String idOrName);
  Future<void> stopContainer(String idOrName);
  Future<void> restartContainer(String idOrName);
  Future<void> removeContainer(String idOrName, {bool force});
  Future<String> containerLogs(String idOrName, {int? tail});
  Future<void> execInContainer(String idOrName, String command);
  Future<ContainerStats> containerStats(String idOrName);

  // —— 镜像 / 模板 ——
  Future<List<ImageInfo>> listImages();
  Future<void> pullImage(String name);
  Future<void> removeImage(String name);
  Future<BuildJob> buildImage(BuildImageRequest req);   // 构建 → jobId,进度轮询

  // —— 卷 ——
  Future<List<VolumeInfo>> listVolumes();
  Future<void> removeVolume(String name);

  // —— 网络 / 端口映射 ——
  Future<List<NetworkInfo>> listNetworks();
  Future<void> addPortMapping(PortMappingRequest req);
  Future<void> removePortMapping(PortMappingRequest req);
}
```

**要点**:

- 容器实例化时,**后端实例的选择由页面所在的上下文决定**:本地实例详情页 → `DockerCliBackend`;集群实例详情页 → 按节点 `platform` 返回 `NodeDockerBackend`(Linux)或 `NodeBastilleBackend`(FreeBSD)。
- 资源管理(镜像/卷/网络/模板/jail 列表)在接口上统一表达,UI 复用同一套组件。
- 本设计**不新增任何 Rust crate**:本地 Docker 走进程调用(与 `ServerProcessManager` 同风格),远程走既有 `NodeApiClient`(Rust http_client)。

---

## 2. 本地 Docker:`DockerCliBackend`

### 通道

直接 spawn `docker` CLI(`Process.run`),命令输出统一加 `--format '{{json .}}'` 逐行解析为 JSON。需要覆盖的命令:

| 功能 | 命令 |
|------|------|
| 可用性 / 版本 | `docker version --format {{json .}}` |
| 容器列表 | `docker ps -a --format {{json .}}` |
| 创建容器 | `docker create [--name] [-p] [-v] [-e] [--restart] [-m] [--cpus] IMAGE [COMMAND]` |
| 启动 / 停止 / 重启 / 删除 | `docker start/stop/restart/rm [-f]` |
| 日志 | `docker logs [-f] [--tail N]` |
| 容器内命令 | `docker exec CONTAINER <cmd>` |
| 统计 | `docker stats --no-stream --format {{json .}}` |
| 镜像列表 / 拉取 / 删除 | `docker images --format {{json .}}` / `pull` / `rmi` |
| 构建 | `docker build -t name:tag -f <dockerfile> <context>`(后台进程,日志流式输出) |
| 卷 / 网络 | `docker volume ls --format {{json .}}` / `docker network ls --format {{json .}}` |

### 与实例的对接

- 本地实例 `runMode = docker` 时,实例卡片的启动/停止/重启按钮改走 `docker start/stop/restart <containerName>`;控制台走 `docker logs -f` + `docker exec` 发命令。
- 实例根目录通过卷挂载进入容器(Minecraft 镜像如 `itzg/minecraft-server` 挂载到 `/data`),文件管理 / 备份仍直接读写本地实例目录,无需穿透。
- 端口映射默认 `25565:25565`,内存经环境变量(`-e MEMORY=2G` 等)传递。

### 可用性检测

应用启动时执行 `docker version`(超时 5s):失败则本地容器 UI 整体隐藏,不影响原生实例功能。Windows/macOS 依赖 Docker Desktop 已安装并在运行。

---

## 3. 多机节点 API 扩展(irix-node)

> 仅 `irix-node` 提供全功能;MCSM 面板保持现有 §6 阉割端点(镜像/容器/网络列表 + 构建镜像),多机 UI 对 MCSM 节点只显示这些受限功能。

### 3.1 运行时信息(能力探测)

```
GET /api/container/info
// 响应 data
{ "runtime": "docker",              // docker | bastille
  "platform": "linux",              // linux | freebsd
  "version": "27.0.3",              // docker 版本 / bastille 版本
  "available": true }
```

节点 `overview` 可同时补 `platform` 字段,供节点列表直接显示「Docker / Bastille」能力标签。

### 3.2 Docker 端点(irix-node,Linux)

```
GET    /api/container/ps?all=1                        → 容器列表
POST   /api/container/create                         → 创建(参数同 §2 表格)
POST   /api/container/{id}/start
POST   /api/container/{id}/stop
POST   /api/container/{id}/restart
POST   /api/container/{id}/kill
DELETE /api/container/{id}
GET    /api/container/{id}/logs?tail=N
POST   /api/container/{id}/exec          body: {command}
GET    /api/container/{id}/stats
GET    /api/image/list
POST   /api/image/pull                    body: {name}
POST   /api/image/build                   body: {dockerfile, name, tag} → {jobId}
GET    /api/image/build-progress?jobId=   → {status: building|done|failed, log: [...], image: "name:tag"}
DELETE /api/image/{name}
GET    /api/volume/list
DELETE /api/volume/{name}
GET    /api/network/list
```

### 3.3 Bastille 端点(irix-node,FreeBSD)

> 命令语法以官方文档 latest 为准（bastille.readthedocs.io / docs.bastillebsd.org）。
> 客户端仅定义 HTTP 契约，以下每条端点附服务端应执行的 bastille 命令。

```
GET    /api/bastille/releases                        → bootstrap 的发行版列表
POST   /api/bastille/bootstrap          body: {release}   → bastille bootstrap RELEASE [update]（后台任务）
GET    /api/bastille/jails                           → jail 列表(含状态/IP/模板 tags)
POST   /api/bastille/jails/create       body: {name, release, ip, type: thin|thick|clone|empty|linux,
                                                vnet: none|vnet|bridge, interface?, volumes?, workdir?,
                                                memoryLimitMb?, cpus?, diskLimitMb?}
                                                 → bastille create [-T|-C|-E|-L] [-V|-B INTERFACE] NAME RELEASE IP
                                                   （empty 类型仅 NAME：bastille create -E NAME）
GET    /api/bastille/jails/{name}/mounts             → MOUNT 挂载列表
POST   /api/bastille/jails/{name}/start
POST   /api/bastille/jails/{name}/stop
POST   /api/bastille/jails/{name}/restart
POST   /api/bastille/jails/{name}/destroy?force=1    → bastille destroy -y [-a] JAIL
                                                        -y 跳过交互确认（必须）;-a 用于摧毁运行中的 jail
POST   /api/bastille/jails/{name}/clone  body: {newName, ip?} → bastille clone TARGET NEW_NAME [IP]
POST   /api/bastille/jails/{name}/export             → bastille export --txz TARGET PATH
                                                        返回 {path}；默认输出到 bastille/backups/
POST   /api/bastille/jails/import        body: {file, release?, force?}
                                                 → bastille import [-f] FILE [RELEASE]
                                                        RELEASE 为「导入到指定发行版」；-f 跳过校验和
POST   /api/bastille/jails/{name}/limits body: {memoryMb?, cpus?, diskMb?}
                                                 → memoryMb: bastille limits JAIL add memoryuse <N>M
                                                   cpus:     bastille limits JAIL cpu 0..N-1（cpuset，核数换算）
                                                   diskMb:   zfs set quota=<N>M（jail 数据集）
GET    /api/bastille/jails/{name}/console?tail=N     → 日志尾部
POST   /api/bastille/jails/{name}/cmd    body: {command}
GET    /api/bastille/jails/{name}/config             → jail.conf 属性
GET    /api/bastille/templates                       → 已 bootstrap 的模板列表(project/template)
POST   /api/bastille/templates/apply     body: {jail, template, args: {KEY=VALUE}}
POST   /api/bastille/rdr                 body: {jail, proto, hostPort, jailPort}
                                                 → bastille rdr JAIL tcp|udp HOST_PORT JAIL_PORT
DELETE /api/bastille/rdr                 body: 同上
                                                 → CLI 无单条删除：服务端读取 rdr list → clear → 重放其余规则
GET    /api/bastille/rdr?jail=                        → 转发规则列表（bastille rdr [JAIL] list 解析）
POST   /api/bastille/setup               body: {mode: default|firewall|vnet|bridge|shared|linux,
                                                extIf?, tunIf?, addr?}
                                                 → {ok, detail?}
                                                 （服务端统一附加 -y 避免交互阻塞）
                                                 default:  bastille setup（自动 loopback+firewall+storage）
                                                 firewall: bastille setup firewall [extIf]
                                                 vnet:     bastille setup vnet [extIf tunIf addr]（部分版本交互式）
                                                 bridge:   bastille setup bridge
                                                 shared:   bastille setup shared [extIf]
                                                 linux:    bastille setup linux（Linuxulator + debootstrap）
```

> 实现提示:Bastille 无面板 API,irix-node 在 FreeBSD 上通过 `Process.run('bastille', ...)` 包装上述命令;bootstrap / 模板应用等长任务以 jobId + 日志流模式暴露。
>
> create 的 `type` 映射:thin(无标志,默认) / thick(-T) / clone(-C) / empty(-E) / linux(-L);
> `vnet=none` 为共享宿主网络(默认),`vnet=vnet` 为 `-V`(INTERFACE 须为物理网卡),
> `vnet=bridge` 为 `-B`(INTERFACE 须为已存在的桥接网卡);VNET 时 IP 必须含子网掩码;
> Linux jail(-L) 与任何 VNET 模式互斥。
> `volumes`(宿主机路径:jail 内路径)在创建后以 nullfs 挂载(`bastille mount JAIL HOST JAILPATH nullfs`);
> `workdir` 设置 `exec.start` 的工作目录(数据目录挂载后强制 cwd)。
>
> Docker 端点同步新增:POST /api/container/{id}/clone body: {name};
> POST /api/container/{id}/limits body: {memoryMb?, cpus?};create body 增加 workdir / diskLimitMb。

### 3.4 响应约定

沿用统一响应体 `{status, data, time}`;所有列表项使用与容器后端接口一致的字段命名(见 §1 的 Dart 模型),避免 UI 层做两套解析。

---

## 4. 数据模型变更

### ServerInstance(lib/models/server_instance.dart)

```dart
enum RunMode { native, docker }

class ServerInstance {
  RunMode runMode;                    // 持久化,默认 native
  ContainerConfig? container;         // runMode == docker 时有效
}

class ContainerConfig {
  String image;                       // 镜像,如 itzg/minecraft-server:latest
  String? containerName;              // 缺省由实例名派生
  List<String> ports;                 // ["25565:25565"]
  List<String> volumes;               // ["<rootPath>:/data"]
  Map<String, String> env;            // MEMORY=2G, EULA=TRUE ...
  String? restartPolicy;              // no | on-failure[:N] | always | unless-stopped
  int? memoryLimitMb;                 // -m
  int? cpus;                          // --cpus
}
```

### ClusterInstance(lib/models/cluster_instance.dart)

```dart
String? runtime;        // "docker" | "bastille" | null(原生)
String? containerId;    // 容器名 / jail 名,与 remoteUuid 的对应关系
```

---

## 5. UI 设计

### 5.1 本地实例详情页(instance_detail_screen.dart)

- 设置 tab 新增「运行方式」:原生进程 / Docker 容器(检测到 docker CLI 时可选;选择容器后展开容器配置表单:镜像、端口、环境变量、重启策略、资源限制)。
- 新增 **「容器」tab**(仅 runMode == docker 或后端可用时显示):
  - 容器状态卡片(运行 / 已停止 / 重启策略 / 内存 / CPU)
  - 端口映射表(可增删,对应 `docker run -p` 变更 → 重建容器)
  - 挂载卷列表
  - 最近日志尾部(复用控制台 UI)
- 总览 tab 的启动 / 停止 / 重启按钮在 runMode == docker 时切换为容器操作。

### 5.2 多机实例详情页(remote_instance_detail_screen.dart)

- 按节点能力动态渲染 tab:
  - `platform=linux` → 「Docker」tab:镜像列表(拉取/删除/构建)、容器列表、卷、网络 —— 与本地页共用组件
  - `platform=freebsd` → 「Bastille」tab:jail 列表、模板应用、rdr 转发、bootstrap 发行版管理
  - MCSM 节点 → 保持现有受限端点映射(仅列表 + 构建)
- 实例的「容器化运行」由节点侧创建容器 / jail 并把实例文件挂载进 /data 或 jail 目录。

### 5.3 节点详情页(node_detail_screen.dart)

- 新增「容器环境」概览卡:运行时类型、版本、可用性;点击进入对应的容器资源管理(与实例页共用同一套页面组件)。

---

## 6. 实施计划

| 阶段 | 内容 | 交付物 | 状态 |
|------|------|--------|------|
| **P0** | 容器后端抽象 + 本地 `DockerCliBackend`(docker CLI 解析、生命周期、镜像/卷/网络) | `lib/services/container/` + 单测(解析测试) | ✅ 已完成 |
| **P0** | `ServerInstance` 模型扩展(`RunMode`/`ContainerConfig`)+ 数据库 v6 迁移 + 本地实例详情页容器化(运行方式表单 + 容器 tab + `ContainerEnvironmentPanel`) | 本地可容器化运行 MC | ✅ 已完成 |
| **P1** | `NodeApiClient` 容器/Bastille 端点 + `NodeDockerBackend`/`NodeBastilleBackend`(含 MCSM 受限回退) | `NODE_API.md` §6.1 已发布,服务端按契约实现 | ✅ 客户端已完成,服务端待对接 |
| **P1** | 多机实例详情页「容器」tab + 节点详情页「容器」tab(按节点平台选 Docker/Bastille 后端,替换旧 `_DockerEnvScreen`) | 复用 `ContainerEnvironmentPanel` | ✅ 已完成(服务端就绪后即生效) |
| **P2** | 多机 Bastille 专属能力(Bastillefile 模板、rdr 编辑、bootstrap 任务进度 UI) | 面板按后端能力裁切 | ✅ 已完成:rdr 增删查、bootstrap、运行时感知 Tab |
| **P2** | Bastille 全功能扩展:bastille setup 初始化(pf/vnet/linux)、创建 jail(类型/VNET/桥接/IP)、clone、destroy -a、import/export、资源限制(内存/CPU/磁盘)、数据目录挂载 + 强制工作目录 | 后端接口 + `ContainerEnvironmentPanel` 设置/转发 Tab + 各对话框 | ✅ 已完成(客户端契约,服务端待按 §3.3 对接) |
| **P2** | 集群实例容器化运行 + 迁移时的容器重建 | 容器实例可迁移 | 待做 |

## 7. 边界与回退

- 本地无 docker CLI → 容器 UI 整体隐藏,实例保持原生运行。
- MCSM 节点永远只有受限容器功能(现有 4 端点),不承诺全功能。
- Bastille 的 `linux` jail 类型(Linuxulator)第一版仅列表展示,创建流程放 P2 之后的增强。
- 容器实例的备份 / 文件管理走挂载目录,不实现 `docker cp` / `bastille cp` 穿透(卷已覆盖 99% 场景)。
