# 高并发压测调优（百万级连接）

本节点用 Go 标准库实现，单进程可承载大量并发连接。但若要把成功率从压测常见的
~44% 拉到接近 100%，**仅靠改代码不够，还需部署侧放行资源上限**——绝大多数
连接被拒（connection refused）发生在到达 HTTP 层之前，是操作系统层面资源耗尽：

- **文件描述符上限（最关键）**：每个 TCP 连接占用一个 fd。Linux 默认软上限常是
  1024，百万连接会瞬间耗尽，Accept 返回 `EMFILE`，客户端表现为大量
  `connection refused`。节点在 Unix 上启动时会**自动把 `RLIMIT_NOFILE` 软上限
  提升到硬上限**（见 `rlimit_unix.go`），但硬上限本身仍需部署侧调高：
  ```bash
  # 临时（当前 shell / systemd 服务）
  ulimit -n 2000000
  # 永久：/etc/security/limits.conf 加 * soft nofile 2000000 / * hard nofile 2000000
  # 或 systemd 单元加 LimitNOFILE=2000000
  ```
- **监听积压 / SYN 队列**：内核 `net.core.somaxconn`（常 4096）与
  `net.ipv4.tcp_max_syn_backlog` 限制未完成/已完成连接队列长度，达到上限后
  新 SYN 被丢弃。压测前调大：
  ```bash
  sysctl -w net.core.somaxconn=65535
  sysctl -w net.ipv4.tcp_max_syn_backlog=65535
  ```
- **客户端端口范围**：百万并发从单机发起时，客户端需足够多的临时端口
  （`net.ipv4.ip_local_port_range` 扩到 `1024 65535`）且开启端口快速复用
  （`net.ipv4.tcp_tw_reuse=1`）以防 TIME_WAIT 堆积。
- **非分页池（Windows）**：每个 TCP 连接消耗非分页池内存，连接数受此而非 fd
  限制；需通过注册表 `TcpNumConnections` 等调大，并注意物理内存余量。

代码层面已做的对应优化：移除每连接 Accept 日志（避免拖慢唯一 Accept 循环）、
`MaxHeaderBytes` 下调到 64KiB（减少百万连接的读缓冲常驻）、审计中间件对
GET/HEAD 等无正文请求跳过 body 包装以减少每请求分配。

## 请求路径优化（鉴权 / 概览 / 门禁）

压测 `/api/overview` 这类高频轮询接口时，瓶颈通常不在 HTTP 层而在每请求的
重复 I/O。已做如下优化：

- **鉴权三级读取**：`accountSystem` 按「进程内缓存 → Redis → SQL」读取会话与
  端点权限。默认部署（SQLite、未配 Redis）此前每个 API 请求要打 2~3 次 SQL
  （`lookupSession` + `rootPasswordSet` + `loadPermissions`），命中进程内缓存后
  降为零查询。写路径（登出/续期/改权限/改密/删账户）会主动失效对应缓存键，
  因此「登出后立即不可访问」「权限收紧立即生效」不受 TTL 影响。
  实测（本机 16 并发）：普通账户 token 通道 2440 → 约 3500 QPS。
- **概览静态信息缓存**：主机名/系统类型/平台/内核版本/发行版运行期内恒定，
  进程内只采集一次。此前每次请求都读磁盘取版本信息（Windows 读
  `C:/Windows/System32/Release.txt`，Linux 各读一次 `/etc/os-release`）。
- **内存与磁盘采样合并**：`totalMem`/`freeMem`/`memUsage` 原本各自采样一次
  （一次请求三遍 `/proc/meminfo`），合并为一次快照三处复用；磁盘容量按 5 秒
  TTL 缓存，不再每请求 statfs。
- **`ReadMemStats` 缓存**：`runtime.ReadMemStats` 是 stop-the-world 操作，
  概览接口每请求调用会反复暂停世界（堆越大代价越高），改为 1 秒 TTL 缓存
  （`processAllocCached`）。
- **保险库门禁去串行**：`vaultGate` 原实现每个数据面请求都取 `v.mu` 写锁，
  等于把所有 API 串行化；改为读锁校验 + 每 10 秒一次的续期写锁。

## GC 与内存（务必注意）

负载调谐器（`loadtuner.go`）**不得**在初始化时关闭 GC。此前实现在包初始化即
`SetGCPercent(-1)`，期望由三态机接管；但 `tick()` 在候选状态等于当前状态时
直接返回、不应用参数，而初始状态就是 `loadNormal`——节点长期处于 normal 区间
（goroutine 21~1999、CPU 5%~60%）时状态永不切换，GC 也就再不会被打开，堆只增
不减直至 OOM。现在的做法是初始化只读基线 GOGC、保持 GC 正常工作，三态机仅在
状态切换时调整；低内存设备另有 `GOMEMLIMIT` 兜底（嵌入式档位自动设置）。

> 提示：若压测目标是「节点进程本身的吞吐上限」而非真实百万长连接，更现实的做法是
> 复用 keep-alive 连接（`mltf` 的并发数设小、用连接池），这样能直接验证 handler
> 吞吐而不被 OS fd 上限干扰。
