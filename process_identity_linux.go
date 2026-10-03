//go:build linux || android

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
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

func terminateRecordedProcess(record processRecord) error {
	current, err := processIdentity(record.PID)
	if os.IsNotExist(err) || err == nil && current != record.Identity {
		return nil
	}
	if err != nil {
		return err
	}
	// 节点创建独立进程组，包含由启动程序拉起的 Java 子进程。
	if err := syscall.Kill(-record.PID, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err = processIdentity(record.PID)
		if os.IsNotExist(err) || err == nil && current != record.Identity {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("进程 %s 未在期限内退出", strconv.Itoa(record.PID))
}
