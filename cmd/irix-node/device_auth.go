// 设备绑定协议（服务端）：Ed25519 / ECDSA P-256 设备公钥、一次性挑战、
// 登录签名校验，以及已绑定会话的高性能逐请求 HMAC-SHA256 证明。
//
// 设计目标：
//   - 私钥只存在客户端（优先 TPM 2.0 / 系统密钥库），服务端只存公钥；
//   - 登录必须用私钥对服务端 nonce 签名；账户可配置 required（无设备不能登录）；
//   - 登录后会话绑定 deviceId，每个请求用 HMAC-SHA256 + 单调序号证明，
//     不依赖 TPM、不做非对称签名 —— 单次校验约 1µs，适合高并发；
//   - 时间戳 ±120 秒 + 每会话 1024 位滑动序号窗口防重放，无全局 nonce 表增长。
//
// 详细字节级协议与客户端交接说明见 docs/device-binding.md。

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"irix/node/internal/timesys"
)

// 设备协议常量。
const (
	deviceChallengeTTL      = 60 * time.Second  // 挑战有效期
	deviceChallengeMax      = 50000             // 挑战表上限（防内存放大）
	deviceProofSkew         = 120 * time.Second // 请求时间戳允许偏差
	deviceProofWindow       = 1024              // 每会话滑动序号窗口
	deviceProofWindowMax    = 100000            // 序号窗口表上限（约 15MB）
	deviceProofKeyBytes     = 32                // 会话证明密钥长度
	deviceKeyTypeEd25519    = "ed25519"
	deviceKeyTypeP256       = "p256"
	devicePurposeLogin      = "login"
	devicePurposeRegister   = "register"
	devicePublicKeyMaxLen   = 128 // base64 文本长度上限（P-256 65 字节约 88 字符）
	deviceSignatureMaxLen   = 256
	deviceProofKeyTextMax   = 96
	deviceChallengeNonceLen = 32
)

// deviceChallenge 一次性的设备登录/注册挑战。
type deviceChallenge struct {
	Username string
	DeviceID string
	Purpose  string
	Expires  time.Time
}

// deviceLoginProof 登录请求中的设备证明（嵌套在 /api/auth/login 的 device 字段）。
type deviceLoginProof struct {
	DeviceID  string `json:"deviceId"`
	Nonce     string `json:"nonce"`
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
	ProofKey  string `json:"proofKey"`
}

// deviceRegisterRequest 设备注册请求（POST /api/auth/devices）。
type deviceRegisterRequest struct {
	DeviceID      string `json:"deviceId"`
	PublicKey     string `json:"publicKey"`
	KeyType       string `json:"keyType"`
	KeyProtection string `json:"keyProtection"`
	Descriptor    string `json:"descriptor"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
	Timestamp     int64  `json:"timestamp"`
	Signature     string `json:"signature"`
	Password      string `json:"password"`
}

// deviceChallengeRequest 挑战申请（公开端点）。
type deviceChallengeRequest struct {
	Username string `json:"username"`
	DeviceID string `json:"deviceId"`
	Purpose  string `json:"purpose"` // login | register
}

// deviceChallengeResponse 挑战响应。
type deviceChallengeResponse struct {
	Nonce          string `json:"nonce"`
	ExpiresAt      int64  `json:"expiresAt"` // Unix 毫秒
	Purpose        string `json:"purpose"`
	TLSFingerprint string `json:"tlsFingerprint,omitempty"` // 服务端证书 SHA-256，客户端应固定校验
}

// proofWindow 单会话的单调序号滑动窗口（bit 0 = 最新序号，bit i = 最新-i）。
type proofWindow struct {
	username string
	last     uint64
	bits     [deviceProofWindow / 64]uint64
	lastUsed time.Time
}

// accept 接受一个序号；重放/过旧序号返回 false。调用方需持有 proofMu。
func (w *proofWindow) accept(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if w.last == 0 {
		w.last = seq
		w.bits = [deviceProofWindow / 64]uint64{}
		w.bits[0] = 1
		return true
	}
	if seq > w.last {
		shift := seq - w.last
		if shift >= deviceProofWindow {
			w.bits = [deviceProofWindow / 64]uint64{}
		} else {
			w.shift(int(shift))
		}
		w.last = seq
		w.bits[0] |= 1
		return true
	}
	diff := w.last - seq
	if diff >= deviceProofWindow {
		return false
	}
	idx := int(diff)
	mask := uint64(1) << uint(idx%64)
	if w.bits[idx/64]&mask != 0 {
		return false
	}
	w.bits[idx/64] |= mask
	return true
}

// shift 把窗口内所有位向「更旧」方向移动 n 位（n < deviceProofWindow）。
func (w *proofWindow) shift(n int) {
	if n <= 0 {
		return
	}
	var next [deviceProofWindow / 64]uint64
	wordShift := n / 64
	bitShift := uint(n % 64)
	for i := 0; i < len(next); i++ {
		src := i - wordShift
		if src < 0 {
			continue
		}
		next[i] |= w.bits[src] << bitShift
		if bitShift > 0 && src-1 >= 0 {
			next[i] |= w.bits[src-1] >> (64 - bitShift)
		}
	}
	w.bits = next
}

// deviceProofTranscript 逐请求证明的规范串（客户端必须逐字节一致）。
func deviceProofTranscript(method, requestURI, timestamp, seq string) []byte {
	return []byte("irix-proof-v1\n" + method + "\n" + requestURI + "\n" + timestamp + "\n" + seq)
}

// deviceLoginTranscript 登录签名规范串。tlsFingerprint 来自挑战响应。
func deviceLoginTranscript(username string, p *deviceLoginProof, tlsFingerprint string) []byte {
	return []byte("irix-device-login-v1\n" + username + "\n" + p.DeviceID + "\n" +
		p.Nonce + "\n" + strconv.FormatInt(p.Timestamp, 10) + "\n" + p.ProofKey + "\n" + tlsFingerprint)
}

// deviceRegisterTranscript 注册签名规范串。descriptorHash 为描述 JSON 的 SHA-256 hex。
// keyType/keyProtection 使用请求原文字符串（客户端签它发送的内容，服务端只在
// 校验/入库时归一化），避免大小写归一化导致签名不一致。
func deviceRegisterTranscript(username string, req *deviceRegisterRequest, deviceID, descriptorHash, tlsFingerprint string) []byte {
	return []byte("irix-device-register-v1\n" + username + "\n" + deviceID + "\n" +
		req.PublicKey + "\n" + req.KeyType + "\n" + req.KeyProtection + "\n" +
		descriptorHash + "\n" + strconv.FormatInt(req.Timestamp, 10) + "\n" + req.Nonce + "\n" + tlsFingerprint)
}

// decodeDeviceBase64 解码设备协议的 base64 文本：优先 raw-url（无填充），
// 同时兼容带填充/标准字母表，方便各语言客户端。
func decodeDeviceBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("空 base64")
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// parseDevicePublicKey 解析设备公钥，返回 crypto.PublicKey 与原始字节。
func parseDevicePublicKey(keyType, publicKeyB64 string) (crypto.PublicKey, []byte, error) {
	raw, err := decodeDeviceBase64(publicKeyB64)
	if err != nil {
		return nil, nil, fmt.Errorf("公钥 base64 无效: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(keyType)) {
	case deviceKeyTypeEd25519:
		if len(raw) != ed25519.PublicKeySize {
			return nil, nil, fmt.Errorf("Ed25519 公钥长度无效: %d", len(raw))
		}
		return ed25519.PublicKey(raw), raw, nil
	case deviceKeyTypeP256:
		if pub, ok := decodeP256Point(raw); ok {
			return pub, raw, nil
		}
		return nil, nil, errors.New("P-256 公钥格式无效（需 65 字节未压缩点）")
	default:
		return nil, nil, fmt.Errorf("不支持的设备密钥类型: %q", keyType)
	}
}

// decodeP256Point 解析 P-256 公钥点（接受未压缩 65 字节与压缩 33 字节）。
func decodeP256Point(raw []byte) (*ecdsa.PublicKey, bool) {
	if len(raw) != 65 && len(raw) != 33 {
		return nil, false
	}
	var (
		x, y *big.Int
	)
	if len(raw) == 65 {
		x, y = elliptic.Unmarshal(elliptic.P256(), raw)
	} else {
		x, y = elliptic.UnmarshalCompressed(elliptic.P256(), raw)
	}
	if x == nil || y == nil {
		return nil, false
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, true
}

// deviceIDFromPublicKey 设备 ID = SHA-256(公钥原始字节) 前 16 字节的 hex（32 字符）。
func deviceIDFromPublicKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// validDeviceID 校验设备 ID 形态（32 位小写 hex）。
func validDeviceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// verifyDeviceSignature 用设备公钥验证签名（Ed25519 原始 64 字节；
// ECDSA 接受 raw r||s 64 字节或 ASN.1 DER）。
func verifyDeviceSignature(dev *accountDevice, publicKeyB64 string, transcript []byte, signatureB64 string) error {
	pub, _, err := parseDevicePublicKey(dev.KeyType, publicKeyB64)
	if err != nil {
		return err
	}
	sig, err := decodeDeviceBase64(signatureB64)
	if err != nil {
		return fmt.Errorf("签名 base64 无效: %w", err)
	}
	switch key := pub.(type) {
	case ed25519.PublicKey:
		if len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, transcript, sig) {
			return errors.New("Ed25519 签名无效")
		}
		return nil
	case *ecdsa.PublicKey:
		digest := sha256.Sum256(transcript)
		if len(sig) == 64 {
			r := new(big.Int).SetBytes(sig[:32])
			s := new(big.Int).SetBytes(sig[32:])
			if ecdsa.Verify(key, digest[:], r, s) {
				return nil
			}
			return errors.New("ECDSA 签名无效")
		}
		if ecdsa.VerifyASN1(key, digest[:], sig) {
			return nil
		}
		return errors.New("ECDSA 签名无效")
	default:
		return errors.New("不支持的设备公钥类型")
	}
}

// issueDeviceChallenge 生成一次性挑战（登录/注册共用）。挑战只保存在本节点内存，
// 多节点共享账户库时客户端应保持会话粘性或重试。
func (s *accountSystem) issueDeviceChallenge(username, deviceID, purpose string) (string, time.Time, error) {
	if purpose != devicePurposeLogin && purpose != devicePurposeRegister {
		return "", time.Time{}, errors.New("purpose 无效（须为 login 或 register）")
	}
	if !validDeviceID(deviceID) {
		return "", time.Time{}, errors.New("deviceId 无效")
	}
	if strings.TrimSpace(username) == "" || len(username) > 64 {
		return "", time.Time{}, errors.New("username 无效")
	}
	buf := make([]byte, deviceChallengeNonceLen)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)
	now := timesys.Now()
	expires := now.Add(deviceChallengeTTL)

	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if s.deviceChallenges == nil {
		s.deviceChallenges = map[string]deviceChallenge{}
	}
	if len(s.deviceChallenges) >= deviceChallengeMax {
		for k, c := range s.deviceChallenges {
			if now.After(c.Expires) {
				delete(s.deviceChallenges, k)
			}
		}
		for len(s.deviceChallenges) >= deviceChallengeMax {
			for k := range s.deviceChallenges {
				delete(s.deviceChallenges, k)
				break
			}
		}
	}
	s.deviceChallenges[nonce] = deviceChallenge{
		Username: username,
		DeviceID: deviceID,
		Purpose:  purpose,
		Expires:  expires,
	}
	return nonce, expires, nil
}

// consumeDeviceChallenge 校验并消费挑战（无论签名是否有效都消费，防重放）。
func (s *accountSystem) consumeDeviceChallenge(nonce, username, deviceID, purpose string) bool {
	if nonce == "" {
		return false
	}
	now := timesys.Now()
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	c, ok := s.deviceChallenges[nonce]
	if !ok {
		return false
	}
	delete(s.deviceChallenges, nonce)
	if now.After(c.Expires) {
		return false
	}
	return c.Username == username && c.DeviceID == deviceID && c.Purpose == purpose
}

// verifySessionProof 校验已绑定会话的逐请求证明（HMAC-SHA256 + 时间戳 + 序号窗口）。
func (s *accountSystem) verifySessionProof(token, username, deviceID, proofKey string, r *http.Request) bool {
	if token == "" || deviceID == "" || proofKey == "" {
		return false
	}
	if r.Header.Get("X-Device-Id") != deviceID {
		return false
	}
	tsText := r.Header.Get("X-Session-Timestamp")
	seqText := r.Header.Get("X-Session-Seq")
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		return false
	}
	if delta := timesys.Now().Unix() - ts; delta > int64(deviceProofSkew/time.Second) || delta < -int64(deviceProofSkew/time.Second) {
		return false
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil || seq == 0 {
		return false
	}
	key, err := decodeDeviceBase64(proofKey)
	if err != nil || len(key) != deviceProofKeyBytes {
		return false
	}
	sig, err := decodeDeviceBase64(r.Header.Get("X-Session-Proof"))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(deviceProofTranscript(r.Method, r.URL.RequestURI(), tsText, seqText))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	return s.acceptProofSeq(token, username, seq)
}

// acceptProofSeq 在会话的滑动窗口中接受序号（O(1)，每会话约 150 字节）。
func (s *accountSystem) acceptProofSeq(token, username string, seq uint64) bool {
	now := timesys.Now()
	s.proofMu.Lock()
	defer s.proofMu.Unlock()
	if s.proofWindows == nil {
		s.proofWindows = map[string]*proofWindow{}
	}
	w := s.proofWindows[token]
	if w == nil {
		if len(s.proofWindows) >= deviceProofWindowMax {
			for k := range s.proofWindows {
				delete(s.proofWindows, k)
				break
			}
		}
		w = &proofWindow{username: username}
		s.proofWindows[token] = w
	}
	w.lastUsed = now
	return w.accept(seq)
}

// dropProofWindow 会话登出/撤销时清理序号窗口。
func (s *accountSystem) dropProofWindow(token string) {
	if token == "" {
		return
	}
	s.proofMu.Lock()
	delete(s.proofWindows, token)
	s.proofMu.Unlock()
}

// dropProofWindowsForUser 撤销某账户全部会话时清理对应序号窗口。
func (s *accountSystem) dropProofWindowsForUser(username, keepToken string) {
	s.proofMu.Lock()
	for token, w := range s.proofWindows {
		if w.username == username && token != keepToken {
			delete(s.proofWindows, token)
		}
	}
	s.proofMu.Unlock()
}

// decodeProofKey 校验会话证明密钥（32 字节 base64url）。
func decodeProofKey(proofKey string) ([]byte, error) {
	if len(proofKey) == 0 || len(proofKey) > deviceProofKeyTextMax {
		return nil, errors.New("proofKey 长度无效")
	}
	key, err := decodeDeviceBase64(proofKey)
	if err != nil {
		return nil, fmt.Errorf("proofKey base64 无效: %w", err)
	}
	if len(key) != deviceProofKeyBytes {
		return nil, fmt.Errorf("proofKey 长度无效: %d", len(key))
	}
	return key, nil
}

// verifyDeviceLogin 校验登录设备证明（挑战 → 设备 → 签名 → 时间戳）。
func (d *Daemon) verifyDeviceLogin(username string, p *deviceLoginProof) (*accountDevice, error) {
	if d.accounts == nil {
		return nil, errors.New("账户系统未启用")
	}
	if p == nil {
		return nil, errors.New("缺少设备证明")
	}
	if len(p.DeviceID) != 32 || !validDeviceID(p.DeviceID) {
		return nil, errors.New("deviceId 无效")
	}
	if p.Nonce == "" || len(p.Nonce) > 64 {
		return nil, errors.New("nonce 无效")
	}
	if len(p.Signature) > deviceSignatureMaxLen {
		return nil, errors.New("签名过长")
	}
	if _, err := decodeProofKey(p.ProofKey); err != nil {
		return nil, err
	}
	if delta := timesys.Now().Unix() - p.Timestamp; delta > int64(deviceProofSkew/time.Second) || delta < -int64(deviceProofSkew/time.Second) {
		return nil, errors.New("时间戳超出允许范围")
	}
	// 先消费挑战：即使签名错误也不能重复使用同一 nonce。
	if !d.accounts.consumeDeviceChallenge(p.Nonce, username, p.DeviceID, devicePurposeLogin) {
		return nil, errors.New("挑战已过期或已使用")
	}
	dev, err := d.accounts.getDevice(username, p.DeviceID)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		return nil, errors.New("设备未绑定")
	}
	if err := verifyDeviceSignature(dev, dev.PublicKey, deviceLoginTranscript(username, p, d.tlsFingerprintValue()), p.Signature); err != nil {
		return nil, err
	}
	d.accounts.touchDevice(dev.DeviceID)
	return dev, nil
}

// registerDevice 校验并保存设备公钥（调用方已确认会话身份与密码）。
func (d *Daemon) registerDevice(username string, req *deviceRegisterRequest) (*accountDevice, error) {
	if d.accounts == nil {
		return nil, errors.New("账户系统未启用")
	}
	if req == nil {
		return nil, errors.New("缺少注册参数")
	}
	keyType := strings.ToLower(strings.TrimSpace(req.KeyType))
	if keyType != deviceKeyTypeEd25519 && keyType != deviceKeyTypeP256 {
		return nil, errors.New("keyType 无效（须为 ed25519 或 p256）")
	}
	if len(req.PublicKey) > devicePublicKeyMaxLen {
		return nil, errors.New("公钥过长")
	}
	_, raw, err := parseDevicePublicKey(keyType, req.PublicKey)
	if err != nil {
		return nil, err
	}
	deviceID := deviceIDFromPublicKey(raw)
	if !validDeviceID(req.DeviceID) || req.DeviceID != deviceID {
		return nil, errors.New("deviceId 与公钥不匹配")
	}
	keyProtection := strings.ToLower(strings.TrimSpace(req.KeyProtection))
	if keyProtection == "" {
		keyProtection = "file"
	}
	if len(keyProtection) > 32 {
		return nil, errors.New("keyProtection 过长")
	}
	for i := 0; i < len(keyProtection); i++ {
		c := keyProtection[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return nil, errors.New("keyProtection 只允许小写字母/数字/-/_")
		}
	}
	if req.Descriptor == "" || len(req.Descriptor) > accountDeviceDescriptorMax {
		return nil, fmt.Errorf("descriptor 长度无效（1-%d 字节）", accountDeviceDescriptorMax)
	}
	if !json.Valid([]byte(req.Descriptor)) {
		return nil, errors.New("descriptor 不是合法 JSON")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "未命名设备"
	}
	if len(name) > accountDeviceNameMax {
		name = name[:accountDeviceNameMax]
	}
	if delta := timesys.Now().Unix() - req.Timestamp; delta > int64(deviceProofSkew/time.Second) || delta < -int64(deviceProofSkew/time.Second) {
		return nil, errors.New("时间戳超出允许范围")
	}
	if len(req.Signature) > deviceSignatureMaxLen {
		return nil, errors.New("签名过长")
	}
	// 挑战一次性消费，绑定 username + deviceId + purpose。
	if !d.accounts.consumeDeviceChallenge(req.Nonce, username, deviceID, devicePurposeRegister) {
		return nil, errors.New("挑战已过期或已使用")
	}
	descriptorHash := sha256.Sum256([]byte(req.Descriptor))
	if err := verifyDeviceSignature(&accountDevice{KeyType: keyType}, req.PublicKey,
		deviceRegisterTranscript(username, req, deviceID, hex.EncodeToString(descriptorHash[:]), d.tlsFingerprintValue()), req.Signature); err != nil {
		return nil, err
	}
	n, err := d.accounts.countDevices(username)
	if err != nil {
		return nil, err
	}
	if n >= accountMaxDevices {
		return nil, fmt.Errorf("单账户最多绑定 %d 台设备", accountMaxDevices)
	}
	dev := &accountDevice{
		DeviceID:      deviceID,
		Username:      username,
		PublicKey:     base64.RawURLEncoding.EncodeToString(raw),
		KeyType:       keyType,
		KeyProtection: keyProtection,
		Descriptor:    req.Descriptor,
		Name:          name,
	}
	if err := d.accounts.createDevice(dev); err != nil {
		if isDuplicateDeviceErr(err) {
			return nil, errors.New("该设备已绑定")
		}
		return nil, err
	}
	return dev, nil
}
