# 节点时间系统（Roughtime + 漂移补偿 + 时钟纪律）

> 适用范围：IriX Node v1.5.0+。实现：`internal/timesys/`、`cmd/irix-node/time_system.go`、
> `cmd/irix-node/time_handlers.go`。

## 1. 为什么需要自有时间系统

节点管理的是游戏服务器进程、账户会话、封禁、审计与 TLS，这些能力都以「当前时间」
为安全边界：

| 依赖时间的判定 | 系统时钟错误的后果 |
| --- | --- |
| 会话 30 天有效期、滑动续期 | 提前失效（全员掉线）或永不过期（凭据泄露无法收敛） |
| 封禁到期、日志轮转、备份文件名 | 封禁提前解除 / 日志覆盖 / 备份互相覆盖 |
| 设备绑定 ±120 秒时间戳、TOTP | 登录被拒（客户端与服务端时间不一致） |
| TLS 证书 NotBefore/NotAfter | 证书「尚未生效/已过期」，管理入口不可用 |
| 审计时间线、实例创建/结束时间 | 取证与合规审计失真 |

系统墙钟依赖 RTC 与 NTP：RTC 无电池、虚拟机暂停、NTP 被劫持/不可达都会让墙钟
偏离真实时间，而 NTP 本身没有签名，无法向应用证明「这个时间是对的」。因此节点
自带一套时间系统：

* **唯一外部真值**：Roughtime（RFC 10049），带 Ed25519 签名与 Merkle 包含证明，
  客户端可以验证「服务器在某个时间段内签署了这个时间」；
* **漂移补偿**：持续估计本机晶振相对真实时间的频率误差（ppm），校正时间推进速率；
* **时钟纪律**：小误差按 NTP 风格缓变（slew），大误差才步进（step）；
* **保持（holdover）**：断网时按漂移上界增长不确定度，并把锚点/频率落盘，重启后
  继续推算；
* **单一时间源**：业务、TLS、日志、HTTP Date 全部通过 `timesys.Now()` 取时间。

## 2. 架构

```
                    ┌─────────────────────────────┐
   config.json ────▶│ timesys.System（管理器）     │
   -time-* 参数     │  · 服务器轮转 / 并发查询     │
                    │  · 多源一致性判定（quorum）   │
                    │  · 状态持久化 {data}/timesys.json
                    └──────────┬──────────────────┘
                               │ Observe(mono, truth, radius)
                    ┌──────────▼──────────────────┐
                    │ timesys.Clock（受纪律时钟）   │
                    │  · 锚定单调时钟               │
                    │  · 速率校正（最小二乘 ppm）    │
                    │  · 相位伺服（slew / step）     │
                    │  · 不确定度 / 可信判定         │
                    └──────────┬──────────────────┘
                               │ timesys.Now()
     ┌───────────────┬─────────┼──────────────┬────────────────┐
     ▼               ▼         ▼              ▼                ▼
  会话/封禁     审计/日志   HTTP Date     TLS 证书签发/校验   实例/集群
```

包结构：

| 文件 | 职责 |
| --- | --- |
| `internal/timesys/roughtime.go` | Roughtime 线上格式（legacy + RFC 10049）、消息编解码、签名/Merkle 校验、UDP 客户端 |
| `internal/timesys/clock.go` | 受纪律时钟：漂移拟合、slew/step、不确定度、保持锚点、快照 |
| `internal/timesys/system.go` | 同步循环、服务器状态、quorum、退避重试、状态文件 |
| `internal/timesys/global.go` | 进程级唯一时间源（`Now`/`Since`/`Until`/`NowMillis`） |
| `internal/timesys/servers.go` | 内置公网服务器列表（长期 Ed25519 公钥） |
| `cmd/irix-node/time_system.go` | 启动编排、TLS 证书维护、日志/HTTP 时间接入 |
| `cmd/irix-node/time_handlers.go` | `GET /api/time`、`POST /api/time/sync` |

## 3. Roughtime 协议与校验

一次查询：

1. 客户端生成 64 字节（legacy）或 32 字节（RFC 10049）随机 nonce，请求至少 1024 字节；
2. 服务器把请求 nonce 放进 Merkle 树，用自己的临时密钥签署响应；
3. 客户端依次校验：
   * `CERT` 中 `DELE` 证书的 Ed25519 签名（用服务器**长期公钥**，即信任根）；
   * `MIDP ∈ [MINT, MAXT]`（临时密钥在有效期内）；
   * `INDX`/`PATH` 能把请求 nonce 重算到 `ROOT`（Merkle 包含证明）；
   * 顶层 `SIG` 用 `DELE` 中的临时公钥验证 `SREP`；
   * `NONC` 与本地 nonce 一致、`TYPE`/`VER` 合法、时间量级与半径在合理范围。

只有全部通过，才得到一个**带半径的时间区间** `(MIDP-RADI, MIDP+RADI)`。服务器
签名只能证明「时间在签名时刻附近」，无法证明「现在」；客户端用本地单调时钟记录
请求/响应时刻，把区间平移到本地时间轴：

```
monoMid = t_sent + RTT/2
unc     = RADI + RTT/2 + 1ms
```

### 3.1 两种线上格式

| 项目 | legacy（已部署公网服务器） | RFC 10049 |
| --- | --- | --- |
| nonce | 64 字节 | 32 字节 |
| 请求 | `NONC` + `PAD` | `VER`+`SRV`+`NONC`+`TYPE`+`ZZZZ` |
| Merkle 节点 | SHA-512 完整 64 字节 | SHA-512 截断 32 字节 |
| 叶子 | `H(0x00 ‖ nonce)` | `H(0x00 ‖ 请求包)` |
| MIDP/RADI | 微秒 | 秒 |
| DELE 签名上下文 | `RoughTime v1 delegation signature--\0` | `RoughTime v1 delegation signature\0` |

`time.servers[].protocol` 可选 `auto`（默认）/`legacy`/`rfc10049`。`auto` 先尝试
legacy，失败再试 RFC 10049，成功后按服务器缓存，后续轮次不重复付超时。
时间戳单位在解码时按数值量级自适应（秒/毫秒/微秒/纳秒），因此同时兼容
「结构为新版但沿用微秒」的混合部署。RADI 先按秒解读，超出 1 小时再按微秒解读
（宁可高估不确定度，偏保守）。

> 公网现状（2026 年快照）：Cloudflare / int08h / roughtime.se 等仍提供
> legacy 兼容服务（公网服务器可能随时下线或更换密钥，请以 `GET /api/time`
> 的成功率为准）；部分服务器（如 Netnod）只支持更早的草稿版本，对 RFC 10049
> 版本 1 会返回「草稿版本 + 不一致时间」的响应，客户端会因版本/签名/Merkle/
> 量级校验失败而拒绝，不会污染时间。默认列表因此优先选择 legacy 可用的服务器，
> 并对连续失败的服务器做冷却。

## 4. 时钟纪律

### 4.1 虚拟时钟模型

时钟锚定在**单调时钟**上（Windows QPC / Linux CLOCK_MONOTONIC / macOS
mach_absolute_time），不随系统墙钟被 NTP 步进或人工改时而跳变：

```
Now(t) = anchorTrue + (t - anchorMono) × (1 + ratePPM × 1e-6) + slewConsumed(t)
```

* `anchorTrue`：锚点处的真实时间；
* `ratePPM`：频率校正（正 = 本机偏慢，需要走快）；
* `slewConsumed`：正在按 `maxSlewPPM` 消化的相位校正量。

### 4.2 频率估计（漂移补偿）

对成功观测样本 `(单调时刻 t_i, 真值 s_i, 不确定度 σ_i)` 做加权最小二乘直线拟合
（x 为相对首样本的秒数，y 为相对首样本的真值秒数，权重 `1/σ_i²`）：

```
s ≈ a + b·t,      ratePPM = (b - 1) × 1e6
```

* 至少 3 个样本且跨度 ≥ `minRateSpanMinutes`（默认 30 分钟）才尝试更新；
* **统计显著性门槛**：拟合值必须偏离当前校正 3 个标准误以上才采纳，单次修正
  上限 50ppm、结果做 50% 平滑，并钳制在 ±`maxRatePPM`；
* 每次更新速率都重新锚定当前读数，**改变速率不会造成时间跳变**；
* 频率校正写入状态文件，重启后作为初值继续收敛。

权重与显著性门槛意味着：legacy 服务器（半径毫秒级）几十个样本即可把漂移估计到
亚 ppm 并快速收敛；RFC 10049 服务器（半径至少 3 秒）在半小时窗口内的拟合噪声可达
上千 ppm，门槛会拒绝更新——**宁可不做速率校正，也不把噪声当成漂移**。这时相位
伺服仍然正常工作；样本累积到数小时（128 个样本上限）后，长跨度拟合才可能通过
门槛。

### 4.3 相位伺服（slew / step）

新观测与当前虚拟时间的误差 `err`：

| 条件 | 动作 |
| --- | --- |
| 首次锁定 | 直接 step 到真值（此前系统时钟偏差未知） |
| `|err| ≤ stepThresholdMillis` 且 `|待校正量+err| ≤ maxSlewOffsetMillis` | re-anchor 当前值，剩余误差进入 slew 队列，按 `maxSlewPPM` 消化 |
| 其他（误差过大 / 累计待校正量过大） | step，并记录步进事件 |

默认 `maxSlewPPM=500`，即最快 **1.8 秒/小时**：常见 NTP/RTC 偏差（几十毫秒到几秒）
不会让进程内时间倒退；步进只发生在首次锁定或系统时钟严重错误时。
`Now()` 在两次 step 之间严格单调不减。

### 4.4 不确定度与可信判定

```
uncertainty = RADI + |剩余待校正量|
            + holdoverDriftPPM × (本次进程内距上次同步时长 + 重启前已过去时长)
```

* `quality=unlocked`：从未同步（直接用系统时钟）；
* `quality=holdover`：曾同步，但当前超过保持窗口或不确定度超限（含重启恢复）；
* `quality=synced`：在保持窗口内且不确定度 ≤ `maxUncertaintyMillis`；
* `trustworthy = 本次进程内在线同步成功 ∧ synced`。

重启后从 `timesys.json` 恢复：频率校正、上次同步时间、`真值-系统墙钟` 偏差、
不确定度。推算方式：

```
offset = savedOffset + ratePPM × 离线时长
Now    = 系统墙钟 + offset
```

恢复的锚点标记为 `holdover` 且 **trustworthy=false**（未经在线验证），直到下一次
Roughtime 同步成功并重新锁定。

### 4.5 多源一致性与虚假源剔除

每轮从服务器列表轮转挑 `samples` 台并发查询，每台给出一个带半径区间；
区间两两相交视为互相印证，取最大相交组：

* 组大小 ≥ `minAgreement`（默认 2；配置的服务器少于该值时自动放宽）才采纳；
* 组内按 `1/uncertainty` 加权合成真值，不确定度取组内最大偏差+半径；
* 首批响应不足以判定一致性时，自动追加查询一批尚未查询的服务器（最多一次），
  避免「本轮恰好轮转到不可用服务器」导致整轮失败；
* 连续 3 次失败的服务器进入 12 轮冷却（约 1 小时），不再占用每轮的超时预算；
  全部服务器都在冷却时退回全量轮转，保证仍会重试；
* 不达标时**不调整时钟**，保持当前时间，并记录 `lastError`；
* 失败按 1.5 倍指数退避（起始 5 秒，上限为 `intervalSeconds`），错误只在变化或
  第 1/5/20… 次时输出，避免离线环境刷屏。

域名会解析为全部 A/AAAA 地址并依次尝试（IPv4 优先、IPv6 兜底）：UDP 的
「连接成功」不代表可达，若只拨第一个地址，双栈网络里不可达的协议族会让整轮超时。

孤立的「±1 小时」错误响应不会被多数派采纳；但若多数服务器同时作恶，任何客户端
都无法识别（需要服务器列表与 malfeasance 报告机制，见 RFC 10049 §8）。

## 5. TLS 集成

TLS 不再是「另一个用系统时间的子系统」，而是时间系统的消费者：

1. **签发**：自签证书 `NotBefore = 节点时间 - max(1h, 不确定度, 未可信时+24h)`，
   `NotAfter = 节点时间 + 10 年`；证书日期用受纪律时间，不是系统墙钟。
2. **链校验**：服务端与出站 HTTPS（集群拉取、核心下载、JDK 安装）的
   `tls.Config.Time` 都指向 `timesys.Now()`，证书有效期用节点时间判定。
3. **握手前检查**（`getTLSCertificate`）：
   * 时间可信且证书在节点时间上过期/未生效：auto 模式立即重签并热替换
     （`GetCertificate`，无需重启）；manual 模式拒绝握手并返回明确原因；
   * 时间不可信：不据此拒绝（避免离线节点把管理入口锁死），交给客户端用自己的
     时钟校验；`time.tlsRequireTrusted=true` 时强制拒绝握手。
4. **巡检**：后台每 5 分钟检查一次证书，按节点时间失效时自动重签（auto）或告警
   （manual）；自签证书续签会改变 SHA-256 指纹，启动日志/审计日志会打印新指纹，
   使用指纹固定（TOFU）的客户端需要重新固定。
5. **证书指纹**：设备绑定挑战返回的 `tlsFingerprint` 读取线程安全的最新值。

## 6. 全应用接入清单

| 位置 | 说明 |
| --- | --- |
| `writeJSON` | 响应体 `time` 字段（Unix 毫秒） |
| `overview.go` | `system.time` 与 `timeQuality`/`timeTrustworthy`/`timeUncertainty` 健康度 |
| `clockDateMiddleware` | HTTP `Date` 响应头（最外层，含被拒绝的请求） |
| `setupClockLogging` | 标准库日志时间戳（含异步日志器 `alog`） |
| `accounts.go` / `accounts_handlers.go` | 会话落库/缓存/过期/滑动续期、Redis TTL、失败限速 |
| `device_auth.go` / `device_store.go` | 设备挑战 TTL、±120 秒时间戳、设备 last_seen |
| `defense.go` | 封禁到期、自动封禁时长、事件时间、手动封禁持久化 |
| `vault*.go` | 解锁会话空闲、初始化令牌、TOTP 窗口、密码有效期 |
| `audit.go` / `logger.go` | 审计时间列、日志归档文件名 |
| `daemon.go` / `instance*.go` | 实例创建/更新/结束时间、指标与统计时间戳 |
| `download.go` | 下载/上传票据过期 |
| `jobs.go` / `backup.go` / `trash.go` | 任务时间、快照文件名、回收站删除时间 |
| `cluster.go` | 集群响应 `time` 字段、轮询等待超时 |
| TLS（`internal/vaultcrypto`） | 证书签发、链校验、续签 |

**仍使用系统时钟的地方**（有意为之，见 §9）：网络读写 deadline、`time.Since`
耗时测量、进程 CPU 采样间隔、进程/文件系统等 OS 时间域（进程启动时间、
`fi.ModTime()`、uptime）、`crypto/tls` 内部会话票据轮换、`sysinfo` 的
uptime/CPU 采样。混用会带来跨时钟域比较错误，因此这些位置明确保留系统时间
并加注释。

## 7. API

### `GET /api/time`

权限组「时间系统」。返回：

```json
{
  "status": 200,
  "data": {
    "enabled": true,
    "quality": "synced",
    "trustworthy": true,
    "now": "2026-11-12T21:00:00.123456789+08:00",
    "unixMilli": 1794490800123,
    "lastSync": "2026-11-12T20:58:00Z",
    "syncAgeSeconds": 120.5,
    "offsetMillis": -12.4,
    "uncertaintyMillis": 23.1,
    "ratePpm": 12.7,
    "steps": 1,
    "slews": 3,
    "pendingSlewMillis": 0,
    "holdoverSeconds": 86400,
    "intervalSeconds": 300,
    "nextSyncInSeconds": 180,
    "lastRound": "2026-11-12T20:58:00Z",
    "lastRoundMillis": 42,
    "totalRounds": 17,
    "totalSuccess": 17,
    "consecutiveFailures": 0,
    "servers": [
      {
        "name": "cloudflare",
        "address": "roughtime.cloudflare.com:2003",
        "protocol": "legacy",
        "lastSuccess": "2026-11-12T20:57:59Z",
        "offsetMillis": -11.9,
        "radiusMillis": 1,
        "rttMillis": 24.2,
        "successes": 6,
        "failures": 0
      }
    ]
  },
  "time": 1794490800123
}
```

`offsetMillis` 为「服务器真值 - 系统墙钟」，正数表示系统墙钟偏慢。

### `POST /api/time/sync`

立即执行一轮同步（串行化，最长 15 秒）：

* 成功且时间可信 → 200 + 最新状态；
* 未启用 / 所有服务器不可达 / 未达到一致性门槛 → 500 或 503，`data` 为中文原因。

## 8. 配置

`config.json` 的 `time` 块（字段说明见 `cmd/irix-node/config.example.json`）：

```json
{
  "time": {
    "enabled": true,
    "servers": [],
    "intervalSeconds": 300,
    "timeoutMillis": 1500,
    "samples": 3,
    "minAgreement": 2,
    "maxSlewPPM": 500,
    "maxRatePPM": 500,
    "stepThresholdMillis": 2000,
    "maxSlewOffsetMillis": 10000,
    "minRateSpanMinutes": 30,
    "holdoverLimitHours": 24,
    "maxUncertaintyMillis": 5000,
    "holdoverDriftPPM": 100,
    "bootWaitMillis": 1500,
    "tlsRequireTrusted": false,
    "stateFile": ""
  }
}
```

`stateFile` 为空时使用 `{data}/timesys.json`；相对路径按数据目录解析。
字段含义见 `cmd/irix-node/config.example.json` 的 `time` 块注释。

命令行（优先级高于配置文件）：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-time-sync` | true | 启用/关闭时间系统（关闭后全部时间判定回退系统时钟） |
| `-time-servers` | 空 | `host:port@base64公钥` 逗号分隔；空则用配置/内置列表 |
| `-time-interval` | 300 | 同步间隔（秒，最小 5） |
| `-time-timeout-ms` | 1500 | 单服务器超时 |
| `-time-samples` | 3 | 每轮查询服务器数 |
| `-time-min-agreement` | 2 | 一致性门槛 |
| `-time-max-slew-ppm` | 500 | 相位校正最大速率 |
| `-time-step-ms` | 2000 | 步进阈值 |
| `-time-boot-wait-ms` | 1500 | 启动等待首轮同步上限（0 = 不等待） |
| `-time-tls-require-trusted` | false | 时间不可信时拒绝 TLS 握手 |

自建服务器示例：

```json
"servers": [
  {"name": "internal-rt", "address": "10.0.0.5:5319",
   "publicKey": "<服务器长期 Ed25519 公钥 base64>", "protocol": "rfc10049"}
]
```

## 9. 运维与故障排查

### 状态解读

| 质量 | 含义 | 业务影响 |
| --- | --- | --- |
| `synced` | 近期同步、不确定度在阈值内 | 全部能力正常，`trustworthy=true` |
| `holdover` | 曾同步但已超出窗口/不确定度 | 时间仍可用；安全判定按保守策略处理 |
| `unlocked` | 从未同步 | 等同系统时钟；TLS 证书日期按系统时钟 |

### 常见问题

* **启动日志「首轮 Roughtime 同步尚未完成」**：UDP 5319/2002/2003 出站被防火墙
  拦截，或服务器列表不可达。用 `POST /api/time/sync` 与 `GET /api/time` 查看
  `lastError`；离线部署可改为内网 Roughtime 服务器，或 `-time-sync=false`。
* **`offsetMillis` 很大（分钟/小时级）**：系统时钟严重错误。首次同步会 step；
  日志会打印「已锁定 Roughtime 时间（本地偏差 ±X）」。
* **`ratePpm` 长期接近 ±500**：晶振/虚拟化时钟漂移异常，或 `minRateSpanMinutes`
  内样本噪声过大；检查是否有频繁 step。
* **TLS 指纹变化**：自签证书按节点时间失效后自动重签，属于预期行为；客户端需
  重新固定新指纹（启动日志、`GET /api/time` 附近日志、设备挑战响应均可获得）。
* **时钟不可信时 TLS 握手失败**：检查是否设置了 `tlsRequireTrusted=true`，
  或 manual 证书确实过期。

### 避免自锁

默认 `tlsRequireTrusted=false`、且时间不可信时不强制证书有效期，因此断网节点
仍可用系统时钟提供管理入口。需要「时间不可信即拒绝 TLS」的高安全部署再显式开启，
并确保内网有可达的 Roughtime 源。

## 10. 安全边界

* 服务器长期公钥是唯一信任根；列表被篡改等于信任被替换。内置列表覆盖多个独立
  运营方，生产环境建议固定自建/可信列表并定期审阅。
* Roughtime 只保证「rough」时间：典型精度为服务器半径（legacy 约毫秒~秒，
  RFC 10049 至少 3 秒）。需要亚秒精度时应缩短同步间隔并降低 `maxUncertaintyMillis`，
  而不是假定协议更精确。
* 不修改操作系统时钟：本时间系统只服务本进程（含其 TLS/会话/审计）。若其他系统
  组件也依赖正确时间，请另行配置 NTP/NTS。
* 状态文件 `{data}/timesys.json` 只含时间锚点/频率，不含密钥，但仍应置于受保护
  的数据目录（默认 0600 写入）。
* 时间步进会让「同一进程内的墙钟」跳变；审计与业务时间戳可能因此出现一次不连续，
  日志中会记录步进事件与步进量。

## 11. 测试

* `internal/timesys/roughtime_test.go`：消息编解码、Merkle 路径（两种格式）、
  端到端 mock 服务器、签名/nonce/根/MIDP 篡改拒绝、RFC 10049 官方示例向量。
* `internal/timesys/clock_test.go`：首次 step、slew 连续性、阈值步进、100ppm
  漂移估计与 1 小时外推、保持不确定度增长、重启锚点、假时钟。
* `internal/timesys/system_test.go`：多源 quorum、虚假源剔除、无一致结果、
  单源、状态落盘与重启保持、未启用。
* `cmd/irix-node/qa_time_test.go`：`/api/time`、响应 `time` 字段、HTTP Date、
  会话过期、证书签发/自动续签/manual 过期拒绝、配置合并。
* 可选公网探测：`RT_PROBE=1 go test ./internal/timesys -run TestProbePublicServers -v`。
