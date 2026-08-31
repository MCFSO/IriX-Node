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

> 提示：若压测目标是「节点进程本身的吞吐上限」而非真实百万长连接，更现实的做法是
> 复用 keep-alive 连接（`mltf` 的并发数设小、用连接池），这样能直接验证 handler
> 吞吐而不被 OS fd 上限干扰。
