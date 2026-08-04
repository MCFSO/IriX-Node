//go:build windows

// Windows 系统信息采集（kernel32 via syscall）。

package main

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32    = syscall.NewLazyDLL("kernel32.dll")
	procTick64  = kernel32.NewProc("GetTickCount64")
	procMemStat = kernel32.NewProc("GlobalMemoryStatusEx")
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
