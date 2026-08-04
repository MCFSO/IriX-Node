//go:build !windows && !linux && !freebsd && !openbsd

// 其他平台（darwin 等）系统信息采集：暂不支持，返回零值。

package main

import "runtime"

// osUptime 系统运行秒数。
func osUptime() float64 { return 0 }

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) { return 0, 0 }

// osVersion 发行版本。
func osVersion() string { return runtime.GOARCH }
