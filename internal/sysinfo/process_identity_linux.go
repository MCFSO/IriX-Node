//go:build linux || android

package sysinfo

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func ProcessIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// 进程刚退出时内核可能返回 ESRCH（"no such process"）而不是 ENOENT
		// （"no such file or directory"），两者都表示「进程已不在」。
		// 统一映射成 os.ErrNotExist，调用方（记录/强杀/孤儿清理）才能按
		// 「进程已退出」处理，否则会把正常退出误判成 500 错误。
		if errors.Is(err, syscall.ESRCH) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	pos := strings.LastIndexByte(string(data), ')')
	if pos < 0 {
		return "", fmt.Errorf("进程 %d 状态格式错误", pid)
	}
	fields := strings.Fields(string(data[pos+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("进程 %d 状态字段不完整", pid)
	}
	return fields[19], nil // /proc stat 的第 22 字段：启动时的内核时钟滴答
}

func TerminateRecordedProcess(pid int, identity string) error {
	current, err := ProcessIdentity(pid)
	if os.IsNotExist(err) || err == nil && current != identity {
		return nil
	}
	if err != nil {
		return err
	}
	// 节点创建独立进程组，包含由启动程序拉起的 Java 子进程。
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
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
	return fmt.Errorf("进程 %s 未在期限内退出", strconv.Itoa(pid))
}
