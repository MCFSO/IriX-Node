// vault_crypto.go — Vault 密码学内核（M2）。
//
// 提供：TOTP（RFC 6238，HMAC-SHA1）、PBKDF2 密钥派生（crypto/pbkdf2，
// Go 1.24 标准库）、AES-256-GCM 信封包裹（wrapBlob = nonce‖ct‖tag，
// 见 docs/vault-design.md §5.2）、挑战签名验证（RSA PKCS#1 v1.5 /
// ECDSA ASN.1 DER）、证书公钥解析与 SPKI 指纹。
// 全部基于 Go 标准库，零第三方依赖。

package main

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// 密码学常量（docs/vault-design.md §5.2/§5.3）。
const (
	vaultGCMNonceLen = 12 // AES-GCM 随机 nonce 长度
	vaultGCMTagLen   = 16 // AES-GCM 认证标签长度

	// defaultPBKDF2Iterations 密码派生 KEK 的默认 PBKDF2-HMAC-SHA256 迭代次数
	// （OWASP 当前建议；服务端解锁为一次性成本，可配置调高）。
	defaultPBKDF2Iterations = 600000

	// totpDigits / totpPeriod TOTP 默认参数：6 位验证码、30 秒周期。
	totpDigits = 6
	totpPeriod = 30
)

// 挑战签名消息前缀（S4：防同一证书签名跨协议重用，用途不同前缀不同）。
const (
	signPrefixUnlock   = "IRIX-VAULT-UNLOCK:1:"
	signPrefixCertBind = "IRIX-VAULT-CERT-BIND:1:"
)

// randomBytes 返回 n 字节密码学安全随机数（crypto/rand）。
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// zeroBytes 尽力清零敏感内存。Go 无法保证物理擦除（GC 可能复制、swap 可能
// 换出），此为 best-effort（docs/vault-design.md §7.2 剩余信息保护）。
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---------------------------------------------------------------------------
// TOTP（RFC 6238）
// ---------------------------------------------------------------------------

// totpCode 计算 RFC 6238 一次性验证码：HMAC-SHA1(secret, 计数器)、
// 动态截断后取低 digits 位。period 为时间步长（秒），默认 30。
func totpCode(secret []byte, t time.Time, digits, period int) string {
	counter := uint64(t.Unix()) / uint64(period)
	mac := hmac.New(sha1.New, secret)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % uint32(pow10(digits))
	return fmt.Sprintf("%0*d", digits, code)
}

// totpVerify 校验验证码：允许前后各 window 个时间步长的偏差
// （默认 ±1 窗口，容忍时钟抖动），比较为恒定时间。
// 返回是否匹配；重放防护（记录最近成功窗口）由会话层实现。
func totpVerify(secret []byte, code string, t time.Time, window, digits, period int) bool {
	if len(code) != digits {
		return false
	}
	for i := -window; i <= window; i++ {
		want := totpCode(secret, t.Add(time.Duration(i*period)*time.Second), digits, period)
		if subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

// pow10 计算 10 的 n 次方（TOTP 取模用；n 为位数，最大 10 位足够）。
func pow10(n int) int {
	r := 1
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

// ---------------------------------------------------------------------------
// 密钥派生与 GCM 信封（wrapBlob）
// ---------------------------------------------------------------------------

// deriveKEK 用 PBKDF2-HMAC-SHA256 从密码与盐派生密钥（Go 1.24 标准库
// crypto/pbkdf2）。同一密码 + 盐 + 迭代次数恒得同一密钥；盐随机即不可预测。
func deriveKEK(password string, salt []byte, iterations, keyLen int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
}

// gcmWrap 用 AES-256-GCM 包裹明文，返回 wrapBlob = nonce(12)‖ct‖tag(16)
// （docs/vault-design.md §5.2 编码，JSON 中以 base64 表示）。
func gcmWrap(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce, err := randomBytes(vaultGCMNonceLen)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, vaultGCMNonceLen+len(plaintext)+vaultGCMTagLen)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

// gcmUnwrap 解开 gcmWrap 的 wrapBlob。GCM 标签校验失败返回错误
// （在密钥树语境中即「密码/密钥错误」信号，隐式验证密码正确性）。
func gcmUnwrap(key, blob []byte) ([]byte, error) {
	if len(blob) < vaultGCMNonceLen+vaultGCMTagLen {
		return nil, errors.New("wrapBlob 长度无效")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, blob[:vaultGCMNonceLen], blob[vaultGCMNonceLen:], nil)
}

// newGCM 构造 AES-256-GCM（密钥长度校验：加密密钥必须为 16/24/32 字节）。
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES 实例失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM 实例失败: %w", err)
	}
	return gcm, nil
}

// ---------------------------------------------------------------------------
// 挑战签名（设备绑定）
// ---------------------------------------------------------------------------

// randomChallenge 生成 32 字节随机挑战，返回其 base64（标准、无填充）编码。
// 客户端签名消息 = 前缀 + 该字符串（UTF-8 字节）。
func randomChallenge() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// verifyChallengeSignature 用登记公钥验证挑战签名：
//   - RSA：PKCS#1 v1.5 + SHA-256；
//   - ECDSA：ASN.1 DER 编码；
//   - 签名与挑战均为 base64（标准、无填充）。
//
// message 为完整签名消息（前缀 + 挑战字符串的 UTF-8 字节）。
func verifyChallengeSignature(pub crypto.PublicKey, message []byte, sigB64 string) error {
	sig, err := base64.RawStdEncoding.DecodeString(sigB64)
	if err != nil {
		return errors.New("签名 base64 解码失败")
	}
	digest := sha256.Sum256(message)
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig); err != nil {
			return errors.New("RSA 签名验证失败")
		}
		return nil
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			return errors.New("ECDSA 签名验证失败")
		}
		return nil
	default:
		return fmt.Errorf("不支持的证书公钥类型: %T", pub)
	}
}

// ---------------------------------------------------------------------------
// 证书公钥解析与指纹
// ---------------------------------------------------------------------------

// parsePublicKeyPEM 解析客户端上传的公钥 PEM（P12 在客户端转换为标准 PEM 后
// 上传，服务端保持零第三方依赖）：
//   - CERTIFICATE：X.509 证书，返回其公钥与证书本体；
//   - PUBLIC KEY：PKIX 裸公钥（RSA/ECDSA）；
//   - RSA PUBLIC KEY：PKCS#1 裸 RSA 公钥。
func parsePublicKeyPEM(pemData []byte) (crypto.PublicKey, *x509.Certificate, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, nil, errors.New("PEM 解码失败")
	}
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 X.509 证书失败: %w", err)
		}
		return cert.PublicKey, cert, nil
	case "PUBLIC KEY":
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 PKIX 公钥失败: %w", err)
		}
		return pub, nil, nil
	case "RSA PUBLIC KEY":
		pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 RSA 公钥失败: %w", err)
		}
		return pub, nil, nil
	default:
		return nil, nil, fmt.Errorf("不支持的 PEM 类型: %s", block.Type)
	}
}

// certSPKIFingerprint 计算证书公钥（SPKI DER）的 SHA-256 十六进制指纹。
// 按公钥而非证书本体绑定：换发证书（同钥）不破坏绑定，换钥才需重新绑定。
func certSPKIFingerprint(pub crypto.PublicKey) string {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:])
}

// keySizeOK 校验证书公钥强度：RSA ≥ 2048 位，ECDSA ≥ P-256。
// 弱密钥（RSA-1024 等）拒绝绑定。
func keySizeOK(pub crypto.PublicKey) bool {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return k.N.BitLen() >= 2048
	case *ecdsa.PublicKey:
		return k.Curve.Params().BitSize >= 256
	default:
		return false
	}
}
