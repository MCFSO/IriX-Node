//go:build windows

package sysinfo

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

func ProcessIdentity(pid int) (string, error) {
	h := openProcessHandle(pid)
	if h == 0 {
		return "", os.ErrNotExist
	}
	defer procCloseHandle.Call(h)
	var creation, exit, kernel, user syscall.Filetime
	r, _, err := procGetProcessTimes.Call(h, uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return "", fmt.Errorf("读取进程创建时间失败: %w", err)
	}
	if exit.LowDateTime != 0 || exit.HighDateTime != 0 {
		return "", os.ErrNotExist
	}
	return strconv.FormatUint(uint64(creation.LowDateTime)|uint64(creation.HighDateTime)<<32, 10), nil
}

func TerminateRecordedProcess(pid int, identity string) error {
	// 先核对，再按进程树终止。PID 复用的窗口由创建时间核对约束。
	current, err := ProcessIdentity(pid)
	if os.IsNotExist(err) || err == nil && current != identity {
		return nil
	}
	if err != nil {
		return err
	}
	out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	if err != nil {
		// 受限账户可能无法调用 taskkill；至少终止已验证身份的主进程。
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			return fmt.Errorf("终止进程树失败: %w: %s", err, out)
		}
		if killErr := proc.Kill(); killErr != nil {
			_ = proc.Release()
			return fmt.Errorf("终止进程树失败: %w: %s；直接终止失败: %v", err, out, killErr)
		}
		_ = proc.Release()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err = ProcessIdentity(pid)
		if os.IsNotExist(err) || err == nil && current != identity {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("进程 %d 未在期限内退出", pid)
}
