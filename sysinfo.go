// 系统信息采集：内存、运行时间、CPU 使用率。
// 平台相关的采集逻辑拆到 sysinfo_windows.go / sysinfo_linux.go /
// sysinfo_freebsd.go / sysinfo_other.go，本文件只保留通用逻辑。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// uptimeSeconds 系统运行秒数。
func uptimeSeconds() float64 {
	return osUptime()
}

// systemMem 返回总内存与可用内存（字节）。
func systemMem() (total, free uint64) {
	return osMem()
}

// totalMem 总内存（字节）。
func totalMem() uint64 {
	total, _ := systemMem()
	return total
}

// freeMem 可用内存（字节）。
func freeMem() uint64 {
	_, free := systemMem()
	return free
}

// memUsage 内存使用率 (0-1)。
func memUsage() float64 {
	total, free := systemMem()
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total)
}

// cpuUsage CPU 使用率（Linux 下采样 500ms 的近似值，其他平台返回 0）。
func cpuUsage() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	prev := cpuJiffies()
	if prev == 0 {
		return 0
	}
	time.Sleep(500 * time.Millisecond)
	curr := cpuJiffies()
	if curr < prev {
		return 0
	}
	idle := 500.0 * float64(runtime.NumCPU())
	return (curr - prev) / (curr - prev + idle)
}

// cpuJiffies 读取 /proc/stat 的 CPU 忙碌 jiffies。
func cpuJiffies() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.Fields(string(data))
	if len(line) < 5 || line[0] != "cpu" {
		return 0
	}
	var total uint64
	for _, v := range line[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
	}
	return float64(total)
}

// processAlloc 当前进程堆内存占用（字节）。
func processAlloc() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Alloc
}

// hostInfo 返回 (系统类型, 平台, 发行版本)。
func hostInfo() (osType, platform, release string) {
	if runtime.GOOS == "windows" {
		return "Windows_NT", "win32", osVersion()
	}
	return runtime.GOOS, runtime.GOOS, osVersion()
}
