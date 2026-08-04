//go:build linux

// Linux 系统信息采集（解析 /proc）。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// osUptime 系统运行秒数。
func osUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			free = v * 1024
		}
	}
	return total, free
}

// osVersion 读取 Linux 内核版本。
func osVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return runtime.GOARCH
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return runtime.GOARCH
}
