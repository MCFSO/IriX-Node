//go:build windows

// Windows 系统信息采集（kernel32 via syscall）。

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32    = syscall.NewLazyDLL("kernel32.dll")
	procTick64  = kernel32.NewProc("GetTickCount64")
	procMemStat = kernel32.NewProc("GlobalMemoryStatusEx")
	procDisk    = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// memoryStatusEx Windows MEMORYSTATUSEX 结构。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// osUptime 系统运行秒数。
func osUptime() float64 {
	r, _, _ := procTick64.Call()
	if r != 0 {
		return float64(uint64(r)) / 1000.0
	}
	return 0
}

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	r, _, _ := procMemStat.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0, 0
	}
	return st.TotalPhys, st.AvailPhys
}

// osDisk 返回路径所在磁盘总容量与可用容量（字节，GetDiskFreeSpaceExW）。
// 后两个参数可传 NULL（返回 0）：只关心总量与剩余量，忽略「可调用者可用」。
func osDisk(path string) (total, free uint64) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	r, _, _ := procDisk.Call(
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r == 0 {
		return 0, 0
	}
	return total, free
}

// netCounters 读取网络收发总字节数（netstat -e 接口统计）。
// 不按 "Bytes" 关键词匹配：中文系统该行显示为「字节」，改为取标题下
// 第一行两个纯数字列（即字节计数行，顺序固定），避免本地化差异。
func netCounters() (rx, tx uint64) {
	out, err := exec.Command("netstat", "-e").Output()
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		r, err1 := strconv.ParseUint(strings.ReplaceAll(fields[0], ",", ""), 10, 64)
		t, err2 := strconv.ParseUint(strings.ReplaceAll(fields[1], ",", ""), 10, 64)
		if err1 == nil && err2 == nil && (r > 0 || t > 0) {
			return r, t
		}
	}
	return 0, 0
}

// osDistro 发行版版本：Windows 无发行版概念，返回空串由调用方回退 release。
func osDistro() string { return "" }

// osTypePlatform 返回 Windows 平台标识（hostInfo 在 windows 下走专用分支，
// 此处仅作编译期兜底定义）。
func osTypePlatform() string { return "win32" }

// osVersion 读取 Windows 版本号。
func osVersion() string {
	data, err := os.ReadFile("C:/Windows/System32/Release.txt")
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data))
	}
	if v := os.Getenv("OS"); v != "" {
		return v + " (" + runtime.GOARCH + ")"
	}
	return runtime.GOARCH
}
