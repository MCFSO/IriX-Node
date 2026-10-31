# 安全说明

- 所有 API 请求都必须先 `POST /api/auth/login` 取得会话 token（30 天有效），
  再以 `X-Auth-Token` 头、`Authorization: Bearer` 头或 `?token=` 查询参数携带；
  未认证或会话过期一律 403。首次启动会在日志中显示一次随机生成的管理员账户凭据。
- **客户端设备绑定（v1.4.0+）**：账户可配置 `devicePolicy=required`，客户端必须
  用设备私钥（推荐 TPM 2.0 / 系统密钥库）对服务端一次性挑战签名才能登录；
  登录后会话绑定设备，每个请求再用 HMAC-SHA256 + 单调序号证明，token 单独泄露
  无法调用 API。`required` 强制节点启用 TLS，客户端必须固定证书指纹，否则无法
  防中间人；设备登录与已绑定会话同样要求 TLS。协议见 `docs/device-binding.md`。
  默认 `optional`，旧客户端不受影响。
- **管理员账户默认仅限内网**：管理员（`is_admin`）默认只允许从环回（127.0.0.1/::1）或
  RFC 1918 内网地址（10/8、172.16/12、192.168/16；IPv6 为 `fc00::/7`、`fe80::/10`）
  登录与访问。公网来源即使密码正确也不签发会话，已签发的会话从公网使用同样 403；
  普通账户不受此限制。可用 `-admin-intranet-only=false` 或配置文件
  `adminIntranetOnly=false` **显式关闭**（默认 true）；关闭后管理员可从公网登录，
  请自行承担风险并配合 TLS、设备绑定、强密码与防火墙。
- **DDoS/CC 防御恒开、不可关闭**：路由之前统一执行封禁表、HTTP 头部规范校验与
  频率/并发限制。单 IP 令牌桶与在途并发上限拦截请求洪峰；全节点匿名预算
  （默认 1000 请求/秒、突发 2000）跨 IP 聚合，轮换 IP 无法重置总额度；
  跟踪表满时淘汰旧条目而不是拒绝所有新来源。同一 User-Agent
  并发请求过多或频率超限时直接封禁该 User-Agent（跨 IP 聚合）；自动封禁到期
  自动解除，重复触发时长翻倍（上限 24 小时）；人工封禁持久化到
  `{data}/defense.json`。阈值可通过 `config.json` 的 `defense` 块调整，
  但不存在关闭开关（`defense.enabled=false` 会被忽略）。
- **HTTP 头部规范校验**：请求走私形态（`Content-Length` 与 `Transfer-Encoding`
  并存、重复 `Content-Length`/`Host`、逐跳头注入、非 chunked 的
  `Transfer-Encoding`）、重复单例头（`Authorization`/`X-Auth-Token`/`User-Agent`）、
  方法覆盖头、非法 `Upgrade`、超量/超大请求头一律拒绝；`Via`、`Forwarded`、
  `X-Forwarded-*`、`X-Real-IP`、`Proxy-Connection` 等代理链标记同样拒绝。
  节点不信任任何转发头，也不从转发头推导来源 IP。
- **反代与环回**：环回来源视为本机面板/CLI/同机反向代理，不受频率限制与自动
  封禁影响（同机反代追加的 `X-Forwarded-*` 只计数不拦截），但人工/配置的
  User-Agent 封禁与硬性头部违规仍对其生效。其他来源携带代理链标记会被拒绝并累计
  封禁。节点不支持跨机反向代理透传真实客户端 IP 的部署方式，需要反代时请部署在
  同机（环回）。
- 文件操作被限制在实例工作目录内（`..` 越界会被拒绝）。
- 实例 `cwd` 拒绝文件系统/磁盘根、系统目录与 Windows 用户 Profile 目录
  （`\Users` 整树，仅豁免 `%TEMP%`）。
- 集群拉取（节点间直传）默认拒绝环回、链路本地与本机地址（防认证后 SSRF），
  并默认拒绝 RFC1918 内网地址；LAN 直传需显式配置 `-transfer-allow-cidr`，
  该配置即信任边界——仅放行自己管辖的网段。
- 建议配合防火墙仅监听 127.0.0.1；如需局域网访问，请为管理员账户设置强密码
  并首次登录后立即修改，同时限制可访问来源。
- 配对码机制与 `-apikey` / `apiKey` 固定密钥自 v1.2.0 起已移除，
  `?apikey=` / `X-Api-Key` 不再被本节点接受（仅 v1.1.0 及更早版本存在）。

更深入的安全设计（等保二级合规要点）见 `docs/compliance-l2.md` 与
`docs/accounts-design.md`（账户权限模型）。
