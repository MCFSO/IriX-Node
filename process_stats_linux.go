//go:build linux

// 进程级采样（Linux：/proc/<pid>/stat + statm）。

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// procCPUTicks 读取进程 CPU 时间（用户 + 系统，单位 jiffies）。
func procCPUTicks(pid int) (uint64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')') // 进程名可能含空格，从最后一个 ) 后取字段
	if i < 0 || i+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	// 第 12、13 个字段（0 基下标 11、12）为 utime、stime
	if len(fields) < 14 {
		return 0, false
	}
	ut, _ := strconv.ParseUint(fields[11], 10, 64)
	st, _ := strconv.ParseUint(fields[12], 10, 64)
	return ut + st, true
}

// procMemoryBytes 读取进程常驻内存（字节，/proc/<pid>/statm 的 RSS 页数）。
func procMemoryBytes(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	rss, _ := strconv.ParseUint(fields[1], 10, 64)
	return int64(rss * uint64(os.Getpagesize()))
}

// hz 缓存：Linux USER_HZ 通常为 100，但不可移植假设，用 /proc/stat 实测校准。
var (
	hzMu sync.Mutex
	hz   float64
	hzAt time.Time
)

// procTicksPerSec 每秒钟的时钟滴答数（Linux：/proc/stat 实测校准）。
func procTicksPerSec() float64 {
	hzMu.Lock()
	defer hzMu.Unlock()
	if hz > 0 && time.Since(hzAt) < 10*time.Second {
		return hz
	}
	j1 := cpuJiffies()
	time.Sleep(200 * time.Millisecond)
	j2 := cpuJiffies()
	hzAt = time.Now()
	if j2 > j1 {
		hz = (j2 - j1) / 0.2
	}
	if hz <= 0 {
		hz = 100 // 兜底：x86 Linux 标准 USER_HZ
	}
	return hz
}
