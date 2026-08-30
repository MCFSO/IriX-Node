//go:build darwin

// macOS 系统信息采集：内存/运行时间/版本（sysctl）+ 磁盘（statfs）+ 网络（netstat -ib）。

package main

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sysctl -n 读取内核参数。
func sysctl(name string) (string, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
	totalStr, err := sysctl("hw.memsize")
	if err != nil {
		return 0, 0
	}
	total, _ = strconv.ParseUint(totalStr, 10, 64)
	// 空闲内存 = 空闲页数 × 页大小（vm.page_free_count 仅在内存压力不大时准确，容错）
	freePages, err1 := sysctl("vm.page_free_count")
	pageSize, err2 := sysctl("hw.pagesize")
	if err1 != nil || err2 != nil {
		return total, 0
	}
	fp, _ := strconv.ParseUint(freePages, 10, 64)
	ps, _ := strconv.ParseUint(pageSize, 10, 64)
	return total, fp * ps
}

// osDisk 返回路径所在文件系统总容量与可用容量（字节，statfs）。
func osDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)
}

// netCounters 读取网络收发总字节数（netstat -ib 解析 Ibytes/Obytes 列）。
func netCounters() (rx, tx uint64) {
	out, err := exec.Command("netstat", "-ib").Output()
	if err != nil {
		return 0, 0
	}
	return parseNetstatIB(string(out))
}

// osDistro 发行版版本：macOS 无发行版号概念，返回空串由调用方回退 release。
func osDistro() string { return "" }

// osTypePlatform 返回 macOS 平台标识。
func osTypePlatform() string { return runtime.GOOS }

// osVersion 读取内核版本。
func osVersion() string {
	v, err := sysctl("kern.osrelease")
	if err != nil || v == "" {
		return runtime.GOARCH
	}
	return v
}
