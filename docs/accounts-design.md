# 账户管理设计（docs/accounts-design.md）

> 目标：IriX Node 采用多账户体系——首次启动在账户库中随机生成一个初始
> 管理员账户（凭据仅在启动日志显示一次）；管理员可创建/删除账户，并对
> **每个 API 端点**独立开关权限（按模块分组）；存储默认 SQLite、可选
> MySQL / PostgreSQL，Redis 可选缓存会话与权限热数据。全部使用连接池。
>
> v1.2.0 起配对码机制已彻底移除：不再有 `-apikey` 命令行参数、配置文件
> `apiKey` 字段、`{data}/auth.hash` 文件，`?apikey=` / `X-Api-Key` 认证通道
> 也不再被本节点接受（这些仅存在于 v1.1.0 及更早版本）。

---

## 1. 认证通道

| 通道 | 凭据 | 身份 | 端点权限 |
| --- | --- | --- | --- |
| 账户会话 token | `X-Auth-Token: <token>` 头 / `Authorization: Bearer <token>` 头 / `?token=<token>` 查询参数 | 账户（管理员或普通账户） | 管理员（`is_admin`）不受限制；普通账户按端点开关逐条判定 |
| 直连票据 | `/download/{票据密码}/...`、`/upload/{票据密码}` 路径中的票据密码 | 无 | 票据自身授权（10 分钟过期），维持不变 |

`?token=` 查询参数供浏览器 / WebSocket 等无法自定义请求头的场景使用（审计日志已对
`token=` 打码）。所有 `/api/*` 数据面请求都必须先登录取得会话 token。

会话 token：`crypto/rand` 32 字节（hex 64 字符），有效期 30 天，
剩余不足一半时滑动续期。

## 2. 初始管理员账户与首次启动引导

- **首次启动引导**：账户库（默认 `{data}/accounts.db`）中不存在任何管理员账户时，
  用 `crypto/rand` 随机生成 12 位用户名与 20 位密码（字符集 `[a-zA-Z0-9]`），
  bcrypt 入库，并在启动日志中**仅显示一次**；已存在管理员账户则跳过（共享账户库
  多节点并发启动时以「已有管理员即让位」避免重复创建）。
- **登录**：`POST /api/auth/login`，body `{"username":"...","password":"..."}`，
  成功返回 `{status,data:{token,username,isAdmin,expiresAt},time}`；密码错误
  401「用户名或密码错误」。退出：`POST /api/auth/logout`。
- **凭据丢失**：删除账户库后重启可重新生成初始管理员，或直接操作账户库恢复。
- **不存在**「root 首次登录强制改密」流程（该机制随配对码一并移除）；
  登录后建议立即在账户管理中修改密码。
- **管理员可以直接改任意密码**：`PUT /api/accounts/password` 携带
  `{username, password}`（管理员模式，无需旧密码），含管理员自身。

## 3. 权限模型：每个端点一个开关

- **权限目录**：注册路由时相邻调用 `perm(组名, 路由模式, 中文描述)`，
  路由模式与 `r.Pattern` 严格一致（如 `"GET /api/instance"`、
  `"POST /api/frp/tunnels/{id}/start"`）。目录按模块分组（概览/审计/账户/
  实例/运行时/文件/回收站/FRP/AI 与指标/集群/保险库/容器/Bastille 基础/
  Bastille 文件），供开关 UI 渲染。
- **判定**：`d.auth` 包装器在 mux 匹配之后执行——管理员（`is_admin` 账户会话）
  **旁路开关**；普通账户查 `accounts.permissions`
  （JSON，键 = 路由模式），**默认全关**，键存在且为 true 才放行，否则
  403 `权限不足: 当前账户无权访问 <模式>`。
- **账户自身端点豁免开关**（任何登录账户可用）：`/api/accounts/me`、
  `/api/accounts/catalog`、`PUT /api/accounts/password`（自己改密）、
  `POST /api/auth/logout`；管理端点（账户增删查、权限开关、管理员重置）
  在 handler 内再校验管理员身份，普通账户即使打开开关也 403。
- **开关接口**：`PUT /api/accounts/permissions`，两种模式：
  - 整组开关：`{username, group: "文件", enabled: true|false}`；
  - 逐条开关：`{username, permissions: {"GET /api/files/list": true, …}}`
    （只更新出现的键；未知键/未知分组 400）。
- **一致性保障**（`qa_perms_test.go`）：
  - 正向：目录中每个键在 ServeMux 中匹配出相同 `r.Pattern`；
  - 反向：扫描路由注册源文件，除直连通道与登录入口外，每个
    `mux.HandleFunc` 注册都必须出现在目录中（漏标注即测试失败）。

## 4. 存储：database/sql 连接池 + 三驱动

- 驱动：`sqlite`（默认，`{data}/accounts.db`，WAL + busy_timeout）、
  `mysql`（go-sql-driver）、`postgres`（pgx stdlib）。SQL 全部使用 `?`
  占位符，postgres 经 `rebindSQL` 改写为 `$n`；MySQL 的 UPSERT 用
  `ON DUPLICATE KEY UPDATE`，SQLite/PostgreSQL 用 `ON CONFLICT`。
- **连接池**（database/sql 自带，参数可配）：
  `maxOpen` 20 / `maxIdle` 10 / `connMaxLifetimeMin` 30 / 空闲回收 5 分钟。
- 表结构：
  - `accounts(username PK, password_hash, is_admin, permissions TEXT JSON, created_at, updated_at)`；
  - `sessions(token PK, username, created_at, expires_at, last_used)`
    + `idx_sessions_expires`（MySQL 不支持 `CREATE INDEX IF NOT EXISTS`，跳过，仅优化项）。
- 密码哈希：bcrypt（`golang.org/x/crypto`，默认 cost）。

## 5. Redis 热缓存（可选，故障自动降级）

- 启用：`accounts.redisAddr` 非空（连接池 `redisPoolSize` 默认 16；
  `MaxRetries=1` + 3s/2s 超时，Redis 抖动绝不拖慢鉴权）。
- 缓存内容：登录会话（`irix:acct:session:<token>`，TTL = 剩余有效期）
  与权限集合（`irix:acct:perm:<username>`，TTL 60 秒，权限变更即失效）。
- **SQL 始终是权威持久层**：写入先落 SQL 再写 Redis；读取先查 Redis，
  未命中回 SQL 并回填。Redis 任何错误 → 进入 30 秒降级冷却
  （期间一律走 SQL），到期自动重试恢复；Redis 不可用不阻断启动、不影响功能。

## 6. API

| 端点 | 认证 | 说明 |
| --- | --- | --- |
| `POST /api/auth/login` | 公开 | `{username, password}` → `{token, username, isAdmin, expiresAt}` |
| `POST /api/auth/logout` | 会话 | 删除当前会话 |
| `GET /api/accounts/me` | 会话 | 当前账户信息与自身权限 |
| `GET /api/accounts/catalog` | 会话 | 权限目录（分组 + 端点 + 描述） |
| `PUT /api/accounts/password` | 会话 | 自己改密 `{oldPassword, newPassword}`；管理员重置 `{username, password}` |
| `GET /api/accounts` | 管理员 | 账户列表（含首次启动生成的管理员账户） |
| `POST /api/accounts` | 管理员 | `{username, password, isAdmin}` 创建（重名 409） |
| `DELETE /api/accounts?username=` | 管理员 | 删除账户及其会话（不能删除当前登录的账户） |
| `PUT /api/accounts/permissions` | 管理员 | 整组开关 / 逐条开关（见 §3） |

登录请求体中的 `password` 字段由审计中间件统一打码（复用 vault 的
敏感字段红名单），不会明文落审计日志。

## 7. 配置（config.json `accounts` 块 / 命令行）

```jsonc
"accounts": {
  "driver": "sqlite",            // sqlite | mysql | postgres
  "dsn": "",                     // sqlite 文件路径（空 = {data}/accounts.db）
                                 // mysql:  user:pass@tcp(127.0.0.1:3306)/irix?charset=utf8mb4
                                 // postgres: postgres://user:pass@127.0.0.1:5432/irix?sslmode=disable
  "maxOpen": 20, "maxIdle": 10, "connMaxLifetimeMin": 30,
  "redisAddr": "",               // 空 = 不启用 Redis 缓存
  "redisPassword": "", "redisDB": 0, "redisPoolSize": 16
}
```

命令行：`-accounts-driver` / `-accounts-dsn` / `-redis-addr` / `-redis-password` / `-redis-db`
（优先级：命令行 > 配置文件 > 默认值）。

## 8. 安全边界与已知取舍

- 账户系统与保险库（vault）是**独立安全域**：`/api/auth/*` 豁免
  vaultGate 数据面门禁，保险库锁定期间仍可登录/登出账户。
- 改密/管理员重置**不吊销既有会话**（会话已认证，保留至过期）；
  删除账户会连带删除其全部会话。
- `PUT /api/accounts/permissions` 对管理员账户返回 400（管理员恒全权限）。
- 密码策略：账户密码 ≥ 8 位；用户名 1-64 位字母/数字/`_`/`-`。
- 遗留兼容：无配置时行为与旧版一致（默认 SQLite 账户库 + 首次启动引导管理员账户），
  但旧版的固定 `apikey` / 配对码认证通道已不可用。
