// 账户管理核心（docs/accounts-design.md）：
//   - 认证三通道：apikey（配对码/固定密钥 = root 管理员）、账户会话 token、
//     直连票据（/download/ /upload/，维持不变）；
//   - 存储：database/sql 统一抽象，默认 SQLite，可选 MySQL / PostgreSQL，
//     连接池参数可配置（database/sql 自带连接池）；
//   - Redis（可选）：缓存登录会话与账户权限热数据（高频鉴权读取），
//     任何时刻不可用都自动回退数据库（SQL 始终是权威持久层），
//     降级后进入 30 秒冷却期，冷却结束自动重试恢复。
//
// 本文件只含数据面（存储/会话/缓存），HTTP 处理器见 accounts_handlers.go。

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // MySQL 驱动（database/sql）
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL 驱动（database/sql）
	"github.com/redis/go-redis/v9"     // Redis 客户端（自带连接池）
	"golang.org/x/crypto/bcrypt"
)

// 账户与会话常量。
const (
	accountNameMaxLen  = 64               // 用户名最大长度
	accountPassMinLen  = 8                // 密码最小长度
	accountSessionTTL  = 24 * time.Hour   // 会话有效期
	accountPermTTL     = time.Minute      // 权限热缓存 TTL
	accountRedisCoold  = 30 * time.Second // Redis 降级冷却期
	accountTokenBytes  = 32               // 会话 token 随机字节数（hex 后 64 字符）
	accountRoot        = "root"           // 内置管理员账户名（配对码登录）
	accountRedisPrefix = "irix:acct:"     // Redis 键前缀
)

// 进程内热缓存 TTL（鉴权高频路径：每个 API 请求都要查会话与权限）。
// 未配 Redis 的默认 SQLite 部署下，每次请求要打 2~3 次 SQL；进程内缓存
// 命中后降为零查询。TTL 不宜过长——写路径（登出/改权限/改密）会主动失效
// 对应键，TTL 只是兜底，防止节点间直改数据库导致本节点长期停在旧值。
const (
	accountSessCacheTTL = 10 * time.Second // 会话缓存（登出 delSession 立即失效）
	accountPermCacheTTL = 30 * time.Second // 端点权限缓存（改权限立即失效）
	accountRootCacheTTL = 10 * time.Second // root 是否已设独立密码
)

// account 账户记录（root 为内置虚拟账户，不存表）。
type account struct {
	Username    string          `json:"username"`
	IsAdmin     bool            `json:"isAdmin"`
	Permissions map[string]bool `json:"permissions"` // 端点开关：键 = 路由模式（如 "GET /api/instance"）
	CreatedAt   int64           `json:"createdAt"`   // Unix 毫秒
	UpdatedAt   int64           `json:"updatedAt"`
}

// accountSession 登录会话（Redis 缓存的 JSON 与 sessions 表共用结构）。
type accountSession struct {
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	ExpiresAt int64  `json:"expiresAt"` // Unix 毫秒
}

// accountsConfig 账户子系统配置（config.json accounts 块合并后的最终值）。
type accountsConfig struct {
	Driver             string // sqlite（默认）| mysql | postgres
	DSN                string // 连接串；sqlite 为文件路径（空 = {data}/accounts.db）
	MaxOpen            int    // SQL 连接池最大连接数（≤0 取 20）
	MaxIdle            int    // SQL 连接池最大空闲连接数（≤0 取 10）
	ConnMaxLifetimeMin int    // SQL 连接最大存活时间（分钟，≤0 取 30）
	RedisAddr          string // Redis 地址（空 = 不启用 Redis 缓存）
	RedisPassword      string // Redis 密码
	RedisDB            int    // Redis 库号
	RedisPoolSize      int    // Redis 连接池大小（≤0 取 16）
}

// accountSystem 账户子系统：SQL 权威存储 + 可选 Redis 热缓存 + 进程内热缓存。
//
// 三级读取顺序（以会话为例）：进程内缓存 → Redis → SQL。前两级都是加速层，
// 写路径（登出/续期/改权限/改密/删账户）会同步失效进程内缓存键，
// 保证「登出后立即不可访问」这类安全语义不被缓存 TTL 拖慢。
type accountSystem struct {
	db     *sql.DB
	driver string // sqlite | mysql | postgres
	redis  *redis.Client

	rdMu   sync.Mutex
	rdDown time.Time // Redis 降级冷却截止时间（零值 = 未降级）

	// 进程内热缓存（cacheMu 保护）。条目自带过期时刻，命中即无需任何 I/O。
	cacheMu sync.RWMutex
	// sessCache token → 会话缓存条目（登出/续期主动失效）
	sessCache map[string]sessCacheEntry
	// permCache username → 端点开关（改权限/删账户主动失效）
	permCache map[string]permCacheEntry
	// rootPwSet root 是否已设置独立密码；rootPwCached=false 表示尚未缓存
	rootPwSet    bool
	rootPwCached bool
	rootPwExpire time.Time

	// 登录失败限速（loginMu 保护）：key = 来源 IP + 用户名
	loginMu    sync.Mutex
	loginFails map[string]*loginFailState
}

// sessCacheEntry 会话缓存条目。
type sessCacheEntry struct {
	sess    accountSession
	expires time.Time // 缓存条目过期时刻（非会话过期时刻）
}

// permCacheEntry 权限缓存条目。
type permCacheEntry struct {
	perms   map[string]bool
	expires time.Time
}

// rebindSQL 将 ? 占位符改写为 PostgreSQL 的 $n（sqlite/mysql 原生 ?）。
// 本项目的 SQL 均为固定语句，不含字符串字面量中的 ?，可安全改写。
func rebindSQL(driver, q string) string {
	if driver != "postgres" {
		return q
	}
	var sb strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&sb, "$%d", n)
			continue
		}
		sb.WriteByte(q[i])
	}
	return sb.String()
}

// initAccounts 打开账户数据库并建表；Redis 配置非空时连接并启用热缓存。
// 任何一步失败都返回错误（调用方决定是否阻断启动）。
func (d *Daemon) initAccounts(cfg accountsConfig) error {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" {
		driver = "sqlite"
	}
	dsn := strings.TrimSpace(cfg.DSN)
	var err error
	switch driver {
	case "sqlite":
		// SQLite 驱动经 build tag 隔离（见 accounts_sqlite.go / accounts_nosqlite.go）：
		// 非 solaris/illumos 平台引入 modernc.org/sqlite；solaris/illumos 下
		// openSqlite 返回错误，强制改用 postgres/mysql（Go 的 SQLite 驱动未覆盖该平台）。
		dsn, err = openSqlite(d.DataDir, dsn)
		if err != nil {
			return err
		}
	case "mysql", "postgres":
		if dsn == "" {
			return fmt.Errorf("accounts.driver=%s 需要配置 accounts.dsn 连接串", driver)
		}
	default:
		return fmt.Errorf("accounts.driver 无效: %q（须为 sqlite / mysql / postgres）", cfg.Driver)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("打开账户数据库失败: %w", err)
	}
	// 连接池：database/sql 自带；参数可配置（默认 20 / 10 / 30 分钟）
	maxOpen, maxIdle := cfg.MaxOpen, cfg.MaxIdle
	if maxOpen <= 0 {
		maxOpen = 20
	}
	if maxIdle <= 0 {
		maxIdle = 10
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	lifetimeMin := cfg.ConnMaxLifetimeMin
	if lifetimeMin <= 0 {
		lifetimeMin = 30
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(time.Duration(lifetimeMin) * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &accountSystem{db: db, driver: driver}
	// 建表：逐条执行（MySQL 不允许多语句单 Exec）
	for _, st := range accountsSchemaTables {
		if _, err := db.Exec(rebindSQL(driver, st)); err != nil {
			db.Close()
			return fmt.Errorf("初始化账户数据表失败: %w", err)
		}
	}
	// 过期清理索引（MySQL 不支持 CREATE INDEX IF NOT EXISTS，跳过；仅为优化项）
	if driver != "mysql" {
		if _, err := db.Exec(rebindSQL(driver, accountsSchemaIndex)); err != nil {
			db.Close()
			return fmt.Errorf("初始化账户索引失败: %w", err)
		}
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return fmt.Errorf("账户数据库连接失败: %w", err)
	}

	if addr := strings.TrimSpace(cfg.RedisAddr); addr != "" {
		pool := cfg.RedisPoolSize
		if pool <= 0 {
			pool = 16
		}
		// MaxRetries=1 + 短超时：Redis 抖动时快速失败回退数据库，绝不拖慢鉴权
		s.redis = redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     cfg.RedisPassword,
			DB:           cfg.RedisDB,
			PoolSize:     pool,
			MaxRetries:   1,
			DialTimeout:  3 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		})
		// 连接失败不阻断启动：缓存不可用自动回退数据库
		if err := s.redis.Ping(context.Background()).Err(); err != nil {
			alog.Printf("警告: Redis %s 不可用（%v），账户会话/权限缓存回退数据库，稍后自动重试", addr, err)
			s.redisFailed()
		}
	}
	d.accounts = s
	return nil
}

// closeAccounts 关闭账户数据库与 Redis 连接（优雅关停时调用）。
func (d *Daemon) closeAccounts() {
	if d.accounts == nil {
		return
	}
	if d.accounts.redis != nil {
		_ = d.accounts.redis.Close()
	}
	_ = d.accounts.db.Close()
}

// accountsSchemaTables 建表语句（sqlite/mysql/postgres 通用子集）。
var accountsSchemaTables = []string{
	`CREATE TABLE IF NOT EXISTS accounts (
		username VARCHAR(64) PRIMARY KEY,
		password_hash VARCHAR(255) NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		permissions TEXT NOT NULL DEFAULT '{}',
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		token VARCHAR(64) PRIMARY KEY,
		username VARCHAR(64) NOT NULL,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		last_used BIGINT NOT NULL
	)`,
}

// accountsSchemaIndex 过期会话清理索引（MySQL 不支持 IF NOT EXISTS，单独执行）。
const accountsSchemaIndex = `CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions (expires_at)`

// ---------- 账户 CRUD（SQL 权威层） ----------

// createAccount 创建账户（密码 bcrypt 哈希后入库）。
func (s *accountSystem) createAccount(username, password string, isAdmin bool) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.Exec(rebindSQL(s.driver,
		`INSERT INTO accounts (username, password_hash, is_admin, permissions, created_at, updated_at)
		 VALUES (?, ?, ?, '{}', ?, ?)`),
		username, string(hash), boolInt(isAdmin), now, now)
	// 清掉该账户可能存在的负缓存（此前查询过的不存在账户会缓存空权限）
	s.invalidatePerms(username)
	if username == accountRoot {
		s.invalidateRootPasswordSet()
	}
	return err
}

// getAccount 读取账户；不存在返回 (nil, nil)。
func (s *accountSystem) getAccount(username string) (*account, error) {
	row := s.db.QueryRow(rebindSQL(s.driver,
		`SELECT username, is_admin, permissions, created_at, updated_at FROM accounts WHERE username = ?`), username)
	var (
		a     account
		isAdm int
		perms string
	)
	if err := row.Scan(&a.Username, &isAdm, &perms, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.IsAdmin = isAdm != 0
	a.Permissions = decodePerms(perms)
	return &a, nil
}

// listAccounts 列出全部账户（不含 root 虚拟账户）。
func (s *accountSystem) listAccounts() ([]*account, error) {
	rows, err := s.db.Query(rebindSQL(s.driver,
		`SELECT username, is_admin, permissions, created_at, updated_at FROM accounts ORDER BY username`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*account
	for rows.Next() {
		var (
			a     account
			isAdm int
			perms string
		)
		if err := rows.Scan(&a.Username, &isAdm, &perms, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.IsAdmin = isAdm != 0
		a.Permissions = decodePerms(perms)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// setPermissions 覆盖账户端点开关并失效权限热缓存。
func (s *accountSystem) setPermissions(username string, perms map[string]bool) error {
	data, err := json.Marshal(perms)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(rebindSQL(s.driver,
		`UPDATE accounts SET permissions = ?, updated_at = ? WHERE username = ?`),
		string(data), time.Now().UnixMilli(), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("账户不存在")
	}
	s.redisDelPerms(username)
	s.invalidatePerms(username) // 进程内缓存同步失效：权限收紧必须立即生效
	return nil
}

// putPassword 写入账户密码（UPSERT）：root 首次设置独立密码时插入
// accounts 表的 root 行；普通账户等价于覆盖。管理员直接重置与用户改密共用。
func (s *accountSystem) putPassword(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	// root 行插入时 is_admin=1（root 恒管理员；conflict 分支不触碰 is_admin，
	// 避免改密时抹掉普通账户的管理员标志）
	isAdmin := boolInt(username == accountRoot)
	if s.driver == "mysql" {
		_, err = s.db.Exec(
			`INSERT INTO accounts (username, password_hash, is_admin, permissions, created_at, updated_at)
			 VALUES (?, ?, ?, '{}', ?, ?)
			 ON DUPLICATE KEY UPDATE password_hash = VALUES(password_hash), updated_at = VALUES(updated_at)`,
			username, string(hash), isAdmin, now, now)
	} else {
		_, err = s.db.Exec(rebindSQL(s.driver,
			`INSERT INTO accounts (username, password_hash, is_admin, permissions, created_at, updated_at)
			 VALUES (?, ?, ?, '{}', ?, ?)
			 ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at`),
			username, string(hash), isAdmin, now, now)
	}
	// root 行由「不存在」变为「存在」，rootPasswordSet 的缓存结果随之改变
	if username == accountRoot {
		s.invalidateRootPasswordSet()
	}
	return err
}

// rootPasswordSet root 是否已设置独立登录密码（accounts 表存在 root 行）。
// 未设置时 root 的登录凭据为配对码/固定 apikey，登录响应强制改密。
// 每个 root 会话请求都会调用，结果走进程内短 TTL 缓存（改密时失效）。
func (s *accountSystem) rootPasswordSet() bool {
	if v, ok := s.cachedRootPasswordSet(); ok {
		return v
	}
	a, err := s.getAccount(accountRoot)
	v := err == nil && a != nil
	s.cacheRootPasswordSet(v)
	return v
}

// checkPassword 校验账户密码并返回账户（bcrypt 比较）。
func (s *accountSystem) checkPassword(username, password string) (*account, bool) {
	var hash string
	err := s.db.QueryRow(rebindSQL(s.driver,
		`SELECT password_hash FROM accounts WHERE username = ?`), username).Scan(&hash)
	if err != nil {
		return nil, false
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, false
	}
	a, err := s.getAccount(username)
	if err != nil || a == nil {
		return nil, false
	}
	return a, true
}

// deleteAccount 删除账户及其全部会话与权限缓存。
func (s *accountSystem) deleteAccount(username string) error {
	res, err := s.db.Exec(rebindSQL(s.driver, `DELETE FROM accounts WHERE username = ?`), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("账户不存在")
	}
	_, _ = s.db.Exec(rebindSQL(s.driver, `DELETE FROM sessions WHERE username = ?`), username)
	s.redisDelPerms(username)
	// SQL 已删其全部会话：进程内会话缓存必须同步清除，否则旧 token 在
	// 缓存过期前仍可通过鉴权（删账户后残留访问权限是严重越权）。
	s.invalidateUserSessions(username)
	s.invalidatePerms(username)
	if username == accountRoot {
		s.invalidateRootPasswordSet()
	}
	return nil
}

// ---------- 进程内热缓存（鉴权高频路径） ----------
//
// 每个 API 请求都要查会话（非 apikey 通道）与端点权限，默认 SQLite 部署下
// 即 2~3 次 SQL。这里用有 TTL 的进程内缓存把命中路径降为零 I/O；
// 写路径（登出/续期/改权限/改密/删账户）主动失效对应键，避免安全语义被
// TTL 拖慢（如「登出后仍可访问 10 秒」）。

// cachedSession 读取会话缓存；未命中或已过期返回 false。
// 会话本身在缓存期内到期时同样视为未命中，并顺手清除条目。
func (s *accountSystem) cachedSession(token string) (accountSession, bool) {
	now := time.Now()
	s.cacheMu.RLock()
	e, ok := s.sessCache[token]
	s.cacheMu.RUnlock()
	if !ok || now.After(e.expires) {
		return accountSession{}, false
	}
	if e.sess.ExpiresAt <= now.UnixMilli() {
		s.invalidateSession(token)
		return accountSession{}, false
	}
	return e.sess, true
}

// cacheSession 写入会话缓存（惰性建 map，兼容直接构造的 accountSystem）。
// root 恒 admin：写入前统一修正，保证进程内缓存与 SQL 层的判定一致。
func (s *accountSystem) cacheSession(token string, sess accountSession) {
	if sess.Username == accountRoot {
		sess.IsAdmin = true
	}
	s.cacheMu.Lock()
	if s.sessCache == nil {
		s.sessCache = map[string]sessCacheEntry{}
	}
	s.sessCache[token] = sessCacheEntry{sess: sess, expires: time.Now().Add(accountSessCacheTTL)}
	s.cacheMu.Unlock()
}

// invalidateSession 使单个会话缓存失效（登出时必须调用，保证立即不可访问）。
func (s *accountSystem) invalidateSession(token string) {
	s.cacheMu.Lock()
	delete(s.sessCache, token)
	s.cacheMu.Unlock()
}

// cachedPerms 读取权限缓存；返回副本，避免调用方改动污染共享缓存。
func (s *accountSystem) cachedPerms(username string) (map[string]bool, bool) {
	s.cacheMu.RLock()
	e, ok := s.permCache[username]
	s.cacheMu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	out := make(map[string]bool, len(e.perms))
	for k, v := range e.perms {
		out[k] = v
	}
	return out, true
}

// cachePerms 写入权限缓存（存副本，防止外部后续改动影响缓存）。
func (s *accountSystem) cachePerms(username string, perms map[string]bool) {
	cp := make(map[string]bool, len(perms))
	for k, v := range perms {
		cp[k] = v
	}
	s.cacheMu.Lock()
	if s.permCache == nil {
		s.permCache = map[string]permCacheEntry{}
	}
	s.permCache[username] = permCacheEntry{perms: cp, expires: time.Now().Add(accountPermCacheTTL)}
	s.cacheMu.Unlock()
}

// invalidatePerms 使单个账户的权限缓存失效（改权限/删账户时调用）。
func (s *accountSystem) invalidatePerms(username string) {
	s.cacheMu.Lock()
	delete(s.permCache, username)
	s.cacheMu.Unlock()
}

// cachedRootPasswordSet 读取 root 是否已设独立密码的缓存结果。
// 第二返回值为 false 表示尚未缓存（需回源查询）。
func (s *accountSystem) cachedRootPasswordSet() (bool, bool) {
	s.cacheMu.RLock()
	cached, val, exp := s.rootPwCached, s.rootPwSet, s.rootPwExpire
	s.cacheMu.RUnlock()
	if !cached || time.Now().After(exp) {
		return false, false
	}
	return val, true
}

// cacheRootPasswordSet 写入 root 密码状态缓存。
func (s *accountSystem) cacheRootPasswordSet(v bool) {
	s.cacheMu.Lock()
	s.rootPwSet, s.rootPwCached = v, true
	s.rootPwExpire = time.Now().Add(accountRootCacheTTL)
	s.cacheMu.Unlock()
}

// invalidateRootPasswordSet 使 root 密码状态缓存失效（root 改密/删账户后）。
func (s *accountSystem) invalidateRootPasswordSet() {
	s.cacheMu.Lock()
	s.rootPwCached = false
	s.rootPwExpire = time.Time{}
	s.cacheMu.Unlock()
}

// ---------- 会话（SQL 权威 + Redis 缓存） ----------

// newAccountToken 生成账户会话 token（crypto/rand，hex 64 字符）。
// 注意：vault.go 已有 newSessionToken（保险库会话），此处独立命名避免混淆。
func newAccountToken() (string, error) {
	b := make([]byte, accountTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// putSession 写入会话：SQL 权威落库（UPSERT）+ Redis 缓存（TTL）。
func (s *accountSystem) putSession(token string, sess accountSession, ttl time.Duration) {
	now := time.Now().UnixMilli()
	if sess.ExpiresAt <= now {
		return
	}
	if s.driver == "mysql" {
		_, _ = s.db.Exec(
			`INSERT INTO sessions (token, username, created_at, expires_at, last_used)
			 VALUES (?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE expires_at = VALUES(expires_at), last_used = VALUES(last_used)`,
			token, sess.Username, now, sess.ExpiresAt, now)
	} else {
		_, _ = s.db.Exec(rebindSQL(s.driver,
			`INSERT INTO sessions (token, username, created_at, expires_at, last_used)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(token) DO UPDATE SET expires_at = excluded.expires_at, last_used = excluded.last_used`),
			token, sess.Username, now, sess.ExpiresAt, now)
	}
	if s.redisReady() {
		data, err := json.Marshal(sess)
		if err == nil {
			if err := s.redis.Set(context.Background(), accountRedisPrefix+"session:"+token, data, ttl).Err(); err != nil {
				s.redisFailed()
			}
		}
	}
	// 写库成功即刷新进程内缓存：滑动续期后新过期时间要立即对后续请求可见
	s.cacheSession(token, sess)
}

// lookupSession 查找会话：进程内缓存 → Redis 热缓存 → SQL 权威。
// 每个 API 请求（非 apikey 通道）都会调用一次，命中进程内缓存时零 I/O。
func (s *accountSystem) lookupSession(token string) (accountSession, bool) {
	// 1) 进程内热缓存（零 I/O）
	if sess, ok := s.cachedSession(token); ok {
		return sess, true
	}
	// 2) Redis 热缓存
	if s.redisReady() {
		if sess, ok := s.redisGetSession(token); ok {
			s.cacheSession(token, sess)
			return sess, true
		}
	}
	// 3) SQL 权威层
	var (
		sess      accountSession
		expiresAt int64
	)
	err := s.db.QueryRow(rebindSQL(s.driver,
		`SELECT username, expires_at FROM sessions WHERE token = ?`), token).Scan(&sess.Username, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return accountSession{}, false
	}
	if err != nil {
		return accountSession{}, false
	}
	sess.ExpiresAt = expiresAt
	if sess.ExpiresAt <= time.Now().UnixMilli() {
		_, _ = s.db.Exec(rebindSQL(s.driver, `DELETE FROM sessions WHERE token = ?`), token)
		return accountSession{}, false
	}
	// 回填 admin 标志（root 恒 admin；普通账户读表）
	sess.IsAdmin = sess.Username == accountRoot
	if sess.Username != accountRoot {
		if a, err := s.getAccount(sess.Username); err == nil && a != nil {
			sess.IsAdmin = a.IsAdmin
		}
	}
	s.cacheSession(token, sess)   // 回填进程内缓存
	s.redisSetSession(token, sess) // 尽力回填（Redis 不可用时内部自动降级）
	return sess, true
}

// delSession 删除单个会话（SQL + Redis + 进程内缓存）。
// 进程内缓存必须同步失效：否则登出后仍能凭旧 token 访问到缓存过期为止。
func (s *accountSystem) delSession(token string) {
	_, _ = s.db.Exec(rebindSQL(s.driver, `DELETE FROM sessions WHERE token = ?`), token)
	if s.redisReady() {
		if err := s.redis.Del(context.Background(), accountRedisPrefix+"session:"+token).Err(); err != nil {
			s.redisFailed()
		}
	}
	s.invalidateSession(token)
}

// purgeExpiredSessions 清理过期会话（登录时顺手执行，避免独立后台任务）。
func (s *accountSystem) purgeExpiredSessions() {
	_, _ = s.db.Exec(rebindSQL(s.driver, `DELETE FROM sessions WHERE expires_at < ?`), time.Now().UnixMilli())
}

// ---------- 权限热缓存（Redis） ----------

// loadPermissions 读取账户端点开关：进程内缓存 → Redis → SQL，逐级回填。
// 每个 API 请求（非管理员通道）都会调用，命中进程内缓存时零 I/O。
func (s *accountSystem) loadPermissions(username string) map[string]bool {
	if m, ok := s.cachedPerms(username); ok {
		return m
	}
	if s.redisReady() {
		if m, ok := s.redisGetPerms(username); ok {
			s.cachePerms(username, m)
			return m
		}
	}
	a, err := s.getAccount(username)
	if err != nil || a == nil {
		// 负缓存：账户不存在时避免每次请求都查库（新建账户时主动失效）
		s.cachePerms(username, map[string]bool{})
		return map[string]bool{}
	}
	s.cachePerms(username, a.Permissions)   // 存副本，返回原始值
	s.redisSetPerms(username, a.Permissions) // 尽力回填
	return a.Permissions
}

// invalidateUserSessions 清除某用户在进程内缓存中的全部会话
// （删账户时调用：SQL 已删其全部会话，缓存不能留下可用 token）。
func (s *accountSystem) invalidateUserSessions(username string) {
	s.cacheMu.Lock()
	for tok, e := range s.sessCache {
		if e.sess.Username == username {
			delete(s.sessCache, tok)
		}
	}
	s.cacheMu.Unlock()
}

// ---------- 登录失败限速 ----------

// 登录失败限速：同一「来源 IP + 用户名」在窗口内失败达上限即锁定。
// 两个目的：
//  1. 防密码爆破——登录入口此前完全没有失败限速（保险库的 unlock/recovery 有）；
//  2. 防 CPU 打满——每次登录都要走 bcrypt（DefaultCost，单次约 100ms CPU 密集），
//     无限制时少量并发登录请求就能把节点 CPU 吃光，拖垮实例。
const (
	loginMaxAttempts   = 10
	loginLockoutPeriod = 5 * time.Minute
)

// loginFailState 单个限速 key 的失败计数与锁定状态。
type loginFailState struct {
	count       int       // 当前窗口内失败次数
	windowStart time.Time // 窗口起点
	lockedUntil time.Time // 锁定截止时刻（零值 = 未锁定）
}

// loginLimited 判断该 key 是否处于锁定中；窗口过期则顺带清理计数。
func (s *accountSystem) loginLimited(key string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	f := s.loginFails[key]
	if f == nil {
		return false
	}
	now := time.Now()
	if now.Before(f.lockedUntil) {
		return true
	}
	if now.Sub(f.windowStart) > loginLockoutPeriod {
		delete(s.loginFails, key) // 窗口已过，重新计数
	}
	return false
}

// loginFail 记录一次登录失败，达到上限即进入锁定。
func (s *accountSystem) loginFail(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string]*loginFailState{}
	}
	now := time.Now()
	f := s.loginFails[key]
	if f == nil || now.Sub(f.windowStart) > loginLockoutPeriod {
		f = &loginFailState{windowStart: now}
		s.loginFails[key] = f
	}
	f.count++
	if f.count >= loginMaxAttempts {
		f.lockedUntil = now.Add(loginLockoutPeriod)
	}
}

// loginReset 登录成功：清零该 key 的失败计数。
func (s *accountSystem) loginReset(key string) {
	s.loginMu.Lock()
	delete(s.loginFails, key)
	s.loginMu.Unlock()
}

// ---------- Redis 缓存与降级 ----------

// redisReady 是否允许使用 Redis（未配置或冷却期内返回 false）。
func (s *accountSystem) redisReady() bool {
	if s.redis == nil {
		return false
	}
	s.rdMu.Lock()
	defer s.rdMu.Unlock()
	return time.Now().After(s.rdDown)
}

// redisFailed 进入降级冷却：冷却期内一律走 SQL，到期自动重试恢复。
func (s *accountSystem) redisFailed() {
	s.rdMu.Lock()
	if time.Now().Before(s.rdDown) {
		s.rdMu.Unlock()
		return // 已在冷却期，不重复告警
	}
	s.rdDown = time.Now().Add(accountRedisCoold)
	s.rdMu.Unlock()
	alog.Printf("Redis 缓存不可用，账户鉴权回退数据库（%s 后自动重试）", accountRedisCoold)
}

func (s *accountSystem) redisGetSession(token string) (accountSession, bool) {
	data, err := s.redis.Get(context.Background(), accountRedisPrefix+"session:"+token).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.redisFailed()
		}
		return accountSession{}, false
	}
	var sess accountSession
	if json.Unmarshal(data, &sess) != nil || sess.ExpiresAt <= time.Now().UnixMilli() {
		return accountSession{}, false
	}
	return sess, true
}

func (s *accountSystem) redisSetSession(token string, sess accountSession) {
	if !s.redisReady() {
		return
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return
	}
	ttl := time.Duration(sess.ExpiresAt-time.Now().UnixMilli()) * time.Millisecond
	if ttl <= 0 {
		return
	}
	if err := s.redis.Set(context.Background(), accountRedisPrefix+"session:"+token, data, ttl).Err(); err != nil {
		s.redisFailed()
	}
}

func (s *accountSystem) redisGetPerms(username string) (map[string]bool, bool) {
	data, err := s.redis.Get(context.Background(), accountRedisPrefix+"perm:"+username).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.redisFailed()
		}
		return nil, false
	}
	m := map[string]bool{}
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return m, true
}

func (s *accountSystem) redisSetPerms(username string, perms map[string]bool) {
	if !s.redisReady() {
		return
	}
	data, err := json.Marshal(perms)
	if err != nil {
		return
	}
	if err := s.redis.Set(context.Background(), accountRedisPrefix+"perm:"+username, data, accountPermTTL).Err(); err != nil {
		s.redisFailed()
	}
}

func (s *accountSystem) redisDelPerms(username string) {
	if !s.redisReady() {
		return
	}
	if err := s.redis.Del(context.Background(), accountRedisPrefix+"perm:"+username).Err(); err != nil {
		s.redisFailed()
	}
}

// ---------- 通用工具 ----------

// boolInt bool → 0/1（跨库布尔兼容）。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// decodePerms 解析权限 JSON；损坏时返回空 map（安全默认：全关）。
func decodePerms(s string) map[string]bool {
	m := map[string]bool{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// validUsername 校验用户名：1-64 位字母/数字/下划线/连字符。
func validUsername(name string) bool {
	if name == "" || len(name) > accountNameMaxLen {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// checkRootPassword 校验 root 密码：固定 apikey 或配对码（恒定时间比较）。
func (d *Daemon) checkRootPassword(password string) bool {
	if password == "" {
		return false
	}
	if d.APIKey != "" {
		return subtle.ConstantTimeCompare([]byte(password), []byte(d.APIKey)) == 1
	}
	if d.PairingHash == "" {
		return false
	}
	return checkPairing(password, d.PairingHash)
}
