// 配置文件（config.json）支持：所有启动参数均可写入配置文件统一管理，
// 优先级为：命令行显式参数 > 配置文件 > 环境变量（仅 bind）> 内置默认值。
// 文件不存在时首次启动自动生成一份示例配置（ensureConfigFile，生成失败仅告警），
// 之后照常加载；行为与纯命令行启动完全一致（向后兼容）。

package main

import (
	_ "embed" // 仅用于内嵌 config.example.json（[]byte 嵌入需空导入，见 embed 包文档）
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TLSConfig 配置文件 tls 块：传输加密（可选，默认 off 保持现状）。
// mode: off（默认，明文 HTTP）/ auto（自动生成自签证书，客户端按启动日志
// 指纹固定校验）/ manual（使用 cert/key 指定的正式证书）。
type TLSConfig struct {
	Mode string `json:"mode"` // off | auto | manual
	Cert string `json:"cert"` // manual：证书文件路径（PEM）
	Key  string `json:"key"`  // manual：私钥文件路径（PEM）
}

// VaultConfig 配置文件 vault 块：加密保险库（可选，默认关闭）。
// 开启后实例配置等数据以 AES-256-GCM 加密存储，解锁需 TOTP+密码+证书签名；
// 开启时强制要求 TLS（tls.mode 不能为 off），否则拒绝启动。
// 字段说明（docs/vault-design.md §6.4/§7.2/§11）：
//   - idleTimeoutMinutes：解锁会话空闲超时（分钟），到期自动锁定，默认 30；
//   - maxAttempts / lockoutMinutes：unlock/recovery/init-verify 统一失败限速
//     （用户+IP 双维度），默认 5 次 / 15 分钟；
//   - pbkdf2Iterations：密码派生 KEK 的 PBKDF2 迭代次数，默认 600000；
//   - passwordMinLength / passwordExpireDays / forceExpire：密码策略，
//     默认 12 位 / 90 天过期 / 到期不强制（forceExpire=true 时解锁必须同请求改密）；
//   - bindSessionIP：会话令牌绑定来源 IP，默认关。
type VaultConfig struct {
	Enabled            *bool `json:"enabled"`            // 加密保险库开关
	IdleTimeoutMinutes *int  `json:"idleTimeoutMinutes"` // 会话空闲超时（分钟）
	MaxAttempts        *int  `json:"maxAttempts"`        // 失败限速阈值
	LockoutMinutes     *int  `json:"lockoutMinutes"`     // 锁定时长（分钟）
	PBKDF2Iterations   *int  `json:"pbkdf2Iterations"`   // PBKDF2 迭代次数
	PasswordMinLength  *int  `json:"passwordMinLength"`  // 密码最小长度
	PasswordExpireDays *int  `json:"passwordExpireDays"` // 密码有效期（天，0=不过期）
	ForceExpire        *bool `json:"forceExpire"`        // 到期强制改密（解锁同请求）
	BindSessionIP      *bool `json:"bindSessionIP"`      // 会话绑定来源 IP
	BlockSizeKB        *int  `json:"blockSizeKB"`        // 密文对象块大小（KB，默认 1024）
	ScrubOnDelete      *bool `json:"scrubOnDelete"`      // 回收/删除前覆盖明文（best-effort）
	DefaultFilesMode   string `json:"defaultFilesMode"`  // 新实例文件区默认模式：plaintext | materialize
}

// Config config.json 配置文件结构。
// 布尔/整数字段用指针：nil = 配置未写该字段（回退默认值）；
// 非 nil = 显式设置（含 false/0，会覆盖内置默认值）。
// 字符串字段零值（""）与「未设置」语义天然一致（bind 回退环境变量/默认值、
// apiKey 空 = 配对码机制、data 空 = 当前目录），无需指针。
type Config struct {
	Bind              string      `json:"bind"`              // 监听地址（IP 或主机名，如 127.0.0.1 / 0.0.0.0 / 192.168.1.5 / ::）
	Port              int         `json:"port"`              // 监听端口（1-65535；0 = 未设置）
	Data              string      `json:"data"`              // 数据目录（空 = 当前目录）
	APIKey            string      `json:"apiKey"`            // 固定 API 密钥（空 = 启用配对码机制）
	InstanceLog       *bool       `json:"instanceLog"`       // 实例日志落盘开关
	InstanceLogMax    *int        `json:"instanceLogMax"`    // 实例日志单文件轮转上限（MB）
	AuditLog          *bool       `json:"auditLog"`          // 审计日志落盘开关
	AuditLogMax       *int        `json:"auditLogMax"`       // 审计日志单文件轮转上限（MB）
	LoadTune          *bool       `json:"loadTune"`          // 负载自适应调谐开关
	TransferAllowCIDR string      `json:"transferAllowCidr"` // 集群拉取放行内网 CIDR（逗号分隔）
	TLS               *TLSConfig  `json:"tls"`               // TLS 传输加密（nil = 未配置）
	Vault             *VaultConfig `json:"vault"`            // 加密保险库（nil = 未配置）
}

//go:embed config.example.json
var exampleConfigJSON []byte

// ensureConfigFile 首次启动时若配置文件不存在，自动落一份示例配置
// （内容与 config.example.json 一致，含字段注释；加载时 _comment 字段会被忽略）。
// 返回是否新建了文件；创建失败返回错误（调用方仅告警，不阻断启动）。
func ensureConfigFile(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil // 已存在，绝不覆盖用户配置
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, exampleConfigJSON, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// loadConfigFile 从 path 加载配置文件。
// 文件不存在时返回空配置与 loaded=false（不视为错误，方便无配置文件启动）。
func loadConfigFile(path string) (cfg *Config, loaded bool, err error) {
	cfg = &Config{}
	if strings.TrimSpace(path) == "" {
		return cfg, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return nil, false, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, true, nil
}

// nodeOptions 启动选项最终值（flag 默认值经配置文件与命令行显式参数合并后的结果）。
type nodeOptions struct {
	Port              int
	BindHost          string
	DataDir           string
	APIKey            string
	InstanceLog       bool
	InstanceLogMax    int
	AuditLog          bool
	AuditLogMax       int
	LoadTune          bool
	TransferAllowCIDR string
	TLSMode           string // off | auto | manual
	TLSCert           string // manual：证书文件路径
	TLSKey            string // manual：私钥文件路径
	VaultEnabled      bool   // 加密保险库开关
	// Vault 调优项（分钟/次数/迭代/天数等，见 VaultConfig）
	VaultIdleTimeout      int
	VaultMaxAttempts      int
	VaultLockoutMinutes   int
	VaultPBKDF2Iterations int
	VaultPasswordMinLen   int
	VaultPasswordExpire   int // 天，0 = 不过期
	VaultForceExpire      bool
	VaultBindSessionIP    bool
	VaultBlockSizeKB      int
	VaultScrubOnDelete    bool
	VaultDefaultFilesMode string
}

// applyConfig 应用配置文件：仅覆盖未被命令行显式设置的项。
// setFlags 为命令行显式设置的参数名集合（见 main 中 flag.Visit）；
// 优先级：命令行显式参数 > 配置文件 > flag 默认值。
func (o *nodeOptions) applyConfig(cfg *Config, setFlags map[string]bool) {
	if cfg == nil {
		return
	}
	if !setFlags["port"] && cfg.Port != 0 {
		o.Port = cfg.Port
	}
	if !setFlags["bind"] && cfg.Bind != "" {
		o.BindHost = cfg.Bind
	}
	if !setFlags["data"] && cfg.Data != "" {
		o.DataDir = cfg.Data
	}
	if !setFlags["apikey"] && cfg.APIKey != "" {
		o.APIKey = cfg.APIKey
	}
	if !setFlags["instance-log"] && cfg.InstanceLog != nil {
		o.InstanceLog = *cfg.InstanceLog
	}
	if !setFlags["instance-log-max"] && cfg.InstanceLogMax != nil {
		o.InstanceLogMax = *cfg.InstanceLogMax
	}
	if !setFlags["audit-log"] && cfg.AuditLog != nil {
		o.AuditLog = *cfg.AuditLog
	}
	if !setFlags["audit-log-max"] && cfg.AuditLogMax != nil {
		o.AuditLogMax = *cfg.AuditLogMax
	}
	if !setFlags["load-tune"] && cfg.LoadTune != nil {
		o.LoadTune = *cfg.LoadTune
	}
	if !setFlags["transfer-allow-cidr"] && cfg.TransferAllowCIDR != "" {
		o.TransferAllowCIDR = cfg.TransferAllowCIDR
	}
	if cfg.TLS != nil {
		if !setFlags["tls-mode"] && cfg.TLS.Mode != "" {
			o.TLSMode = cfg.TLS.Mode
		}
		if !setFlags["tls-cert"] && cfg.TLS.Cert != "" {
			o.TLSCert = cfg.TLS.Cert
		}
		if !setFlags["tls-key"] && cfg.TLS.Key != "" {
			o.TLSKey = cfg.TLS.Key
		}
	}
	if cfg.Vault != nil && !setFlags["vault"] && cfg.Vault.Enabled != nil {
		o.VaultEnabled = *cfg.Vault.Enabled
	}
	if cfg.Vault != nil {
		if !setFlags["vault-idle-timeout"] && cfg.Vault.IdleTimeoutMinutes != nil {
			o.VaultIdleTimeout = *cfg.Vault.IdleTimeoutMinutes
		}
		if !setFlags["vault-max-attempts"] && cfg.Vault.MaxAttempts != nil {
			o.VaultMaxAttempts = *cfg.Vault.MaxAttempts
		}
		if !setFlags["vault-lockout-minutes"] && cfg.Vault.LockoutMinutes != nil {
			o.VaultLockoutMinutes = *cfg.Vault.LockoutMinutes
		}
		if !setFlags["vault-pbkdf2-iterations"] && cfg.Vault.PBKDF2Iterations != nil {
			o.VaultPBKDF2Iterations = *cfg.Vault.PBKDF2Iterations
		}
		if !setFlags["vault-password-min-length"] && cfg.Vault.PasswordMinLength != nil {
			o.VaultPasswordMinLen = *cfg.Vault.PasswordMinLength
		}
		if !setFlags["vault-password-expire-days"] && cfg.Vault.PasswordExpireDays != nil {
			o.VaultPasswordExpire = *cfg.Vault.PasswordExpireDays
		}
		if !setFlags["vault-force-expire"] && cfg.Vault.ForceExpire != nil {
			o.VaultForceExpire = *cfg.Vault.ForceExpire
		}
		if !setFlags["vault-bind-session-ip"] && cfg.Vault.BindSessionIP != nil {
			o.VaultBindSessionIP = *cfg.Vault.BindSessionIP
		}
		if !setFlags["vault-block-size-kb"] && cfg.Vault.BlockSizeKB != nil {
			o.VaultBlockSizeKB = *cfg.Vault.BlockSizeKB
		}
		if !setFlags["vault-scrub-on-delete"] && cfg.Vault.ScrubOnDelete != nil {
			o.VaultScrubOnDelete = *cfg.Vault.ScrubOnDelete
		}
		if !setFlags["vault-default-files-mode"] && cfg.Vault.DefaultFilesMode != "" {
			o.VaultDefaultFilesMode = cfg.Vault.DefaultFilesMode
		}
	}
}
