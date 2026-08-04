//go:build freebsd || openbsd

// BSD 系统信息采集（sysctl，零依赖、无 cgo）。

package main

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// sysctl 执行 sysctl -n 读取内核参数。
func sysctl(name string) (string, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// freePagesName 空闲内存页数的 sysctl 参数名。
func freePagesName() string {
	if runtime.GOOS == "openbsd" {
		return "uvmexp.free"
	}
	return "vm.stats.vm.v_free_count"
}

// osUptime 系统运行秒数（自 kern.boottime 推算）。
func osUptime() float64 {
	bt, err := sysctl("kern.boottime")
	if err != nil {
		return 0
	}
	// 形如 { sec = 1712345678, usec = 123456 }
	parts := strings.Split(bt, "=")
	if len(parts) < 2 {
		return 0
	}
	secStr := strings.Fields(strings.ReplaceAll(parts[1], ",", ""))
	if len(secStr) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(secStr[0], 64)
	if err != nil || sec <= 0 {
		return 0
	}
	return float64(time.Now().Unix()) - sec
}

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) {
	totalStr, err := sysctl("hw.physmem")
	if err != nil {
		return 0, 0
	}
	total, _ = strconv.ParseUint(totalStr, 10, 64)
	if total == 0 {
		return 0, 0
	}
	// 空闲内存 = 空闲页数 × 页大小
	freePages, err1 := sysctl(freePagesName())
	pageSize, err2 := sysctl("hw.pagesize")
	if err1 != nil || err2 != nil {
		return total, 0
	}
	fp, _ := strconv.ParseUint(freePages, 10, 64)
	ps, _ := strconv.ParseUint(pageSize, 10, 64)
	return total, fp * ps
}

// osVersion 读取发行版本。
func osVersion() string {
	v, err := sysctl("kern.osrelease")
	if err != nil || v == "" {
		return runtime.GOARCH
	}
	return v
}
