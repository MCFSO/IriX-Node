//go:build windows

// CPU 累计时间采样：Windows（kernel32!GetSystemTimes）。
//
// GetSystemTimes 返回三个 FILETIME：空闲、内核、用户。内核时间**已包含**
// 空闲时间，因此：
//
//	busy = kernel + user - idle
//	idle = idle
//
// 与 Linux 的 jiffies 语义一致，调用方只需比较两次采样的差值。
// 只用标准库 syscall 调用系统 API，不引入额外依赖、也不需要 cgo。
package sysinfo

import (
	"syscall"
	"unsafe"
)

var (
	kernel32ProcGetSystemTimes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemTimes")
)

// filetimeToUint64 把 FILETIME（高低位各 32 位）拼成 100ns 为单位的计数。
func filetimeToUint64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// sysCPUTimes 返回 (busy, idle) 累计时间（100ns 单位）。
func sysCPUTimes() (busy, idle uint64, ok bool) {
	var idleFT, kernelFT, userFT syscall.Filetime
	r1, _, _ := kernel32ProcGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if r1 == 0 {
		return 0, 0, false
	}
	idle = filetimeToUint64(idleFT)
	kernel := filetimeToUint64(kernelFT)
	user := filetimeToUint64(userFT)
	// 内核时间已含空闲时间，扣掉才是真正的忙碌时间
	if kernel+user > idle {
		busy = kernel + user - idle
	}
	return busy, idle, true
}
