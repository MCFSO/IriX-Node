# 用真实面板/daemon 做兼容层验收（E2E）

这里的目标只有一个：**用对方真实的程序**（不是我们自己的测试客户端）跑一遍，
确认兼容层在真正的面板/daemon 面前能用，而不是只在自造用例里自洽。

两套验证都用官方发行版二进制，不改对方代码：

| 验证 | 对方程序 | 我们的角色 | 脚本 |
| --- | --- | --- | --- |
| MCSManager | 面板 v10.22.1（`mcsmanager_windows_release.zip` 里的 `web/`，Node 启动） | 扮演 daemon，监听 `compat.mcsm.port`（默认 24444） | `scripts/e2e/mcsm-panel.mjs` |
| MSLX | Daemon v1.7.8（`MSLX-Daemon_v1.7.8_win-x64.zip`），作为**主节点** | 扮演**子节点**，监听 `compat.mslx.port`（默认 7891） | `scripts/e2e/mslx-master.mjs` |

## 怎么跑

```powershell
# 1) 起本节点（两层兼容层都开，固定端口与已知凭据）
go build -o D:\irix-e2e\node\irix-node.exe ./cmd/irix-node
#    config.json 里 compat.mcsm={enabled:true,port:24444,key:"<自定>"}
#    compat.mslx={enabled:true,port:7891,username:admin,password:"<自定>",linkKey:"<自定>"}
D:\irix-e2e\node\irix-node.exe -config D:\irix-e2e\node\config.json

# 2) 起 MCSManager 面板（官方发行版）
cd D:\irix-e2e\mcsm\mcsmanager\web && node app.js

# 3) 跑 MCSManager 验证（面板 API：注册节点 → 系统信息 → 建/启/停/删实例 → 文件 → 控制台）
cd D:\irix-e2e\mcsm
node <仓库>/scripts/e2e/mcsm-panel.mjs        # 需要 npm i socket.io-client（脚本只用到 fetch）

# 4) 起 MSLX 主节点（官方发行版），首次启动会把管理员账号打印到控制台
D:\irix-e2e\mslx\MSLX-Daemon.exe            # 默认 http://127.0.0.1:1027

# 5) 跑 MSLX 验证（主节点 link → 节点令牌 → 面板式直连 → 状态上报 → 解除链接）
cd D:\irix-e2e\mslx && npm i @microsoft/signalr
MSLX_ADMIN_PASS=<首启密码> node <仓库>/scripts/e2e/mslx-master.mjs
```

脚本里的端口/密钥是常量，按需改。MSLX 主节点首启会在
`DaemonData/默认账户信息.txt` 写出账号密码。

## 2026-11 实测结果

MCSManager 面板 v10.22.1（12 项全过）：

```
✅ 面板登录 / ✅ 节点注册 / ✅ 节点在线（socket.io 认证）
✅ info/overview 系统信息（CPU、内存、实例计数）
✅ 创建实例 / ✅ 实例列表 / ✅ 启动（status=3 运行中）
✅ file/list / ✅ 控制台输出（outputlog 有内容）
✅ 停止（异步回包，随后状态归零）/ ✅ 删除
```

MSLX Daemon v1.7.8 主节点（17 项全过）：

```
✅ 主节点登录 / ✅ 子节点协议 protocolVersion=1
✅ 主节点 link（主节点主动 POST 子节点 accept-link，子节点回源核验）
✅ 主节点签发节点令牌 / ✅ 面板式探活 /api/status（x-node-id + x-user-token）
✅ 面板数据通路 /api/instance/list
✅ 节点令牌下 createServer → 任务 state=2 → 启动 → status=2（MSLX 语义运行中）
✅ SignalR 协商接受节点令牌（面板控制台路径）
✅ 主节点 SignalR ReceiveSystemStats 里能看到本子节点的 cpu/mem（状态上报生效）
✅ 主节点解除链接 → 主节点异步回呼 remove-link → 子节点 linked=false
```

## 计划任务（MCSManager）

面板「计划任务」页走 `/api/protected_schedule`，面板后端转发 daemon 的
`schedule/register|list|delete`。实测（真实面板 v10.22.1）：

```
✅ 注册（Specific Time Task，type=3）      → 200 true
✅ 列表                                    → 字段齐全（instanceUuid/name/count/time/actions/type）
✅ 删除（task_name 查询参数）              → 200 true
✅ 到点真实触发（2 秒后 start 动作）       → 实例状态 0 → 3
✅ count=1 执行后自动删除任务              → 剩余任务数 0
```

脚本：`scripts/e2e/mcsm-schedule.mjs`（用面板 API，走完整转发链路）。

## 这一轮真机跑出来的 5 个缺陷（都已修 + 补回归用例）

| # | 现象 | 根因 | 修复 |
| --- | --- | --- | --- |
| 1 | 面板节点页「系统信息」空白，且每 6 秒重连一次 | 面板的请求包只有 `{uuid}`（`data` 为 undefined 被 JSON 丢掉），信封拆包要求必须有 `data` 键 → uuid 丢失 → 回包 uuid=null → 面板匹配不到超时 | 拆包判定改为「有 uuid **或** 有 data 就拆」（`compat_mcsm_engine.go`） |
| 2 | 面板完全收不到回包 | 把多个 engine.io 包合并进一个 WS 帧；`\x1e` 多包只有 HTTP 轮询允许，socket.io-client 在 WS 上按单包解析 | WS 出站改为**一帧一包**（`wsOutbox.noMerge`），仍保留异步写协程 |
| 3 | 认证前到达的只读事件被门禁拒绝 | 并发通道抢在 `auth` 之前执行 | 认证前到达的只读事件**暂存**，`auth` 成功后放行；3 秒内未认证则照常分发并返回明确拒绝 |
| 4 | 面板「停止」报 `Request timeout!` | 停止同步等进程退出（软关停等待可达数十秒） | 停止/重启改为后台执行、**立即回包** |
| 5 | 链接建立瞬间主节点回 401（日志噪音） | 主节点是在 accept-link 返回**之后**才落库，首轮上报必然 401 | 首轮上报短退避重试（300ms/700ms/1.5s/3s），成功即停 |

对应的回归用例在 `cmd/irix-node/qa_compat_mcsm_regress_test.go`。

## 排错

- WS 层有问题时设 `IRIX_DEBUG_WS=1` 启动节点，会打印入站包与心跳（只用于排查）。
- 面板侧日志：MCSManager 在 `web/logs/current.log`；MSLX 在主节点控制台输出。
- 兼容层自身的请求日志与其他 API 一样走审计日志（`{data}/logs/audit.log`）。
