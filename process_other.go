//go:build !windows && !linux && !android && !freebsd

// 非 Windows 平台无需特殊进程属性。

package main

import (
	"io"
	"syscall"
)

// sysProcAttr 返回 Unix 平台的进程启动属性。
func sysProcAttr() *syscall.SysProcAttr {
	return nil
}

func attachProcessContainment(pid int) (io.Closer, error) { return nil, nil }
