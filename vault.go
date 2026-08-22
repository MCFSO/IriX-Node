// vault.go — 加密保险库：账号、解锁会话、挑战、限速、数据面门禁（M3）。
//
// 实现 docs/vault-design.md §6/§7/§11：init/initToken/TOTP 绑定/证书绑定/
// unlock/lock/password/recovery/users 端点、vault.json 持久化、统一限速
// （unlock/recovery/init-verify，用户+IP 双维度）、masterKey 生命周期锁
// （S7）、挑战池（分用途/一次性/上限 1024/TTL 清理）、数据面 vault 门禁。
//
// 安全要点：
//   - masterKey 仅驻内存，锁定即尽力清零（zeroBytes，best-effort）；
//   - 认证类失败一律返回统一 401「认证失败」，不暴露失败原因；
//   - 挑战首次使用即作废（无论成败），防同一签名对重放爆破；
//   - 会话令牌仅经 X-Vault-Token 头传输，禁止 query string（D17）。

package main

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// vault 常量（docs/vault-design.md §7.1/§8.6）。
const (
	vaultFileVersion = 1 // vault.json 结构版本

	maxChallenges = 1024             // 挑战池上限，防刷爆内存
	challengeTTL  = 5 * time.Minute  // 挑战有效期
	initTokenTTL  = 10 * time.Minute // 初始化令牌有效期
	recoveryTTL   = 5 * time.Minute  // 恢复会话有效期
	maxTOTPFails  = 5                // 初始化 TOTP 验证失败上限（作废 initToken）

	vaultJSONPerm = 0o600 // vault.json 权限
)

// vaultState 保险库运行时状态（Daemon.vault；enabled=false 时全部 vault 路由返回未启用）。
type vaultState struct {
	d *Daemon

	enabled     bool // vault.enabled 配置（main 设置）
	initialized bool // vault.json 已存在且已 init

	// 配置（main 从 opts 写入，之后只读）
	idleTimeout      time.Duration // 会话空闲超时
	maxAttempts      int           // 失败限速阈值
	lockoutDuration  time.Duration // 锁定时长
	pbkdf2Iterations int           // PBKDF2 迭代次数
	passwordMinLen   int           // 密码最小长度
	passwordExpire   time.Duration // 密码有效期（0 = 不过期）
	forceExpire      bool          // 到期强制改密（解锁同请求）
	bindSessionIP    bool          // 会话绑定来源 IP
	scrubOnDelete    bool          // 回收/删除前覆盖明文（best-effort）
	blockSizeKB      int           // 密文对象块大小（KB）
	defaultFilesMode string        // 新实例文件区默认模式：plaintext | materialize

	// mu 生命周期锁（S7）：保护 masterKey 与全部内存态。
	// 数据面门禁与解锁/锁定在同一把锁下完成「令牌校验 → 解密」与「销毁」，
	// 避免 use-after-zero 竞态（列入 -race 测试）。
	mu sync.RWMutex

	masterKey []byte // 数据域主密钥（仅内存；锁定即清零置空）
	loading   bool   // 解锁后初始化中（存储层/实例列表加载，数据面短暂 403）

	store     *vaultStore     // 加密存储层（M4）
	migration *vaultMigration // 迁移状态（持久化）
	migrating bool            // 迁移运行中（数据面 403 vault migrating）

	users       map[string]*vaultUser
	recovery    *vaultRecovery
	seq         int64 // 单调递增序号（防回滚弱防护，见设计 §8.6）
	createdAt   time.Time
	file        string           // vault.json 路径
	lastTOTPWin map[string]int64 // 用户 → 最近成功 TOTP 窗口（防重放）
	sessions    map[string]*vaultSession
	challenges  map[string]*vaultChallenge
	initTokens  map[string]*vaultInitToken
	failures    map[string]*vaultFail // 限速器（unlock/recovery/init-verify 共用）

	indexDEKWrapB64 string // 索引密钥包裹（vault.json；解锁时解开）
}

// vaultUser 保险库用户（持久化于 vault.json；lastTOTPWindow 仅内存）。
type vaultUser struct {
	Name              string    `json:"name"`
	TOTPSecretB64     string    `json:"totpSecretB64"`
	TOTPBound         bool      `json:"totpBound"`
	CertFingerprint   string    `json:"certFingerprint"`
	CertPublicPEM     string    `json:"certPublicPEM"`
	KEKSaltB64        string    `json:"kekSaltB64"`
	MasterKeyWrapB64  string    `json:"masterKeyWrapB64"`
	PasswordChangedAt time.Time `json:"passwordChangedAt"`
	CreatedAt         time.Time `json:"createdAt"`

	lastTOTPWindow int64 // 内存态：最近成功 TOTP 窗口（防重放）
}

// vaultRecovery 恢复令牌记录（仅存哈希，复用配对码模式）。
type vaultRecovery struct {
	Hash             string `json:"hash"`             // 恢复令牌 SHA-256 hex
	MasterKeyWrapB64 string `json:"masterKeyWrapB64"` // 恢复密钥包裹的 masterKey（wrapBlob）
}

// vaultSession 会话（内存态）。
// unlocked 会话：解锁建立，可访问数据面；recovery 会话：恢复令牌建立，
// 仅可用于改密/重绑 TOTP/换绑证书，不开放数据面（masterKey 副本仅用于 rewrap）。
type vaultSession struct {
	token      string
	user       string
	certFP     string
	ip         string // bindSessionIP 时绑定
	recovery   bool
	masterKey  []byte // recovery 会话持有（rewrap 用）；解锁会话不持有
	lastActive time.Time
	expiresAt  time.Time
}

// vaultChallenge 一次性挑战（内存态）。
type vaultChallenge struct {
	id      string
	purpose string // unlock | cert-bind
	value   string // base64 挑战（签名消息 = 前缀 + value）
	expires time.Time
	used    bool
}

// vaultInitToken 初始化（onboarding）令牌：init 或 user/add 后绑定 TOTP/证书用。
type vaultInitToken struct {
	token   string
	user    string
	expires time.Time
	fails   int
}

// vaultFail 限速器条目：窗口内失败计数；超过阈值进入锁定。
type vaultFail struct {
	windowStart time.Time
	count       int
	until       time.Time // 锁定截止（零值 = 未锁定）
}

// vaultFile vault.json 持久化结构。
type vaultFile struct {
	Version         int             `json:"version"`
	Seq             int64           `json:"seq"`
	Users           []*vaultUser    `json:"users"`
	Recovery        *vaultRecovery  `json:"recovery"`
	IndexDEKWrapB64 string          `json:"indexDEKWrapB64"` // 索引密钥包裹（M4）
	Migration       *vaultMigration `json:"migration"`       // 迁移标记（M4）
	CreatedAt       time.Time       `json:"createdAt"`
}

// newVaultState 创建保险库状态（默认配置；main 按 opts 覆盖）。
func newVaultState(d *Daemon) *vaultState {
	vaultDir := filepath.Join(d.DataDir, "vault")
	v := &vaultState{
		d:                d,
		idleTimeout:      30 * time.Minute,
		maxAttempts:      5,
		lockoutDuration:  15 * time.Minute,
		pbkdf2Iterations: defaultPBKDF2Iterations,
		passwordMinLen:   12,
		passwordExpire:   90 * 24 * time.Hour,
		blockSizeKB:      1024,
		defaultFilesMode: "plaintext",
		users:            map[string]*vaultUser{},
		lastTOTPWin:      map[string]int64{},
		sessions:         map[string]*vaultSession{},
		challenges:       map[string]*vaultChallenge{},
		initTokens:       map[string]*vaultInitToken{},
		failures:         map[string]*vaultFail{},
		store: newVaultStore(d,
			filepath.Join(vaultDir, "objects"),
			filepath.Join(vaultDir, "index.json.enc"),
			1024*1024),
	}
	go v.janitor() // 定期清理过期会话/挑战/令牌/限速条目
	return v
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// load 从 path 加载 vault.json；不存在视为未初始化（不报错）。
func (v *vaultState) load(path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.file = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var f vaultFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if f.Version != vaultFileVersion {
		return fmt.Errorf("%s 版本不支持: %d", path, f.Version)
	}
	v.seq = f.Seq
	v.createdAt = f.CreatedAt
	v.recovery = f.Recovery
	v.indexDEKWrapB64 = f.IndexDEKWrapB64
	v.migration = f.Migration
	v.users = map[string]*vaultUser{}
	for _, u := range f.Users {
		v.users[u.Name] = u
	}
	if len(v.users) > 0 || v.recovery != nil {
		v.initialized = true
	}
	return nil
}

// save 原子写 vault.json（tmp+rename，0600）；seq 单调递增。
// 调用方必须已持有 v.mu。
func (v *vaultState) save() error {
	if v.file == "" {
		return errors.New("vault.json 路径未设置")
	}
	names := make([]string, 0, len(v.users))
	for name := range v.users {
		names = append(names, name)
	}
	sort.Strings(names)
	users := make([]*vaultUser, 0, len(names))
	for _, name := range names {
		users = append(users, v.users[name])
	}
	v.seq++
	f := vaultFile{
		Version:         vaultFileVersion,
		Seq:             v.seq,
		Users:           users,
		Recovery:        v.recovery,
		IndexDEKWrapB64: v.indexDEKWrapB64,
		Migration:       v.migration,
		CreatedAt:       v.createdAt,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(v.file), 0o700); err != nil {
		return fmt.Errorf("创建保险库目录失败: %w", err)
	}
	tmp := v.file + ".tmp"
	if err := os.WriteFile(tmp, data, vaultJSONPerm); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, v.file); err != nil {
		return fmt.Errorf("落盘 %s 失败: %w", v.file, err)
	}
	return nil
}

// janitor 定期清理：过期会话（最后一个解锁会话过期 → 清零 masterKey）、
// 过期挑战、过期初始化令牌、限速条目。
func (v *vaultState) janitor() {
	for {
		time.Sleep(time.Minute)
		v.mu.Lock()
		now := time.Now()
		unlockedAny := false
		for k, s := range v.sessions {
			if now.After(s.expiresAt) {
				delete(v.sessions, k)
				if !s.recovery {
					v.d.auditLogf("vault.timeout 会话过期 user=%s", s.user)
				}
				continue
			}
			if !s.recovery {
				unlockedAny = true
			}
		}
		if !unlockedAny && v.masterKey != nil {
			// 最后一个解锁会话过期：落盘索引、清零密钥（剩余信息保护）
			if err := v.store.flush(); err != nil {
				v.d.auditLogf("vault.store 超时落盘失败: %v", err)
			}
			v.store.zeroKeys()
			zeroBytes(v.masterKey)
			v.masterKey = nil
		}
		for k, c := range v.challenges {
			if now.After(c.expires) {
				delete(v.challenges, k)
			}
		}
		for k, it := range v.initTokens {
			if now.After(it.expires) {
				delete(v.initTokens, k)
			}
		}
		for k, f := range v.failures {
			if now.After(f.until) && now.Sub(f.windowStart) > v.lockoutDuration {
				delete(v.failures, k)
			}
		}
		v.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// 密码策略 / 限速器
// ---------------------------------------------------------------------------

// validateVaultPassword 校验密码策略：最小长度 + 大小写 + 数字。
func (v *vaultState) validateVaultPassword(pw string) error {
	if len(pw) < v.passwordMinLen {
		return fmt.Errorf("密码长度不能少于 %d 位", v.passwordMinLen)
	}
	hasUpper, hasLower, hasDigit := false, false, false
	for _, ch := range pw {
		switch {
		case ch >= 'A' && ch <= 'Z':
			hasUpper = true
		case ch >= 'a' && ch <= 'z':
			hasLower = true
		case ch >= '0' && ch <= '9':
			hasDigit = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return errors.New("密码必须包含大写字母、小写字母与数字")
	}
	return nil
}

// vaultLimited 检查 key 是否处于锁定中（限速器；调用方须持有 v.mu）。
func (v *vaultState) vaultLimited(key string) bool {
	f := v.failures[key]
	return f != nil && time.Now().Before(f.until)
}

// vaultFail 记录一次失败；窗口内达到 maxAttempts → 进入锁定（调用方须持有 v.mu）。
func (v *vaultState) vaultFail(key string) {
	now := time.Now()
	f := v.failures[key]
	if f == nil || now.Sub(f.windowStart) > v.lockoutDuration {
		f = &vaultFail{windowStart: now}
		v.failures[key] = f
	}
	f.count++
	if f.count >= v.maxAttempts {
		f.until = now.Add(v.lockoutDuration)
		v.d.auditLogf("vault.lockout 触发限速 key=%s", key)
	}
}

// vaultReset 成功时清零该 key 的失败计数（调用方须持有 v.mu）。
func (v *vaultState) vaultReset(key string) {
	delete(v.failures, key)
}

// ---------------------------------------------------------------------------
// 会话 / 授权辅助（调用方须持有 v.mu）
// ---------------------------------------------------------------------------

// sessionFor 校验 X-Vault-Token 并返回未过期会话（含 IP 绑定校验）。
func (v *vaultState) sessionFor(r *http.Request) *vaultSession {
	token := r.Header.Get("X-Vault-Token")
	if token == "" {
		return nil
	}
	s := v.sessions[token]
	if s == nil || time.Now().After(s.expiresAt) {
		return nil
	}
	if v.bindSessionIP && s.ip != "" && s.ip != clientIP(r) {
		return nil
	}
	return s
}

// unlocked 是否处于解锁状态（存在解锁会话且 masterKey 在内存）。
// 调用方必须已持有 v.mu。
func (v *vaultState) unlocked() bool {
	return v.masterKey != nil
}

// unlockedSafe 加锁读取解锁状态（供门禁豁免路径如 /api/overview 脱敏判断）。
func (v *vaultState) unlockedSafe() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.initialized && v.unlocked()
}

// vaultTarget 解析操作目标用户与授权上下文：
//   - initToken（onboarding）→ 目标用户；
//   - 解锁会话 → 会话用户；
//   - recovery 会话 → 会话用户。
//
// 返回 (用户, 授权类型, 错误)；调用方须持有 v.mu。
func (v *vaultState) vaultTarget(r *http.Request) (*vaultUser, string, error) {
	if token := r.Header.Get("X-Vault-Token"); token != "" {
		if it := v.initTokens[token]; it != nil && time.Now().Before(it.expires) {
			u := v.users[it.user]
			if u != nil {
				return u, "init", nil
			}
		}
		if s := v.sessionFor(r); s != nil {
			u := v.users[s.user]
			if u != nil {
				if s.recovery {
					return u, "recovery", nil
				}
				if v.unlocked() {
					return u, "session", nil
				}
			}
		}
	}
	return nil, "", errors.New("未授权")
}

// newSessionToken 生成会话令牌（base64 无填充，32B）。
func newSessionToken() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// totpBase32 / otpauthURI：TOTP 密钥展示与扫码绑定。
func totpBase32(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

func otpauthURI(user string, secret []byte) string {
	return fmt.Sprintf("otpauth://totp/IriXNode:%s?secret=%s&issuer=IriXNode&algorithm=SHA1&digits=%d&period=%d",
		url.QueryEscape(user), totpBase32(secret), totpDigits, totpPeriod)
}

// ---------------------------------------------------------------------------
// 路由与门禁
// ---------------------------------------------------------------------------

// registerVaultRoutes 注册保险库路由（RegisterRoutes 调用）。
func (d *Daemon) registerVaultRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/vault/status", d.auth(d.handleVaultStatus))
	mux.HandleFunc("POST /api/vault/init", d.auth(d.handleVaultInit))
	mux.HandleFunc("POST /api/vault/totp/verify", d.auth(d.handleVaultTOTPVerify))
	mux.HandleFunc("POST /api/vault/totp/reset", d.auth(d.handleVaultTOTPReset))
	mux.HandleFunc("POST /api/vault/challenge", d.auth(d.handleVaultChallenge))
	mux.HandleFunc("POST /api/vault/cert", d.auth(d.handleVaultCert))
	mux.HandleFunc("POST /api/vault/unlock", d.auth(d.handleVaultUnlock))
	mux.HandleFunc("POST /api/vault/lock", d.auth(d.handleVaultLock))
	mux.HandleFunc("POST /api/vault/password", d.auth(d.handleVaultPassword))
	mux.HandleFunc("POST /api/vault/recovery", d.auth(d.handleVaultRecovery))
	mux.HandleFunc("POST /api/vault/user/add", d.auth(d.handleVaultUserAdd))
	mux.HandleFunc("POST /api/vault/user/remove", d.auth(d.handleVaultUserRemove))
	mux.HandleFunc("GET /api/vault/users", d.auth(d.handleVaultUsers))
	mux.HandleFunc("POST /api/vault/migrate", d.auth(d.handleVaultMigrate))
	mux.HandleFunc("GET /api/vault/migrate/status", d.auth(d.handleVaultMigrateStatus))
	mux.HandleFunc("POST /api/vault/backup", d.auth(d.handleVaultBackup))
}

// vaultGate 数据面门禁（docs/vault-design.md §7.3/§8.8）：
// vault 未启用 → 放行；启用后未初始化 → 403 vault not initialized；
// 锁定 → 403 vault locked；解锁 → 校验 X-Vault-Token 会话并滑动续期。
// /api/vault/*、/api/overview、/api/load 豁免（overview 由处理器脱敏）。
func (d *Daemon) vaultGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/vault/") || p == "/api/overview" || p == "/api/load" {
			next.ServeHTTP(w, r)
			return
		}
		v := d.vault
		if v == nil || !v.enabled {
			next.ServeHTTP(w, r)
			return
		}
		v.mu.RLock()
		initialized := v.initialized
		unlocked := v.unlocked()
		loading := v.loading
		migrating := v.migrating
		v.mu.RUnlock()
		if !initialized {
			writeError(w, http.StatusForbidden, "vault not initialized")
			return
		}
		if loading {
			writeError(w, http.StatusForbidden, "vault loading")
			return
		}
		if migrating {
			writeError(w, http.StatusForbidden, "vault migrating")
			return
		}
		if !unlocked {
			writeError(w, http.StatusForbidden, "vault locked")
			return
		}
		// 解锁状态：校验会话令牌并滑动续期
		v.mu.Lock()
		s := v.sessionFor(r)
		if s == nil || s.recovery {
			v.mu.Unlock()
			writeError(w, http.StatusForbidden, "vault locked")
			return
		}
		s.lastActive = time.Now()
		s.expiresAt = time.Now().Add(v.idleTimeout)
		v.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// vaultOK 校验保险库已启用；未启用时写 403 并返回 false。
func (d *Daemon) vaultOK(w http.ResponseWriter) bool {
	if d.vault == nil || !d.vault.enabled {
		writeError(w, http.StatusForbidden, "加密保险库未启用")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// M4：数据面集成辅助
// ---------------------------------------------------------------------------

// instancesMigrated 实例列表是否已加密迁移（启动时据此跳过明文加载）。
func (v *vaultState) instancesMigrated() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.migration != nil && v.migration.InstancesDone
}

// vaultActive 数据面是否处于「保险库模式」：已启用、已初始化、已解锁、
// 实例列表已迁移（instances.json 走加密对象）。
func (d *Daemon) vaultActive() bool {
	if d.vault == nil || !d.vault.enabled {
		return false
	}
	v := d.vault
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.initialized && v.masterKey != nil && v.migration != nil && v.migration.InstancesDone
}

// loadInstancesVault 从加密对象加载实例列表（解锁后调用；幂等）。
func (d *Daemon) loadInstancesVault() error {
	v := d.vault
	if v == nil || !v.enabled {
		return nil
	}
	if v.store.stat(systemInstancesPath) == nil {
		d.mu.Lock()
		d.Instances = []*Instance{}
		d.mu.Unlock()
		return nil
	}
	data, err := v.store.readFile(v, systemInstancesPath)
	if err != nil {
		if errors.Is(err, lockedErr()) {
			return err
		}
		d.auditLogf("警告: 加密实例列表读取失败（%v），按空列表加载", err)
		d.mu.Lock()
		d.Instances = []*Instance{}
		d.mu.Unlock()
		return nil
	}
	var list []PersistedInstance
	if err := json.Unmarshal(data, &list); err != nil {
		d.auditLogf("警告: 加密实例列表解析失败（%v），按空列表加载", err)
		d.mu.Lock()
		d.Instances = []*Instance{}
		d.mu.Unlock()
		return nil
	}
	d.mu.Lock()
	d.Instances = []*Instance{}
	for _, p := range list {
		inst := NewInstance(p.InstanceUuid, p.Config)
		inst.Started = p.Started
		d.Instances = append(d.Instances, inst)
	}
	d.mu.Unlock()
	return nil
}

// postUnlockInit 解锁后的初始化（阶段 2，须在释放 v.mu 后调用）：
// 加载存储层（indexDEK/索引/孤儿回收）→ 加载实例列表 → 崩溃残留回收 →
// 启动迁移。任一步失败由调用方回滚会话与密钥。
func (v *vaultState) postUnlockInit(masterKey []byte) error {
	if err := v.store.load(v, masterKey, v.indexDEKWrapB64); err != nil {
		return err
	}
	// 阶段一必须先于实例列表加载：加密对象可能尚未创建
	// （崩溃于「对象→标记→删明文」之间时，重跑幂等）
	if err := v.migrateInstancesJSON(); err != nil {
		return err
	}
	if err := v.d.loadInstancesVault(); err != nil {
		return err
	}
	// 崩溃残留回收（D9）：vaultFiles 实例明文目录有残留 → 加密入库（幂等）
	d := v.d
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()
	for _, inst := range insts {
		if !inst.Config.VaultFiles {
			continue
		}
		cwd := inst.Config.Cwd
		if plainTreeDirty(cwd) {
			if err := v.store.encryptBack(v, inst.InstanceUuid, cwd); err != nil {
				return fmt.Errorf("实例 %s 崩溃残留回收失败: %w", inst.InstanceUuid, err)
			}
			d.auditLogf("vault.recycle 实例 %s 崩溃残留已回收", inst.InstanceUuid)
		}
	}
	// 启动/续跑迁移（无实质文件工作时同步完成）
	v.startMigration()
	// 解锁后初始化完成：数据面恢复可用
	v.mu.Lock()
	v.loading = false
	v.mu.Unlock()
	return nil
}

// vaultMaterialize 物化实例文件树（启动前，D9）：
// 崩溃残留回收（幂等）→ 磁盘余量预检 → 解密物化到明文工作目录。
func (d *Daemon) vaultMaterialize(inst *Instance, cwd string) error {
	v := d.vault
	if v == nil || !v.enabled || !v.unlockedSafe() {
		return errors.New("保险库未解锁，无法物化实例文件")
	}
	if cwd == "" {
		return errors.New("实例工作目录为空")
	}
	if plainTreeDirty(cwd) {
		if err := v.store.encryptBack(v, inst.InstanceUuid, cwd); err != nil {
			return fmt.Errorf("崩溃残留回收失败: %w", err)
		}
		d.auditLogf("vault.recycle 实例 %s 崩溃残留已回收（启动前）", inst.InstanceUuid)
	}
	// 物化所需空间 = 索引中该实例文件树大小
	var need int64
	if err := v.store.walkPrefix(v, "/"+inst.InstanceUuid+"/", func(_ string, e *vaultIndexEntry) error {
		need += e.Size
		return nil
	}); err != nil {
		return err
	}
	if free, ok := diskFreeBytes(cwd); ok && free < need {
		return fmt.Errorf("磁盘余量不足：物化需要约 %s，可用 %s", FormatSize(need), FormatSize(free))
	}
	if err := v.store.materialize(v, inst.InstanceUuid, cwd); err != nil {
		return fmt.Errorf("物化失败: %w", err)
	}
	d.auditLogf("vault.materialize 实例 %s 文件树已物化（%s）", inst.InstanceUuid, FormatSize(need))
	return nil
}

// vaultRecycle 回收实例文件树（停止后/优雅关停，D9）：整树加密入库并删除明文。
func (d *Daemon) vaultRecycle(inst *Instance, cwd string) error {
	v := d.vault
	if v == nil || !v.enabled || !v.unlockedSafe() {
		return errors.New("保险库未解锁")
	}
	if cwd == "" {
		return nil
	}
	if err := v.store.encryptBack(v, inst.InstanceUuid, cwd); err != nil {
		return err
	}
	d.auditLogf("vault.recycle 实例 %s 文件树已加密回收", inst.InstanceUuid)
	return nil
}

// applyVaultDefault 创建/导入实例时应用 vault.defaultFilesMode 默认值
// （materialize 模式新实例默认 vaultFiles=true）。仅在创建路径调用 ——
// 加载路径不得翻转既有实例（NewInstance 不应用此默认）。
func (d *Daemon) applyVaultDefault(cfg *InstanceConfig) {
	if d.vault == nil || !d.vault.enabled || d.vault.defaultFilesMode != "materialize" {
		return
	}
	cfg.VaultFiles = true
}
