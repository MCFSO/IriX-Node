//go:build freebsd

package main

import (
	"io"
	"syscall"
)

// sysProcAttr 为 FreeBSD 受管进程创建独立进程组。
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

type freebsdProcessGroup struct{ pid int }

func (g freebsdProcessGroup) Close() error {
	err := syscall.Kill(-g.pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func attachProcessContainment(pid int) (io.Closer, error) {
	return freebsdProcessGroup{pid: pid}, nil
}
