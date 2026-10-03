//go:build freebsd

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// processIdentity 使用进程创建时间识别 FreeBSD PID，避免复用后误杀。
func processIdentity(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "lstart=", "-p", fmt.Sprint(pid)).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(strings.TrimSpace(string(out))) == 0 {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("读取进程创建时间失败: %w: %s", err, out)
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", os.ErrNotExist
	}
	return identity, nil
}

func terminateRecordedProcess(record processRecord) error {
	current, err := processIdentity(record.PID)
	if os.IsNotExist(err) || err == nil && current != record.Identity {
		return nil
	}
	if err != nil {
		return err
	}
	if err := syscall.Kill(-record.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
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
	return fmt.Errorf("进程 %d 未在期限内退出", record.PID)
}
