// 配对码机制：首次启动生成 20 位随机配对码并仅显示一次。
// 磁盘上只保存其 SHA-256 哈希，后续启动不会再次显示；
// 丢失配对码只能删除 auth.hash 后重新生成。

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// PairingDigits 配对码位数。
const PairingDigits = 20

// pairingFile 配对码哈希持久化文件路径。
func (d *Daemon) pairingFile() string {
	return filepath.Join(d.DataDir, "auth.hash")
}

// LoadPairing 加载配对码配置。
// 首次启动（auth.hash 不存在）时生成 20 位随机配对码并返回 isNew=true，
// 磁盘仅保存哈希；后续启动只加载哈希，配对码不再显示。
func (d *Daemon) LoadPairing() (code string, isNew bool, err error) {
	raw, err := os.ReadFile(d.pairingFile())
	if err == nil {
		h := strings.TrimSpace(string(raw))
		if len(h) == sha256.Size*2 {
			d.PairingHash = h
			return "", false, nil
		}
		return "", false, fmt.Errorf("配对码文件内容无效: %s", d.pairingFile())
	}
	if !os.IsNotExist(err) {
		return "", false, err
	}

	code, err = generatePairingCode()
	if err != nil {
		return "", false, err
	}
	d.PairingHash = pairingHash(code)
	if err := os.WriteFile(d.pairingFile(), []byte(d.PairingHash+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("保存配对码失败: %w", err)
	}
	return code, true, nil
}

// generatePairingCode 生成 PairingDigits 位十进制随机数（crypto/rand，无偏差）。
func generatePairingCode() (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(PairingDigits), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", PairingDigits, n), nil
}

// pairingHash 计算配对码的 SHA-256 十六进制摘要。
func pairingHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// checkPairing 恒定时间比较请求携带的配对码与已存哈希。
func checkPairing(got, storedHex string) bool {
	if got == "" {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(storedHex)) == 1
}
