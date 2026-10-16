//go:build !linux && !android && !windows

// 进程级采样（其他平台：暂无 CPU/内存采集，返回 0/不可用）。

package sysinfo

// ProcCPUTicks 其他平台不支持，返回不可用（stats 接口相应字段为 0）。
func ProcCPUTicks(pid int) (uint64, bool) { return 0, false }

// ProcMemoryBytes 其他平台返回 0。
func ProcMemoryBytes(pid int) int64 { return 0 }

// ProcTicksPerSec 其他平台占位。
func ProcTicksPerSec() float64 { return 1 }
