//go:build windows

// 进程级采样（Windows：GetProcessTimes + GetProcessMemoryInfo via syscall）。

package main

import (
	"syscall"
	"unsafe"
)

var (
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	procOpenProcess          = kernel32.NewProc("OpenProcess")
	procGetProcessTimes      = kernel32.NewProc("GetProcessTimes")
	procCloseHandle          = kernel32.NewProc("CloseHandle")
)

// processQueryLimitedInfo 仅查询权限（无需完全访问权即可读进程信息）。
const processQueryLimitedInfo = 0x1000

// processMemoryCounters PROCESS_MEMORY_COUNTERS（psapi）。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uint64
	WorkingSetSize             uint64
	QuotaPeakPagedPoolUsage    uint64
	QuotaPagedPoolUsage        uint64
	QuotaPeakNonPagedPoolUsage uint64
	QuotaNonPagedPoolUsage     uint64
	PagefileUsage              uint64
	PeakPagefileUsage          uint64
}

// openProcessHandle 打开进程句柄（失败返回 0）。
func openProcessHandle(pid int) uintptr {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	return h
}

// procCPUTicks 读取进程 CPU 时间（用户 + 内核，单位 100ns）。
func procCPUTicks(pid int) (uint64, bool) {
	h := openProcessHandle(pid)
	if h == 0 {
		return 0, false
	}
	defer procCloseHandle.Call(h)
	var creation, exit, kernel, user syscall.Filetime
	r, _, _ := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return 0, false
	}
	k := uint64(kernel.LowDateTime) | uint64(kernel.HighDateTime)<<32
	u := uint64(user.LowDateTime) | uint64(user.HighDateTime)<<32
	return k + u, true
}

// procMemoryBytes 读取进程工作集大小（字节，WorkingSetSize）。
func procMemoryBytes(pid int) int64 {
	h := openProcessHandle(pid)
	if h == 0 {
		return 0
	}
	defer procCloseHandle.Call(h)
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	r, _, _ := procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if r == 0 {
		return 0
	}
	return int64(pmc.WorkingSetSize)
}

// procTicksPerSec Windows CPU 时间单位为 100ns，每秒 1e7 滴答。
func procTicksPerSec() float64 { return 1e7 }
