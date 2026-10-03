package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRecoverOrphanProcess 验证异常退出后节点可凭落盘记录终止遗留进程。
func TestRecoverOrphanProcess(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("当前平台尚未实现持久化进程身份")
	}
	d, dir := newTestDaemon(t)
	inst := NewInstance("recovery-case", InstanceConfig{StartCommand: longRunCommand(), Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.startInstance(inst); err != nil {
		t.Fatal(err)
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	defer func() { _ = proc.Kill() }()
	if _, err := os.Stat(d.processRecordPath(inst.InstanceUuid)); err != nil {
		t.Fatalf("启动后未落盘进程身份: %v", err)
	}
	data, err := os.ReadFile(d.processRecordPath(inst.InstanceUuid))
	if err != nil || bytes.Contains(data, []byte(inst.InstanceUuid)) {
		t.Fatalf("进程记录不应暴露实例标识: %v", err)
	}
	restarted := NewDaemon(dir, "test-key")
	if err := restarted.recoverOrphanProcesses(); err != nil {
		t.Fatalf("清理遗留进程失败: %v", err)
	}
	if proc.IsRunning() {
		t.Fatal("恢复后遗留进程仍在运行")
	}
	if _, err := os.Stat(d.processRecordPath(inst.InstanceUuid)); !os.IsNotExist(err) {
		t.Fatalf("恢复后进程记录仍存在: %v", err)
	}
}

// TestRecoverIgnoresReusedPID 身份不匹配时不能终止现有进程。
func TestRecoverIgnoresReusedPID(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("当前平台尚未实现持久化进程身份")
	}
	d, _ := newTestDaemon(t)
	record := processRecord{Key: processRecordKey("reused-pid"), PID: os.Getpid(), Identity: "不匹配的创建标识", Jail: "old-jail", JID: 1, Watch: true}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := d.processRecordFile(record)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.recoverOrphanProcesses(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("陈旧进程记录未删除: %v", err)
	}
}
