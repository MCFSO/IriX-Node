# IriX Node 加密保险库（Vault）详细设计（v2，评审修订版）

- 状态：已按评审意见修订，待再次评审
- 目标等保级别：等保二级（GB/T 22239-2019 第二级）
- 架构决策：方案 A（私钥留客户端、服务端会话解锁）+ 证书格式方案①（客户端转换、服务端保持零第三方依赖）
- 约束：Go 1.24 纯标准库、零第三方依赖、MCSM 风格 API、中文注释与文档
- 修订说明：本版修复评审提出的 A1–A3 架构级问题、S1–S8 安全合规项、协议含糊点与实现层问题；修订对照见附录 A。

---

## 1. 背景与目标

客户端（GUI）希望通过「证书 + 密码 + TOTP」机制保护节点数据：

- 用户初始设置时：配置 TOTP、设置密码、绑定客户端证书（P12）。
- 每次需要访问节点数据时：输入 TOTP 验证码 + 密码，客户端用私钥对挑战签名（私钥不出设备），服务端校验通过后在内存中解锁数据密钥。
- 按需解密：只有被实际调取的数据才解密，未解锁时数据面 API 一律 403。
- 目标合规：支撑等保二级技术控制项（身份鉴别、登录失败处理、传输保密、安全审计、数据完整性/保密性、备份恢复、剩余信息保护）。

## 2. 决策记录

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| D1 | 解密架构 A：私钥留在客户端，服务端会话内持有数据主密钥 | 私钥永不上送；磁盘被偷无法离线解密；符合「输入 TOTP+密码」的交互描述 |
| D2 | 证书格式方案①：P12/GPG 在客户端解析，服务端只接收标准 PEM/X.509 | 维持「零第三方依赖」红线；证书本来就存在于客户端，格式转换放客户端最自然 |
| D3 | 认证因素三重：TOTP（人）+ 密码（知识）+ 证书签名（设备） | 满足等保二级身份鉴别，且超额实现双因素 |
| D4 | 解锁后数据主密钥仅驻内存，空闲超时自动销毁 | 剩余信息保护；重启即锁定 |
| D5 | 密码派生 KEK 包裹主密钥，密码本身不落盘；**所有包裹统一为 `nonce‖ct‖tag` blob 编码** | 换密码 = rewrap，无需重加密数据；编码定死避免客户端/服务端对不上 |
| D6 | 恢复令牌（一次性打印、仅存哈希）提供兜底恢复路径 | 复用现有配对码机制模式（auth.hash） |
| D7 | TLS **默认 `off`（不强制，由用户显式开启，与现状兼容）**；`tls.mode=auto/manual` 由用户选择；**vault.enabled=true 时要求 TLS 已开启，否则拒绝启动** | 用户修订：TLS 不是强制项，用户自行开启；等保二级「远程管理传输保密」作为部署红线由用户选择。vault 的安全性依赖 TLS，开启 vault（本身即显式选择）时强制 TLS 是底线 |
| D8 | 加密范围分级（A1 决策）：**instances.json 与系统元数据始终加密；实例文件区默认明文，敏感实例按 `vaultFiles=true` 启用「启停物化加密」** | 本地游戏进程无法透明读取密文对象（无 FS 垫片，纯标准库不可行）；全员物化会让启停变成全量拷贝并破坏崩溃安全。D8 由「默认收窄范围」+「可选物化」两级构成，具体语义见 §8.1 |
| D9 | 启停物化语义（D8 的物化级细则）：启动前解密物化到工作目录、停止后加密回收；运行期明文窗口与崩溃残留为既定边界，锁定状态不杀运行中实例 | 见 §8.2 |
| D10 | 初始化会话令牌 `initToken`：init 返回、10 分钟有效、内存态，/totp/verify 与 /cert 凭其鉴权 | 补评审 S3：初始化期鉴权凭证必须有定义 |
| D11 | 追加写崩溃语义：先追加块到 EOF，再原地更新头部块计数，崩溃后孤儿尾截断，不丢数据 | 避免对象级 tmp+rename 对大文件追加的不可接受开销 |
| D12 | 索引独立 DEK（由 masterKey 包裹）+ 脏标记延迟落盘（≤1s）+ 解锁时孤儿对象回收 | 统一「masterKey 只包裹 DEK」表述；避免 10 万条目全量重写写放大 |
| D13 | 迁移两阶段 + 幂等游标 + 进度 API；迁移前检查磁盘余量 | 评审 A2：崩溃可断点续迁、重跑不重复加密、大文件区进度可见 |
| D14 | copy/move 语义定案：**copy = 新对象+新 DEK+新索引项；move = 仅索引项迁移，对象与 DEK 复用（无引用计数）** | 消除「二选一未决」；move 不产生别名，无需引用计数 |
| D15 | 密码过期强制改密合并进解锁：`unlock` 携带 `newPassword` 时同请求完成「解锁 + rewrap」 | 评审 A3：消除「过期 → 不能解锁 → 不能改密」死锁 |
| D16 | 失败计数覆盖整个 unlock/recovery 流程（用户+IP 双维度），不限 TOTP | 评审 S1/S2：与「登录失败处理」控制项对齐 |
| D17 | 会话令牌仅走 `X-Vault-Token` 头，**禁止 query string 传令牌** | 评审 S5：防令牌进访问日志/代理日志/浏览器历史 |

## 3. 威胁模型与信任边界

### 3.1 假设的威胁

| 威胁 | 防护 |
| --- | --- |
| 攻击者窃取磁盘/备份（密文+索引+元数据） | 密文无密钥不可读；文件名随机化 + 索引整体加密，不泄露文件名/大小/时间 |
| 攻击者窃取磁盘后离线暴力破解密码 | PBKDF2 高迭代（默认 600k，可配）；密码复杂度策略；二期可选「证书公钥包裹主密钥」增强（见 §12） |
| 攻击者在线尝试解锁/恢复（有网络访问） | TOTP 必须通过；**unlock/recovery/init-verify 全流程统一计数限速 + 临时锁定**（D16）；响应统一模糊；挑战一次性 |
| 会话令牌被窃取 | 32B 随机、绑定用户与证书指纹、空闲滑动过期；可配置绑定来源 IP；仅头传输（D17） |
| 网络窃听 | vault 开启时强制 TLS；默认 off，由用户显式开启（D7） |
| 解锁会话期间节点进程被攻破 | 残余风险：内存主密钥可读。缓解：空闲超时、手动锁定、审计留痕、masterKey 生命周期锁（S7）。彻底消除需架构 B，不在本期 |
| 私钥泄露（客户端被攻破） | 私钥 + 密码 + TOTP 三者齐备才能解锁；证书指纹可吊销（恢复令牌换绑） |
| **vault.json 被篡改/回滚（评审实现项）** | GCM 包裹自带篡改检测（解不开即锁定，DoS 而非静默泄密）；`seq` 单调递增字段做弱防回滚（见 §8.6）；回滚旧备份可复活旧凭证为**明示的残余风险**：运维红线 = 只保留最新备份 |
| **运行期明文窗口（D8/D9 既定边界）** | `vaultFiles=true` 实例运行期间工作目录为明文；崩溃残留明文在下次解锁时回收。威胁模型明示：静态加密承诺不覆盖运行窗口 |

### 3.2 信任边界

- **服务端（本节点）**：存储密文、校验认证、会话内持有主密钥。被攻破的解锁会话 = 数据可读（残余风险，明示）。
- **客户端（GUI）**：持有 P12 私钥与证书密码。P12 密码只在客户端本地使用，**永不上送**。
- **网络上送内容**：账号密码、TOTP 验证码、挑战签名、会话令牌 —— 全部经 TLS。

## 4. 总体架构

```
┌────────────┐   TLS    ┌──────────────────────────────┐
│  客户端 GUI  │◄────────►│  IriX Node（本服务端）          │
│ · P12 私钥   │          │  ┌────────────────────────┐  │
│ · TOTP 计算  │          │  │ vault.go  会话/路由/限速   │  │
│ · 挑战签名   │          │  │ vault_crypto.go 密码学    │  │
│ (forge/     │          │  │ vault_store.go 加密存储   │  │
│  openssl)   │          │  │ vault_migrate.go 迁移     │  │
└────────────┘          │  │ vault_tls.go  自签/加载   │  │
                         │  └──────────┬─────────────┘  │
                         │             │ 按需解密        │
                         │  ┌──────────▼─────────────┐  │
                         │  │ files.go / daemon.go    │  │
                         │  │ instances.json / 文件区  │  │
                         │  └────────────────────────┘  │
                         └──────────────────────────────┘
```

新增文件（全部标准库）：

| 文件 | 职责 |
| --- | --- |
| `vault.go` | 路由注册、会话管理（内存密钥 + 令牌 + 超时 + **RWMutex 生命周期锁**）、限速器、解锁/锁定/状态、用户管理 |
| `vault_crypto.go` | TOTP（RFC 6238）、PBKDF2、AES-256-GCM 信封（wrapBlob 编码）、挑战签发/验签、密钥树 |
| `vault_store.go` | 加密存储层：密文对象、分块、追加日志化、索引（DEK+脏落盘）、孤儿回收 |
| `vault_migrate.go` | 两阶段迁移：instances.json + 物化实例文件树，幂等游标、进度、磁盘余量检查 |
| `vault_tls.go` | 自签证书生成/加载、TLS 配置校验 |
| `disk_free_{windows,other}.go` | 磁盘余量探测（构建标签区分，平台不支持时告警降级） |
| `qa_vault_test.go` | 测试（见 §13） |

改造点：`config.go`（配置项）、`main.go`（TLS 启动、vault 初始化）、`files.go`/`daemon.go`（数据面接入加密层）、`audit.go`（掩码与安全事件）、`download.go`（直连通道 vault 适配）。

## 5. 密钥体系（信封加密）

### 5.1 密钥树

```
masterKey（256-bit，随机，数据域主密钥，仅内存）
   │  包裹（wrapBlob，每个受保护对象一把独立 DEK）
   ├──► DEK_file_i   每密文对象（文件）一把
   ├──► indexDEK     加密索引专用（D12）
   │
   │  包裹副本（持久化于 vault.json，wrapBlob）
   ├──► per-user：KEK = PBKDF2-SHA256(账号密码, per-user salt)
   └──► recovery：RecoveryKey = 恢复令牌（256-bit 随机，一次性打印）
```

- **DEK 层**：每文件/对象独立数据密钥。某把 DEK 泄露只影响单个文件；copy 生成新 DEK（D14）。
- **masterKey 层**：只包裹 DEK（含索引 DEK），不直接加密数据。换密码、换证书、新增用户 = 只 rewrap，**无需重加密任何数据**。
- **索引加密**：用独立 `indexDEK`（masterKey 包裹，存 vault.json）——与「masterKey 只包裹 DEK」的表述统一（评审一致性项）。
- **恢复令牌**：256-bit 随机；磁盘只存 SHA-256 哈希（复用 `auth.go` 配对码模式）。恢复包裹副本供在线恢复流程使用（§8.10）；离线解包工具不在本期（二期点名，见 §12）。

### 5.2 wrapBlob 编码（协议定死，客户端联调基准）

所有「密钥包裹」统一为二进制 `nonce(12B) ‖ ciphertext ‖ tag(16B)`：

| 对象 | 明文长度 | wrapBlob 长度 | JSON 表示 |
| --- | --- | --- | --- |
| masterKey / indexDEK / DEK | 32B | 60B | base64 标准编码（无换行） |
| 密文对象头部内的 DEK 包裹 | 32B | 60B | 二进制（非 JSON） |

### 5.3 密码学参数

| 项 | 取值 | 说明 |
| --- | --- | --- |
| 数据加密 | AES-256-GCM，随机 12B nonce | 认证加密，完整性+保密性一体（等保二级双覆盖） |
| 密钥派生 | PBKDF2-HMAC-SHA256（`crypto/pbkdf2`，Go 1.24 标准库） | 默认 600,000 次迭代，可配置；服务端解锁为一次性成本 |
| TOTP | RFC 6238，HMAC-SHA1，6 位，30s，窗口 ±1 | 标准实现，兼容主流验证器 APP |
| 证书 | RSA ≥2048 或 ECDSA P-256/P-384 | 签名：RSA 用 PKCS#1 v1.5 SHA-256，ECDSA 用 ASN.1 DER |
| 随机源 | `crypto/rand` | 与现有配对码机制一致 |

### 5.4 密码的正确性验证方式

**不存密码、不存密码哈希验证器**：密码派生 KEK 后解包裹 masterKey，GCM 标签校验失败即密码错误（隐式验证）。解锁校验顺序固定为 挑战 → TOTP → 密码 → 签名，任一失败返回统一响应 `认证失败`，审计中记录内部失败点。

**时序侧信道说明**（评审一致性项）：TOTP 错误微秒级返回、密码错误需跑完 600k 次 PBKDF2（约 0.5s）。因 TOTP 失败已限速（D16），在线利用需先通过 TOTP，实际可利用性低；文档明示此差异，不额外伪装耗时。

## 6. 账号与初始化流程

### 6.1 用户模型

```
User {
  name              // 唯一用户名
  totpSecret        // base32，20B 随机
  totpBound         // 是否已完成首次验证绑定
  certFingerprint   // 证书 SPKI SHA-256 指纹（hex，小写）——按公钥而非证书本体绑定
  certPublicPEM     // 登记的公钥证书（PEM）
  kekSalt           // 该用户 KEK 的随机盐
  masterKeyWrap     // masterKey 的 GCM 包裹（KEK 加密，wrapBlob，§5.2）
  passwordChangedAt // 支撑「定期更换」策略
  createdAt
}
```

- 证书绑定按 **SPKI 指纹**（公钥本身），换发证书（同钥）不破坏绑定，换钥才需重新绑定。
- 多用户共享同一数据域（masterKey），审计可区分到人。权限分离属三级要求，留二期。

### 6.2 用户管理（评审实现项补全）

| 操作 | 端点 | 边界规则 |
| --- | --- | --- |
| 新增用户 | `POST /api/vault/user/add`（需解锁会话） | 新用户复用现有 TOTP/密码/证书绑定流程（先建用户 → totp 验证 → 证书绑定） |
| 删除用户 | `POST /api/vault/user/remove`（需解锁会话） | **禁止删除最后一个用户**；删除即吊销其 KEK 包裹副本与证书指纹；删除当前会话用户 → 该会话立即失效并审计 |
| 列出用户 | `GET /api/vault/users`（需解锁会话） | 返回用户名/指纹/绑定状态，不含任何秘密材料 |

等保二级不要求权限分离，任何已解锁会话可管理用户；三员分立属三级，二期。

### 6.3 初始化流程（首次启用）

```
1. 管理员配置 vault.enabled=true 并配置 TLS（默认 off，由用户开启）→ 启动校验通过
2. POST /api/vault/init { user, password }
   · 校验：vault 未初始化；密码长度/复杂度策略（默认 ≥12 位，含大小写+数字）
   · 生成 masterKey、恢复令牌（一次性返回，仅此一次）、TOTP secret
   · 返回 { initToken, totpSecret, otpauthURI, recoveryToken }
   · initToken：32B 随机，10 分钟有效，内存态（S3 补全），再次 init 会作废旧令牌
3. 用户扫码绑定 TOTP（otpauth://totp/IriXNode:{user}?...）
4. POST /api/vault/totp/verify { code }（携带 X-Vault-Token: initToken）
   · 校验正确 → totpBound=true；失败累计 5 次 → 作废该 initToken（需重新 init）
5. 客户端从 P12 提取公钥证书（PEM）→ 获取「cert-bind」用途挑战 → 私钥签名
   POST /api/vault/cert { certPem, challengeId, signature }（携带 initToken）
6. 初始化完成。进入迁移（§8.7）。未完成初始化期间数据面一律 403（见 §8.8）
```

**未初始化窗口（评审协议项定案）**：`vault.enabled=true` 且未完成 init → 数据面（含 files/instance 等）一律 `403 vault not initialized`，避免「明文可用」部署窗口。仅有 `/api/vault/*`、认证类与 `/api/overview`（脱敏）可用。

### 6.4 密码策略

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `passwordMinLength` | 12 | 复杂度：含大写、小写、数字（可配） |
| `passwordExpireDays` | 90 | 到期后解锁响应携带 `passwordExpired: true` 警告 |
| `forceExpire` | false | 到期拒绝「纯解锁」；**解锁请求必须携带 `newPassword`，同请求完成解锁 + rewrap**（A3/D15 定案） |

`POST /api/vault/password { oldPassword, newPassword }`（需解锁会话）为常规改密；改密后**作废该用户其他会话令牌**。

## 7. 解锁与会话

### 7.1 解锁流程

```
客户端                             服务端
  │  POST /api/vault/challenge       │  生成 32B 随机挑战（一次性，5 分钟过期，
  │  { purpose: "unlock" }           │  带用途标记，内存上限 1024 + 定时清理）
  │◄─────────────────────────────────┤  返回 { challengeId, challenge(base64) }
  │  本地：P12 密码解开私钥           │
  │  签名消息（UTF-8 字节）=          │
  │    "IRIX-VAULT-UNLOCK:1:" + challenge
  │  计算 TOTP code                  │
  │  POST /api/vault/unlock          │
  │  { user, password, totp,         │
  │    challengeId, signature,       │
  │    newPassword? }                │
  │─────────────────────────────────►│  限速检查（用户+IP，D16）
  │                                  │  校验顺序（任一失败 → 统一 401）：
  │                                  │  1) 挑战存在/未用/未过期/用途匹配
  │                                  │     → 无论成败，首次使用即作废
  │                                  │  2) TOTP（±1 窗口、防重放、计数）
  │                                  │  3) 密码 → KEK → 解 masterKey 包裹
  │                                  │  4) 公钥验签（与登记 SPKI 指纹一致）
  │                                  │  通过：
  │                                  │  · 密码过期且 forceExpire → 校验 newPassword
  │                                  │    策略并同事务 rewrap（A3）
  │                                  │  · masterKey 驻内存（RWMutex 保护）
  │◄─────────────────────────────────┤  签发 sessionToken（32B 随机）
  │   { sessionToken, expiresIn,     │  审计：vault.unlock
  │     passwordExpired? }           │
```

要点：

- **挑战语义定案**：一次性 —— **首次使用即作废（无论成败）**，防同一签名对重放爆破；`purpose` 字段区分 `unlock` / `cert-bind` / `recovery`，不同用途签名前缀不同（S4）：
  - 解锁：`IRIX-VAULT-UNLOCK:1:`
  - 证书绑定：`IRIX-VAULT-CERT-BIND:1:`
  - 挑战池上限 1024、TTL 5 分钟、定时清理（复用 `download.go` ticketStore 的清理模式）。
- **签名编码定案**：挑战以 base64（标准、无填充）下发；签名消息 = 前缀 + 挑战字符串（UTF-8）；RSA 签名 = PKCS#1 v1.5 + SHA-256；ECDSA = ASN.1 DER；签名结果 base64（标准、无填充）放入 JSON。客户端联调以此为准。
- **两个密码的区分**：P12 证书密码只在客户端本地解开私钥用，永不上送；上送的是账号密码（经 TLS）。
- **newPassword**：可选；`forceExpire=true` 且密码过期时必填（D15）。

### 7.2 会话

- 会话 = 内存中的 `{ token → { user, certFingerprint, masterKey, lastActive, expiresAt } }`。
- **masterKey 生命周期锁（S7）**：`vault.mu`（`sync.RWMutex`）保护 masterKey 指针；数据面请求在 `RLock` 下校验令牌并完成解密；`lock()` 取 `Lock`，阻塞在途解密，确保「令牌校验 → 解密」与「销毁」之间无 use-after-zero。列入 `-race` 测试。
- 令牌仅经 **`X-Vault-Token` 头**传输（D17），禁止 query string。
- 空闲超时（默认 30 分钟，可配置）自动销毁；`POST /api/vault/lock` 立即销毁；节点重启 = 强制锁定。
- 令牌绑定用户 + 证书指纹；`bindSessionIP=true` 时额外绑定来源 IP（默认关）。
- **锁定与运行中实例（D9）**：锁定不杀运行中实例进程；但实例「启动/停止/重启」在锁定态一律 403（停止触发物化回收，必须解锁后执行）；auto-restart 与计划任务在锁定态禁用并审计。
- **会话活跃判定**：仅 API 请求刷新空闲计时器；实例进程活动不计入（30 分钟空闲锁定可能发生在实例运行期间 —— 已由「锁定不杀进程」语义覆盖，属设计行为）。
- **销毁语义如实表述（S8）**：Go 无法保证物理擦除（GC 可能复制、swap 可能换出）。实现做 best-effort：`[]byte` 显式归零 + `runtime.KeepAlive` + 引用隔离 + 锁定后立即置空。等保自查表按「部分符合（best-effort）」表述，避免测评被 challenge。

### 7.3 锁定状态的数据面行为

| API 类别 | 锁定行为 |
| --- | --- |
| `/api/vault/*`、`/api/overview`（脱敏版）、`/api/load`、认证相关 | 正常 |
| 实例 CRUD / 启停 / 文件区 / 日志查询 / 备份等 | `403 { status:false, data:"vault locked" }` |
| auto-restart、计划任务 | 禁用并审计（不执行） |
| `/download/` `/upload/` 直连通道 | 拒绝签发新票据；已签发票据读取时校验 vault 锁定状态 |

## 8. 加密存储层（按需解密）

### 8.1 文件区加密策略（A1/D8 定案）

**两级策略，按实例配置 `vaultFiles` 选择：**

| 模式 | 加密范围 | 运行进程访问 | 适用 |
| --- | --- | --- | --- |
| `vaultFiles=false`（默认） | 仅 instances.json + 系统元数据 | 工作目录明文，完全不受影响（现状行为） | 绝大多数实例：运行期数据量大、要求启停快 |
| `vaultFiles=true`（物化） | 实例文件树整体加密（对象+索引） | 启动前物化解密到工作目录；停止后回收加密 | 敏感实例：希望静态数据加密 |

设计理由：本地游戏进程必须从真实磁盘路径读取文件，纯标准库无法提供透明加密文件系统垫片；全员物化将导致每次启停全量拷贝、崩溃残留明文、磁盘翻倍 —— 故默认收窄范围，物化作为显式 opt-in。若产品要求默认全员加密，可配置 `vault.defaultFilesMode=materialize` 翻转默认值（实现不变）。

### 8.2 物化模式语义（D9 定案）

```
实例启动（需解锁）：
  1. 检查磁盘余量：可用空间 ≥ 文件树明文大小（不足则拒绝启动）
  2. 按索引把对象逐块解密到 {data}/instances/<uuid>/（物化）
  3. 启动进程（命令仍取自已加密的 instances.json，解锁后可读）
实例停止（需解锁）：
  1. 进程停止、日志 flush
  2. 扫描工作目录 → 逐文件加密为对象、更新索引 → 删除明文
     （可选 scrubOnDelete=true：删除前覆盖随机数据，best-effort）
  3. 置干净标记；物化/回收全程审计
崩溃/被强杀（未走停止流程）：
  1. 节点侧检测 `dirty` 标记（启动前物化时置位，正常回收后清除）
  2. 下次解锁后：对 dirty 实例执行回收（对象树比对索引，新增/变更文件入库）
  3. 回收完成前该实例禁止启动（避免明文/密文双写）
运行期明文窗口：物化实例在运行期间工作目录为明文 —— 威胁模型既定边界（§3.1），
  静态加密承诺不覆盖运行窗口；明文残留于介质无法物理清除（§8.9 如实表述）
```

### 8.3 数据目录布局

```
{data}/
  vault/
    objects/<64hex>      # 密文对象（文件名 = 随机 ID，无明文痕迹）
    index.json.enc       # 加密索引（indexDEK，D12，原子写）
    vault.json           # 元数据：用户、包裹副本、盐、证书、恢复哈希、迁移标记、seq
  instances/<uuid>/      # 实例明文工作目录（默认模式 / 物化模式的运行期窗口）
  tls/                   # 自签证书（0600）
  backup/audit/          # 审计日志归档
  instances.json         # vault 开启并迁移后不再使用（§8.7）
  auth.hash              # 配对码哈希（不加密，启动认证所需，非数据）
  logs/                  # 实例日志 + audit.log（不加密，见 D8 范围决策）
```

文件权限（M1 加固项）：`vault/`、`instances/`、`tls/` 目录 0700；`vault.json`、对象文件、证书私钥 0600。Windows 上 chmod 语义有限，以文档说明为准（POSIX 完整生效）。

### 8.4 密文对象格式（分块 GCM）

```
magic         "IRIXVT01"    8B
version       uint8         1B
blockSize     uint32        4B    # 默认 1 MiB
dekNonce      [12]byte            # 包裹 DEK 的 nonce
dekCipher     [48]byte            # DEK 包裹 + GCM tag（§5.2 编码）
blockCount    uint32        4B    # 头部可变字段（追加时原地更新）
lastBlockSize uint32        4B
body          (blockNonce[12] + blockCipher[blockSize+16]) × N
```

- 每块独立随机 nonce；非末块定长，末块 ≤ blockSize。
- **按需读取**：由文件偏移定位块号 → 计算块偏移 → 只解密目标块。Tail/截取日志、大文件随机读友好。
- **追加写（D11 崩溃语义）**：① 新块 append 到 EOF + fsync；② 原地更新头部 `blockCount/lastBlockSize` + fsync。崩溃于①后 → 头部计数未变，孤儿尾被下一次打开时截断（**不丢数据**）；崩溃于②前/中 → 计数可能已更新但块完整（fsync 保证序），无损坏。不接受「整对象 tmp+rename」方案（大文件不可承受）。
- **copy/move（D14）**：copy = 新对象 + 新 DEK + 新索引项；move = 仅索引项迁移（对象与 DEK 复用，无引用计数，无别名）。
- 删除 = 删索引项 + 删对象文件（best-effort 清除，见 §8.9）。

### 8.5 加密索引（D12）

```json
{ "version": 1, "seq": 0,
  "entries": { "/实例UUID/server.properties": { "id": "<64hex>", "size": 4096, "mtime": 1720000000, "blocks": 1 } } }
```

- 明文路径、大小、mtime 全部收进索引并整体加密（indexDEK）—— 未解锁时不泄露任何文件元数据。
- 解锁期间常驻内存，`sync.RWMutex` 保护。
- **落盘策略**：脏标记 + 延迟落盘 —— 写操作只改内存，标记脏；≤1s 定时 flush（原子写 tmp+rename）；lock/优雅关停时强制 flush。**取舍明示**：崩溃最多丢失最近 ≤1s 的索引变更（数据对象仍在盘上），由解锁时的**孤儿回收**兜底：扫描 objects 目录与索引比对，孤儿（无索引项）对象删除/隔离，审计记录。
- 目录列表 = 索引前缀匹配，不扫盘。

### 8.6 vault.json（元数据，含防篡改弱防护）

```json
{ "version": 1, "seq": 7,
  "users": [ { "name": "...", "totpSecretB64": "...", "totpBound": true,
               "certFingerprint": "...", "certPublicPEM": "...",
               "kekSaltB64": "...", "masterKeyWrapB64": "...",
               "passwordChangedAt": "...", "createdAt": "..." } ],
  "recovery": { "hash": "sha256hex", "masterKeyWrapB64": "..." },
  "indexDEKWrapB64": "...",
  "migration": { "instancesDone": true, "filesDone": { "<uuid>": true },
                 "cursor": { "uuid": "...", "path": "...", "done": 0, "total": 0 },
                 "startedAt": "...", "completedAt": null },
  "createdAt": "..." }
```

- 每次变更原子写，`seq` 单调递增（评审实现项：回滚旧备份会暴露旧 seq + 旧包裹，写操作校验 seq 不倒退并告警 —— **弱防护，明示**；强防护需外部单调存储，不做）。
- GCM 包裹自带篡改检测：元数据被改 → 解包失败 → 拒绝解锁（DoS 而非静默泄密，威胁模型已列）。

### 8.7 迁移设计（A2/D13 定案）

**阶段一：instances.json**（小、快）

```
1. 明文 instances.json → 加密对象 + 索引项（写盘 + fsync）
2. 写迁移标记 migration.instancesDone=true（原子写 vault.json）
3. 删除明文 instances.json
崩溃恢复：重启后检查标记 —— 有标记 → 直接删明文（幂等）；
无标记但对象存在 → 重新加密并覆盖对象（幂等，重复加密无害）
```

**阶段二：物化实例文件树**（仅 `vaultFiles=true` 实例，大、慢）

```
1. 预检：磁盘余量 ≥ 文件树总大小（disk_free 探测，平台不支持时仅告警 + 文档红线）
2. 后台 goroutine 逐文件：加密对象 → 索引项 → 删明文 → 每 N 个文件（默认 100）
   原子写一次索引 + 更新迁移游标（migration.cursor）
3. 幂等：游标之前的条目跳过；游标处按「索引项存在且 size/mtime 一致」跳过（重跑不重复加密）
4. 进度：POST /api/vault/migrate（会话，幂等续跑）+ GET /api/vault/migrate/status
   → { phase, done, total, bytes }（客户端可展示进度）
5. 迁移期间数据面文件区 API 返回 403 "vault migrating"（避免与文件遍历竞态）；
   迁移完成标记 migration.completedAt 后恢复
```

**迁移期间磁盘翻倍**为既定成本：阶段二逐文件「加密→删除明文」使峰值 ≈ 明文+单文件密文（非全量翻倍），但预检仍按总大小保守要求余量。

### 8.8 未初始化/锁定/迁移中的访问矩阵

| 状态 | 数据面 API | vault API |
| --- | --- | --- |
| 未初始化（vault.enabled 且未 init） | `403 vault not initialized` | init / status 可用 |
| 已初始化、锁定 | `403 vault locked` | challenge / unlock / status 可用 |
| 迁移中 | `403 vault migrating` | 会话类 + migrate/status 可用 |
| 已解锁 | 正常 | 全部可用 |

### 8.9 删除与剩余信息保护（如实表述）

- 明文文件 unlink 后数据仍可能残留于介质（SSD 磨损均衡、日志型文件系统），**无法保证物理清除**；可选 `scrubOnDelete`（删除/回收前覆盖随机数据，best-effort）。
- 等保自查表「剩余信息保护」按**部分符合（best-effort）**表述，附上述限制说明。

### 8.10 备份与恢复

- `POST /api/vault/backup`（需解锁）：导出加密备份包（zip：vault.json + 索引 + 全部对象，**不含任何密钥材料**）。恢复 = 包放回数据目录对应位置，用密码或恢复令牌解锁。
- 现有 `instance/backups` 在物化模式下备份密文对象，语义不变。
- **恢复令牌 = 最高权限凭证**：一次性打印、仅存哈希、物理保管。丢失后仍可用密码解锁；密码也丢失 = 数据永久不可恢复（特性而非缺陷，初始化时明示）。
- 恢复流程（在线）：`POST /api/vault/recovery { recoveryToken, newPassword?, newTotp?, newCert? }` → 校验令牌哈希（恒定时间比较）→ 建立 **recovery 会话（5 分钟）** → 期间可重设密码/重绑 TOTP/换绑证书（换绑证书需新钥对 `cert-bind` 挑战签名）。recovery 纳入统一限速（D16）。

## 9. TLS 设计（等保二级硬性项）

- `tls.mode`：`off`（**默认**，与现状兼容，不强制）| `auto`（自签）| `manual`（正式证书）—— 由用户显式开启。
- **`vault.enabled=true` 且 TLS 未开启 → 启动失败**（明确报错），杜绝「vault 明文跑」。
- **TLS 未开启时启动打印一行提示**（`TLS 未开启，等保二级部署请设置 tls.mode=auto 或 manual`），不阻断启动；文档列为部署红线（S6）。
- `auto`：首次启动生成自签证书（RSA-2048，10 年，SAN：localhost/127.0.0.1/::1/主机名）存 `{data}/tls/`；启动日志打印证书 SHA-256 指纹，客户端按指纹固定（TOFU）校验。
- `manual`：读取 `tls.cert` / `tls.key` 路径。
- 部署要求：**服务器时钟需 NTP 同步**（TOTP 依赖）。
- 兼容性：默认 off 与现状行为一致，无破坏性变更；开启 TLS 后既有客户端需支持 HTTPS + 自签证书指纹校验（TOFU）。

## 10. API 规范（草案，v2）

统一 `{status, data, time}`（`writeJSON`）；认证仍走 apikey/配对码；vault 会话与初始化会话走 `X-Vault-Token` 头（D17）。

| 方法/路径 | 权限 | 说明 |
| --- | --- | --- |
| `GET /api/vault/status` | apikey | `{ enabled, initialized, locked, user?, expiresIn?, passwordExpired?, migrating? }` |
| `POST /api/vault/init` | apikey | `{ user, password }` → `{ initToken, totpSecret, otpauthURI, recoveryToken }`（仅未初始化时） |
| `POST /api/vault/totp/verify` | initToken | `{ code }` → 确认 TOTP 绑定（5 次失败作废 initToken） |
| `POST /api/vault/challenge` | apikey | `{ purpose: "unlock"\|"cert-bind"\|"recovery" }` → `{ challengeId, challenge }`（签发即状态变更，POST） |
| `POST /api/vault/cert` | initToken / 解锁会话 / recovery 会话 | `{ certPem, challengeId, signature }` → 绑定证书（SPKI 指纹） |
| `POST /api/vault/unlock` | apikey | `{ user, password, totp, challengeId, signature, newPassword? }` → `{ sessionToken, expiresIn, passwordExpired? }` |
| `POST /api/vault/lock` | 会话 | 立即锁定（阻塞在途解密后销毁） |
| `POST /api/vault/password` | 会话 | `{ oldPassword, newPassword }` → rewrap + 作废该用户其他会话 |
| `POST /api/vault/recovery` | apikey | `{ recoveryToken, newPassword?, newTotp?, newCert? }` → recovery 会话（5 分钟） |
| `POST /api/vault/user/add` | 会话 | 新增用户（触发该用户的 totp/cert 绑定流程） |
| `POST /api/vault/user/remove` | 会话 | 删除用户（禁删最后一个；删当前会话用户 → 会话失效） |
| `GET /api/vault/users` | 会话 | 用户列表（无秘密材料） |
| `POST /api/vault/migrate` | 会话 | 启动/续跑迁移（幂等） |
| `GET /api/vault/migrate/status` | 会话 | `{ phase, done, total, bytes, completedAt? }` |
| `POST /api/vault/backup` | 会话 | 加密备份包下载 |

错误响应统一：认证类失败一律 `401 认证失败`（不区分 TOTP/密码/签名）；锁定/未初始化/迁移中分别 `403 vault locked / vault not initialized / vault migrating`。

## 11. 审计与安全事件

- **掩码扩展**（`audit.go` 现有 apikey 打码机制改为掩码列表）：`password`、`newPassword`、`totp`、`code`、`signature`、`recoveryToken`、`sessionToken`、`vaultToken`（头与 body 同掩）一律打码；`X-Vault-Token` 头请求也记路径但令牌打码。
- **统一限速（D16/S1/S2）**：unlock、recovery、init 的 totp/verify 全部计入同一限速器（用户+IP 双维度，内存态，定时清理）；`maxAttempts`（默认 5）次失败 → 锁定 `lockoutMinutes`（默认 15）。
- **新增事件**：`vault.init`、`vault.totp.verify`、`vault.cert.bind`、`vault.unlock`（成功/失败 + 内部失败点）、`vault.lock`、`vault.timeout`、`vault.password.change`、`vault.recovery`（成功/失败）、`vault.lockout`、`vault.user.add/remove`、`vault.migrate`（起止/进度节点）、`vault.orphan.reclaim`、`vault.backup`、`vault.scrub`、`vault.locked.op`（锁定态被拒请求）、`vault.autoRestart.blocked`。
- **审计记录保护**：轮转时把 `audit.log.1` 归档到 `{data}/backup/audit/<时间戳>.log`，防止轮转覆盖（等保二级「审计记录保护与定期备份」）。

## 12. 风险与二期（未决）项

| 项 | 说明 |
| --- | --- |
| 离线暴力破解密码 | 主设计下密码强度 = 离线安全上限（PBKDF2 600k+ 缓解）。二期增强：masterKey 额外用证书公钥包裹，解锁时客户端私钥解开后回传 |
| 会话内服务端被攻破 | 残余风险（架构 A 固有），空闲超时/锁定/审计缓解；彻底消除需架构 B |
| 运行期明文窗口（物化实例） | D9 既定边界；崩溃残留下次解锁回收；scrub 为 best-effort |
| vault.json 回滚攻击 | `seq` 弱防护 + 运维红线（只保留最新备份）；GCM 篡改检测保证「解不开」而非「静默泄密」 |
| 索引延迟落盘崩溃窗口 | ≤1s 索引变更丢失，孤儿回收兜底（§8.5）；可接受 |
| 日志不加密 | 范围决策（D8） |
| GPG 证书 | 本期只支持标准 PEM/X.509（P12 客户端转换）；GPG 走系统 `gpg` 验签或二期 |
| 离线恢复工具 | 本期恢复走在线 API；离线解包工具（脱离节点进程）二期点名 |
| 多证书/权限分离（三员分立） | 三级需求，二期 |
| apikey 明文 | `config.json` 中 apikey 不在加密范围（启动需读）；二期评估随 vault 移除或加密 |

## 13. 测试计划（qa_vault_test.go）

| 维度 | 用例 |
| --- | --- |
| TOTP | RFC 6238 附录 B 官方测试向量；±1 窗口边界；重放拒绝；错误计数与锁定 |
| 密钥树 | wrapBlob 编解码、错误密码 GCM 失败、rewrap（旧密码失效、数据不重加密）、恢复令牌解包 |
| 挑战签名 | 一次性（**首次使用即作废，含失败尝试**）、过期、用途不匹配拒绝、前缀防滥用、RSA/ECDSA 两类证书、base64 编码定案回归 |
| 解锁流程 | 各因素单独错误 → 统一 401；全正确 → 会话建立；**forceExpire + newPassword 同请求解锁改密**（A3）；锁定态数据面 403 |
| 限速（S1/S2） | unlock/recovery/init-verify 统一计数；锁定阈值；锁定解除；用户与 IP 双维度独立 |
| 生命周期锁（S7） | 并发解锁/锁定与数据面请求混跑，`go test -race` 无竞态 |
| 存储层 | 单块/多块往返、随机块读取定位、**追加崩溃语义（模拟孤儿尾 → 截断不丢数据）**、篡改检测、索引脏落盘 + **孤儿回收**、copy/move 语义、并发读写（`-race`） |
| 迁移（A2） | 阶段一崩溃恢复幂等（标记缺失/对象已存在两分支）；阶段二游标续跑、重复加密跳过、进度上报、迁移中 403 |
| 物化（A1/D9） | 启停物化往返、dirty 崩溃回收、锁定态停止被拒、auto-restart 锁定态禁用 |
| 集成 | 初始化全流程（initToken 过期/作废）、用户增删（禁删最后一个）、审计事件与掩码、备份包恢复、TLS 默认 auto 指纹、`tls.mode=off` 启动告警 |
| 性能 | 1 GiB 大文件随机读定位、10 万条目索引加载、索引脏落盘频率、迁移吞吐 |

## 14. 实施里程碑（含评审门禁）

| 里程碑 | 内容 | 门禁 |
| --- | --- | --- |
| M1 | TLS 与配置：`config.go` 扩展、`main.go` 启动改造、`vault_tls.go` 自签、文件权限加固、TLS 未开启提示与 vault 强制 TLS 校验 | 无（可先行） |
| M2 | 密码学内核：`vault_crypto.go`（TOTP/PBKDF2/GCM 信封 wrapBlob/签名验证）+ 单测 | 无（可先行） |
| M3 | 账号与解锁：`vault.go`（init/initToken/totp/cert/unlock/lock/password/recovery/users）、统一限速、生命周期锁、审计事件 | 评审 S1–S4、协议项已在本版收口 |
| M4 | 存储与数据面：`vault_store.go`（分块/追加/索引/孤儿）、`vault_migrate.go`（两阶段迁移）、物化启停（D9）、files/daemon/download 改造、备份 | 评审 A1–A3 已在本版收口（D8/D9/D13/D15） |
| M4 范围边界 | 已停止的 vaultFiles 实例：直连下载/上传通道与压缩/解压返回明确错误（需明文整树，建议先启动实例或用文件读写接口）；加密层 sha256 计算限 8 MiB 内文件 | 实现取舍，已写入威胁模型与 API 错误消息 |
| M5 | 审计归档、文档、等保二级自查表、全量 `go vet` + `go test -race` | M1–M4 |

预计新增约 2000–2600 行（含测试）。每个里程碑可独立交付与评审。

## 15. 等保二级符合性自查（技术面，v2）

| 控制项 | 落地 | 状态 |
| --- | --- | --- |
| 身份标识唯一、鉴别信息复杂度、定期更换 | 唯一用户名；密码 ≥12 位复杂度；90 天更换策略 + forceExpire 解锁改密（A3） | 符合 |
| 登录失败处理（限制非法登录次数） | **unlock/recovery/init-verify 全流程统一限速 + 临时锁定 + 会话超时**（D16） | 符合 |
| 远程管理传输保密（密码技术） | **TLS 由用户显式开启（默认 off）；vault 开启时强制 TLS（D7）；等保部署须开启 TLS（部署红线）** | 符合（部署前提） |
| 双因素（超额） | TOTP（密码技术）+ 密码 + 证书签名三重（§7） | 超额 |
| 安全审计（覆盖每用户、重要安全事件、记录保护与备份） | 用户维度事件 + 掩码 + 归档备份（§11） | 符合 |
| 数据完整性/保密性（传输+存储） | TLS + AES-256-GCM 密文存储（instances.json/元数据/物化实例文件树） | 符合（范围见 D8） |
| 数据备份恢复 | 加密备份包 + 恢复令牌 + 迁移幂等标记（§8.7/§8.10） | 符合 |
| 剩余信息保护 | 内存密钥归零 + 锁定销毁（best-effort，S8 如实表述）；明文删除无法物理清除（§8.9） | **部分符合（best-effort）** |

非技术项（管理制度、物理环境、人员、运维）由部署方配合测评机构完成，不在本设计范围。

---

## 附录 A：评审意见修订对照表

| 评审项 | 修订位置 | 结论 |
| --- | --- | --- |
| A1 运行中实例与文件区 | §2 D8/D9、§8.1/§8.2、§7.2 | 定案：默认明文 + 物化 opt-in 两级策略；物化语义（启停/崩溃/锁定/空闲）全量明确 |
| A2 文件区迁移缺失 | §2 D13、§8.7、§10 migrate API | 定案：两阶段 + 幂等游标 + 进度 API + 磁盘余量预检 + 迁移中 403 |
| A3 密码过期死锁 | §2 D15、§7.1、§6.4 | 定案：unlock 携带 newPassword 同请求解锁+rewrap |
| S1 失败计数只覆盖 TOTP | §2 D16、§11 | 全流程统一限速 |
| S2 recovery 无限速 | §11、§10 | recovery 纳入统一限速 + 审计 + recovery 会话 5 分钟 |
| S3 初始化会话凭证 | §2 D10、§6.3、§10 | initToken（32B/10min/内存态/5 次失败作废） |
| S4 签名前缀跨操作复用 | §7.1 | 分用途前缀 + challenge.purpose 字段 |
| S5 vaultToken 走 query | §2 D17、§7.2、§10 | 仅 Header，禁 query |
| S6 TLS 默认 off | §2 D7、§9 | 用户修订：默认 off 不强制（与现状兼容）；vault 开启时强制 TLS；等保部署红线由用户选择 |
| S7 锁定与在途请求竞态 | §7.2、§13 | masterKey RWMutex 生命周期锁 + -race 测试 |
| S8 销毁不可保证 | §7.2、§15 | best-effort 如实表述，自查表降为部分符合 |
| GET/POST 矛盾 | §7.1、§10 | challenge 统一 POST（签发即状态变更） |
| 签名编码未定 | §7.1 | base64 挑战 + 前缀消息 + RSA/ECDSA 编码定案 |
| wrap nonce 字段缺失 | §2 D5、§5.2、§6.1 | wrapBlob = nonce‖ct‖tag 统一编码 |
| copy/move 语义 | §2 D14、§8.4 | copy=新对象新 DEK；move=仅索引迁移 |
| 未初始化窗口 | §8.8 | 未初始化数据面 403 + 访问矩阵 |
| 挑战作废语义 + 内存上限 | §7.1 | 首试即作废；池上限 1024 + TTL + 定时清理 |
| 末块非原子追加 | §2 D11、§8.4 | 追加块 + 头部计数两段 fsync + 孤儿尾截断 |
| 锁定态后台任务 | §7.3 | auto-restart/计划任务禁用 + 审计 |
| vault.json 篡改/回滚 | §3.1、§8.6 | GCM 篡改检测 + seq 弱防回滚 + 运维红线 |
| 明文删除非清除 | §8.9、§15 | best-effort + scrubOnDelete 可选 + 自查表降级 |
| 多用户管理 API | §6.2、§10 | user add/remove/list + 禁删最后一个 |
| 索引全量重写写放大 | §2 D12、§8.5 | 脏标记延迟落盘 + 孤儿回收兜底 |
| NTP | §9 | 部署要求 |
| 文件权限 | §8.3、§14 M1 | 0700/0600 加固 |
| 恢复包裹离线表述 | §5.1、§8.10、§12 | 在线恢复流程为准；离线工具二期点名 |
| 索引加密与「masterKey 只包裹 DEK」矛盾 | §5.1、§8.5 | 索引独立 indexDEK |
| 401 时序侧信道 | §5.4 | 明示差异 + 可利用性分析 |
