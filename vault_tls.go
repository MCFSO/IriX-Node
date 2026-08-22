// vault_tls.go — TLS 传输加密（M1）。
//
// 支持三种模式（配置 tls.mode，默认 off）：
//   - off：明文 HTTP，与既有部署完全兼容；启动时打印一行提示（不阻断）。
//   - auto：首次启动自动生成自签证书（RSA-2048，10 年，SAN 覆盖
//     localhost/127.0.0.1/::1/主机名），存 {data}/tls/；启动日志打印证书
//     SHA-256 指纹，客户端按指纹固定（TOFU）校验。
//   - manual：加载 tls.cert / tls.key 指定的正式证书。
//
// 加密保险库（vault.enabled=true）强制要求 TLS：main 启动校验拒绝
// 「vault + tls-mode=off」组合，杜绝密码/TOTP/签名明文过网。

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certFingerprintHex 计算证书 DER 的 SHA-256 十六进制指纹（启动日志打印，
// 客户端首次连接按此指纹固定校验，防中间人）。
func certFingerprintHex(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// ensureSelfSignedTLS 在 dir 下生成或加载自签证书。
// 已存在 cert.pem/key.pem 时直接加载（重启复用同一证书，指纹不变）；
// 否则生成 RSA-2048 自签证书（10 年有效期），临时文件 + rename 原子落盘，
// 私钥权限 0600、目录 0700。返回 tls.Config 与证书指纹。
func ensureSelfSignedTLS(dir string) (*tls.Config, string, error) {
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certFile); err == nil {
		// 已存在：直接加载；指纹按实际文件计算（与首次生成的输出一致）
		return loadManualTLS(certFile, keyFile)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("创建 TLS 目录失败: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", fmt.Errorf("生成 RSA 密钥失败: %w", err)
	}
	hostname, _ := os.Hostname()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", fmt.Errorf("生成证书序列号失败: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "irix-node"},
		NotBefore:    time.Now().Add(-time.Hour), // 容忍客户端时钟轻微偏差
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", hostname},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", fmt.Errorf("生成自签证书失败: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	// 原子落盘：先写 .tmp 再 rename，避免崩溃留下半截证书/密钥
	for _, f := range []struct {
		path string
		data []byte
	}{
		{certFile, certPEM},
		{keyFile, keyPEM},
	} {
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, f.data, 0o600); err != nil {
			return nil, "", fmt.Errorf("写入 %s 失败: %w", tmp, err)
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return nil, "", fmt.Errorf("落盘 %s 失败: %w", f.path, err)
		}
	}

	cfg, _, err := loadManualTLS(certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	return cfg, certFingerprintHex(der), nil
}

// loadManualTLS 加载 PEM 证书/私钥文件并构造服务端 tls.Config。
// 返回配置与证书指纹（取 PEM 第一块 DER 计算；解析失败时指纹为空，不阻断）。
// 最低 TLS 1.2：旧协议（SSLv3/TLS1.0/1.1）存在已知弱点，一律拒绝。
func loadManualTLS(certFile, keyFile string) (*tls.Config, string, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, "", fmt.Errorf("加载 TLS 证书失败: %w", err)
	}
	fp := ""
	if raw, err := os.ReadFile(certFile); err == nil {
		if block, _ := pem.Decode(raw); block != nil {
			fp = certFingerprintHex(block.Bytes)
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, fp, nil
}
