//go:build windows

// Windows 进程属性：隐藏控制台窗口。

package main

import "syscall"

// sysProcAttr 返回 Windows 专属的进程启动属性。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
