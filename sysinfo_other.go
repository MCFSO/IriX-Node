//go:build !windows && !linux && !freebsd && !openbsd && !darwin

// 其他平台系统信息采集：暂不支持，返回零值。

package main

import "runtime"

// osUptime 系统运行秒数。
func osUptime() float64 { return 0 }

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) { return 0, 0 }

// osDisk 返回路径所在文件系统总容量与可用容量（字节）。
func osDisk(path string) (total, free uint64) { return 0, 0 }

// netCounters 读取网络收发总字节数。
func netCounters() (rx, tx uint64) { return 0, 0 }

// osDistro 发行版版本。
func osDistro() string { return "" }

// osVersion 发行版本。
func osVersion() string { return runtime.GOARCH }
