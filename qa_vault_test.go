// qa_vault_test.go — Vault M1/M2 单测。
//
// 覆盖：TOTP（RFC 6238 附录 B 官方向量）、PBKDF2（RFC 7914 向量）、
// GCM 信封 wrapBlob 往返与篡改检测、挑战签名验证（RSA/ECDSA）、
// 证书 PEM 解析与 SPKI 指纹、公钥强度校验、自签 TLS 生成/复用/权限。

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestTOTPRFC6238Vectors 用 RFC 6238 附录 B 官方测试向量验证 TOTP 实现
// （HMAC-SHA1、8 位验证码、30 秒周期）。
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890") // RFC 6238 指定测试密钥
	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		got := totpCode(secret, time.Unix(c.unix, 0), 8, 30)
		if got != c.want {
			t.Errorf("TOTP(T=%d) = %s，期望 %s", c.unix, got, c.want)
		}
	}
}

// TestTOTPVerifyWindow 验证 ±1 窗口匹配、错误验证码拒绝、错误长度拒绝。
func TestTOTPVerifyWindow(t *testing.T) {
	secret := []byte("test-secret-bytes")
	now := time.Now()
	code := totpCode(secret, now, totpDigits, totpPeriod)

	if !totpVerify(secret, code, now, 1, totpDigits, totpPeriod) {
		t.Error("当前窗口验证码应通过")
	}
	if !totpVerify(secret, code, now.Add(-30*time.Second), 1, totpDigits, totpPeriod) {
		t.Error("前一窗口验证码应通过（窗口 ±1）")
	}
	if !totpVerify(secret, code, now.Add(30*time.Second), 1, totpDigits, totpPeriod) {
		t.Error("后一窗口验证码应通过（窗口 ±1）")
	}
	if totpVerify(secret, code, now.Add(90*time.Second), 1, totpDigits, totpPeriod) {
		t.Error("超出 ±1 窗口的验证码应拒绝")
	}
	if totpVerify(secret, "000000", now, 1, totpDigits, totpPeriod) {
		t.Error("错误验证码应拒绝")
	}
	if totpVerify(secret, "12345", now, 1, totpDigits, totpPeriod) {
		t.Error("长度不符的验证码应拒绝")
	}
	if totpVerify([]byte("wrong-secret"), code, now, 1, totpDigits, totpPeriod) {
		t.Error("错误密钥计算出的验证码应拒绝")
	}
}

// TestPBKDF2RFC7914Vector 用 RFC 7914 指定向量验证 PBKDF2-HMAC-SHA256
// （P="password" S="salt" c=1 dkLen=32）。
func TestPBKDF2RFC7914Vector(t *testing.T) {
	want, _ := hex.DecodeString("120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b")
	got, err := deriveKEK("password", []byte("salt"), 1, 32)
	if err != nil {
		t.Fatalf("deriveKEK 失败: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("PBKDF2 结果不符:\n got  %x\n want %x", got, want)
	}
	// 同一密码+盐+迭代 → 同一密钥（确定性）
	got2, err := deriveKEK("password", []byte("salt"), 1, 32)
	if err != nil {
		t.Fatalf("deriveKEK 失败: %v", err)
	}
	if string(got) != string(got2) {
		t.Error("同一参数派生结果应一致")
	}
}

// TestGCMWrapRoundtrip 验证 wrapBlob 往返一致、长度结构正确、篡改被检出。
func TestGCMWrapRoundtrip(t *testing.T) {
	kek, err := randomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := randomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := gcmWrap(kek, masterKey)
	if err != nil {
		t.Fatalf("gcmWrap 失败: %v", err)
	}
	// wrapBlob = nonce(12) + ct(32) + tag(16)
	if len(blob) != vaultGCMNonceLen+32+vaultGCMTagLen {
		t.Fatalf("wrapBlob 长度 = %d，期望 %d", len(blob), vaultGCMNonceLen+32+vaultGCMTagLen)
	}
	got, err := gcmUnwrap(kek, blob)
	if err != nil {
		t.Fatalf("gcmUnwrap 失败: %v", err)
	}
	if string(got) != string(masterKey) {
		t.Error("解包结果与原文不一致")
	}

	// 篡改：翻转 blob 最后一个字节（tag 内）→ 必须失败
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := gcmUnwrap(kek, tampered); err == nil {
		t.Error("篡改后的 wrapBlob 应解包失败")
	}
	// 错误密钥 → 必须失败
	otherKey, _ := randomBytes(32)
	if _, err := gcmUnwrap(otherKey, blob); err == nil {
		t.Error("错误密钥解包应失败")
	}
	// 过短 blob → 必须失败
	if _, err := gcmUnwrap(kek, []byte("short")); err == nil {
		t.Error("过短 blob 应失败")
	}
}

// TestChallengeSignatureRSA 验证 RSA 挑战签名（PKCS#1 v1.5 + SHA-256）。
func TestChallengeSignatureRSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := randomChallenge()
	if err != nil {
		t.Fatal(err)
	}
	message := []byte(signPrefixUnlock + challenge)
	digest := sha256.Sum256(message)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	sigB64 := base64.RawStdEncoding.EncodeToString(sig)

	if err := verifyChallengeSignature(&priv.PublicKey, message, sigB64); err != nil {
		t.Errorf("合法签名验证失败: %v", err)
	}
	// 错误消息（篡改挑战）→ 拒绝
	if err := verifyChallengeSignature(&priv.PublicKey, []byte(signPrefixUnlock+"tampered"), sigB64); err == nil {
		t.Error("篡改消息的签名应拒绝")
	}
	// 前缀不匹配（跨用途重用）→ 拒绝
	if err := verifyChallengeSignature(&priv.PublicKey, []byte(signPrefixCertBind+challenge), sigB64); err == nil {
		t.Error("跨用途前缀的签名应拒绝")
	}
	// 错误公钥 → 拒绝
	otherPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	if err := verifyChallengeSignature(&otherPriv.PublicKey, message, sigB64); err == nil {
		t.Error("错误公钥验证应拒绝")
	}
	// 非法 base64 → 拒绝
	if err := verifyChallengeSignature(&priv.PublicKey, message, "!!not-base64!!"); err == nil {
		t.Error("非法 base64 签名应拒绝")
	}
}

// TestChallengeSignatureECDSA 验证 ECDSA（P-256）挑战签名（ASN.1 DER）。
func TestChallengeSignatureECDSA(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := randomChallenge()
	if err != nil {
		t.Fatal(err)
	}
	message := []byte(signPrefixUnlock + challenge)
	digest := sha256.Sum256(message)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	sigB64 := base64.RawStdEncoding.EncodeToString(sig)

	if err := verifyChallengeSignature(&priv.PublicKey, message, sigB64); err != nil {
		t.Errorf("合法 ECDSA 签名验证失败: %v", err)
	}
	if err := verifyChallengeSignature(&priv.PublicKey, []byte(signPrefixUnlock+"tampered"), sigB64); err == nil {
		t.Error("篡改消息的 ECDSA 签名应拒绝")
	}
}

// TestParsePublicKeyPEM 验证三种 PEM 形态解析与 SPKI 指纹。
func TestParsePublicKeyPEM(t *testing.T) {
	// 1. 裸 PKIX 公钥（RSA）
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&rsaPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})
	pub, cert, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("解析 PKIX 公钥失败: %v", err)
	}
	if cert != nil {
		t.Error("裸公钥不应返回证书")
	}
	if !keySizeOK(pub) {
		t.Error("RSA-2048 应通过强度校验")
	}

	// 2. X.509 自签证书
	der := makeSelfSignedCertDER(t, &rsaPriv.PublicKey, rsaPriv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pub2, cert2, err := parsePublicKeyPEM(certPEM)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if cert2 == nil {
		t.Error("证书 PEM 应返回证书本体")
	}
	if certSPKIFingerprint(pub) != certSPKIFingerprint(pub2) {
		t.Error("证书与裸公钥的 SPKI 指纹应一致（同钥）")
	}

	// 3. 垃圾输入 → 拒绝
	if _, _, err := parsePublicKeyPEM([]byte("not a pem")); err == nil {
		t.Error("垃圾输入应解析失败")
	}
	if _, _, err := parsePublicKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaPriv)})); err == nil {
		t.Error("私钥 PEM 应拒绝（不支持的类型）")
	}

	// 4. 指纹稳定且随钥变化
	fp1 := certSPKIFingerprint(&rsaPriv.PublicKey)
	fp2 := certSPKIFingerprint(&rsaPriv.PublicKey)
	if fp1 == "" || fp1 != fp2 {
		t.Error("SPKI 指纹应稳定非空")
	}
	otherPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	if certSPKIFingerprint(&otherPriv.PublicKey) == fp1 {
		t.Error("不同密钥的指纹应不同")
	}
}

// TestKeySizeOK 验证弱密钥拒绝。
func TestKeySizeOK(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if keySizeOK(&weak.PublicKey) {
		t.Error("RSA-1024 应被拒绝")
	}
	ecWeak, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if keySizeOK(&ecWeak.PublicKey) {
		t.Error("P-224 应被拒绝")
	}
	if keySizeOK("not-a-key") {
		t.Error("未知类型应被拒绝")
	}
}

// TestSelfSignedTLS 验证自签证书生成、指纹稳定复用、文件落盘与权限。
func TestSelfSignedTLS(t *testing.T) {
	// 使用不存在的子目录路径：ensureSelfSignedTLS 创建时应为 0700
	//（Linux 上 t.TempDir() 本身已存在且为 0755，MkdirAll 不会改权限）
	dir := filepath.Join(t.TempDir(), "tls")
	cfg1, fp1, err := ensureSelfSignedTLS(dir)
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if cfg1 == nil || len(cfg1.Certificates) != 1 {
		t.Fatal("应返回含证书的 tls.Config")
	}
	if fp1 == "" {
		t.Fatal("证书指纹不应为空")
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("缺少文件 %s: %v", f, err)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("私钥权限 = %o，期望 600", perm)
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("tls 目录权限 = %o，期望 700", perm)
		}
	}

	// 二次调用：复用同一证书，指纹不变
	cfg2, fp2, err := ensureSelfSignedTLS(dir)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if fp1 != fp2 {
		t.Error("重启后证书指纹应保持不变（复用同一证书）")
	}
	if cfg2 == nil {
		t.Error("二次加载应返回配置")
	}

	// 加载非法证书路径 → 报错
	if _, _, err := loadManualTLS(filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")); err == nil {
		t.Error("证书/私钥对调加载应失败")
	}
}

// makeSelfSignedCertDER 构造测试用自签证书 DER（RSA 公钥）。
func makeSelfSignedCertDER(t *testing.T, pub *rsa.PublicKey, priv *rsa.PrivateKey) []byte {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "irix-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("创建测试证书失败: %v", err)
	}
	return der
}
