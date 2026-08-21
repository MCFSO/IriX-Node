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

// Config config.json 配置文件结构。
// 布尔/整数字段用指针：nil = 配置未写该字段（回退默认值）；
// 非 nil = 显式设置（含 false/0，会覆盖内置默认值）。
// 字符串字段零值（""）与「未设置」语义天然一致（bind 回退环境变量/默认值、
// apiKey 空 = 配对码机制、data 空 = 当前目录），无需指针。
type Config struct {
	Bind              string `json:"bind"`              // 监听地址（IP 或主机名，如 127.0.0.1 / 0.0.0.0 / 192.168.1.5 / ::）
	Port              int    `json:"port"`              // 监听端口（1-65535；0 = 未设置）
	Data              string `json:"data"`              // 数据目录（空 = 当前目录）
	APIKey            string `json:"apiKey"`            // 固定 API 密钥（空 = 启用配对码机制）
	InstanceLog       *bool  `json:"instanceLog"`       // 实例日志落盘开关
	InstanceLogMax    *int   `json:"instanceLogMax"`    // 实例日志单文件轮转上限（MB）
	AuditLog          *bool  `json:"auditLog"`          // 审计日志落盘开关
	AuditLogMax       *int   `json:"auditLogMax"`       // 审计日志单文件轮转上限（MB）
	LoadTune          *bool  `json:"loadTune"`          // 负载自适应调谐开关
	TransferAllowCIDR string `json:"transferAllowCidr"` // 集群拉取放行内网 CIDR（逗号分隔）
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
}
