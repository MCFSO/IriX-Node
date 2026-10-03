//go:build windows

// Windows 进程属性：隐藏控制台窗口。

package main

import (
	"io"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sysProcAttr 返回 Windows 专属的进程启动属性。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

type processJob struct{ handle windows.Handle }

func (j *processJob) Close() error { return windows.CloseHandle(j.handle) }

// attachProcessContainment 让 Windows 在节点异常退出或主进程结束时清理整个进程树。
func attachProcessContainment(pid int) (io.Closer, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var limit windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limit.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limit)), uint32(unsafe.Sizeof(limit))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &processJob{handle: job}, nil
}
