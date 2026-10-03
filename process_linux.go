//go:build linux || android

package main

import (
	"io"
	"syscall"
)

// sysProcAttr 为实例创建独立进程组，便于异常退出后清理子进程树。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

type processGroup struct{ pid int }

func (g processGroup) Close() error {
	err := syscall.Kill(-g.pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

// attachProcessContainment 在主进程退出后清理仍在同一进程组中的子进程。
func attachProcessContainment(pid int) (io.Closer, error) { return processGroup{pid: pid}, nil }
