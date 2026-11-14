# 功能

## 能力概览

- **概览**：`GET /api/overview` 返回主机信息、内存、实例统计。
- **自有时间系统（Roughtime）**：节点不信任系统墙钟，用 Roughtime（RFC 10049，
  兼容 Cloudflare/int08h/roughtime.se 等已部署旧格式）获取带签名与 Merkle 证明的时间区间；
  多源区间相交取最大一致组（默认至少 2 台）剔除虚假源；受纪律时钟锚定单调时钟，
  持续拟合晶振漂移（ppm）并平滑校正，小误差按最多 500ppm 缓变、大误差才步进；
  断网时进入 holdover，不确定度按漂移上界增长，状态落盘 `{data}/timesys.json`。
  会话/封禁/审计/实例时间戳、响应 `time` 字段、HTTP `Date`、日志时间戳、
  **TLS 证书签发与证书链校验**全部使用该时间；自签证书在节点时间失效时自动重签。
  状态与手动同步：`GET /api/time`、`POST /api/time/sync`。详见
  [时间系统](time-system.md)。
- **实例管理**：列表 / 详情 / 创建 / 更新 / 删除，启动 / 停止 / 重启 / 强制终止，
  命令下发（标准输入），输出日志（环形缓冲）。
- **文件管理**：列表 / 读写 / 删除 / 移动 / 复制 / 压缩 / 解压 / 新建目录 /
  新建文件，以及带票据的下载 / 上传直连通道。
- **审计日志**：每次 API 请求（时间、来源 IP、方法、路径与参数、状态码、耗时、
  请求体）落盘到 `{data}/logs/audit.log`，查询串中的会话 `token` 与历史字段
  `apikey` 自动打码；下载/上传直连通道同样在审计范围内。
- **客户端设备绑定**：客户端生成设备密钥（Ed25519 / P-256，私钥推荐放 TPM 2.0
  或系统密钥库），登录时对服务端一次性挑战签名；会话绑定设备后，每个请求再用
  HMAC-SHA256 + 单调序号证明，单独泄露 token 无法调用 API。账户策略
  `optional`（默认，兼容旧客户端）/ `required`（无已绑定设备不能登录）。
  设备登录与绑定会话要求节点启用 TLS（`required` 还要求至少一台设备）。
  管理接口：`GET/POST /api/auth/devices`、`POST /api/auth/devices/revoke`、
  `PUT /api/accounts/device-policy`、`GET/DELETE /api/accounts/devices`。
  协议与客户端交接清单见 `docs/device-binding.md`。
- **DDoS/CC 防御（恒开，不可关闭）**：在路由与审计之前执行封禁表、HTTP 头部规范
  校验与频率/并发限制。单 IP 令牌桶限速与在途并发上限拦截请求洪峰；全节点还有
  **跨 IP 聚合的匿名请求预算**（默认 1000 请求/秒、突发 2000），海量 IP 轮换无法
  重置总额度；跟踪表满时淘汰旧条目继续服务，不会因表满拒绝所有新来源。同一
  User-Agent 并发请求过多（跨 IP 聚合）或频率超限时**直接封禁该 User-Agent**；
  代理链标记（`Via`、`Forwarded`、`X-Forwarded-*`、`X-Real-IP`、`Proxy-Connection`
  等）、请求走私形态（`Content-Length` 与 `Transfer-Encoding` 并存、重复单例头）、
  方法覆盖头、非法 `Upgrade`、超量请求头均被识别并拒绝。自动封禁到期自动解除，
  重复触发时长翻倍（上限 24 小时）；人工封禁持久化到 `{data}/defense.json`。
  状态与人工封禁接口：`GET /api/defense/status`（阈值/统计/封禁表/UA 排行/
  头部规则命中排行/最近事件）、`POST /api/defense/ban`（人工封禁）、
  `DELETE /api/defense/ban`（解封；环回来源的该请求豁免 UA 封禁，避免管理员
  误封自己的浏览器 UA 后无法恢复）。
  防御只能调整阈值（`config.json` 的 `defense` 块），没有关闭开关，
  写 `defense.enabled=false` 会被忽略并在启动日志提示。
- **管理员内网限制（可开关）**：管理员账户（`is_admin`）默认只允许从环回或
  RFC 1918 内网地址（IPv6 为 `fc00::/7` / `fe80::/10`）登录与访问；公网来源即使
  凭据正确也不签发会话、已有会话也返回 403。普通账户不受限制。可用
  `-admin-intranet-only=false` 或配置文件 `adminIntranetOnly=false` 关闭（默认 true），
  关闭后管理员可从公网登录，风险自负。跨公网集群调用若使用管理员 token
  会被拒绝，请改用只授予集群端点权限的普通账户。
- 实例配置持久化在 `{data}/instances.json`，采用**异步合并落盘**：
  增删改只写入内存并打脏标记，后台循环按防抖窗口（默认 500ms）合并写盘，
  优雅关停时兜底落盘。进程被强杀（断电 / kill -9）最多丢一个窗口内的
  配置变更。Windows、Linux/Android、FreeBSD 上的受管实例进程身份另行同步写入
  `{data}/processes/`；节点重启前会核对 PID 与创建标识，清理异常退出遗留的
  进程，再执行自动启动。身份无法核对或清理失败时节点拒绝启动，避免重复拉起。
  Linux/Android 还会随节点退出终止直接子进程；Windows 可用时使用 Job Object
  随节点退出清理进程树。
  节点不会终止未由本节点启动的其他 Java 进程。

## 数据目录结构

```
{data}/
  instances.json   # 实例配置列表（保险库模式下加密存储，见下）
  accounts.db      # 账户库（-accounts-driver 可换 mysql/postgres）
  defense.json     # 人工封禁的 IP / User-Agent（自动封禁仅在运行期）
  timesys.json     # 时间系统状态（频率校正/上次同步/保持锚点）
  processes/       # 受管进程身份记录（实例退出后删除）
  vault/           # 加密保险库（-vault 开启时）
    vault.json     # 用户/密钥包裹/恢复令牌哈希/迁移标记
    objects/       # 密文对象（文件名随机化，无明文痕迹）
    index.json.enc # 加密索引（文件名/大小/时间不泄露）
  tls/             # 自签 TLS 证书（-tls-mode auto 时）
  backup/audit/    # 审计日志轮转归档（防覆盖丢失）
  logs/            # 实例日志 {uuid}.log（-instance-log 开启时）
                   # 审计日志 audit.log（-audit-log 开启时）
```
