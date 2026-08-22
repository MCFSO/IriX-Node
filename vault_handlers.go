// vault_handlers.go — 保险库 HTTP 处理器（M3）。
//
// 实现 docs/vault-design.md §6/§7/§10 的全部端点语义：
//   - init：首次初始化（生成 masterKey/恢复令牌/TOTP secret/initToken）；
//   - totp/verify：绑定确认（initToken 或恢复/解锁会话，5 次失败作废 initToken）；
//   - totp/reset：重绑 TOTP（恢复/解锁会话）；
//   - challenge：签发一次性挑战（unlock / cert-bind 分用途）；
//   - cert：绑定证书（initToken / 解锁 / 恢复会话，SPKI 指纹）；
//   - unlock：TOTP+密码+证书签名三重认证解锁（可同请求改密，A3/D15）；
//   - lock：立即锁定（清零 masterKey、清空会话）；
//   - password：改密 rewrap（解锁或恢复会话）；
//   - recovery：恢复令牌建立 5 分钟恢复会话；
//   - user/add、user/remove、users：多用户管理（禁删最后一个用户）。
//
// 安全约定：认证类失败统一 401「认证失败」（限速锁定除外，见 handleVaultUnlock）；
// 所有处理器持有 v.mu（生命周期锁），保证 masterKey 读改与锁定互斥（S7）。

package main

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// authFailed 统一认证失败响应（不区分 TOTP/密码/签名，防信息泄露）。
func authFailed(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "认证失败")
}

// b64e / b64d：wrapBlob 与盐的 base64 编解码。
func b64e(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func b64d(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// handleVaultStatus 获取保险库状态。
// GET /api/vault/status → { enabled, initialized, locked, user?, expiresIn?, passwordExpired? }
func (d *Daemon) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	v := d.vault
	if v == nil || !v.enabled {
		writeOK(w, map[string]any{"enabled": false, "initialized": false, "locked": true})
		return
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	data := map[string]any{"enabled": true, "initialized": v.initialized}
	if v.initialized {
		data["locked"] = !v.unlocked()
		if s := v.sessionFor(r); s != nil && !s.recovery {
			data["user"] = s.user
			data["expiresIn"] = int(time.Until(s.expiresAt).Seconds())
			if u := v.users[s.user]; u != nil && v.passwordExpire > 0 &&
				time.Since(u.PasswordChangedAt) >= v.passwordExpire {
				data["passwordExpired"] = true
			}
		}
	}
	writeOK(w, data)
}

// handleVaultInit 首次初始化保险库（仅未初始化时可用）。
// POST /api/vault/init { user, password } →
//
//	{ initToken, totpSecret, otpauthURI, recoveryToken }
//
// recoveryToken 仅此一次返回（仅存哈希），丢失后只能删除 vault 目录重新初始化。
func (d *Daemon) handleVaultInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.initialized {
		writeError(w, http.StatusBadRequest, "保险库已初始化")
		return
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" {
		writeError(w, http.StatusBadRequest, "用户名不能为空")
		return
	}
	if err := v.validateVaultPassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 生成密钥树（docs/vault-design.md §5）
	masterKey, err := randomBytes(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	kekSalt, err := randomBytes(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	kek, err := deriveKEK(req.Password, kekSalt, v.pbkdf2Iterations, 32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥派生失败")
		return
	}
	masterKeyWrap, err := gcmWrap(kek, masterKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹失败")
		return
	}
	totpSecret, err := randomBytes(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	recoveryToken, err := randomBytes(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	recoveryWrap, err := gcmWrap(recoveryToken, masterKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹失败")
		return
	}
	recoveryTokenStr := base64.RawStdEncoding.EncodeToString(recoveryToken)
	recoveryHash := sha256.Sum256([]byte(recoveryTokenStr))
	// 索引密钥（M4）：indexDEK 由 masterKey 包裹，存 vault.json
	indexDEK, err := randomBytes(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	indexDEKWrap, err := gcmWrap(masterKey, indexDEK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹失败")
		return
	}
	now := time.Now()

	user := &vaultUser{
		Name:              req.User,
		TOTPSecretB64:     b64e(totpSecret),
		KEKSaltB64:        b64e(kekSalt),
		MasterKeyWrapB64:  b64e(masterKeyWrap),
		PasswordChangedAt: now,
		CreatedAt:         now,
	}
	v.users[req.User] = user
	v.recovery = &vaultRecovery{
		Hash:             hex.EncodeToString(recoveryHash[:]),
		MasterKeyWrapB64: b64e(recoveryWrap),
	}
	v.indexDEKWrapB64 = b64e(indexDEKWrap)
	v.initialized = true
	v.createdAt = now
	if err := v.save(); err != nil {
		// 落盘失败：回滚内存态，避免「内存已初始化、磁盘未初始化」的不一致
		delete(v.users, req.User)
		v.recovery = nil
		v.initialized = false
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	initToken, err := newSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "令牌生成失败")
		return
	}
	v.initTokens[initToken] = &vaultInitToken{
		token:   initToken,
		user:    req.User,
		expires: time.Now().Add(initTokenTTL),
	}
	d.auditLogf("vault.init 初始化完成 user=%s", req.User)
	writeOK(w, map[string]any{
		"initToken":     initToken,
		"totpSecret":    totpBase32(totpSecret),
		"otpauthURI":    otpauthURI(req.User, totpSecret),
		"recoveryToken": recoveryTokenStr,
	})
}

// handleVaultTOTPVerify 确认 TOTP 绑定。
// 授权：initToken（初始化/新增用户 onboarding）或恢复/解锁会话（重绑后确认）。
// initToken 路径：失败累计 maxTOTPFails 次作废令牌（需重新 init 或走恢复流程）。
// POST /api/vault/totp/verify { code }
func (d *Daemon) handleVaultTOTPVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()

	token := r.Header.Get("X-Vault-Token")
	it := v.initTokens[token]
	ip := clientIP(r)
	limiterKey := "verify:" + ip
	if v.vaultLimited(limiterKey) {
		writeError(w, http.StatusUnauthorized, "尝试次数过多，请稍后再试")
		return
	}
	// 授权解析：initToken 优先；其次恢复/解锁会话
	var u *vaultUser
	isInit := false
	if it != nil && time.Now().Before(it.expires) {
		u = v.users[it.user]
		isInit = true
	} else if s := v.sessionFor(r); s != nil {
		u = v.users[s.user]
	}
	if u == nil {
		writeError(w, http.StatusUnauthorized, "未授权或初始化会话已过期")
		return
	}

	secret, err := b64d(u.TOTPSecretB64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOTP 密钥损坏")
		return
	}
	now := time.Now()
	win := now.Unix() / totpPeriod
	if !totpVerify(secret, req.Code, now, 1, totpDigits, totpPeriod) || win <= u.lastTOTPWindow {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.totp.verify 失败 user=%s", u.Name)
		if isInit {
			it.fails++
			if it.fails >= maxTOTPFails {
				delete(v.initTokens, token)
				writeError(w, http.StatusUnauthorized, "验证失败次数过多，初始化会话已作废（可通过恢复令牌重新绑定）")
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "验证码错误")
		return
	}
	u.TOTPBound = true
	// 注意：绑定不写入 lastTOTPWindow —— 防重放只针对解锁操作本身；
	// 若绑定即写窗口，onboarding 后同一 30 秒窗口内的首次解锁会被误判为
	// 重放拒绝（设计 §6.3 流程第 5 步之后的解锁是正常用户行为）。
	// initToken 保留至证书绑定完成（设计 §6.3 第 5 步仍需使用）；
	// 仅当绑定失败累计 maxTOTPFails 次或到期时才作废。
	if err := v.save(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	v.vaultReset(limiterKey)
	d.auditLogf("vault.totp.verify 成功 user=%s", u.Name)
	writeOK(w, map[string]any{"bound": true})
}

// handleVaultTOTPReset 重绑 TOTP（恢复或解锁会话）：生成新 secret，置 totpBound=false，
// 客户端须随后调用 /totp/verify 确认。POST /api/vault/totp/reset
func (d *Daemon) handleVaultTOTPReset(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	u, kind, err := v.vaultTarget(r)
	if err != nil || (kind != "session" && kind != "recovery") {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	secret, err := randomBytes(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	u.TOTPSecretB64 = b64e(secret)
	u.TOTPBound = false
	if err := v.save(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.auditLogf("vault.totp.reset user=%s", u.Name)
	writeOK(w, map[string]any{
		"totpSecret": totpBase32(secret),
		"otpauthURI": otpauthURI(u.Name, secret),
	})
}

// handleVaultChallenge 签发一次性挑战（分用途，防跨协议签名重用 S4）。
// POST /api/vault/challenge { purpose: "unlock"|"cert-bind" } →
//
//	{ challengeId, challenge }；挑战 base64（无填充），TTL 5 分钟，首试即作废。
func (d *Daemon) handleVaultChallenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Purpose string `json:"purpose"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Purpose != "unlock" && req.Purpose != "cert-bind" {
		writeError(w, http.StatusBadRequest, "purpose 须为 unlock 或 cert-bind")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	// 池上限：先回收过期项，仍满则拒绝（防刷爆内存）
	if len(v.challenges) >= maxChallenges {
		now := time.Now()
		for k, c := range v.challenges {
			if now.After(c.expires) {
				delete(v.challenges, k)
			}
		}
		if len(v.challenges) >= maxChallenges {
			writeError(w, http.StatusTooManyRequests, "挑战池已满，请稍后再试")
			return
		}
	}
	chValue, err := randomChallenge()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "挑战生成失败")
		return
	}
	id, err := newSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "挑战生成失败")
		return
	}
	v.challenges[id] = &vaultChallenge{
		id:      id,
		purpose: req.Purpose,
		value:   chValue,
		expires: time.Now().Add(challengeTTL),
	}
	writeOK(w, map[string]any{"challengeId": id, "challenge": chValue})
}

// handleVaultCert 绑定证书（SPKI 指纹）。
// 授权：initToken / 解锁会话 / 恢复会话；签名消息 = "IRIX-VAULT-CERT-BIND:1:" + challenge。
// POST /api/vault/cert { certPem, challengeId, signature }
func (d *Daemon) handleVaultCert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CertPEM     string `json:"certPem"`
		ChallengeID string `json:"challengeId"`
		Signature   string `json:"signature"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	u, kind, err := v.vaultTarget(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	ch := v.challenges[req.ChallengeID]
	if ch == nil || ch.used || time.Now().After(ch.expires) || ch.purpose != "cert-bind" {
		writeError(w, http.StatusBadRequest, "挑战无效或已使用")
		return
	}
	ch.used = true // 首试即作废
	pub, _, err := parsePublicKeyPEM([]byte(req.CertPEM))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !keySizeOK(pub) {
		writeError(w, http.StatusBadRequest, "证书密钥强度不足（RSA ≥2048 或 ECDSA ≥P-256）")
		return
	}
	if err := verifyChallengeSignature(pub, []byte(signPrefixCertBind+ch.value), req.Signature); err != nil {
		writeError(w, http.StatusUnauthorized, "签名验证失败")
		return
	}
	u.CertFingerprint = certSPKIFingerprint(pub)
	u.CertPublicPEM = req.CertPEM
	if kind == "init" {
		// onboarding 完成：作废 initToken（设计 §6.3）
		delete(v.initTokens, r.Header.Get("X-Vault-Token"))
	}
	if err := v.save(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.auditLogf("vault.cert.bind user=%s fingerprint=%s", u.Name, u.CertFingerprint)
	writeOK(w, map[string]any{"fingerprint": u.CertFingerprint})
}

// handleVaultUnlock 三重认证解锁：挑战（一次性）→ TOTP → 密码 → 证书签名。
// forceExpire 且密码过期时须携带 newPassword 同请求完成解锁+改密（A3/D15）。
// POST /api/vault/unlock { user, password, totp, challengeId, signature, newPassword? }
func (d *Daemon) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User        string `json:"user"`
		Password    string `json:"password"`
		TOTP        string `json:"totp"`
		ChallengeID string `json:"challengeId"`
		Signature   string `json:"signature"`
		NewPassword string `json:"newPassword"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	released := false
	defer func() {
		if !released {
			v.mu.Unlock()
		}
	}()
	if !v.initialized {
		writeError(w, http.StatusBadRequest, "保险库未初始化")
		return
	}
	limiterKey := "unlock:" + req.User + ":" + clientIP(r)
	if v.vaultLimited(limiterKey) {
		writeError(w, http.StatusUnauthorized, "尝试次数过多，请稍后再试")
		return
	}
	// 1) 挑战：存在性/用途/有效期校验；首试即作废（无论成败）
	ch := v.challenges[req.ChallengeID]
	if ch == nil || ch.purpose != "unlock" || ch.used || time.Now().After(ch.expires) {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	ch.used = true
	u := v.users[req.User]
	if u == nil {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.unlock 失败 user=%s 失败点=user", req.User)
		authFailed(w)
		return
	}
	// 2) TOTP（含重放防护：窗口必须严格递增）
	secret, err := b64d(u.TOTPSecretB64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOTP 密钥损坏")
		return
	}
	now := time.Now()
	win := now.Unix() / totpPeriod
	if !u.TOTPBound || !totpVerify(secret, req.TOTP, now, 1, totpDigits, totpPeriod) || win <= u.lastTOTPWindow {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.unlock 失败 user=%s 失败点=totp", req.User)
		authFailed(w)
		return
	}
	// 3) 密码 → KEK → 解 masterKey 包裹（GCM 失败即密码错误）
	salt, err := b64d(u.KEKSaltB64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥盐损坏")
		return
	}
	kek, err := deriveKEK(req.Password, salt, v.pbkdf2Iterations, 32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥派生失败")
		return
	}
	wrap, err := b64d(u.MasterKeyWrapB64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹损坏")
		return
	}
	masterKey, err := gcmUnwrap(kek, wrap)
	if err != nil {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.unlock 失败 user=%s 失败点=password", req.User)
		authFailed(w)
		return
	}
	// 4) 证书签名（与登记 SPKI 指纹一致）
	pub, _, err := parsePublicKeyPEM([]byte(u.CertPublicPEM))
	if err != nil || pub == nil || certSPKIFingerprint(pub) != u.CertFingerprint {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.unlock 失败 user=%s 失败点=cert", req.User)
		authFailed(w)
		return
	}
	if err := verifyChallengeSignature(pub, []byte(signPrefixUnlock+ch.value), req.Signature); err != nil {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.unlock 失败 user=%s 失败点=signature", req.User)
		authFailed(w)
		return
	}

	// 密码过期策略（A3/D15）
	passwordExpired := v.passwordExpire > 0 && time.Since(u.PasswordChangedAt) >= v.passwordExpire
	rewrapped := false
	if passwordExpired && v.forceExpire {
		if req.NewPassword == "" {
			writeError(w, http.StatusUnauthorized, "密码已过期，请在解锁请求中携带 newPassword 设置新密码")
			return
		}
		if err := v.validateVaultPassword(req.NewPassword); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		newSalt, err := randomBytes(16)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥生成失败")
			return
		}
		newKEK, err := deriveKEK(req.NewPassword, newSalt, v.pbkdf2Iterations, 32)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥派生失败")
			return
		}
		newWrap, err := gcmWrap(newKEK, masterKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥包裹失败")
			return
		}
		u.KEKSaltB64 = b64e(newSalt)
		u.MasterKeyWrapB64 = b64e(newWrap)
		u.PasswordChangedAt = time.Now()
		passwordExpired = false
		rewrapped = true
	}

	// 会话建立 + masterKey 驻内存（loading 期间数据面短暂 403）
	u.lastTOTPWindow = win
	sessionToken, err := newSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "令牌生成失败")
		return
	}
	v.sessions[sessionToken] = &vaultSession{
		token:      sessionToken,
		user:       u.Name,
		certFP:     u.CertFingerprint,
		ip:         clientIP(r),
		lastActive: now,
		expiresAt:  now.Add(v.idleTimeout),
	}
	v.masterKey = masterKey
	v.loading = true
	if rewrapped {
		if err := v.save(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.auditLogf("vault.password.change user=%s 途径=unlock-force-expire", u.Name)
	}
	v.vaultReset(limiterKey)
	v.mu.Unlock()
	released = true

	// 阶段 2（M4）：加载存储层/实例列表/崩溃残留回收/启动迁移。
	// 释放 v.mu 后执行（store 操作自行 RLock）；失败回滚会话与密钥。
	if err := v.postUnlockInit(masterKey); err != nil {
		v.mu.Lock()
		delete(v.sessions, sessionToken)
		if v.masterKey != nil {
			zeroBytes(v.masterKey)
			v.masterKey = nil
		}
		v.loading = false
		v.mu.Unlock()
		v.store.zeroKeys()
		d.auditLogf("vault.unlock 失败 阶段2初始化: %v", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("解锁后初始化失败: %v", err))
		return
	}
	d.auditLogf("vault.unlock 成功 user=%s", u.Name)
	writeOK(w, map[string]any{
		"sessionToken":    sessionToken,
		"expiresIn":       int(v.idleTimeout.Seconds()),
		"passwordExpired": passwordExpired,
	})
}

// handleVaultLock 立即锁定：清零 masterKey、清空全部会话。
// POST /api/vault/lock（X-Vault-Token 头）
func (d *Daemon) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.sessionFor(r)
	if s == nil || s.recovery || !v.unlocked() {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	for k, sess := range v.sessions {
		if !sess.recovery {
			delete(v.sessions, k)
		}
	}
	if v.masterKey != nil {
		// 索引落盘后清零存储层密钥与主密钥（剩余信息保护）
		if err := v.store.flush(); err != nil {
			d.auditLogf("vault.store 锁定落盘失败: %v", err)
		}
		v.store.zeroKeys()
		zeroBytes(v.masterKey)
		v.masterKey = nil
	}
	v.loading = false
	d.auditLogf("vault.lock user=%s", s.user)
	writeOK(w, map[string]any{"locked": true})
}

// handleVaultPassword 修改密码（rewrap masterKey，不重加密数据）。
// 解锁会话：须携带 oldPassword 验证；恢复会话：无需旧密码。
// POST /api/vault/password { oldPassword?, newPassword }
func (d *Daemon) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	u, kind, err := v.vaultTarget(r)
	if err != nil || (kind != "session" && kind != "recovery") {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	if err := v.validateVaultPassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 取得 masterKey：解锁会话直接使用 v.masterKey；恢复会话用其副本
	var masterKey []byte
	if kind == "recovery" {
		s := v.sessionFor(r)
		if s == nil || s.masterKey == nil {
			writeError(w, http.StatusUnauthorized, "恢复会话无效")
			return
		}
		masterKey = s.masterKey
	} else {
		if !v.unlocked() {
			writeError(w, http.StatusUnauthorized, "未授权")
			return
		}
		masterKey = v.masterKey
	}
	if kind == "session" {
		// 验证旧密码（防会话持有者无密码改密）
		salt, err := b64d(u.KEKSaltB64)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥盐损坏")
			return
		}
		oldKEK, err := deriveKEK(req.OldPassword, salt, v.pbkdf2Iterations, 32)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥派生失败")
			return
		}
		oldWrap, err := b64d(u.MasterKeyWrapB64)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密钥包裹损坏")
			return
		}
		check, err := gcmUnwrap(oldKEK, oldWrap)
		if err != nil || !bytesEqual(check, masterKey) {
			writeError(w, http.StatusUnauthorized, "旧密码错误")
			return
		}
	}
	newSalt, err := randomBytes(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	newKEK, err := deriveKEK(req.NewPassword, newSalt, v.pbkdf2Iterations, 32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥派生失败")
		return
	}
	newWrap, err := gcmWrap(newKEK, masterKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹失败")
		return
	}
	u.KEKSaltB64 = b64e(newSalt)
	u.MasterKeyWrapB64 = b64e(newWrap)
	u.PasswordChangedAt = time.Now()
	// 作废该用户除当前会话外的全部会话（设计 §6.4）
	currentToken := r.Header.Get("X-Vault-Token")
	for k, s := range v.sessions {
		if s.user == u.Name && s.token != currentToken && !s.recovery {
			delete(v.sessions, k)
		}
	}
	if err := v.save(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.auditLogf("vault.password.change user=%s 途径=%s", u.Name, kind)
	writeOK(w, map[string]any{"changed": true})
}

// handleVaultRecovery 恢复令牌建立 5 分钟恢复会话（可改密/重绑 TOTP/换绑证书）。
// POST /api/vault/recovery { recoveryToken, user? }；user 缺省且仅一个用户时取该用户。
func (d *Daemon) handleVaultRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecoveryToken string `json:"recoveryToken"`
		User          string `json:"user"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.initialized || v.recovery == nil {
		writeError(w, http.StatusBadRequest, "保险库未初始化")
		return
	}
	ip := clientIP(r)
	limiterKey := "recovery:" + ip
	if v.vaultLimited(limiterKey) {
		writeError(w, http.StatusUnauthorized, "尝试次数过多，请稍后再试")
		return
	}
	// 恒定时间比较令牌哈希（复用配对码模式）
	sum := sha256.Sum256([]byte(req.RecoveryToken))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(v.recovery.Hash)) != 1 {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.recovery 失败 ip=%s", ip)
		authFailed(w)
		return
	}
	// 目标用户：显式指定；否则仅一个用户时取该用户
	var target *vaultUser
	if req.User != "" {
		target = v.users[req.User]
	} else if len(v.users) == 1 {
		for _, u := range v.users {
			target = u
		}
	}
	if target == nil {
		writeError(w, http.StatusBadRequest, "无法确定目标用户，请指定 user")
		return
	}
	// 解开 masterKey（恢复密钥包裹）
	key, err := base64.RawStdEncoding.DecodeString(req.RecoveryToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "恢复令牌格式无效")
		return
	}
	wrap, err := b64d(v.recovery.MasterKeyWrapB64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "恢复包裹损坏")
		return
	}
	masterKey, err := gcmUnwrap(key, wrap)
	if err != nil {
		v.vaultFail(limiterKey)
		d.auditLogf("vault.recovery 失败 ip=%s 原因=令牌包裹解开失败", ip)
		authFailed(w)
		return
	}
	sessionToken, err := newSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "令牌生成失败")
		return
	}
	now := time.Now()
	v.sessions[sessionToken] = &vaultSession{
		token:      sessionToken,
		user:       target.Name,
		certFP:     target.CertFingerprint,
		ip:         ip,
		recovery:   true,
		masterKey:  masterKey,
		lastActive: now,
		expiresAt:  now.Add(recoveryTTL),
	}
	v.vaultReset(limiterKey)
	d.auditLogf("vault.recovery 成功 user=%s", target.Name)
	writeOK(w, map[string]any{
		"sessionToken": sessionToken,
		"expiresIn":    int(recoveryTTL.Seconds()),
		"recovery":     true,
	})
}

// handleVaultUserAdd 新增用户（解锁会话）：创建用户并进入 onboarding
// （返回 initToken，随后 totp/verify + cert 绑定）。禁与现有用户重名。
// POST /api/vault/user/add { user, password }
func (d *Daemon) handleVaultUserAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.unlocked() {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	s := v.sessionFor(r)
	if s == nil || s.recovery {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" {
		writeError(w, http.StatusBadRequest, "用户名不能为空")
		return
	}
	if v.users[req.User] != nil {
		writeError(w, http.StatusBadRequest, "用户名已存在")
		return
	}
	if err := v.validateVaultPassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kekSalt, err := randomBytes(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	kek, err := deriveKEK(req.Password, kekSalt, v.pbkdf2Iterations, 32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥派生失败")
		return
	}
	wrap, err := gcmWrap(kek, v.masterKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥包裹失败")
		return
	}
	totpSecret, err := randomBytes(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密钥生成失败")
		return
	}
	now := time.Now()
	v.users[req.User] = &vaultUser{
		Name:              req.User,
		TOTPSecretB64:     b64e(totpSecret),
		KEKSaltB64:        b64e(kekSalt),
		MasterKeyWrapB64:  b64e(wrap),
		PasswordChangedAt: now,
		CreatedAt:         now,
	}
	if err := v.save(); err != nil {
		delete(v.users, req.User)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	initToken, err := newSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "令牌生成失败")
		return
	}
	v.initTokens[initToken] = &vaultInitToken{
		token:   initToken,
		user:    req.User,
		expires: time.Now().Add(initTokenTTL),
	}
	d.auditLogf("vault.user.add user=%s 操作者=%s", req.User, s.user)
	writeOK(w, map[string]any{
		"initToken":  initToken,
		"totpSecret": totpBase32(totpSecret),
		"otpauthURI": otpauthURI(req.User, totpSecret),
	})
}

// handleVaultUserRemove 删除用户（解锁会话）。禁止删除最后一个用户；
// 删除当前会话用户 → 当前会话立即失效（若因此无解锁会话，保险库自动锁定）。
// POST /api/vault/user/remove { user }
func (d *Daemon) handleVaultUserRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.unlocked() {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	s := v.sessionFor(r)
	if s == nil || s.recovery {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	if v.users[req.User] == nil {
		writeError(w, http.StatusBadRequest, "用户不存在")
		return
	}
	if len(v.users) <= 1 {
		writeError(w, http.StatusBadRequest, "禁止删除最后一个用户")
		return
	}
	delete(v.users, req.User)
	// 吊销该用户全部会话（含当前会话）
	currentToken := r.Header.Get("X-Vault-Token")
	removedCurrent := false
	for k, sess := range v.sessions {
		if sess.user == req.User && !sess.recovery {
			delete(v.sessions, k)
			if k == currentToken {
				removedCurrent = true
			}
		}
	}
	// 若无剩余解锁会话 → 清零 masterKey（自动锁定）
	unlockedAny := false
	for _, sess := range v.sessions {
		if !sess.recovery {
			unlockedAny = true
			break
		}
	}
	if !unlockedAny && v.masterKey != nil {
		zeroBytes(v.masterKey)
		v.masterKey = nil
	}
	if err := v.save(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.auditLogf("vault.user.remove user=%s 操作者=%s 当前会话失效=%v", req.User, s.user, removedCurrent)
	writeOK(w, map[string]any{"removed": true, "currentSessionInvalidated": removedCurrent})
}

// handleVaultMigrate 启动/续跑迁移（幂等；后台执行）。
// POST /api/vault/migrate（需解锁会话）
func (d *Daemon) handleVaultMigrate(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.Lock()
	ok := v.unlocked() && v.sessionFor(r) != nil && !v.sessionFor(r).recovery
	v.mu.Unlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	if v.migrationPhase() == 3 {
		writeOK(w, map[string]any{"started": false, "completed": true})
		return
	}
	v.startMigration()
	writeOK(w, map[string]any{"started": true})
}

// handleVaultMigrateStatus 迁移进度。GET /api/vault/migrate/status（需解锁会话）
func (d *Daemon) handleVaultMigrateStatus(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.RLock()
	ok := v.unlocked() && v.sessionFor(r) != nil && !v.sessionFor(r).recovery
	v.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	writeOK(w, v.migrationProgress())
}

// handleVaultBackup 导出加密备份包（zip：vault.json + 索引 + 全部对象，
// 不含任何密钥材料，docs/vault-design.md §8.10）。POST /api/vault/backup（需解锁会话）
func (d *Daemon) handleVaultBackup(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.RLock()
	ok := v.unlocked() && v.sessionFor(r) != nil && !v.sessionFor(r).recovery
	v.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	if err := v.store.flush(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("索引落盘失败: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=irix-vault-backup-%d.zip", time.Now().Unix()))
	zw := zip.NewWriter(w)
	addZipFile := func(name, path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fw, err := zw.Create("vault/" + name)
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := addZipFile("vault.json", v.file); err != nil {
		zw.Close()
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("备份打包失败: %v", err))
		return
	}
	if _, err := os.Stat(v.store.indexFile); err == nil {
		if err := addZipFile("index.json.enc", v.store.indexFile); err != nil {
			zw.Close()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("备份打包失败: %v", err))
			return
		}
	}
	entries, err := os.ReadDir(v.store.objectsDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
				continue
			}
			if err := addZipFile("objects/"+e.Name(), filepath.Join(v.store.objectsDir, e.Name())); err != nil {
				zw.Close()
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("备份打包失败: %v", err))
				return
			}
		}
	}
	zw.Close()
	d.auditLogf("vault.backup 导出加密备份包")
}

// handleVaultUsers 用户列表（解锁会话；不含任何秘密材料）。
// GET /api/vault/users
func (d *Daemon) handleVaultUsers(w http.ResponseWriter, r *http.Request) {
	if !d.vaultOK(w) {
		return
	}
	v := d.vault
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.unlocked() {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	s := v.sessionFor(r)
	if s == nil || s.recovery {
		writeError(w, http.StatusUnauthorized, "未授权")
		return
	}
	names := make([]string, 0, len(v.users))
	for name := range v.users {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]map[string]any, 0, len(names))
	for _, name := range names {
		u := v.users[name]
		list = append(list, map[string]any{
			"name":            u.Name,
			"totpBound":       u.TOTPBound,
			"certFingerprint": u.CertFingerprint,
			"createdAt":       u.CreatedAt.Format(time.RFC3339),
		})
	}
	writeOK(w, map[string]any{"users": list})
}

// bytesEqual 恒定时间字节比较（改密旧密码校验用）。
func bytesEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
