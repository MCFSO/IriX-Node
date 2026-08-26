# Vault 保险库 / 集群 / FRP 隧道 HTTP API 契约

> 面向前端（React 面板）开发者。本文从 IriX-Node Go 源码精确提取，字段名与 Go struct 的 `json` tag **一字不差**。
>
> 源码基准：`IriX-Node/vault.go`、`vault_handlers.go`、`vault_crypto.go`、`vault_migrate.go`、`cluster.go`、`frp.go`、`instance.go`、`files.go`、`download.go`。

---

## 0. 通用约定（务必先读）

### 0.1 基础地址与认证

| 项 | 约定 |
|----|------|
| 基础地址 | `http://<host>:<port>`（TLS 开启时为 `https://…`） |
| API 认证 | `?apikey=<key>` 查询参数，或请求头 `X-Api-Key: <key>`。`-apikey` 未配置时校验配对码（同字段）。认证失败统一 `403 API 密钥无效` |
| 请求体 | `application/json; charset=utf-8`（除特别说明外） |
| 请求体上限 | 16 MiB（`/upload/` 直连通道、FRP 二进制上传除外） |

### 0.2 统一响应信封（MCSM 风格）

```json
{
  "status": 200,
  "data": <payload>,
  "time": 1740000000123
}
```

- 成功：`status == 200`，`data` 为各端点 payload（下文「响应 data」均指该字段）。
- 失败：`status` 为真实 HTTP 状态码，`data` 为**中文字符串错误消息**。
- **HTTP 状态码与 `body.status` 一致**（服务端用 `WriteHeader(status)` 透传，不是恒 200）。

### 0.3 错误响应格式

```json
{ "status": 401, "data": "认证失败", "time": 1740000000123 }
```

常见 HTTP 状态码：`400`（参数/格式错误）、`401`（认证失败/未授权/限速）、`403`（API 密钥无效 / vault 门禁）、`429`（挑战池已满）、`500`（内部错误）、`503`（票据已满）。

### 0.4 会话令牌机制（Vault 解锁后）

- 解锁/恢复成功后，令牌在**响应体 `data.sessionToken`** 中返回（不在 HTTP header，也不在 query string）。
- 客户端后续请求必须把令牌放进请求头 **`X-Vault-Token: <sessionToken>`**。
- 令牌是 32 字节随机数的 base64（标准字母表、无填充），客户端按不透明字符串处理即可。
- 解锁会话默认空闲超时 30 分钟（每次数据面请求自动滑动续期）；恢复会话固定 5 分钟、不滑动、且不能访问数据面。

### 0.5 vaultGate 数据面门禁（极其重要）

处理链：`auditMiddleware → vaultGate → limitAPIBody → auth → 具体 handler`。

vaultGate 拦截**除 `/api/vault/*`、`/api/overview`、`/api/load` 以外的一切路径**（包括实例/文件/集群/FRP/直连下载上传）。当 `vault.enabled=true` 时：

| 保险库状态 | 数据面返回 |
|-----------|-----------|
| 未初始化 | `403 vault not initialized` |
| 解锁后加载中 | `403 vault loading` |
| 迁移中 | `403 vault migrating` |
| 已锁定 / 无有效解锁会话 / 仅恢复会话 | `403 vault locked` |
| 已解锁且有有效解锁会话 | 放行，并滑动会话空闲超时 |

**结论**：一旦启用 Vault 并完成初始化，前端调用任何非 Vault API（集群、FRP、文件等）都必须同时携带 `apikey` 与 `X-Vault-Token`（解锁会话）。且 vaultGate 先于 `auth` 执行，因此在「已启用但未解锁」状态下，数据面请求（即使不带 apikey）会先收到 `403 vault locked` 而不是 `403 API 密钥无效`。

---

## 1. 端点总表

| 方法 | 路径 | 认证 | 需要 X-Vault-Token | 说明 |
|------|------|------|-------------------|------|
| GET | `/api/vault/status` | apikey | 否（可选，携带则返回会话信息） | 保险库状态 |
| POST | `/api/vault/init` | apikey | 否 | 首次初始化 |
| POST | `/api/vault/totp/verify` | apikey | 是（initToken 或会话） | 确认 TOTP 绑定 |
| POST | `/api/vault/totp/reset` | apikey | 是（解锁/恢复会话） | 重绑 TOTP |
| POST | `/api/vault/challenge` | apikey | 否 | 签发一次性挑战 |
| POST | `/api/vault/cert` | apikey | 是（initToken 或会话） | 绑定证书 |
| POST | `/api/vault/unlock` | apikey | 否 | 三重认证解锁 |
| POST | `/api/vault/lock` | apikey | 是（解锁会话） | 立即锁定 |
| POST | `/api/vault/password` | apikey | 是（解锁/恢复会话） | 修改密码 |
| POST | `/api/vault/recovery` | apikey | 否 | 恢复令牌建立恢复会话 |
| POST | `/api/vault/user/add` | apikey | 是（解锁会话） | 新增用户 |
| POST | `/api/vault/user/remove` | apikey | 是（解锁会话） | 删除用户 |
| GET | `/api/vault/users` | apikey | 是（解锁会话） | 用户列表 |
| POST | `/api/vault/migrate` | apikey | 是（解锁会话） | 启动/续跑迁移 |
| GET | `/api/vault/migrate/status` | apikey | 是（解锁会话） | 迁移进度 |
| POST | `/api/vault/backup` | apikey | 是（解锁会话） | 导出加密备份包（zip 二进制） |
| GET | `/api/cluster/status` | apikey | 受 vaultGate 约束 | 集群状态 |
| POST | `/api/cluster/heartbeat` | apikey | 受 vaultGate 约束 | 上报心跳 |
| POST | `/api/cluster/events` | apikey | 受 vaultGate 约束 | 上报事件 |
| GET | `/api/cluster/peers` | apikey | 受 vaultGate 约束 | 对等节点列表 |
| POST | `/api/cluster/transfer` | apikey | 受 vaultGate 约束 | 发起节点间拉取 |
| GET | `/api/cluster/transfer` | apikey | 受 vaultGate 约束 | 查询拉取进度 |
| GET | `/api/cluster/files/list` | apikey | 受 vaultGate 约束 | 同步区目录列表 |
| POST | `/api/cluster/files/mkdir` | apikey | 受 vaultGate 约束 | 同步区建目录 |
| DELETE | `/api/cluster/files` | apikey | 受 vaultGate 约束 | 同步区删除 |
| POST | `/api/cluster/files/download` | apikey | 受 vaultGate 约束 | 同步区下载票据 |
| POST | `/api/cluster/files/upload` | apikey | 受 vaultGate 约束 | 同步区上传票据 |
| GET | `/api/cluster/sync/list` | apikey | 受 vaultGate 约束 | 同步区递归快照 |
| GET | `/api/instance/sync/list` | apikey | 受 vaultGate 约束 | 实例目录递归快照 |
| GET | `/api/frp/status` | apikey | 受 vaultGate 约束 | frpc 状态与隧道列表 |
| POST | `/api/frp/tunnels` | apikey | 受 vaultGate 约束 | 创建隧道 |
| POST | `/api/frp/tunnels/{id}/start` | apikey | 受 vaultGate 约束 | 启动隧道 |
| POST | `/api/frp/tunnels/{id}/stop` | apikey | 受 vaultGate 约束 | 停止隧道 |
| DELETE | `/api/frp/tunnels/{id}` | apikey | 受 vaultGate 约束 | 删除隧道 |
| GET | `/api/frp/tunnels/{id}/logs` | apikey | 受 vaultGate 约束 | 隧道日志 |
| POST | `/api/frp/binary` | apikey | 受 vaultGate 约束 | 上传 frpc 二进制 |

> 上表「受 vaultGate 约束」= Vault 未启用时按普通 API 处理；Vault 启用并初始化后，还需要有效解锁会话（`X-Vault-Token`），否则 `403 vault locked` 等。

---

## 2. Vault 端点

### 2.1 GET /api/vault/status

获取保险库状态。**四种状态下字段不同**。

- 查询参数：无
- 请求体：无
- 请求头：可选 `X-Vault-Token`（携带有效解锁会话时返回会话信息）

**响应 data（vault 未启用）**

```json
{ "enabled": false, "initialized": false, "locked": true }
```

**响应 data（已启用、未初始化）**

```json
{ "enabled": true, "initialized": false }
```

**响应 data（已启用、已初始化、锁定/无会话）**

```json
{ "enabled": true, "initialized": true, "locked": true }
```

**响应 data（已启用、已初始化、解锁且携带有效解锁会话）**

```json
{
  "enabled": true,
  "initialized": true,
  "locked": false,
  "user": "admin",
  "expiresIn": 1795,
  "passwordExpired": false
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `enabled` | bool | vault 是否启用 |
| `initialized` | bool | 是否已初始化 |
| `locked` | bool | 是否锁定（仅 `initialized=true` 时出现） |
| `user` | string | 当前会话用户名（仅有效解锁会话时出现） |
| `expiresIn` | int | 会话剩余秒数 |
| `passwordExpired` | bool | 密码是否已过期（仅在配置了密码有效期且已到期时出现，值为 `true`） |

---

### 2.2 POST /api/vault/init

首次初始化保险库（仅未初始化时可用）。生成 masterKey / 恢复令牌 / TOTP 密钥 / 初始化令牌。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user` | string | 是 | 首个用户名（服务端 trim） |
| `password` | string | 是 | 密码（策略：≥12 位且含大写、小写、数字） |

```json
{ "user": "admin", "password": "StrongPassw0rd123" }
```

- 响应 data：

```json
{
  "initToken": "u7x…（base64 无填充，32 字节）",
  "totpSecret": "JBSWY3DPEHPK3PXP",
  "otpauthURI": "otpauth://totp/IriXNode:admin?secret=JBSWY3DPEHPK3PXP&issuer=IriXNode&algorithm=SHA1&digits=6&period=30",
  "recoveryToken": "…（base64 无填充，32 字节）"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `initToken` | string | 初始化令牌，10 分钟有效；用于后续 `totp/verify` 与 `cert`（放 `X-Vault-Token` 头） |
| `totpSecret` | string | TOTP 密钥（base32 无填充） |
| `otpauthURI` | string | 扫码绑定 URI |
| `recoveryToken` | string | 恢复令牌，**仅此一次返回**（服务端只存哈希），丢失后只能删除 vault 目录重新初始化 |

- 错误：`403 加密保险库未启用`；`400 保险库已初始化`；`400 用户名不能为空`；`400` 密码策略错误消息；`500` 各种密钥生成失败。

---

### 2.3 POST /api/vault/totp/verify

确认 TOTP 绑定。授权方式：initToken（初始化/新增用户 onboarding）或解锁/恢复会话（重绑后确认）。

- 请求头：`X-Vault-Token` = initToken 或会话令牌
- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `code` | string | 是 | 6 位 TOTP 验证码 |

```json
{ "code": "123456" }
```

- 响应 data：

```json
{ "bound": true }
```

- 说明：initToken 路径累计失败 5 次即作废该令牌（需重新 init 或走恢复流程）。限速键 `verify:<IP>`，阈值 5 次 / 锁定 15 分钟。
- 错误：`401 验证码错误`；`401 未授权或初始化会话已过期`；`401 尝试次数过多，请稍后再试`；`401 验证失败次数过多，初始化会话已作废（可通过恢复令牌重新绑定）`。

---

### 2.4 POST /api/vault/totp/reset

重绑 TOTP：生成新 secret 并置 `totpBound=false`，客户端随后须调 `totp/verify` 确认。

- 请求头：`X-Vault-Token` = 解锁会话或恢复会话
- 请求体：无
- 响应 data：

```json
{
  "totpSecret": "JBSWY3DPEHPK3PXP",
  "otpauthURI": "otpauth://totp/IriXNode:admin?secret=…&issuer=IriXNode&algorithm=SHA1&digits=6&period=30"
}
```

- 错误：`401 未授权`。

---

### 2.5 POST /api/vault/challenge

签发一次性挑战（分用途，防跨协议签名重用）。仅要求 vault 已启用，不要求已初始化、也不需要会话。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `purpose` | string | 是 | `"unlock"`（解锁用）或 `"cert-bind"`（证书绑定用） |

```json
{ "purpose": "unlock" }
```

- 响应 data：

```json
{
  "challengeId": "…（base64 无填充，32 字节）",
  "challenge": "…（base64 无填充，32 字节随机挑战值）"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `challengeId` | string | 挑战 ID，随后传入 unlock/cert |
| `challenge` | string | 挑战值。**签名消息 = 前缀 + 该字符串（UTF-8 字节）** |

- 说明：TTL 5 分钟，**首次使用即作废（无论成败）**。池上限 1024。
- 错误：`400 purpose 须为 unlock 或 cert-bind`；`429 挑战池已满，请稍后再试`。

---

### 2.6 POST /api/vault/cert

绑定证书（按 SPKI 公钥指纹绑定，换发同钥证书不影响绑定）。

- 请求头：`X-Vault-Token` = initToken 或解锁会话或恢复会话
- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `certPem` | string | 是 | PEM：`CERTIFICATE` / `PUBLIC KEY` / `RSA PUBLIC KEY` |
| `challengeId` | string | 是 | 来自 `challenge(purpose=cert-bind)` |
| `signature` | string | 是 | 对消息 `"IRIX-VAULT-CERT-BIND:1:" + challenge` 的签名（base64 无填充） |

```json
{
  "certPem": "-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----",
  "challengeId": "…",
  "signature": "…"
}
```

- 响应 data：

```json
{ "fingerprint": "a1b2…（SPKI 公钥 SHA-256 十六进制，64 字符）" }
```

- 说明：initToken 路径绑定成功后**作废 initToken**（onboarding 完成）。签名算法：RSA PKCS#1 v1.5 + SHA-256，或 ECDSA ASN.1 DER。密钥强度：RSA ≥ 2048、ECDSA ≥ P-256。
- 错误：`400 挑战无效或已使用`；`400 PEM 解码失败/解析失败`；`400 证书密钥强度不足（RSA ≥2048 或 ECDSA ≥P-256）`；`401 签名验证失败`；`401 未授权`。

---

### 2.7 POST /api/vault/unlock（完整流程）

三重认证解锁：**挑战 → TOTP → 密码 → 证书签名**。

#### 完整流程

1. `POST /api/vault/challenge { "purpose": "unlock" }` → 得到 `challengeId`、`challenge`。
2. 客户端计算 6 位 TOTP（`/init` 或 `/totp/reset` 返回的 `totpSecret`，30 秒周期、HMAC-SHA1）。
3. 客户端用绑定的证书私钥对消息 `"IRIX-VAULT-UNLOCK:1:" + challenge` 签名（RSA PKCS#1 v1.5 SHA-256 或 ECDSA ASN.1 DER），签名结果 base64（无填充）。
4. `POST /api/vault/unlock` 提交下述 body → 得到 `sessionToken`。
5. 后续所有数据面请求携带 `X-Vault-Token: <sessionToken>`。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user` | string | 是 | 用户名 |
| `password` | string | 是 | 密码 |
| `totp` | string | 是 | 6 位 TOTP |
| `challengeId` | string | 是 | unlock 挑战 ID |
| `signature` | string | 是 | `"IRIX-VAULT-UNLOCK:1:" + challenge` 的签名（base64 无填充） |
| `newPassword` | string | 否 | 仅当 `forceExpire` 且密码已过期时必填，同请求完成解锁+改密 |

```json
{
  "user": "admin",
  "password": "StrongPassw0rd123",
  "totp": "123456",
  "challengeId": "…",
  "signature": "…"
}
```

- 响应 data：

```json
{
  "sessionToken": "…（base64 无填充）",
  "expiresIn": 1800,
  "passwordExpired": false
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `sessionToken` | string | 会话令牌（放 `X-Vault-Token` 头） |
| `expiresIn` | int | 空闲超时秒数（默认 1800 = 30 分钟） |
| `passwordExpired` | bool | 解锁时密码是否仍处于过期态（true 表示需尽快改密） |

- 错误：`401 认证失败`（统一口径，不区分 TOTP/密码/签名）；`401 尝试次数过多，请稍后再试`（限速键 `unlock:<user>:<IP>`，5 次失败锁定 15 分钟）；`400 保险库未初始化`；`401 密码已过期，请在解锁请求中携带 newPassword 设置新密码`；`400` 密码策略错误；`500 解锁后初始化失败: …`（此时会话与密钥已回滚）。

---

### 2.8 POST /api/vault/lock

立即锁定：清零 masterKey、清空全部解锁会话。

- 请求头：`X-Vault-Token` = 解锁会话
- 请求体：无
- 响应 data：

```json
{ "locked": true }
```

- 错误：`401 未授权`。

---

### 2.9 POST /api/vault/password

修改密码（rewrap masterKey，不重加密数据）。

- 请求头：`X-Vault-Token` = 解锁会话或恢复会话
- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `oldPassword` | string | 解锁会话必填；恢复会话忽略 | 旧密码（解锁会话须验证，防会话持有者无密码改密） |
| `newPassword` | string | 是 | 新密码（同密码策略） |

```json
{ "oldPassword": "OldPassw0rd123", "newPassword": "NewPassw0rd123" }
```

- 响应 data：

```json
{ "changed": true }
```

- 说明：改密后吊销该用户除当前会话外的全部解锁会话（恢复会话保留）。
- 错误：`401 未授权`；`401 旧密码错误`；`401 恢复会话无效`；`400` 密码策略错误。

---

### 2.10 POST /api/vault/recovery

用恢复令牌建立 5 分钟恢复会话（可改密 / 重绑 TOTP / 换绑证书，**不开放数据面**）。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `recoveryToken` | string | 是 | `/init` 一次性返回的恢复令牌 |
| `user` | string | 否 | 目标用户；缺省且仅一个用户时自动取该用户 |

```json
{ "recoveryToken": "…" }
```

- 响应 data：

```json
{
  "sessionToken": "…",
  "expiresIn": 300,
  "recovery": true
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `sessionToken` | string | 恢复会话令牌（放 `X-Vault-Token` 头） |
| `expiresIn` | int | 固定 300（5 分钟） |
| `recovery` | bool | 恒为 `true`（标记恢复会话） |

- 错误：`400 保险库未初始化`；`400 无法确定目标用户，请指定 user`；`400 恢复令牌格式无效`；`401 认证失败`；`401 尝试次数过多，请稍后再试`（限速键 `recovery:<IP>`）。

---

### 2.11 POST /api/vault/user/add

新增用户（需解锁会话），创建后进入 onboarding：返回 initToken，随后走 `totp/verify` + `cert` 完成绑定。

- 请求头：`X-Vault-Token` = 解锁会话
- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user` | string | 是 | 用户名（trim，不可与现有重名） |
| `password` | string | 是 | 密码（同密码策略） |

```json
{ "user": "alice", "password": "AlicePassw0rd123" }
```

- 响应 data：

```json
{
  "initToken": "…",
  "totpSecret": "JBSWY3DPEHPK3PXP",
  "otpauthURI": "otpauth://totp/IriXNode:alice?secret=…&issuer=IriXNode&algorithm=SHA1&digits=6&period=30"
}
```

- 错误：`401 未授权`；`400 用户名不能为空`；`400 用户名已存在`；`400` 密码策略错误。

---

### 2.12 POST /api/vault/user/remove

删除用户（需解锁会话）。禁止删除最后一个用户；删除当前会话用户 → 当前会话立即失效。

- 请求头：`X-Vault-Token` = 解锁会话
- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user` | string | 是 | 要删除的用户名 |

```json
{ "user": "alice" }
```

- 响应 data：

```json
{ "removed": true, "currentSessionInvalidated": false }
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `removed` | bool | 恒为 `true` |
| `currentSessionInvalidated` | bool | 是否吊销了当前会话（删的是自己时为 `true`） |

- 错误：`401 未授权`；`400 用户不存在`；`400 禁止删除最后一个用户`。

---

### 2.13 GET /api/vault/users

用户列表（需解锁会话；不含任何秘密材料）。

- 请求头：`X-Vault-Token` = 解锁会话
- 响应 data：

```json
{
  "users": [
    {
      "name": "admin",
      "totpBound": true,
      "certFingerprint": "a1b2…",
      "createdAt": "2026-08-13T10:00:00+08:00"
    }
  ]
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `users` | array | 用户数组（按名称排序） |
| `users[].name` | string | 用户名 |
| `users[].totpBound` | bool | 是否已绑定 TOTP |
| `users[].certFingerprint` | string | 证书 SPKI SHA-256 指纹（十六进制） |
| `users[].createdAt` | string | 创建时间（RFC3339） |

- 错误：`401 未授权`。

---

### 2.14 POST /api/vault/migrate

启动/续跑数据迁移（幂等，后台执行）。阶段一：`instances.json` 加密；阶段二：`vaultFiles=true` 实例文件树加密。

- 请求头：`X-Vault-Token` = 解锁会话
- 请求体：无
- 响应 data（已启动）：

```json
{ "started": true }
```

- 响应 data（迁移已全部完成，无需启动）：

```json
{ "started": false, "completed": true }
```

- 错误：`401 未授权`。

---

### 2.15 GET /api/vault/migrate/status

迁移进度。

- 请求头：`X-Vault-Token` = 解锁会话
- 响应 data：

```json
{
  "phase": 2,
  "done": 1280,
  "total": 5120,
  "bytes": 4294967296,
  "running": true,
  "completedAt": "2026-08-13T10:05:00+08:00"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `phase` | int | 0 未开始 / 1 阶段一 / 2 阶段二 / 3 已完成 |
| `done` | int | 已迁移文件数 |
| `total` | int | 总文件数 |
| `bytes` | int | 已迁移字节数 |
| `running` | bool | 是否正在后台运行 |
| `completedAt` | string | 完成时间（RFC3339，仅完成后出现） |

- 错误：`401 未授权`。

---

### 2.16 POST /api/vault/backup

导出加密备份包（zip：`vault.json` + 索引 + 全部密文对象，不含任何密钥材料）。**响应不是 JSON，而是 zip 二进制流**。

- 请求头：`X-Vault-Token` = 解锁会话
- 请求体：无
- 响应：`Content-Type: application/zip`，`Content-Disposition: attachment; filename=irix-vault-backup-<unix秒>.zip`。zip 内结构：`vault/vault.json`、`vault/index.json.enc`、`vault/objects/*`。
- 错误：`401 未授权`；`500 索引落盘失败: …` / `备份打包失败: …`。

---

## 3. 集群端点

> 所有集群端点都注册在 `registerClusterRoutes`（`cluster.go`），路径前缀为 `/api/cluster/` 与 `/api/instance/sync/list`。它们受 vaultGate 约束（见 0.5）。认证仍走 `apikey` / `X-Api-Key`。

### 3.1 GET /api/cluster/status

- 查询参数：无
- 响应 data：

```json
{
  "monitorNodeId": "n-monitor",
  "role": "monitor",
  "peers": [
    { "id": "n-1", "address": "http://192.168.1.6:12346", "available": true }
  ],
  "self": { "id": "n-2", "address": "http://192.168.1.5:12346" }
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `monitorNodeId` | string | 监控节点 id（可为空串） |
| `role` | string | 本节点角色（`monitor` / `worker`） |
| `peers` | array | 已登记对等节点 |
| `peers[].id` | string | 对等节点 id |
| `peers[].address` | string | 对等节点地址 |
| `peers[].available` | bool | 是否可用 |
| `self` | object | 本节点 |
| `self.id` | string | 本节点 UUID |
| `self.address` | string | 本节点地址（`http://` + publicAddr） |

---

### 3.2 POST /api/cluster/heartbeat

上报资源快照 + 运行实例 + 待处理事件。

- 请求体（`id`/`address` 存在时自动登记/更新对等节点）：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `resource` | object | 否 | 资源快照 |
| `instances` | array | 否 | 运行实例列表 |
| `events` | array | 否 | 待处理事件 |
| `id` | string | 否 | 本节点 id |
| `address` | string | 否 | 本节点地址 |

```json
{
  "resource": { "cpuUsage": 0.4, "memUsage": 0.62, "totalmem": 17179869184, "freemem": 6528475136 },
  "instances": [ { "uuid": "…", "status": 3 } ],
  "events": [ { "type": "crash", "instanceUuid": "…", "count": 2 } ],
  "id": "n-2",
  "address": "http://192.168.1.5:12346"
}
```

- 响应 data：`true`（服务端会额外写入 `time` 字段后存起来）
- 错误：`400 请求体格式错误: …`。

---

### 3.3 POST /api/cluster/events

上报事件（崩溃 / 资源不足 / 同步完成 / 迁移）。事件保留最近 100 条。

- 请求体：自由 JSON 对象，需含 `type` 字段：

| `type` | 含义 | 附加字段 |
|--------|------|----------|
| `crash` | 实例非人为崩溃 | `instanceUuid`, `count` |
| `resource_pressure` | 节点资源不足 | `memUsage`, `threshold` |
| `sync_done` | 实例数据已同步到本节点 | `instanceId`, `bytes`, `files` |
| `migrated` | 实例已迁移至本节点 | `instanceId`, `fromNodeId` |

```json
{ "type": "sync_done", "instanceId": "i-abcd", "bytes": 12345, "files": 42 }
```

- 响应 data：`true`（服务端补写 `time` 后追加到事件列表）
- 错误：`400 请求体格式错误: …`。

---

### 3.4 GET /api/cluster/peers

- 响应 data（数组）：

```json
[
  { "id": "n-1", "address": "http://192.168.1.6:12346", "available": true }
]
```

字段含义同 3.1 的 `peers[]`。

---

### 3.5 POST /api/cluster/transfer

指示本节点从对等节点**拉取**实例数据到本地同步区（节点间直传，协调器不代理字节）。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `instanceId` | string | 是 | 集群实例全局 id |
| `source` | object | 是 | 源节点 |
| `source.address` | string | 是 | 源节点地址（http/https，缺省 scheme 时自动补 `http://`） |
| `source.apikey` | string | 否 | 源节点 apikey（拼到远端查询参数） |
| `source.uuid` | string | 是 | 源节点上实例 uuid |
| `source.daemonId` | string | 否 | 源守护进程 id |
| `dest` | string | 否 | 本地同步区目标目录（如 `/mirrors/i-abcd`，空则同步区根） |

```json
{
  "instanceId": "i-abcd",
  "source": { "address": "http://192.168.1.6:12346", "apikey": "", "uuid": "…", "daemonId": "…" },
  "dest": "/mirrors/i-abcd"
}
```

- 响应 data：

```json
{ "jobId": "a1b2…（UUID v4）" }
```

- 后端执行流程（供前端理解进度）：申请远端 `/api/instance/snapshot` → 轮询 `/api/instance/snapshot-progress` → 申请 `/api/instance/backups/download` 票据 → 直连 `/download/{password}/{fileName}` 下载 zip → 解压到 `dest`。
- **SSRF 安全限制**：`source.address` 仅允许 http/https；拒绝环回、未指定、链路本地（含 169.254.x.x 云元数据）、组播、广播、本机自身，以及 RFC1918 内网地址（10/8、172.16/12、192.168/16、IPv6 fc00::/7）。内网直传需服务端 `-transfer-allow-cidr` 显式放行；重定向与远端返回的下载地址逐跳校验。
- 错误：`400 缺少 instanceId/source.address/source.uuid 参数`；`400` SSRF 拒绝消息（如 `禁止访问内网地址…`）；`400` `dest` 路径错误。

---

### 3.6 GET /api/cluster/transfer

轮询拉取任务进度。

- 查询参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| `jobId` | 是 | `POST /api/cluster/transfer` 返回的 jobId |

- 响应 data：

```json
{ "status": "running", "bytes": 0 }
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `status` | string | `running` / `done` / `failed` |
| `bytes` | int | 已下载归档字节数（完成时有效） |

- 错误：`400 任务不存在或已过期`。

---

### 3.7 GET /api/cluster/files/list

列出同步区目录。

- 查询参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| `path` | 否 | 同步区路径，支持 `/mirrors/...` 前缀或相对路径；空 = 同步区根 |
| `page` | 否 | 页码，默认 1（`<=0` 视为 1） |
| `page_size` | 否 | 每页条数，默认 100（`<=0` 视为 100） |

- 响应 data：

```json
{
  "items": [
    {
      "name": "level.dat",
      "size": 1024,
      "time": "Wed Aug 13 2026 09:00:00 GMT+0800 (中国标准时间)",
      "mtime": "2026-08-13 09:00:00",
      "mode": 420,
      "type": 1,
      "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
    }
  ],
  "total": 1,
  "absolutePath": "/mirrors/i-abcd/world"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `items` | array | 当前页条目（目录在前，按名称排序） |
| `items[].name` | string | 文件/目录名 |
| `items[].size` | int | 大小（字节；目录为 0） |
| `items[].time` | string | MCSM 风格修改时间字符串 |
| `items[].mtime` | string | 增量同步用修改时间（`2006-01-02 15:04:05`） |
| `items[].mode` | int | 权限位（`os.FileMode.Perm()`） |
| `items[].type` | int | `0` = 目录，`1` = 文件 |
| `items[].sha256` | string | 文件 SHA-256 十六进制摘要（目录为空串） |
| `total` | int | 总条目数 |
| `absolutePath` | string | 当前目录的 `/mirrors` 虚拟前缀绝对路径（可直接复用为下次 `path`） |

---

### 3.8 POST /api/cluster/files/mkdir

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 要创建的目录（`/mirrors/...` 或相对路径） |

```json
{ "path": "/mirrors/i-abcd/world" }
```

- 响应 data：`true`
- 错误：`400 请求体格式错误: …`；`400` 路径越界错误；`500 创建失败: …`。

---

### 3.9 DELETE /api/cluster/files

删除同步区文件/目录（注意 DELETE 带 body，部分 HTTP 客户端需显式支持）。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `targets` | array<string> | 是 | 要删除的路径列表 |

```json
{ "targets": ["/mirrors/i-abcd/world/level.dat"] }
```

- 响应 data：`true`
- 错误：`400 请求体格式错误: …`；`400` 路径越界；`500 删除失败: …`。

---

### 3.10 POST /api/cluster/files/download

申请同步区下载票据（目录范围票据，可下载同步区内任意文件）。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 要下载的文件（校验存在且为文件） |

```json
{ "path": "/mirrors/i-abcd/world/level.dat" }
```

- 响应 data：

```json
{ "password": "a1b2…（UUID v4）", "addr": "192.168.1.5:12346" }
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `password` | string | 下载票据密码 |
| `addr` | string | 直连下载地址（`host:port`，客户端自行拼 `http://` 或 `https://`） |

- 直连下载：`GET http://<addr>/download/<password>/<fileName>`。集群票据兼容 `/mirrors/...` 前缀路径（`/download/<password>/mirrors/<relativePath>`）。
- 错误：`400 文件不存在: <path>`；`503 下载票据已满，请稍后重试`。

---

### 3.11 POST /api/cluster/files/upload

申请同步区上传票据。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `upload_dir` | string | 否 | 上传目标目录（`/mirrors/...` 或相对路径）；空 = 同步区根 `/` |

```json
{ "upload_dir": "/mirrors/i-abcd/world" }
```

- 响应 data：

```json
{
  "password": "a1b2…",
  "addr": "192.168.1.5:12346",
  "upload_dir": "/mirrors/i-abcd/world"
}
```

- 直连上传：`POST http://<addr>/upload/<password>`，multipart 表单字段 `file`（只取文件名，丢弃路径）。
- 错误：`503 上传票据已满，请稍后重试`；`400` 路径错误。

---

### 3.12 GET /api/cluster/sync/list

递归枚举同步区目录（单次返回整树扁平清单，含 sha256/mtime）。

- 查询参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| `path` | 否 | 同步区路径；空 = 同步区根 |

- 响应 data：

```json
{
  "items": [
    { "path": "/world/level.dat", "size": 1024, "mtime": "2026-08-13 09:00:00", "sha256": "9f86…", "type": 1 },
    { "path": "/world/region", "size": 0, "mtime": "2026-08-13 09:00:00", "sha256": "", "type": 0 }
  ],
  "total": 2,
  "root": "/"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `items[].path` | string | 相对根目录的 `/` 前缀路径 |
| `items[].size` | int | 大小（目录 0） |
| `items[].mtime` | string | 修改时间字符串 |
| `items[].sha256` | string | 文件摘要（目录空串） |
| `items[].type` | int | `0` 目录 / `1` 文件 |
| `total` | int | 条目总数 |
| `root` | string | 恒为 `/` |

---

### 3.13 GET /api/instance/sync/list

递归枚举**实例**工作目录（单次整树清单）。

- 查询参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| `uuid` | 是 | 实例 uuid（`daemonId` 参数可传但实现未使用） |

- 响应 data：结构与 3.12 完全一致（`{items,total,root}`）。
- 错误：`400 实例不存在: <uuid>`；`400 实例工作目录为空`；`500 枚举失败: …`。

---

## 4. FRP 隧道端点

> FRP 路由注册在 `instance.go` 的 `RegisterRoutes`。隧道在节点上运行（frpc 由节点管理），客户端只下发配置与查看状态。受 vaultGate 约束。

### 4.1 GET /api/frp/status

获取 frpc 二进制状态与隧道列表。

- 响应 data：

```json
{
  "binary": {
    "present": true,
    "path": "D:\\data\\frp\\frpc.exe",
    "version": "frpc version 0.52.3"
  },
  "tunnels": [
    {
      "id": "a1b2…",
      "name": "mc-tunnel",
      "provider": "openfrp",
      "status": "running",
      "config": { "node": "frp.example.com", "localPort": 25565 }
    }
  ]
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `binary.present` | bool | 是否找到 frpc 二进制 |
| `binary.path` | string | frpc 路径（present 时） |
| `binary.version` | string | `frpc -v` 首行（找不到或失败为空串） |
| `tunnels[].id` | string | 隧道 id（UUID v4） |
| `tunnels[].name` | string | 隧道名 |
| `tunnels[].provider` | string | `openfrp` / `sakura` / `self` |
| `tunnels[].status` | string | `running` / `stopped` / `failed` |
| `tunnels[].config` | object | 创建时下发的 config |

---

### 4.2 POST /api/frp/tunnels

创建隧道（写配置并**立即启动**）。

- 请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 是 | 隧道名（trim 后非空） |
| `provider` | string | 是 | `openfrp` / `sakura` / `self` |
| `config` | object | 是 | 按 provider 不同的配置（见下） |

**`config` 字段（provider 不同）**

`self`（自建 frps，`config.toml` 为完整 frpc 配置原样下发）：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `toml` | string | 是 | 完整 frpc 配置文本（TOML） |

`openfrp` / `sakura`（`sakura` 多一个可选 `user`，Sakura 访问密钥拆 user/token 时用）：

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `node` | string | 是 | — | frps 服务器地址 |
| `serverPort` | number | 否 | — | frps 端口（>0 时写入 `serverPort`） |
| `token` | string | 否 | — | 认证 token（写入 `auth.token`） |
| `user` | string | 否 | — | 认证用户名（写入 `auth.user`，sakura 用） |
| `name` | string | 否 | 隧道 `name` → `"tunnel"` | 代理名 |
| `localPort` | number | 是 | — | 本地服务端口 |
| `remotePort` | number | 否 | — | 远程端口（>0 时写入 `remotePort`） |
| `localIP` | string | 否 | `"127.0.0.1"` | 本地 IP |
| `type` | string | 否 | `"tcp"` | 协议（`tcp` / `udp`） |
| `customDomains` | string | 否 | — | 自定义域名（单域名，写入 `customDomains=[...]`） |

```json
{
  "name": "mc-tunnel",
  "provider": "openfrp",
  "config": {
    "node": "frp.example.com",
    "serverPort": 7000,
    "token": "xxx",
    "localPort": 25565,
    "remotePort": 25565,
    "localIP": "127.0.0.1",
    "type": "tcp"
  }
}
```

- 响应 data：

```json
{ "tunnelId": "a1b2…（UUID v4）" }
```

- 错误：`400 缺少 name 参数`；`400 不支持的 provider: …` / `缺少 node（frps 服务器地址）` / `缺少 localPort（本地服务端口）` / `self 类型需要 config.toml（完整 frpc 配置）`；`500 保存隧道失败: …`；`500 隧道已创建但启动失败: …`（此时隧道已持久化，状态为 stopped/failed，可通过 start 重试）。

---

### 4.3 POST /api/frp/tunnels/{id}/start

启动指定隧道（`{id}` 为路径参数，Go 1.22 `r.PathValue("id")`）。

- 响应 data：

```json
{ "tunnelId": "a1b2…" }
```

- 错误：`400 隧道不存在`；`400 隧道已在运行`；`500 未找到 frpc 二进制（请上传到节点或安装到 PATH）` 等启动错误。

---

### 4.4 POST /api/frp/tunnels/{id}/stop

停止指定隧道（直接终止进程）。

- 响应 data：

```json
{ "tunnelId": "a1b2…" }
```

- 错误：`400 隧道不存在`；`500 停止失败: …`。

---

### 4.5 DELETE /api/frp/tunnels/{id}

删除隧道（停止进程 + 删配置 + 从列表移除）。

- 响应 data：`true`
- 错误：`400 隧道不存在`。

---

### 4.6 GET /api/frp/tunnels/{id}/logs

隧道运行日志。

- 查询参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| `tail` | 否 | 返回末尾多少 **KB**，默认 100，范围 `[0, 2048]`（越界被钳制） |

- 响应 data：**纯字符串**（日志文本，非 JSON 对象）。隧道从未启动过时为 `""`。
- 错误：`400 隧道不存在`。

---

### 4.7 POST /api/frp/binary

上传 frpc 二进制（multipart，非 JSON）。

- 请求体：`multipart/form-data`，字段 `file` = frpc 二进制文件。内存阈值 64 MiB。
- 说明：落盘到 `{data}/frp/frpc`（Windows 为 `frpc.exe`）；非 Windows 加执行权限；覆盖已有文件。
- 响应 data：

```json
{
  "path": "D:\\data\\frp\\frpc.exe",
  "version": "frpc version 0.52.3"
}
```

| 字段 | 类型 | 含义 |
|------|------|------|
| `path` | string | 落盘路径 |
| `version` | string | `frpc -v` 首行 |

- 错误：`400 解析上传失败: …`；`400 缺少 file 字段: …`；`500` 写文件/就位失败。

---

## 附录 A：Vault 关键常量

| 项 | 值 |
|----|----|
| TOTP | 6 位数字，30 秒周期，HMAC-SHA1，校验允许 ±1 窗口 |
| 挑战 TTL | 5 分钟 |
| initToken TTL | 10 分钟 |
| 恢复会话 TTL | 5 分钟（不滑动） |
| 解锁会话空闲超时 | 默认 30 分钟（数据面请求滑动续期） |
| 认证限速 | 5 次失败锁定 15 分钟（unlock/recovery/totp-verify 分别按 `user+IP` / `IP` 维度） |
| TOTP 绑定失败上限 | 5 次作废 initToken |
| 挑战池上限 | 1024 |
| 密码策略 | ≥12 位，含大写、小写、数字 |
| 签名消息前缀 | unlock: `IRIX-VAULT-UNLOCK:1:`；cert-bind: `IRIX-VAULT-CERT-BIND:1:` |
| 签名格式 | RSA PKCS#1 v1.5 + SHA-256，或 ECDSA ASN.1 DER；base64（标准字母表、无填充） |
| 证书公钥强度 | RSA ≥ 2048 位，ECDSA ≥ P-256 |

## 附录 B：直连传输通道（票据）

| 用途 | 申请票据 | 直连调用 |
|------|----------|----------|
| 同步区下载 | `POST /api/cluster/files/download {path}` | `GET http://<addr>/download/<password>/<fileName>` |
| 同步区上传 | `POST /api/cluster/files/upload {upload_dir}` | `POST http://<addr>/upload/<password>`（multipart `file`） |

- 票据 10 分钟过期，全局上限 10000。
- 集群下载票据是目录范围票据（可下载同步区任意文件，兼容 `/mirrors/...` 前缀）；实例级下载票据绑定单文件。
