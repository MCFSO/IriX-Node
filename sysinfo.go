// 系统信息采集：内存、运行时间、CPU 使用率。
// 纯标准库实现，Windows 使用 kernel32 API，Linux 解析 /proc。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---- Windows 系统信息（kernel32 via syscall）----

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	procTick64 = kernel32.NewProc("GetTickCount64")
	procMemStat = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx Windows MEMORYSTATUSEX 结构。
type memoryStatusEx struct {
	Length                uint32
	MemoryLoad            uint32
	TotalPhys             uint64
	AvailPhys             uint64
	TotalPageFile         uint64
	AvailPageFile         uint64
	TotalVirtual          uint64
	AvailVirtual          uint64
	AvailExtendedVirtual  uint64
}

// uptimeSeconds 系统运行秒数。
func uptimeSeconds() float64 {
	if runtime.GOOS == "windows" {
		r, _, _ := procTick64.Call()
		if r != 0 {
			return float64(uint64(r)) / 1000.0
		}
		return 0
	}
	// Linux /proc/uptime
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

// systemMem 返回总内存与可用内存（字节）。
func systemMem() (total, free uint64) {
	if runtime.GOOS == "windows" {
		var st memoryStatusEx
		st.Length = uint32(unsafe.Sizeof(st))
		r, _, _ := procMemStat.Call(uintptr(unsafe.Pointer(&st)))
		if r == 0 {
			return 0, 0
		}
		return st.TotalPhys, st.AvailPhys
	}
	// Linux /proc/meminfo
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
	switch runtime.GOOS {
	case "windows":
		return "Windows_NT", "win32", windowsVersion()
	case "linux":
		return "Linux", "linux", linuxVersion()
	case "darwin":
		return "Darwin", "darwin", runtime.GOARCH
	default:
		return runtime.GOOS, runtime.GOOS, runtime.GOARCH
	}
}

// windowsVersion 读取 Windows 版本号。
func windowsVersion() string {
	data, err := os.ReadFile("C:/Windows/System32/Release.txt")
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data))
	}
	if v := os.Getenv("OS"); v != "" {
		return v + " (" + runtime.GOARCH + ")"
	}
	return runtime.GOARCH
}

// linuxVersion 读取 Linux 内核版本。
func linuxVersion() string {
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
