# 容器环境（Docker / Bastille）

- **Linux 节点**：`docker` CLI 可用时暴露 Docker 全功能（容器 / 镜像 / 卷 / 网络，
  含构建长任务与克隆 / 限额），端点见 `NODE_API.md` §6.1。
- **FreeBSD 节点**：暴露 Bastille 全功能（jail 创建 / 启停 / 克隆 / 导入导出 /
  rdr 端口转发 / setup 环境初始化），服务端包装 `bastille` CLI。
- 能力探测 `GET /api/container/info`：CLI 缺失时 `available=false`，客户端自动隐藏容器 UI。
- 客户端契约细节见 `docs/container-support.md`（字段级契约，实现须逐条对齐）。

>  **Bastille PF 初始化注意事项**：`bastille setup firewall` 生成的
> `/etc/pf.conf` 把 `block in all` 放在 `pass in proto tcp port ssh` **之前**，
> 而 PF 是 last-match 语义——直接 `service pf start` 会立刻切断 SSH 等一切入站连接。
> 启用 PF 前必须把管理流量放行（SSH、ICMP）移到 `block in all` 之前并加 `quick`：
>
> ```
> pass in quick proto tcp from any to any port ssh flags S/SA keep state
> pass in quick inet proto icmp from any to any icmp-type echoreq keep state
> block in all
> ```
>
> 未初始化 PF 时 rdr 端口转发会失败（`pfctl: /dev/pf: No such file or directory`）：
> 需先 `POST /api/bastille/setup {"mode":"firewall"}` 写入 pf.conf，再手动
> `service pf start`（bastille setup 只写配置、不启动服务）。
