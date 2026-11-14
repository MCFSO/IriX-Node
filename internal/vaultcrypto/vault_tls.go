// vault_tls.go — TLS 传输加密（M1）与节点时间系统的衔接。
//
// 支持三种模式（配置 tls.mode，默认 off）：
//   - off：明文 HTTP，与既有部署完全兼容；启动时打印一行提示（不阻断）。
//   - auto：首次启动自动生成自签证书（RSA-2048，10 年，SAN 覆盖
//     localhost/127.0.0.1/::1/主机名），存 {data}/tls/；启动日志打印证书
//     SHA-256 指纹，客户端按指纹固定（TOFU）校验。
//   - manual：加载 tls.cert / tls.key 指定的正式证书。
//
// 时间系统（internal/timesys）在这里承担三件事：
//  1. 证书 NotBefore/NotAfter 用节点受纪律时间签发，并按当前不确定度回拨
//     NotBefore（客户端时钟偏差不会导致「证书尚未生效」）；
//  2. 所有 tls.Config 都设置 Time = timesys.Now，证书链校验用同一时间源；
//  3. 已存在证书按节点时间判定是否已过期/尚未生效：auto 模式自动重签，
//     manual 模式由调用方拒绝握手并给出明确原因。
//
// 加密保险库（vault.enabled=true）强制要求 TLS：main 启动校验拒绝
// 「vault + tls-mode=off」组合，杜绝密码/TOTP/签名明文过网。
package vaultcrypto

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"irix/node/internal/timesys"
)

// 自签证书有效期与回拨上界。
const (
	selfSignedLifetime = 10 * 365 * 24 * time.Hour
	minCertBackdate    = time.Hour
	maxCertBackdate    = 7 * 24 * time.Hour
)

// certFingerprintHex 计算证书 DER 的 SHA-256 十六进制指纹（启动日志打印，
// 客户端首次连接按此指纹固定校验，防中间人）。
func certFingerprintHex(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// CertificateValidity 返回证书的 NotBefore/NotAfter（按第一张证书解析）。
func CertificateValidity(cert *tls.Certificate) (time.Time, time.Time, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return time.Time{}, time.Time{}, errors.New("证书为空")
	}
	leaf, err := ParseLeaf(cert)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return leaf.NotBefore, leaf.NotAfter, nil
}

// ParseLeaf 解析证书链中的叶证书。
func ParseLeaf(cert *tls.Certificate) (*x509.Certificate, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("证书为空")
	}
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %w", err)
	}
	cert.Leaf = leaf
	return leaf, nil
}

// CertificateValidAt 判断证书在时刻 now 是否有效（按节点时间系统判定）。
func CertificateValidAt(cert *tls.Certificate, now time.Time) error {
	notBefore, notAfter, err := CertificateValidity(cert)
	if err != nil {
		return err
	}
	if now.Before(notBefore) {
		return fmt.Errorf("证书在节点时间 %s 尚未生效（NotBefore=%s，可能节点时钟偏差过大，请等待 Roughtime 同步或检查证书）",
			now.Format(time.RFC3339), notBefore.Format(time.RFC3339))
	}
	if now.After(notAfter) {
		return fmt.Errorf("证书在节点时间 %s 已过期（NotAfter=%s）",
			now.Format(time.RFC3339), notAfter.Format(time.RFC3339))
	}
	return nil
}

// certBackdate 按节点时间系统的不确定度决定 NotBefore 回拨量：
// 时钟不确定度越大，回拨越多（上限 7 天），避免其他机器因时钟偏差判定
// 「证书尚未生效」。
func certBackdate() time.Duration {
	back := minCertBackdate
	snap := timesys.Current().Snapshot()
	if snap.Uncertainty > back {
		back = snap.Uncertainty + time.Minute
	}
	if !snap.Trustworthy {
		// 尚未完成在线同步：额外留一天余量，覆盖常见的 RTC/系统时钟偏差。
		back += 24 * time.Hour
	}
	if back > maxCertBackdate {
		back = maxCertBackdate
	}
	return back
}

// EnsureSelfSignedCert 在 dir 下生成或加载自签证书，返回证书与 DER 指纹。
// 已存在证书按节点时间判定：有效则复用（指纹稳定）；已过期/尚未生效则重签
// （新指纹由调用方记录并在日志中提示客户端重新固定）。
func EnsureSelfSignedCert(dir string) (tls.Certificate, string, error) {
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certFile); err == nil {
		if cert, fp, err := LoadCertFiles(certFile, keyFile); err == nil {
			if verr := CertificateValidAt(&cert, timesys.Now()); verr == nil {
				return cert, fp, nil
			}
			// 过期/未生效：重签（自签证书没有外部信任锚，重签是唯一自愈路径）
		}
	}
	return generateSelfSignedCert(dir)
}

// generateSelfSignedCert 生成自签证书并原子落盘。
func generateSelfSignedCert(dir string) (tls.Certificate, string, error) {
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("创建 TLS 目录失败: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("生成 RSA 密钥失败: %w", err)
	}
	hostname, _ := os.Hostname()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("生成证书序列号失败: %w", err)
	}
	// 唯一时间来源：节点受纪律时钟（不是系统墙钟）。
	now := timesys.Now()
	back := certBackdate()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "irix-node"},
		NotBefore:    now.Add(-back),
		NotAfter:     now.Add(selfSignedLifetime),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", hostname},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("生成自签证书失败: %w", err)
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
			return tls.Certificate{}, "", fmt.Errorf("写入 %s 失败: %w", tmp, err)
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("落盘 %s 失败: %w", f.path, err)
		}
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("加载新生成证书失败: %w", err)
	}
	if _, err := ParseLeaf(&cert); err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, certFingerprintHex(der), nil
}

// LoadCertFiles 加载 PEM 证书/私钥文件，返回证书与指纹。
func LoadCertFiles(certFile, keyFile string) (tls.Certificate, string, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("加载 TLS 证书失败: %w", err)
	}
	if _, err := ParseLeaf(&cert); err != nil {
		return tls.Certificate{}, "", err
	}
	fp := ""
	if raw, err := os.ReadFile(certFile); err == nil {
		if block, _ := pem.Decode(raw); block != nil {
			fp = certFingerprintHex(block.Bytes)
		}
	}
	return cert, fp, nil
}

// ServerTLSConfig 构造服务端 tls.Config：最低 TLS 1.2，证书按节点时间校验。
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		Time:         func() time.Time { return timesys.Now() },
	}
}

// EnsureSelfSignedTLS 在 dir 下生成或加载自签证书（兼容旧调用）。
// 已存在且按节点时间有效的证书直接复用（重启指纹不变）；否则重签。
func EnsureSelfSignedTLS(dir string) (*tls.Config, string, error) {
	cert, fp, err := EnsureSelfSignedCert(dir)
	if err != nil {
		return nil, "", err
	}
	return ServerTLSConfig(cert), fp, nil
}

// LoadManualTLS 加载 PEM 证书/私钥文件并构造服务端 tls.Config。
// 返回配置与证书指纹（取 PEM 第一块 DER 计算；解析失败时指纹为空，不阻断）。
// 证书是否在有效期内由调用方按节点时间判定（CertificateValidAt）。
func LoadManualTLS(certFile, keyFile string) (*tls.Config, string, error) {
	cert, fp, err := LoadCertFiles(certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	return ServerTLSConfig(cert), fp, nil
}
