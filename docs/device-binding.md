# 客户端设备绑定协议（v1.4.0）

服务端设备绑定：客户端生成设备密钥（推荐放进 TPM 2.0 / 系统密钥库），登录时用
私钥对服务端 nonce 签名；登录后的会话绑定 `deviceId`，每个请求再用 HMAC-SHA256
证明持有会话密钥。账户可配置 `required`，此时**没有已绑定设备就无法登录**。

本文件是服务端实现（`device_auth.go`、`device_store.go`、`device_handlers.go`）的
字节级协议说明，供客户端团队直接对接。服务端只保存**设备公钥**与签名描述，
不保存设备私钥；会话级 `proofKey` 与 `token` 同级存放在会话表/缓存中，
泄露影响限于该会话，不能冒充设备。

---

## 1. 威胁模型与边界

能挡住：

- 只有用户名/密码的攻击者：没有设备私钥签不出登录挑战；
- 会话 token 被日志/内存泄漏：没有 `proofKey` 签不出逐请求证明；
- 网络中间人（在客户端固定校验服务端 TLS 指纹的前提下）：无法伪造挑战签名/证明；
- 重放：挑战一次性 + 请求序号窗口 + 时间戳窗口。

挡不住（明确非目标）：

- 客户端机器被完全控制（私钥可用）；
- 服务端数据库被写（可改公钥/策略）；
- 纯流量型 DDoS（连接/带宽洪峰，需上游清洗 + 节点全局预算）；
- 未固定 TLS 指纹时的主动中间人（可实时转发协议）；
- 请求 **body** 被中间人篡改（证明只覆盖 method/URI/时间戳/序号；body 完整性依赖 TLS）。

`required` 策略强制要求节点启用 TLS（`tls-mode=auto|manual`），客户端必须把
挑战响应里的 `tlsFingerprint` 与预期值做固定校验；否则拒绝登录。
设备登录与已绑定的设备会话同样要求 TLS（`optional` 只表示允许纯密码登录，
并不允许明文环境下的设备绑定）。

---

## 2. 密钥、标识与编码

| 项 | 定义 |
| --- | --- |
| 密钥类型 | `ed25519`（推荐桌面/TPM）或 `p256`（WebCrypto/ECDSA P-256） |
| 公钥编码 | `base64url` 无填充（RFC 4648 §5） |
| Ed25519 公钥 | 32 字节原始公钥 |
| P-256 公钥 | 65 字节未压缩点 `0x04||X||Y`（服务端也接受 33 字节压缩点） |
| 签名编码 | `base64url` 无填充；Ed25519 为 64 字节原始签名；ECDSA 推荐 64 字节 `r||s`（P1363），也接受 ASN.1 DER |
| `deviceId` | `hex(SHA-256(公钥原始字节))[:32]`，32 位小写 hex |
| `proofKey` | 客户端生成的 32 字节随机数，`base64url` 无填充 |
| 文本编码 | UTF-8，签名/哈希的输入不含 BOM、不做换行转换 |

`keyProtection` 为客户端自报的密钥载体（服务端 v1 只记录、不做证明）：
`tpm2` / `os-keychain` / `file`（小写字母、数字、`-`、`_`，≤32 字节）。

`descriptor` 是客户端签名的 JSON 字符串（≤4096 字节，必须是合法 JSON），建议字段：

```json
{"os":"windows","osVersion":"11","build":"26100","arch":"amd64",
 "appVersion":"1.4.0","installId":"<随机 hex>","hardwareClass":"desktop"}
```

服务端原样保存 `descriptor`，不做硬相等校验；它用于设备列表与后续风控。
签名哈希直接对请求里 `descriptor` 字符串的 UTF-8 字节计算，客户端不要重新序列化。

---

## 3. 挑战申请（公开）

```http
POST /api/auth/device/challenge
Content-Type: application/json

{"username":"admin","deviceId":"<32位hex>","purpose":"login"}
```

`purpose`：`login` 或 `register`（默认 `login`）。成功：

```json
{"status":200,"data":{
  "nonce":"<base64url 32字节>",
  "expiresAt":1790000000000,
  "purpose":"login",
  "tlsFingerprint":"<服务端证书 SHA-256；TLS 关闭时省略>"
},"time":...}
```

- 挑战 **60 秒**有效、一次性、绑定 `username + deviceId + purpose`；
- 挑战只存在**签发节点内存**：多节点共享账户库时请保持会话粘性，或失败后重新申请
  （不同节点的挑战不通用）；
- 服务端不因挑战申请泄漏账户是否存在。

---

## 4. 设备注册（需登录 + 账户密码）

先申请 `purpose=register` 的挑战，再：

```http
POST /api/auth/devices
X-Auth-Token: <当前会话 token>
Content-Type: application/json

{
  "deviceId":"<32位hex，必须等于由公钥计算的值>",
  "publicKey":"<base64url 公钥>",
  "keyType":"ed25519",
  "keyProtection":"tpm2",
  "descriptor":"{\"os\":\"windows\",...}",
  "name":"我的电脑",
  "nonce":"<挑战 nonce>",
  "timestamp":1790000000,
  "signature":"<base64url 签名>",
  "password":"<账户密码>"
}
```

密码用于防止“仅有被窃会话 token 的人偷偷添加设备”；密码校验复用登录限速。
单账户最多 **5** 台设备。设备记录主键为 `(username, deviceId)`，同一把设备密钥
可以绑定多个账户（推荐仍按账户生成独立密钥，便于吊销/隔离）。

### 注册签名规范串（逐字节）

```
irix-device-register-v1\n
<username>\n
<deviceId>\n
<publicKey 请求原文字符串>\n
<keyType 请求原文字符串>\n
<keyProtection 请求原文字符串>\n
<hex(SHA-256(descriptor UTF-8))>\n
<timestamp 十进制>\n
<nonce>\n
<tlsFingerprint>
```

最后一行是挑战响应里的 `tlsFingerprint`（TLS 关闭时为空字符串）；除上述 `\n`
分隔外**不要**追加结尾换行。`timestamp` 为 Unix 秒，允许 ±120 秒偏差。
空 `tlsFingerprint` 时规范串仍保留它前面的 `\n`（即规范串以换行结束、最后一段为空），
客户端不要“省略空字段”。

成功返回设备对象：

```json
{"status":200,"data":{
  "deviceId":"...","keyType":"ed25519","keyProtection":"tpm2",
  "descriptor":"{...}","name":"我的电脑","createdAt":...,"lastSeen":...
},"time":...}
```

### 设备列表 / 撤销

```http
GET  /api/auth/devices
POST /api/auth/devices/revoke   {"deviceId":"...","password":"..."}
```

- 撤销会立即注销该账户全部会话（含当前会话），客户端需重新登录；
- 策略为 `required` 时不能撤销最后一台设备（403）；管理员可删除并自动降级。

---

## 5. 登录（密码 + 设备签名）

先申请 `purpose=login` 的挑战，然后：

```http
POST /api/auth/login
Content-Type: application/json

{
  "username":"admin",
  "password":"<密码>",
  "device":{
    "deviceId":"<32位hex>",
    "nonce":"<挑战 nonce>",
    "timestamp":1790000000,
    "signature":"<base64url 签名>",
    "proofKey":"<base64url 32字节随机数>"
  }
}
```

### 登录签名规范串

```
irix-device-login-v1\n
<username>\n
<deviceId>\n
<nonce>\n
<timestamp 十进制>\n
<proofKey>\n
<tlsFingerprint>
```

成功响应在原有字段上增加：

```json
{"status":200,"data":{
  "token":"...","username":"admin","isAdmin":true,"expiresAt":...,
  "deviceBound":true,"deviceId":"..."
},"time":...}
```

策略行为：

| 账户策略 | 请求带 device | 行为 |
| --- | --- | --- |
| `optional`（默认） | 否 | 普通密码登录，会话不绑定设备（兼容旧客户端） |
| `optional` | 是 | 校验设备；成功则绑定会话，失败返回 403（不会静默降级） |
| `required` | 否 | 密码正确后返回 403：必须使用已绑定设备 |
| `required` | 是 | 校验设备；成功绑定，失败 403 |

设备校验顺序：时间戳 → `proofKey` 格式 → 消费挑战（一次性）→ 查设备 →
验签 → 更新 `lastSeen`。挑战即使验签失败也会被消费，客户端需重新申请。

---

## 6. 逐请求证明（已绑定会话）

`deviceBound=true` 的会话，**每个**请求（含 `/api/accounts/me`、登出、WebSocket
握手）都必须携带以下请求头：

| 请求头 | 值 |
| --- | --- |
| `X-Device-Id` | 登录响应里的 `deviceId`，必须与会话绑定值一致 |
| `X-Session-Timestamp` | Unix 秒，±120 秒 |
| `X-Session-Seq` | 十进制 uint64，从 1 开始，每个请求递增 |
| `X-Session-Proof` | `base64url(HMAC-SHA256(proofKey, canonical))` |

### 证明规范串

```
irix-proof-v1\n
<HTTP 方法大写>\n
<requestURI>\n
<X-Session-Timestamp 原文字符串>\n
<X-Session-Seq 原文字符串>
```

- `requestURI` = `path` + `?` + 原始查询串（与客户端实际发送的字节一致，含
  percent-encoding；无查询串时只有 path；不含 fragment）；
- 服务端从 `r.URL.RequestURI()` 取同一字符串；
- 时间戳/序号使用请求头里的**原文字符串**参与 HMAC（例如前导零会影响签名）。

### 序号窗口与重放

服务端为每个会话维护 1024 位滑动窗口：

- 请求可以在窗口内**乱序**到达（并发请求可以先发后到）；
- 同一序号重复出现 → 403（重放）；
- 序号落后超过 1024 → 403；
- 客户端**重试必须使用新序号**；
- 进程重启后服务端窗口清零，但时间戳 ±120 秒仍限制旧请求重放窗口；
- 客户端应把 `token + proofKey + 下一个 seq` 一起安全持久化；如果丢失 seq，
  直接用设备重新登录换取新会话（不要从 1 重试旧 token）。

### 失败响应

- 缺少/错误证明：`403 设备凭据校验失败（缺少或无效的 X-Device-Id / X-Session-* 请求头）`
- 登录设备校验失败：`403 设备绑定校验失败: <原因>`

---

## 7. 账户策略

- 新账户默认 `optional`。
- `required` 需要同时满足：
  1. 节点已启用 TLS（`tls-mode=auto|manual`，`tlsFingerprint` 非空）；
  2. 账户至少绑定 1 台设备。
- 设备登录（带 `device` 字段）以及 `deviceBound=true` 的后续请求都要求节点 TLS；
  节点 TLS 关闭时设备登录直接 403（不会退化为明文设备登录）。
- 切换策略会立即撤销该账户全部会话，强制重新登录。
- 管理员删除账户 `required` 状态下的最后一台设备时，自动降级为 `optional`
  并注销会话，避免永久锁死；普通用户不能自行撤销最后一台设备。

```http
PUT /api/accounts/device-policy      (管理员)
{"username":"alice","policy":"required"}
```

`GET /api/accounts/me` 会返回 `devicePolicy`、`deviceCount`、`deviceBound`；
`GET /api/accounts` 列表包含 `devicePolicy`。

### 恢复（break-glass）

- 普通用户：用另一台已绑定设备登录后撤销旧设备；`required` 下不能撤销最后一台。
- 管理员：`DELETE /api/accounts/devices` 删除最后一台设备会自动把策略降为
  `optional` 并注销会话，用户可重新用密码登录、绑定新设备、再开启 `required`。
- **唯一管理员丢失设备/策略被锁死**：停止节点，直接改账户库：
  `UPDATE accounts SET device_policy='optional' WHERE username='<管理员>';`
  （必要时 `DELETE FROM account_devices WHERE username='<管理员>';`），重启后
  用密码登录并重新绑定。账户库文件等同 root 凭据，请限制文件权限并做好备份。
- 密码重置不会绕过设备绑定；恢复必须走上述显式管理员/DB 操作。

---

## 8. 管理员接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/accounts/devices?username=` | 查看指定账户设备 |
| `DELETE` | `/api/accounts/devices?username=&deviceId=` | 删除指定设备（必要时自动降级策略） |
| `PUT` | `/api/accounts/device-policy` | 设置 `optional/required` |

均要求管理员身份；普通账户的自身设备接口见第 4 节。

---

## 9. TLS 与中间人

- `POST /api/auth/device/challenge` 返回 `tlsFingerprint`（服务端证书 SHA-256）。
  客户端必须与配对时记录的指纹比对，不一致立即中止（不要签名、不要发密码）。
- `required` 策略在节点 TLS 关闭时拒绝开启；`optional` 允许明文，但此时只保证
  “密码 + 设备签名”，不保证 MITM 防护。
- 设备签名/证明不覆盖 body；body 完整性依赖 TLS。大文件上传/下载走票据通道，
  由 TLS 保护。
- 反向代理终止 TLS 时节点看到的是明文，`tlsFingerprint` 为空：`required` 不可用。
  需要反代请让节点自己持有证书（或使用同机环回 + 节点 TLS）。

---

## 10. 客户端交接清单

1. **密钥生命周期**：每台设备/每次安装生成一对密钥；私钥优先写入 TPM 2.0
   （Windows CNG Platform Crypto Provider / Linux `/dev/tpmrm0`），否则系统密钥库，
   最后才回退到 0600 文件。私钥**不可导出**；`keyProtection` 如实上报。
2. **deviceId**：按第 2 节公式计算；不要随机生成后与公钥不匹配。
3. **descriptor**：稳定、最小化；`installId` 用随机数；避免收集敏感硬件序列号。
4. **挑战**：每次登录/注册前申请；60 秒内完成；失败重新申请（节点粘性）。
5. **TLS 指纹**：先校验 `tlsFingerprint`，再签名/发送密码。
6. **签名**：严格按规范串拼接（`\n`、字段顺序、原文时间戳、base64url 无填充）。
7. **登录**：保存 `token`、`proofKey`、`nextSeq=1`；`deviceBound` 为 true 时后续
   所有请求都要带 4 个证明头（WebSocket 握手同样带头；浏览器 WS 不支持）。
8. **并发**：用一个原子计数器分配 seq；服务端接受 1024 窗口内乱序。
9. **重试**：每次重试分配新 seq，不能复用已发送的证明。
10. **时钟**：与服务器同步（NTP）；偏差超过 ±120 秒会 403。
11. **URI**：签名与实际发送的 path+query 完全一致；不要让中间层改写/补斜杠。
12. **错误处理**：403 设备凭据失败不要降级为无证明请求；required 账户必须提示
    用户绑定设备/联系管理员。
13. **持久化**：`token/proofKey/seq` 放在安全存储；丢失 seq 就重新登录，不要复用旧 token。
14. **退出登录**：带证明调用 `POST /api/auth/logout`；本地同时清除 token/proofKey/seq。

---

## 11. 性能与容量

- 登录验签：Ed25519/P-256 每会话一次，可忽略；
- 逐请求证明：HMAC-SHA256 + 序号窗口，无 DB I/O、无非对称运算；
  本机基准（含客户端 HMAC + 构造请求）约 **3–5µs/op、33 allocs**（Windows amd64）；
- 挑战表上限 5 万条、60 秒过期；序号窗口表上限 10 万会话（约 15MB）；
- 已绑定会话的鉴权仍走进程内会话缓存，热路径无 SQL/Redis 查询。

---

## 12. 兼容性与限制

- 默认 `optional`，旧客户端无需改动；管理员可先对管理员账户开 `required`，
  再逐步推广到新账户。
- 浏览器面板：`required` 模式需要客户端能设置自定义请求头并持有设备私钥；
  浏览器 WebSocket 无法设置请求头，因此 `required` 仅支持桌面客户端。
  浏览器面板请使用 `optional` 账户。
- 集群/节点间调用：若使用已绑定设备的账户 token 作为 `source.token`，对端会因
  缺少逐请求证明返回 403。跨节点自动化请使用 `optional` 策略的普通服务账户，
  并只授予集群端点权限。
- `/download/`、`/upload/` 票据通道不复核会话证明：票据本身 10 分钟有效、
  受限实例 cwd；签发前的 API 调用已通过证明。
- 服务端 v1 只记录 `keyProtection`，**不验证** TPM/EK 证书/PCR；平台远程证明
  属于后续版本。

---

## 13. 附录：注册 / 登录 / 证明伪代码

```go
// 1. 设备密钥（私钥放 TPM/系统密钥库；这里仅示意）
pub, priv := ed25519.GenerateKey()
pubB64 := base64.RawURLEncoding.EncodeToString(pub)
sum := sha256.Sum256(pub)
deviceID := hex.EncodeToString(sum[:16])

// 2. 注册
ch := challenge(username, deviceID, "register") // POST /api/auth/device/challenge
descriptor := `{"os":"windows","osVersion":"11","build":"26100","arch":"amd64",
                "appVersion":"1.4.0","installId":"...","hardwareClass":"desktop"}`
dh := sha256.Sum256([]byte(descriptor))
ts := time.Now().Unix()
reg := "irix-device-register-v1\n" + username + "\n" + deviceID + "\n" +
       pubB64 + "\n" + "ed25519" + "\n" + "tpm2" + "\n" +
       hex.EncodeToString(dh[:]) + "\n" + strconv.FormatInt(ts, 10) + "\n" +
       ch.Nonce + "\n" + ch.TLSFingerprint
sig := ed25519.Sign(priv, []byte(reg))
// POST /api/auth/devices {deviceId, publicKey:pubB64, keyType:"ed25519",
//   keyProtection:"tpm2", descriptor, name, nonce:ch.Nonce, timestamp:ts,
//   signature:b64url(sig), password}

// 3. 登录
ch = challenge(username, deviceID, "login")
proofKey := random32()
ts = time.Now().Unix()
login := "irix-device-login-v1\n" + username + "\n" + deviceID + "\n" +
         ch.Nonce + "\n" + strconv.FormatInt(ts, 10) + "\n" +
         b64url(proofKey) + "\n" + ch.TLSFingerprint
sig = ed25519.Sign(priv, []byte(login))
// POST /api/auth/login {username,password,device:{deviceId,nonce:ch.Nonce,
//   timestamp:ts,signature:b64url(sig),proofKey:b64url(proofKey)}}
// -> {token, deviceBound:true, deviceId}

// 4. 逐请求证明（seq 用原子计数器；每次重试都要新 seq）
seq := atomic.AddUint64(&nextSeq, 1)
tsText := strconv.FormatInt(time.Now().Unix(), 10)
seqText := strconv.FormatUint(seq, 10)
canon := "irix-proof-v1\n" + method + "\n" + requestURI + "\n" + tsText + "\n" + seqText
mac := hmac.New(sha256.New, proofKey)
mac.Write([]byte(canon))
// Header: X-Device-Id, X-Session-Timestamp=tsText, X-Session-Seq=seqText,
//         X-Session-Proof=b64url(mac.Sum(nil))
```

