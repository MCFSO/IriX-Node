// 设备绑定数据面：设备公钥的持久化与账户设备策略（SQL 权威层）。
//
// 服务端只保存设备**公钥**与客户端上报的签名描述（descriptor），不保存任何
// 可用于冒充客户端的秘密；私钥始终留在客户端（优先放在 TPM / 系统密钥库）。

package main

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 设备绑定策略与限制。
const (
	accountDevicePolicyOptional = "optional" // 默认：密码可登录；提供设备证明时绑定会话
	accountDevicePolicyRequired = "required" // 必须已绑定设备签名才能登录

	accountMaxDevices          = 5    // 单账户最多绑定设备数
	accountDeviceNameMax       = 64   // 设备名长度上限
	accountDeviceDescriptorMax = 4096 // 签名的客户端描述 JSON 上限（字节）
)

// accountDevice 已绑定设备（公钥与元数据）。
type accountDevice struct {
	DeviceID      string `json:"deviceId"`
	Username      string `json:"-"`
	PublicKey     string `json:"-"` // base64url 原始公钥；不外露（客户端自己持有）
	KeyType       string `json:"keyType"`
	KeyProtection string `json:"keyProtection"`
	Descriptor    string `json:"descriptor"` // 客户端签名上报的 JSON 描述（原文）
	Name          string `json:"name"`
	CreatedAt     int64  `json:"createdAt"`
	LastSeen      int64  `json:"lastSeen"`
}

// normalizeDevicePolicy 校验并归一化设备策略。
func normalizeDevicePolicy(policy string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", accountDevicePolicyOptional:
		return accountDevicePolicyOptional, nil
	case accountDevicePolicyRequired:
		return accountDevicePolicyRequired, nil
	default:
		return "", errors.New("设备绑定策略无效（须为 optional 或 required）")
	}
}

// normalizeStoredDevicePolicy 读取数据库值时归一化（无法识别的一律按 optional，
// 避免脏数据把 required 静默变成“允许纯密码登录”或反之）。
func normalizeStoredDevicePolicy(policy string) string {
	if strings.EqualFold(strings.TrimSpace(policy), accountDevicePolicyRequired) {
		return accountDevicePolicyRequired
	}
	return accountDevicePolicyOptional
}

// setDevicePolicy 更新账户设备策略。
func (s *accountSystem) setDevicePolicy(username, policy string) error {
	normalized, err := normalizeDevicePolicy(policy)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(rebindSQL(s.driver,
		`UPDATE accounts SET device_policy = ?, updated_at = ? WHERE username = ?`),
		normalized, time.Now().UnixMilli(), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("账户不存在")
	}
	return nil
}

// createDevice 新建设备绑定。device_id 已存在时返回可识别的重复错误。
func (s *accountSystem) createDevice(dev *accountDevice) error {
	now := time.Now().UnixMilli()
	if dev.CreatedAt == 0 {
		dev.CreatedAt = now
	}
	if dev.LastSeen == 0 {
		dev.LastSeen = now
	}
	_, err := s.db.Exec(rebindSQL(s.driver,
		`INSERT INTO account_devices
		   (device_id, username, public_key, key_type, key_protection, descriptor, name, created_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		dev.DeviceID, dev.Username, dev.PublicKey, dev.KeyType, dev.KeyProtection,
		dev.Descriptor, dev.Name, dev.CreatedAt, dev.LastSeen)
	return err
}

// isDuplicateDeviceErr 判断主键冲突（sqlite/mysql/postgres 错误文案不同）。
func isDuplicateDeviceErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "constraint")
}

// getDevice 读取指定账户名下的设备；不存在返回 (nil, nil)。
func (s *accountSystem) getDevice(username, deviceID string) (*accountDevice, error) {
	row := s.db.QueryRow(rebindSQL(s.driver,
		`SELECT device_id, username, public_key, key_type, key_protection, descriptor, name, created_at, last_seen
		   FROM account_devices WHERE device_id = ? AND username = ?`), deviceID, username)
	var dev accountDevice
	err := row.Scan(&dev.DeviceID, &dev.Username, &dev.PublicKey, &dev.KeyType,
		&dev.KeyProtection, &dev.Descriptor, &dev.Name, &dev.CreatedAt, &dev.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

// listDevices 列出账户全部设备（按创建时间升序）。
func (s *accountSystem) listDevices(username string) ([]*accountDevice, error) {
	rows, err := s.db.Query(rebindSQL(s.driver,
		`SELECT device_id, username, public_key, key_type, key_protection, descriptor, name, created_at, last_seen
		   FROM account_devices WHERE username = ? ORDER BY created_at`), username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*accountDevice
	for rows.Next() {
		var dev accountDevice
		if err := rows.Scan(&dev.DeviceID, &dev.Username, &dev.PublicKey, &dev.KeyType,
			&dev.KeyProtection, &dev.Descriptor, &dev.Name, &dev.CreatedAt, &dev.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, &dev)
	}
	return out, rows.Err()
}

// countDevices 统计账户已绑定设备数。
func (s *accountSystem) countDevices(username string) (int, error) {
	var n int
	err := s.db.QueryRow(rebindSQL(s.driver,
		`SELECT COUNT(*) FROM account_devices WHERE username = ?`), username).Scan(&n)
	return n, err
}

// deleteDevice 删除指定设备；返回是否确实删除了记录。
func (s *accountSystem) deleteDevice(username, deviceID string) (bool, error) {
	res, err := s.db.Exec(rebindSQL(s.driver,
		`DELETE FROM account_devices WHERE device_id = ? AND username = ?`), deviceID, username)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deleteDevicesByUser 删除某账户的全部设备（删除账户时调用）。
func (s *accountSystem) deleteDevicesByUser(username string) {
	_, _ = s.db.Exec(rebindSQL(s.driver,
		`DELETE FROM account_devices WHERE username = ?`), username)
}

// touchDevice 更新设备最近使用时间（登录成功时调用；失败不影响登录）。
func (s *accountSystem) touchDevice(deviceID string) {
	_, _ = s.db.Exec(rebindSQL(s.driver,
		`UPDATE account_devices SET last_seen = ? WHERE device_id = ?`), time.Now().UnixMilli(), deviceID)
}
