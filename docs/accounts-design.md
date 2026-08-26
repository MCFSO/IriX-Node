# 账户管理设计（docs/accounts-design.md）

> 目标：IriX Node 从「单一配对码」升级为多账户体系——
> 配对码登录即 root 管理员；管理员可创建/删除账户，并对**每个 API 端点**
> 独立开关权限（按模块分组）；存储默认 SQLite、可选 MySQL / PostgreSQL，
> Redis 可选缓存会话与权限热数据。全部使用连接池。

---

## 1. 认证通道（三通道并存）

| 通道 | 凭据 | 身份 | 端点权限 |
| --- | --- | --- | --- |
| apikey | `?apikey=` 参数 / `X-Api-Key` 头（配对码或 `-apikey` 固定密钥） | root（管理员） | 不受限制（兼容既有前端） |
| 会话 token | `Authorization: Bearer <token>` 或 `X-Auth-Token` 头 | 账户 | 按端点开关逐条判定 |
| 直连票据 | `/download/`、`/upload/` 路径中的票据密码 | 无 | 票据自身授权（10 分钟过期），维持不变 |

会话 token：`crypto/rand` 32 字节（hex 64 字符），有效期 24 小时，
剩余不足一半时滑动续期（每天最多写一次存储）。

## 2. root 管理员与首次登录强制改密

- **root 是内置管理员账户**，初始登录凭据 = 配对码（或 `-apikey` 固定密钥）。
- **首次登录强制改密**：`POST /api/auth/login` 用配对码登录成功时，
  响应 `mustChangePassword: true`；该会话除以下端点外一律 403
  （错误消息：`首次登录必须先修改密码（PUT /api/accounts/password）`）：
  - `PUT /api/accounts/password`（自己改密，携带 `oldPassword` = 配对码）
  - `GET /api/accounts/me` / `GET /api/accounts/catalog` / `POST /api/auth/logout`
- 改密后 root 的 bcrypt 哈希落入 accounts 表（root 行，`is_admin=1`）；
  **配对码不再用于登录**（`apikey` 通道为兼容既有客户端保持不变）。
- **管理员可以直接改任意密码**：`PUT /api/accounts/password` 携带
  `{username, password}`（管理员模式，无需旧密码），含 root。

## 3. 权限模型：每个端点一个开关

- **权限目录**：注册路由时相邻调用 `perm(组名, 路由模式, 中文描述)`，
  路由模式与 `r.Pattern` 严格一致（如 `"GET /api/instance"`、
  `"POST /api/frp/tunnels/{id}/start"`）。目录按模块分组（概览/审计/账户/
  实例/运行时/文件/回收站/FRP/AI 与指标/集群/保险库/容器/Bastille 基础/
  Bastille 文件），供开关 UI 渲染。
- **判定**：`d.auth` 包装器在 mux 匹配之后执行——管理员（apikey 通道、
  root 会话、`is_admin` 账户）**旁路开关**；普通账户查 `accounts.permissions`
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
| `POST /api/auth/login` | 公开 | `{username, password}` → `{token, username, isAdmin, mustChangePassword, expiresAt}` |
| `POST /api/auth/logout` | 会话 | 删除当前会话 |
| `GET /api/accounts/me` | 会话/apikey | 当前账户信息与自身权限 |
| `GET /api/accounts/catalog` | 会话/apikey | 权限目录（分组 + 端点 + 描述） |
| `PUT /api/accounts/password` | 会话 | 自己改密 `{oldPassword, newPassword}`；管理员重置 `{username, password}` |
| `GET /api/accounts` | 管理员 | 账户列表（root 内置条目在首位，含 `mustChangePassword`） |
| `POST /api/accounts` | 管理员 | `{username, password, isAdmin}` 创建（重名 409） |
| `DELETE /api/accounts?username=` | 管理员 | 删除账户及其会话（root 不可删） |
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
- `PUT /api/accounts/permissions` 对 root 返回 400（root 恒全权限）。
- 密码策略：账户密码 ≥ 8 位；用户名 1-64 位字母/数字/`_`/`-`。
- 遗留兼容：无配置时行为与旧版完全一致（apikey 通道 + 默认 SQLite）。
