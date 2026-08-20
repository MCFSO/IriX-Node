//go:build !linux && !windows

// 进程级采样（其他平台：暂无 CPU/内存采集，返回 0/不可用）。

package main

// procCPUTicks 其他平台不支持，返回不可用（stats 接口相应字段为 0）。
func procCPUTicks(pid int) (uint64, bool) { return 0, false }

// procMemoryBytes 其他平台返回 0。
func procMemoryBytes(pid int) int64 { return 0 }

// procTicksPerSec 其他平台占位。
func procTicksPerSec() float64 { return 1 }
