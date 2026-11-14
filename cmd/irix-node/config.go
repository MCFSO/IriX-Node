// 配置文件（config.json）支持：所有启动参数均可写入配置文件统一管理，
// 优先级为：命令行显式参数 > 配置文件 > 环境变量（仅 bind）> 内置默认值。
// 文件不存在时首次启动自动生成一份示例配置（ensureConfigFile，生成失败仅告警），
// 之后照常加载；行为与纯命令行启动完全一致（向后兼容）。

package main

import (
	"bytes"
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
	Enabled            *bool  `json:"enabled"`            // 加密保险库开关
	IdleTimeoutMinutes *int   `json:"idleTimeoutMinutes"` // 会话空闲超时（分钟）
	MaxAttempts        *int   `json:"maxAttempts"`        // 失败限速阈值
	LockoutMinutes     *int   `json:"lockoutMinutes"`     // 锁定时长（分钟）
	PBKDF2Iterations   *int   `json:"pbkdf2Iterations"`   // PBKDF2 迭代次数
	PasswordMinLength  *int   `json:"passwordMinLength"`  // 密码最小长度
	PasswordExpireDays *int   `json:"passwordExpireDays"` // 密码有效期（天，0=不过期）
	ForceExpire        *bool  `json:"forceExpire"`        // 到期强制改密（解锁同请求）
	BindSessionIP      *bool  `json:"bindSessionIP"`      // 会话绑定来源 IP
	BlockSizeKB        *int   `json:"blockSizeKB"`        // 密文对象块大小（KB，默认 1024）
	ScrubOnDelete      *bool  `json:"scrubOnDelete"`      // 回收/删除前覆盖明文（best-effort）
	DefaultFilesMode   string `json:"defaultFilesMode"`   // 新实例文件区默认模式：plaintext | materialize
}

// AccountsConfig 配置文件 accounts 块：账户管理存储（docs/accounts-design.md）。
// 默认 SQLite（{data}/accounts.db），可选 MySQL / PostgreSQL；Redis 可选
// 缓存登录会话与权限热数据（不可用时自动回退数据库）。连接池参数均可调。
type AccountsConfig struct {
	Driver             string `json:"driver"`             // sqlite（默认）| mysql | postgres
	DSN                string `json:"dsn"`                // 连接串；sqlite 为文件路径（空 = {data}/accounts.db）
	MaxOpen            *int   `json:"maxOpen"`            // SQL 连接池最大连接数（默认 20）
	MaxIdle            *int   `json:"maxIdle"`            // SQL 连接池最大空闲连接数（默认 10）
	ConnMaxLifetimeMin *int   `json:"connMaxLifetimeMin"` // SQL 连接最大存活时间（分钟，默认 30）
	RedisAddr          string `json:"redisAddr"`          // Redis 地址（空 = 不启用 Redis 缓存）
	RedisPassword      string `json:"redisPassword"`      // Redis 密码
	RedisDB            int    `json:"redisDB"`            // Redis 库号
	RedisPoolSize      *int   `json:"redisPoolSize"`      // Redis 连接池大小（默认 16）
}

// DefenseConfig 配置文件 defense 块：DDoS/CC 防御阈值调优。
// 防御本身恒开、**没有关闭开关**：写 enabled=false 会被忽略并在启动日志提示；
// 这里只能调整阈值与配置级静态封禁的 User-Agent。字段含义见 config.example.json。
type DefenseConfig struct {
	MaxIPConcurrent        *int     `json:"maxIPConcurrent"`        // 单 IP 在途并发上限
	MaxUserAgentConcurrent *int     `json:"maxUserAgentConcurrent"` // 单 User-Agent 在途并发上限（超过即封禁该 UA）
	IPRatePerSecond        *int     `json:"ipRatePerSecond"`        // 单 IP 每秒请求上限
	UserAgentRatePerSecond *int     `json:"userAgentRatePerSecond"` // 单 User-Agent 每秒请求上限（跨 IP 聚合）
	GlobalRatePerSecond    *int     `json:"globalRatePerSecond"`    // 全节点匿名请求每秒上限（跨 IP 聚合）
	GlobalBurst            *int     `json:"globalBurst"`            // 全节点匿名请求突发容量
	BanMinutes             *int     `json:"banMinutes"`             // 自动封禁时长（分钟）
	BlockedUserAgents      []string `json:"blockedUserAgents"`      // 静态封禁的 User-Agent 列表
	Enabled                *bool    `json:"enabled"`                // 仅为识别误配；防御不可关闭
}

// MCSMCompatConfig 配置文件 compat.mcsm 块：MCSManager Daemon 协议兼容。
// 开启后本节点在 prefix 子路径下同时提供：
//   - socket.io（engine.io v4）事件通道（MCSManager 面板按「节点」连接时使用）；
//   - /download/<票据>/<文件>、/upload/<票据> 直连传输通道与首页占位页。
//
// 认证沿用 MCSM 的「节点密钥」模型：面板节点配置里的 apiKey 必须与 key 一致。
// 该密钥等价于节点最高权限，请使用长随机串，且不要与账户密码复用。
type MCSMCompatConfig struct {
	Enabled       *bool    `json:"enabled"`       // 是否开启（默认 false，需显式开启）
	Prefix        string   `json:"prefix"`        // 挂载前缀（默认 /mcsm；面板节点 URL 需带上此前缀）
	Key           string   `json:"key"`           // 面板配对密钥（等价节点最高权限，必填）
	AllowPanelIPs []string `json:"allowPanelIps"` // 允许的面板来源 IP/CIDR（空 = 不限制来源）
}

// MSLXCompatConfig 配置文件 compat.mslx 块：MSLX Daemon 协议兼容。
// 开启后本节点在 prefix 子路径下提供 MSLX 的 REST 接口（/api/...）与
// SignalR 控制台/系统监视通道（/api/hubs/...），供 MSLX WebPanel 连接。
// 认证使用 JWT（HS256，「x-user-token」头 / access_token 查询参数）：
// 账号取自 username/password，签名密钥取自 jwtSecret
// （空 = 首次启动随机生成并写入 {data}/compat-mslx-secret，重启后保持）。
type MSLXCompatConfig struct {
	Enabled       *bool    `json:"enabled"`       // 是否开启（默认 false，需显式开启）
	Prefix        string   `json:"prefix"`        // 挂载前缀（默认 /mslx）
	Username      string   `json:"username"`      // 登录用户名（默认 admin）
	Password      string   `json:"password"`      // 登录密码（必填，建议长随机串）
	JWTSecret     string   `json:"jwtSecret"`     // JWT 签名密钥（空 = 自动生成并持久化）
	AllowPanelIPs []string `json:"allowPanelIps"` // 允许的面板来源 IP/CIDR（空 = 不限制来源）
}

// CompatConfig 配置文件 compat 块：第三方 Daemon 协议兼容（两个开关可同时开启）。
// 两层各自挂在独立子路径下，互不干扰，也不与节点自身 API 冲突。
// 底层处理逻辑全部为本节点自有代码：只对齐线上协议与字段语义，不引入其源码
// （避免 MCSManager 的 Apache-2.0 与 MSLX 的许可证与本项目产生许可证冲突）。
type CompatConfig struct {
	MCSM *MCSMCompatConfig `json:"mcsm"` // MCSManager Daemon 兼容（nil = 未配置）
	MSLX *MSLXCompatConfig `json:"mslx"` // MSLX Daemon 兼容（nil = 未配置）
}

// TimeServerConfig 配置文件 time.servers 条目：一台 Roughtime 服务器。
type TimeServerConfig struct {
	Name      string `json:"name"`      // 展示名（空 = 地址）
	Address   string `json:"address"`   // UDP 地址 host:port
	PublicKey string `json:"publicKey"` // 长期 Ed25519 公钥（base64，信任根）
	Protocol  string `json:"protocol"`  // auto（默认）/ legacy / rfc10049
}

// TimeConfig 配置文件 time 块：节点自有时间系统（docs/time-system.md）。
// 节点以 Roughtime 为唯一外部时间源，通过多源一致性判定与时钟伺服
// （漂移补偿 + slew/step）维护自己的时间，业务/TLS/会话全部使用该时间。
type TimeConfig struct {
	Enabled              *bool              `json:"enabled"`              // 是否启用 Roughtime 同步（默认 true）
	Servers              []TimeServerConfig `json:"servers"`              // 服务器列表（空 = 内置默认列表）
	IntervalSeconds      *int               `json:"intervalSeconds"`      // 同步间隔（秒，默认 300）
	TimeoutMillis        *int               `json:"timeoutMillis"`        // 单服务器查询超时（毫秒，默认 1500）
	Samples              *int               `json:"samples"`              // 每轮查询服务器数（默认 3）
	MinAgreement         *int               `json:"minAgreement"`         // 至少多少台一致才采纳（默认 2）
	MaxSlewPPM           *int               `json:"maxSlewPPM"`           // 相位校正最大速率（ppm，默认 500）
	MaxRatePPM           *int               `json:"maxRatePPM"`           // 频率校正钳制（ppm，默认 500）
	StepThresholdMillis  *int               `json:"stepThresholdMillis"`  // 超过该误差直接步进（毫秒，默认 2000）
	MaxSlewOffsetMillis  *int               `json:"maxSlewOffsetMillis"`  // 待校正量上限（毫秒，默认 10000）
	MinRateSpanMinutes   *int               `json:"minRateSpanMinutes"`   // 频率拟合最小跨度（分钟，默认 30）
	HoldoverLimitHours   *int               `json:"holdoverLimitHours"`   // 保持窗口（小时，默认 24）
	MaxUncertaintyMillis *int               `json:"maxUncertaintyMillis"` // 可信判定的不确定度上限（毫秒，默认 5000）
	HoldoverDriftPPM     *int               `json:"holdoverDriftPPM"`     // 保持模式漂移上界（ppm，默认 100）
	BootWaitMillis       *int               `json:"bootWaitMillis"`       // 启动等待首轮同步（毫秒，默认 1500；0 = 不等待）
	TLSRequireTrusted    *bool              `json:"tlsRequireTrusted"`    // 时间不可信时拒绝 TLS 握手（默认 false，避免离线节点自锁）
	StateFile            string             `json:"stateFile"`            // 状态文件（空 = {data}/timesys.json）
}

// Config config.json 配置文件结构。
// 布尔/整数字段用指针：nil = 配置未写该字段（回退默认值）；
// 非 nil = 显式设置（含 false/0，会覆盖内置默认值）。
// 字符串字段零值（""）与「未设置」语义天然一致（bind 回退环境变量/默认值、
// data 空 = 当前目录），无需指针。
type Config struct {
	Bind              string          `json:"bind"`              // 监听地址（IP 或主机名，如 127.0.0.1 / 0.0.0.0 / 192.168.1.5 / ::）
	Port              int             `json:"port"`              // 监听端口（1-65535；0 = 未设置）
	Data              string          `json:"data"`              // 数据目录（空 = 当前目录）
	AdminIntranetOnly *bool           `json:"adminIntranetOnly"` // 管理员账户仅允许内网来源（默认 true；false = 关闭限制）
	InstanceLog       *bool           `json:"instanceLog"`       // 实例日志落盘开关
	InstanceLogMax    *int            `json:"instanceLogMax"`    // 实例日志单文件轮转上限（MB）
	AuditLog          *bool           `json:"auditLog"`          // 审计日志落盘开关
	AuditLogMax       *int            `json:"auditLogMax"`       // 审计日志单文件轮转上限（MB）
	LoadTune          *bool           `json:"loadTune"`          // 负载自适应调谐开关
	LowResource       *bool           `json:"lowResource"`       // 强制嵌入式预设（nil = 按硬件自动判定）
	LogBufferKB       *int            `json:"logBufferKB"`       // 每实例日志环形缓冲上限（KB，nil = 用默认值）
	LogLines          *int            `json:"logLines"`          // 每实例行缓冲上限（行，nil = 用默认值）
	TransferAllowCIDR string          `json:"transferAllowCidr"` // 集群拉取放行内网 CIDR（逗号分隔）
	CorsOrigins       string          `json:"corsOrigins"`       // 额外放行的跨源面板来源（逗号分隔，如 https://panel.example.com）
	TLS               *TLSConfig      `json:"tls"`               // TLS 传输加密（nil = 未配置）
	Vault             *VaultConfig    `json:"vault"`             // 加密保险库（nil = 未配置）
	Accounts          *AccountsConfig `json:"accounts"`          // 账户管理存储（nil = 默认 SQLite）
	Defense           *DefenseConfig  `json:"defense"`           // DDoS/CC 防御调优（nil = 全部默认值）
	Time              *TimeConfig     `json:"time"`              // 节点自有时间系统（nil = 默认启用 + 内置服务器）
	Compat            *CompatConfig   `json:"compat"`            // 第三方 Daemon 协议兼容（nil = 两层均关闭）
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
	// 示例配置里 compat 的 key/password 为空；首次生成时直接填入随机值，
	// 保证「就算兼容层没开，配置里也有东西」，用户开启后无需自己编密钥。
	content := withGeneratedCompatSecrets(exampleConfigJSON)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// withGeneratedCompatSecrets 在示例配置文本里填入随机生成的兼容层凭据。
// 生成失败（几乎不可能）时原样返回，不影响配置文件创建。
func withGeneratedCompatSecrets(template []byte) []byte {
	text := string(template)
	text = fillEmptyJSONField(text, "mcsm", "key", randomToken(compatSecretLen))
	text = fillEmptyJSONField(text, "mslx", "password", randomToken(compatSecretLen))
	return []byte(text)
}

// fillEmptyJSONField 把 text 里 block 段内仍为空的 field 字段填上 value。
// 只在该字段确实为空时替换，模板被改过（已有值）就原样返回。
func fillEmptyJSONField(text, block, field, value string) string {
	if value == "" {
		return text
	}
	pos, ok := emptyJSONFieldPos(text, block, field)
	if !ok {
		return text
	}
	quoted, err := json.Marshal(value)
	if err != nil {
		return text
	}
	return text[:pos] + string(quoted) + text[pos+2:]
}

// emptyJSONFieldPos 定位 block 段内 "field": "" 中空字符串的起始引号下标。
//
// 只在 block 出现位置之后 4096 字节范围内查找：配置文件里同名键可能在别处
// 出现（如 _comment_* 说明文字里），限定范围可避免误改其它字段。
// 采用文本定位而不是整体 json 解析+重写，是为了保留用户配置里的注释顺序与排版。
func emptyJSONFieldPos(text, block, field string) (int, bool) {
	blockPos := strings.Index(text, `"`+block+`"`)
	if blockPos < 0 {
		return 0, false
	}
	limit := blockPos + 4096
	if limit > len(text) {
		limit = len(text)
	}
	fieldPos := strings.Index(text[blockPos:limit], `"`+field+`"`)
	if fieldPos < 0 {
		return 0, false
	}
	i := blockPos + fieldPos + len(field) + 2
	skipSpace := func() {
		for i < len(text) {
			switch text[i] {
			case ' ', '\t', '\r', '\n':
				i++
			default:
				return
			}
		}
	}
	skipSpace()
	if i >= len(text) || text[i] != ':' {
		return 0, false
	}
	i++
	skipSpace()
	if i+1 >= len(text) || text[i] != '"' || text[i+1] != '"' {
		return 0, false // 已有值（或不是字符串）：不动
	}
	return i, true
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
	// 容忍 UTF-8 BOM：Windows 记事本等编辑器保存的 JSON 会带 BOM，
	// 而 encoding/json 遇到 BOM 会直接报错（表现为「配置文件无法解析」）。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
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
	AdminIntranetOnly bool // 管理员账户仅允许内网来源（默认 true）
	InstanceLog       bool
	InstanceLogMax    int
	AuditLog          bool
	AuditLogMax       int
	LoadTune          bool
	LogBufferKB       int // 每实例日志环形缓冲上限（KB，0 = 用默认值）
	LogLines          int // 每实例行缓冲上限（行，0 = 用默认值）
	TransferAllowCIDR string
	CorsOrigins       string // 额外放行的跨源面板来源（逗号分隔）
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
	// 账户管理（docs/accounts-design.md）
	AccountsDriver             string
	AccountsDSN                string
	AccountsMaxOpen            int
	AccountsMaxIdle            int
	AccountsConnMaxLifetimeMin int
	RedisAddr                  string
	RedisPassword              string
	RedisDB                    int
	RedisPoolSize              int
	// DDoS/CC 防御调优（防御恒开，没有关闭项；0/空 = 用默认阈值）
	DefenseIPConcurrent int
	DefenseUAConcurrent int
	DefenseIPRate       int
	DefenseUARate       int
	DefenseGlobalRate   int
	DefenseGlobalBurst  int
	DefenseBanMinutes   int
	DefenseBlockedUAs   string // 静态封禁 UA（换行分隔；保持 nodeOptions 可比较）
	DefenseDisableWarn  bool   // 配置写了 defense.enabled=false（会被忽略）
	// 时间系统（docs/time-system.md）：Roughtime + 漂移补偿 + 时钟纪律
	TimeEnabled            bool   // 是否启用 Roughtime 同步
	TimeServers            string // 命令行覆盖的服务器列表（host:port@base64，逗号分隔）
	TimeServersConfig      string // 配置文件 time.servers 编码（每行 name|address|key|protocol；保持可比较）
	TimeIntervalSeconds    int
	TimeTimeoutMillis      int
	TimeSamples            int
	TimeMinAgreement       int
	TimeMaxSlewPPM         int
	TimeMaxRatePPM         int
	TimeStepMillis         int
	TimeMaxSlewOffsetMs    int
	TimeMinRateSpanMinutes int
	TimeHoldoverLimitHours int
	TimeMaxUncMillis       int
	TimeHoldoverDriftPPM   int
	TimeBootWaitMillis     int
	TimeTLSRequireTrusted  bool
	TimeStateFile          string
	// 第三方 Daemon 协议兼容（compat 块；两层可同时开启）
	ConfigPath        string // 配置文件实际路径（兼容层自动生成凭据后回写用；不参与合并）
	CompatMCSMEnabled bool   // MCSManager Daemon 兼容开关
	CompatMCSMPrefix  string // 挂载前缀（默认 /mcsm）
	CompatMCSMKey     string // 面板配对密钥
	CompatMCSMIPs     string // 允许的面板来源（换行分隔，保持 nodeOptions 可比较）
	CompatMSLXEnabled bool   // MSLX Daemon 兼容开关
	CompatMSLXPrefix  string // 挂载前缀（默认 /mslx）
	CompatMSLXUser    string // 登录用户名
	CompatMSLXPass    string // 登录密码
	CompatMSLXSecret  string // JWT 签名密钥（空 = 自动生成）
	CompatMSLXIPs     string // 允许的面板来源（换行分隔，保持 nodeOptions 可比较）
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
	if !setFlags["admin-intranet-only"] && cfg.AdminIntranetOnly != nil {
		o.AdminIntranetOnly = *cfg.AdminIntranetOnly
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
	if !setFlags["log-buffer-kb"] && cfg.LogBufferKB != nil {
		o.LogBufferKB = *cfg.LogBufferKB
	}
	if !setFlags["log-lines"] && cfg.LogLines != nil {
		o.LogLines = *cfg.LogLines
	}
	if !setFlags["transfer-allow-cidr"] && cfg.TransferAllowCIDR != "" {
		o.TransferAllowCIDR = cfg.TransferAllowCIDR
	}
	if !setFlags["cors-origins"] && cfg.CorsOrigins != "" {
		o.CorsOrigins = cfg.CorsOrigins
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
	if cfg.Accounts != nil {
		if !setFlags["accounts-driver"] && cfg.Accounts.Driver != "" {
			o.AccountsDriver = cfg.Accounts.Driver
		}
		if !setFlags["accounts-dsn"] && cfg.Accounts.DSN != "" {
			o.AccountsDSN = cfg.Accounts.DSN
		}
		if cfg.Accounts.MaxOpen != nil {
			o.AccountsMaxOpen = *cfg.Accounts.MaxOpen
		}
		if cfg.Accounts.MaxIdle != nil {
			o.AccountsMaxIdle = *cfg.Accounts.MaxIdle
		}
		if cfg.Accounts.ConnMaxLifetimeMin != nil {
			o.AccountsConnMaxLifetimeMin = *cfg.Accounts.ConnMaxLifetimeMin
		}
		if !setFlags["redis-addr"] && cfg.Accounts.RedisAddr != "" {
			o.RedisAddr = cfg.Accounts.RedisAddr
		}
		if !setFlags["redis-password"] && cfg.Accounts.RedisPassword != "" {
			o.RedisPassword = cfg.Accounts.RedisPassword
		}
		if !setFlags["redis-db"] && cfg.Accounts.RedisDB != 0 {
			o.RedisDB = cfg.Accounts.RedisDB
		}
		if cfg.Accounts.RedisPoolSize != nil {
			o.RedisPoolSize = *cfg.Accounts.RedisPoolSize
		}
	}
	if cfg.Defense != nil {
		// 防御不可关闭：enabled=false 不生效，只记录一条启动告警。
		if cfg.Defense.Enabled != nil && !*cfg.Defense.Enabled {
			o.DefenseDisableWarn = true
		}
		if cfg.Defense.MaxIPConcurrent != nil {
			o.DefenseIPConcurrent = *cfg.Defense.MaxIPConcurrent
		}
		if cfg.Defense.MaxUserAgentConcurrent != nil {
			o.DefenseUAConcurrent = *cfg.Defense.MaxUserAgentConcurrent
		}
		if cfg.Defense.IPRatePerSecond != nil {
			o.DefenseIPRate = *cfg.Defense.IPRatePerSecond
		}
		if cfg.Defense.UserAgentRatePerSecond != nil {
			o.DefenseUARate = *cfg.Defense.UserAgentRatePerSecond
		}
		if cfg.Defense.GlobalRatePerSecond != nil {
			o.DefenseGlobalRate = *cfg.Defense.GlobalRatePerSecond
		}
		if cfg.Defense.GlobalBurst != nil {
			o.DefenseGlobalBurst = *cfg.Defense.GlobalBurst
		}
		if cfg.Defense.BanMinutes != nil {
			o.DefenseBanMinutes = *cfg.Defense.BanMinutes
		}
		if len(cfg.Defense.BlockedUserAgents) > 0 {
			o.DefenseBlockedUAs = strings.Join(cfg.Defense.BlockedUserAgents, "\n")
		}
	}
	if cfg.Time != nil {
		tc := cfg.Time
		if !setFlags["time-sync"] && tc.Enabled != nil {
			o.TimeEnabled = *tc.Enabled
		}
		if !setFlags["time-servers"] && len(tc.Servers) > 0 {
			o.TimeServersConfig = encodeTimeServersConfig(tc.Servers)
		}
		if !setFlags["time-interval"] && tc.IntervalSeconds != nil {
			o.TimeIntervalSeconds = *tc.IntervalSeconds
		}
		if !setFlags["time-timeout-ms"] && tc.TimeoutMillis != nil {
			o.TimeTimeoutMillis = *tc.TimeoutMillis
		}
		if tc.Samples != nil {
			o.TimeSamples = *tc.Samples
		}
		if tc.MinAgreement != nil {
			o.TimeMinAgreement = *tc.MinAgreement
		}
		if tc.MaxSlewPPM != nil {
			o.TimeMaxSlewPPM = *tc.MaxSlewPPM
		}
		if tc.MaxRatePPM != nil {
			o.TimeMaxRatePPM = *tc.MaxRatePPM
		}
		if !setFlags["time-step-ms"] && tc.StepThresholdMillis != nil {
			o.TimeStepMillis = *tc.StepThresholdMillis
		}
		if tc.MaxSlewOffsetMillis != nil {
			o.TimeMaxSlewOffsetMs = *tc.MaxSlewOffsetMillis
		}
		if tc.MinRateSpanMinutes != nil {
			o.TimeMinRateSpanMinutes = *tc.MinRateSpanMinutes
		}
		if tc.HoldoverLimitHours != nil {
			o.TimeHoldoverLimitHours = *tc.HoldoverLimitHours
		}
		if tc.MaxUncertaintyMillis != nil {
			o.TimeMaxUncMillis = *tc.MaxUncertaintyMillis
		}
		if tc.HoldoverDriftPPM != nil {
			o.TimeHoldoverDriftPPM = *tc.HoldoverDriftPPM
		}
		if !setFlags["time-boot-wait-ms"] && tc.BootWaitMillis != nil {
			o.TimeBootWaitMillis = *tc.BootWaitMillis
		}
		if !setFlags["time-tls-require-trusted"] && tc.TLSRequireTrusted != nil {
			o.TimeTLSRequireTrusted = *tc.TLSRequireTrusted
		}
		if tc.StateFile != "" {
			o.TimeStateFile = tc.StateFile
		}
	}
	// 第三方 Daemon 协议兼容（compat 块）：两个开关独立，可同时开启。
	if cfg.Compat != nil {
		if mc := cfg.Compat.MCSM; mc != nil {
			if mc.Enabled != nil {
				o.CompatMCSMEnabled = *mc.Enabled
			}
			if mc.Prefix != "" {
				o.CompatMCSMPrefix = mc.Prefix
			}
			if mc.Key != "" {
				o.CompatMCSMKey = mc.Key
			}
			if len(mc.AllowPanelIPs) > 0 {
				o.CompatMCSMIPs = strings.Join(mc.AllowPanelIPs, "\n")
			}
		}
		if mx := cfg.Compat.MSLX; mx != nil {
			if mx.Enabled != nil {
				o.CompatMSLXEnabled = *mx.Enabled
			}
			if mx.Prefix != "" {
				o.CompatMSLXPrefix = mx.Prefix
			}
			if mx.Username != "" {
				o.CompatMSLXUser = mx.Username
			}
			if mx.Password != "" {
				o.CompatMSLXPass = mx.Password
			}
			if mx.JWTSecret != "" {
				o.CompatMSLXSecret = mx.JWTSecret
			}
			if len(mx.AllowPanelIPs) > 0 {
				o.CompatMSLXIPs = strings.Join(mx.AllowPanelIPs, "\n")
			}
		}
	}
}
