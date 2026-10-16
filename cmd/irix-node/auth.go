// 首次启动引导：账户库中不存在任何管理员账户时，随机生成管理员用户名
// 与密码（crypto/rand），bcrypt 入库后在终端仅显示一次。
// 磁盘不保存明文凭据；凭据丢失且无人能登录时，需删除账户库（默认
// {data}/accounts.db）后重启重新生成，或直接操作账户库恢复。

package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// 引导管理员凭据长度（字符数）。
const (
	bootstrapUserLen = 12 // 用户名：字母/数字（符合 validUsername 规则）
	bootstrapPassLen = 20 // 密码：大小写字母与数字（远超最小密码长度）
)

// 凭据字符集（无易混淆要求，均满足用户名/密码校验规则）。
const (
	credCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// randomString 生成 n 位随机字符串（crypto/rand，无模偏差）。
func randomString(n int, charset string) (string, error) {
	max := big.NewInt(int64(len(charset)))
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = charset[idx.Int64()]
	}
	return string(out), nil
}

// ensureAdminAccount 首次启动引导：账户库中没有任何管理员账户时，
// 随机生成管理员用户名与密码并入库（bcrypt），返回明文凭据供调用方
// 在终端仅显示一次。created=false 表示已存在管理员账户（含共享账户库
// 多节点并发启动时由其他节点抢先创建的情况），此时无需显示任何凭据。
func (d *Daemon) ensureAdminAccount() (username, password string, created bool, err error) {
	has, err := d.accounts.hasAdminAccount()
	if err != nil {
		return "", "", false, err
	}
	if has {
		return "", "", false, nil
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		u, err := randomString(bootstrapUserLen, credCharset)
		if err != nil {
			return "", "", false, err
		}
		p, err := randomString(bootstrapPassLen, credCharset)
		if err != nil {
			return "", "", false, err
		}
		if err := d.accounts.createAccount(u, p, true); err != nil {
			lastErr = err
			// 用户名撞库/共享库并发初始化：重查是否已有管理员，有则让位
			if has, herr := d.accounts.hasAdminAccount(); herr == nil && has {
				return "", "", false, nil
			}
			continue
		}
		return u, p, true, nil
	}
	return "", "", false, fmt.Errorf("创建初始管理员账户失败: %w", lastErr)
}
