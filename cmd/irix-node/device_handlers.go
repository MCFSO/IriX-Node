// 设备绑定管理接口：挑战申请、设备注册/列出/撤销、账户设备策略。
//
// 端点（公开/自身/管理员）：
//   - POST   /api/auth/device/challenge   申请一次性挑战（公开，登录与注册共用）
//   - GET    /api/auth/devices            列出自己的已绑定设备
//   - POST   /api/auth/devices            注册设备（需登录 + 密码 + 设备私钥签名）
//   - POST   /api/auth/devices/revoke     撤销自己的设备（需密码）
//   - GET    /api/accounts/devices        管理员查看任意账户设备
//   - DELETE /api/accounts/devices        管理员删除任意账户设备
//   - PUT    /api/accounts/device-policy  管理员设置账户设备策略（optional|required）
//
// 设备绑定默认 optional，旧客户端不受影响；把账户设为 required 后，
// 没有已绑定设备签名的登录一律 403。协议细节见 docs/device-binding.md。

package main

import (
	"errors"
	"net/http"
	"strings"
)

// registerDeviceRoutes 注册设备绑定相关路由。
func (d *Daemon) registerDeviceRoutes(mux *http.ServeMux) {
	// 挑战申请是公开端点（登录前必须能拿到 nonce），与 /api/auth/login 同级。
	mux.HandleFunc("POST /api/auth/device/challenge", d.handleDeviceChallenge)

	perm("账户", "GET /api/auth/devices", "查看已绑定设备")
	mux.HandleFunc("GET /api/auth/devices", d.auth(d.handleDeviceList))
	perm("账户", "POST /api/auth/devices", "注册绑定设备")
	mux.HandleFunc("POST /api/auth/devices", d.auth(d.handleDeviceRegister))
	perm("账户", "POST /api/auth/devices/revoke", "撤销绑定设备")
	mux.HandleFunc("POST /api/auth/devices/revoke", d.auth(d.handleDeviceRevoke))

	perm("账户", "GET /api/accounts/devices", "查看指定账户设备")
	mux.HandleFunc("GET /api/accounts/devices", d.auth(d.handleAccountDevicesList))
	perm("账户", "DELETE /api/accounts/devices", "删除指定账户设备")
	mux.HandleFunc("DELETE /api/accounts/devices", d.auth(d.handleAccountDeviceDelete))
	perm("账户", "PUT /api/accounts/device-policy", "设置账户设备绑定策略")
	mux.HandleFunc("PUT /api/accounts/device-policy", d.auth(d.handleAccountDevicePolicy))
}

// handleDeviceChallenge 发放一次性挑战。
// POST /api/auth/device/challenge body: {username, deviceId, purpose}
func (d *Daemon) handleDeviceChallenge(w http.ResponseWriter, r *http.Request) {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	var body deviceChallengeRequest
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	body.DeviceID = strings.ToLower(strings.TrimSpace(body.DeviceID))
	body.Purpose = strings.ToLower(strings.TrimSpace(body.Purpose))
	if body.Purpose == "" {
		body.Purpose = devicePurposeLogin
	}
	nonce, expires, err := d.accounts.issueDeviceChallenge(body.Username, body.DeviceID, body.Purpose)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, deviceChallengeResponse{
		Nonce:          nonce,
		ExpiresAt:      expires.UnixMilli(),
		Purpose:        body.Purpose,
		TLSFingerprint: d.tlsFingerprintValue(),
	})
}

// handleDeviceList 列出当前账户的已绑定设备与策略。
// GET /api/auth/devices
func (d *Daemon) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	id := identityOf(r)
	if id == nil {
		writeError(w, http.StatusForbidden, "未认证")
		return
	}
	d.writeDeviceList(w, id.username)
}

// writeDeviceList 输出某账户的设备列表与策略（自身/管理员共用）。
func (d *Daemon) writeDeviceList(w http.ResponseWriter, username string) {
	devices, err := d.accounts.listDevices(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取设备列表失败: "+err.Error())
		return
	}
	acc, err := d.accounts.getAccount(username)
	if err != nil || acc == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	writeOK(w, map[string]any{
		"username":     username,
		"devicePolicy": acc.DevicePolicy,
		"maxDevices":   accountMaxDevices,
		"devices":      devices,
	})
}

// handleDeviceRegister 注册绑定设备。
// POST /api/auth/devices body: 见 deviceRegisterRequest（必须含密码）。
func (d *Daemon) handleDeviceRegister(w http.ResponseWriter, r *http.Request) {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	id := identityOf(r)
	if id == nil {
		writeError(w, http.StatusForbidden, "未认证")
		return
	}
	var body deviceRegisterRequest
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Password) == "" {
		writeError(w, http.StatusBadRequest, "注册设备必须提供账户密码")
		return
	}
	if !d.checkDevicePassword(w, r, id.username, body.Password) {
		return
	}
	dev, err := d.registerDevice(id.username, &body)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "已绑定") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	d.auditLogf("设备 | 注册 | 账户 %s | 设备 %s（%s，%s）",
		id.username, dev.DeviceID, dev.Name, dev.KeyProtection)
	writeOK(w, dev)
}

// handleDeviceRevoke 撤销当前账户的一台设备（需密码）。
// POST /api/auth/devices/revoke body: {deviceId, password}
func (d *Daemon) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	id := identityOf(r)
	if id == nil {
		writeError(w, http.StatusForbidden, "未认证")
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Password string `json:"password"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	body.DeviceID = strings.ToLower(strings.TrimSpace(body.DeviceID))
	if !validDeviceID(body.DeviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 无效")
		return
	}
	if !d.checkDevicePassword(w, r, id.username, body.Password) {
		return
	}
	acc, err := d.accounts.getAccount(id.username)
	if err != nil || acc == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	if acc.DevicePolicy == accountDevicePolicyRequired {
		n, err := d.accounts.countDevices(id.username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取设备数量失败: "+err.Error())
			return
		}
		if n <= 1 {
			writeError(w, http.StatusForbidden, "设备绑定已开启，不能撤销最后一台设备；请先由管理员关闭设备绑定")
			return
		}
	}
	removed, err := d.accounts.deleteDevice(id.username, body.DeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "撤销设备失败: "+err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "设备不存在")
		return
	}
	// 撤销该账户全部会话：被移除设备的会话不能继续访问。
	d.accounts.revokeUserSessions(id.username, "")
	d.auditLogf("设备 | 撤销 | 账户 %s | 设备 %s", id.username, body.DeviceID)
	writeOK(w, map[string]any{"removed": true, "deviceId": body.DeviceID})
}

// handleAccountDevicesList 管理员查看指定账户设备。
// GET /api/accounts/devices?username=
func (d *Daemon) handleAccountDevicesList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	username := strings.TrimSpace(queryParam(r, "username"))
	if username == "" {
		writeError(w, http.StatusBadRequest, "缺少 username 参数")
		return
	}
	d.writeDeviceList(w, username)
}

// handleAccountDeviceDelete 管理员删除指定账户设备。
// DELETE /api/accounts/devices?username=&deviceId=
func (d *Daemon) handleAccountDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	username := strings.TrimSpace(queryParam(r, "username"))
	deviceID := strings.ToLower(strings.TrimSpace(queryParam(r, "deviceId")))
	if username == "" || !validDeviceID(deviceID) {
		writeError(w, http.StatusBadRequest, "需要 username 与合法 deviceId 参数")
		return
	}
	acc, err := d.accounts.getAccount(username)
	if err != nil || acc == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	count, err := d.accounts.countDevices(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取设备数量失败: "+err.Error())
		return
	}
	removed, err := d.accounts.deleteDevice(username, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除设备失败: "+err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "设备不存在")
		return
	}
	// 删除最后一台设备且策略为 required 时自动降级为 optional，避免账户永久锁死。
	policyReset := false
	if acc.DevicePolicy == accountDevicePolicyRequired && count <= 1 {
		if err := d.accounts.setDevicePolicy(username, accountDevicePolicyOptional); err != nil {
			writeError(w, http.StatusInternalServerError, "设备已删除但策略降级失败，请手动调用 device-policy 设为 optional: "+err.Error())
			return
		}
		policyReset = true
	}
	d.accounts.revokeUserSessions(username, "")
	d.auditLogf("设备 | 管理员删除 | 账户 %s | 设备 %s | 策略降级: %v", username, deviceID, policyReset)
	writeOK(w, map[string]any{"removed": true, "deviceId": deviceID, "policyReset": policyReset})
}

// handleAccountDevicePolicy 管理员设置账户设备策略。
// PUT /api/accounts/device-policy body: {username, policy}
func (d *Daemon) handleAccountDevicePolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return
	}
	var body struct {
		Username string `json:"username"`
		Policy   string `json:"policy"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	policy, err := normalizeDevicePolicy(body.Policy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Username == "" {
		writeError(w, http.StatusBadRequest, "缺少 username 参数")
		return
	}
	acc, err := d.accounts.getAccount(body.Username)
	if err != nil || acc == nil {
		writeError(w, http.StatusNotFound, "账户不存在")
		return
	}
	if policy == accountDevicePolicyRequired {
		// required 的安全前提是客户端能固定校验服务端身份；节点未启用 TLS 时
		// 中间人可终止明文连接并转发设备签名，设备绑定无法阻止 MITM。
		if d.tlsFingerprintValue() == "" {
			writeError(w, http.StatusBadRequest, "开启设备绑定 required 必须先启用 TLS（tls-mode=auto 或 manual）")
			return
		}
		n, err := d.accounts.countDevices(body.Username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取设备数量失败: "+err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusBadRequest, "该账户尚未绑定设备，不能开启 required（会立即锁死登录）")
			return
		}
	}
	if err := d.accounts.setDevicePolicy(body.Username, policy); err != nil {
		writeError(w, http.StatusInternalServerError, "设置设备策略失败: "+err.Error())
		return
	}
	// 策略变化立即撤销旧会话：required 后不允许未绑定设备的旧 token 继续使用。
	d.accounts.revokeUserSessions(body.Username, "")
	d.auditLogf("设备 | 策略变更 | 账户 %s | %s → %s", body.Username, acc.DevicePolicy, policy)
	writeOK(w, map[string]any{"username": body.Username, "devicePolicy": policy})
}

// checkDevicePassword 设备管理操作的密码校验（复用登录限速，防在线爆破）。
func (d *Daemon) checkDevicePassword(w http.ResponseWriter, r *http.Request, username, password string) bool {
	if d.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账户系统未启用")
		return false
	}
	ip := clientIP(r)
	if d.accounts.loginBlocked(username, ip) {
		writeError(w, http.StatusTooManyRequests, "密码尝试次数过多，请稍后再试")
		return false
	}
	if _, ok := d.accounts.checkPassword(username, password); !ok {
		d.accounts.recordLoginFailure(username, ip)
		writeError(w, http.StatusForbidden, "密码错误")
		return false
	}
	d.accounts.clearLoginFailures(username, ip)
	return true
}

// errDevicePolicyRequired 账户开启 required 但登录请求未携带设备证明。
var errDevicePolicyRequired = errors.New("该账户已启用设备绑定，必须使用已绑定设备登录")
