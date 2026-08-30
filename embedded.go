// 嵌入式/低资源平台自适应：守护进程启动时按硬件能力自动套用「嵌入式预设」，
// 让 ARM 开发板、MIPS 路由器、Android Termux、OpenHarmony 等受限设备在
// 保留全部功能的前提下，自动按 CPU 核数/物理内存调优，避免内存爆满或慢核卡顿。
//
// 设计原则（与 AGENTS.md 一致）：
//   - 功能一个不少：本文件只调整「默认值」，所有 knob 均可被命令行/配置文件显式覆盖
//     （优先级：命令行显式参数 > 配置文件 > 本预设 > 内置默认）。
//   - 不引入 -tags minimal 等编译期阉割；Vault/Redis/SQLite 能力全保留。
//   - 利用平台 CPU 特性的做法：arm64 白嫖 Go 标准库对 SHA-2/AES 硬件指令的自动探测
//     （无需手写汇编）；MIPS/armv7 无硬件 crypto，故对 CPU 密集的 PBKDF2 给出更低
//     默认迭代下限，避免数秒级解锁延迟。

package main

import (
	"runtime"
)

// 嵌入式预设阈值：满足其一即判定为「低资源环境」，自动套用更激进的默认值。
const (
	// embeddedMaxCPU 核数上限：≤2 核（单核路由器/双核开发板）视为嵌入式。
	embeddedMaxCPU = 2
	// embeddedMaxMemBytes 物理内存上限：≤512MB 视为嵌入式。
	embeddedMaxMemBytes = 512 * 1024 * 1024
)

// 嵌入式档位下的默认内存 knob（仅为上限；最终值可被命令行/配置覆盖）。
const (
	embeddedLogBufferKB = 512 // 每实例日志环形缓冲上限（KB），默认 2MB → 512KB
	embeddedLogLines    = 300 // 每实例行缓冲上限（行），默认 1000 → 300
	embeddedMetricsRing = 30  // 指标环形保留条数（15s × 30 = 7.5 分钟），默认 60
	// embeddedPBKDF2Iterations MIPS/armv7（无硬件 SHA）下 Vault 默认 PBKDF2 迭代：
	// 仍为 OWASP 级安全值，但远低于 arm64 的 600000，避免软浮点/无指令加速设备
	// 解锁时数秒级阻塞。用户可用 -vault-pbkdf2-iterations 显式覆盖。
	embeddedPBKDF2Iterations = 100000
)

// 常规（非嵌入式）默认内存 knob。
const (
	defaultLogBufferKB = 2 * 1024 // 每实例日志环形缓冲默认 2MB
	defaultLogLines    = 1000     // 每实例行缓冲默认 1000 行
	defaultMetricsRing = 60       // 指标环形默认保留 60 条（15 分钟）
)

// embeddedProfile 保存自动检测结果与套用的预设档位。
// 显式 -low-resource 可强制开/关；未设置时按硬件能力自动判定。
type embeddedProfile struct {
	enabled bool   // 是否套用嵌入式预设
	cpu     int    // 检测到的 CPU 核数
	mem     uint64 // 检测到的物理总内存（字节，0 = 不可得）
	reason  string // 判定原因（日志/调试展示）
}

// embedded 全局嵌入式检测结果（启动时一次性填充）。
var embedded = detectEmbedded()

// detectEmbedded 探测当前是否处于低资源环境。
func detectEmbedded() embeddedProfile {
	cpu := runtime.NumCPU()
	total, _ := systemMem() // 已有 sysinfo 跨平台封装，返回 (total, free)
	reason := ""
	switch {
	case cpu <= embeddedMaxCPU:
		reason = "CPU 核数少"
	case total > 0 && total <= embeddedMaxMemBytes:
		reason = "物理内存小"
	}
	return embeddedProfile{enabled: reason != "", cpu: cpu, mem: total, reason: reason}
}

// applyLowResourceOverride 用显式 -low-resource 开关覆盖自动检测结果。
// forced 为 nil 表示用户未显式设置（回退自动检测）；非 nil 则按用户意图强制开/关。
func (p *embeddedProfile) applyLowResourceOverride(forced *bool) {
	if forced == nil {
		return
	}
	if *forced {
		if p.reason == "" {
			p.reason = "显式 -low-resource"
		}
		p.enabled = true
	} else {
		p.enabled = false
		p.reason = "显式关闭"
	}
}

// isEmbedded 当前是否套用嵌入式预设。
func (p *embeddedProfile) isEmbedded() bool {
	return p.enabled
}

// hasHardwareSHA 当前架构是否具备 Go 标准库自动利用的 SHA-256 硬件指令。
// arm64 经 internal/cpu 探测 HasSHA2 后由 crypto/sha256 自动加速；
// 其他架构（含 MIPS 全系、armv7、x86 旧核）回退软件实现，PBKDF2 成本高。
// 这里用 GOARCH 粗判（不与运行时探测耦合），仅用于选择 PBKDF2 默认迭代档位。
func hasHardwareSHA() bool {
	switch runtime.GOARCH {
	case "arm64", "amd64", "s390x", "ppc64le":
		return true
	}
	return false
}

// defaultPBKDF2ForPlatform 按架构返回 Vault 默认 PBKDF2 迭代：
// 具备硬件 SHA 的平台维持默认 600000；其余平台给更低默认，避免慢核解锁阻塞。
// 注意：这只是「默认值」来源，用户仍可用 -vault-pbkdf2-iterations 覆盖，
// 且 main.go 的强制下限（≥10000）语义不变。
func defaultPBKDF2ForPlatform() int {
	if hasHardwareSHA() {
		return defaultPBKDF2Iterations // 600000（见 vault_crypto.go）
	}
	return embeddedPBKDF2Iterations // 100000
}
