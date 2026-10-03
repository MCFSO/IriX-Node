package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// processRecord 记录节点启动的进程身份，供异常退出后的下一次启动清理。
// PID 必须和操作系统的创建标识一起核对，避免误杀复用该 PID 的其他进程。
type processRecord struct {
	Key      string `json:"key"`
	PID      int    `json:"pid"`
	Identity string `json:"identity"`
	Jail     string `json:"jail,omitempty"`
	JID      int    `json:"jid,omitempty"`
	Watch    bool   `json:"watch,omitempty"`
}

var processRecordMu sync.Mutex

var errProcessIdentityUnsupported = errors.New("当前平台暂不支持持久化进程身份")

func (d *Daemon) processRecordPath(uuid string) string {
	return filepath.Join(d.DataDir, "processes", processRecordKey(uuid)+".json")
}

func processRecordKey(uuid string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(uuid)))
}

func (d *Daemon) processRecordFile(record processRecord) string {
	return filepath.Join(d.DataDir, "processes", record.Key+".json")
}

func (d *Daemon) recordProcess(uuid string, p *Process) (processRecord, error) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return processRecord{}, fmt.Errorf("进程尚未启动")
	}
	return d.recordCommandProcess(uuid, p.cmd, "", 0, false)
}

func (d *Daemon) recordCommandProcess(uuid string, cmd *exec.Cmd, jail string, jid int, watch bool) (processRecord, error) {
	if cmd == nil || cmd.Process == nil {
		return processRecord{}, fmt.Errorf("进程尚未启动")
	}
	pid := cmd.Process.Pid
	identity, err := processIdentity(pid)
	if errors.Is(err, errProcessIdentityUnsupported) {
		return processRecord{}, nil
	}
	if err != nil {
		return processRecord{}, fmt.Errorf("读取进程身份失败: %w", err)
	}
	record := processRecord{Key: processRecordKey(uuid), PID: pid, Identity: identity, Jail: jail, JID: jid, Watch: watch}
	data, err := json.Marshal(record)
	if err != nil {
		return processRecord{}, err
	}
	processRecordMu.Lock()
	defer processRecordMu.Unlock()
	path := d.processRecordPath(uuid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return processRecord{}, err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return processRecord{}, err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return processRecord{}, err
	}
	return record, nil
}

// forgetProcess 仅删除同一代进程的记录，避免旧退出监听误删新进程记录。
func (d *Daemon) forgetProcess(record processRecord) error {
	if record.Identity == "" {
		return nil
	}
	processRecordMu.Lock()
	defer processRecordMu.Unlock()
	path := d.processRecordFile(record)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var current processRecord
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if current != record {
		return nil
	}
	return os.Remove(path)
}

func (d *Daemon) forgetProcessFor(uuid string, p *Process) error {
	if p == nil || p.record.Key != processRecordKey(uuid) {
		return nil
	}
	return d.forgetProcess(p.record)
}

// recoverOrphanProcesses 在启动服务前清理上次异常退出遗留的受管进程。
// 无法确认身份或无法终止时拒绝启动，避免自动启动同一实例造成双进程。
func (d *Daemon) recoverOrphanProcesses() error {
	dir := filepath.Join(d.DataDir, "processes")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".json.tmp") {
			continue
		}
		entryPath := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(entryPath)
		if err != nil {
			return err
		}
		var record processRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("进程记录 %s 损坏: %w", entry.Name(), err)
		}
		expected := record.Key + ".json"
		_, keyErr := hex.DecodeString(record.Key)
		if record.PID <= 0 || record.Identity == "" || len(record.Key) != 64 || keyErr != nil || entry.Name() != expected && entry.Name() != expected+".tmp" {
			return fmt.Errorf("进程记录 %s 身份无效", entry.Name())
		}
		clearRecord := func() error {
			if entry.Name() == expected+".tmp" {
				return os.Remove(entryPath)
			}
			return d.forgetProcess(record)
		}
		current, err := processIdentity(record.PID)
		if os.IsNotExist(err) {
			if record.Jail != "" {
				if err := recoverBastilleRecord(record); err != nil {
					return fmt.Errorf("恢复 Bastille 会话 %s 失败: %w", entry.Name(), err)
				}
			}
			if err := clearRecord(); err != nil {
				return err
			}
			continue
		}
		if err == nil && current != record.Identity {
			// PID 已复用，不能以旧进程组编号清理新进程。
			if err := clearRecord(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("核对进程记录 %s 失败: %w", entry.Name(), err)
		}
		alog.Printf("清理异常退出遗留进程 %d（记录 %s）", record.PID, entry.Name())
		if err := terminateRecordedProcess(record); err != nil {
			return fmt.Errorf("清理进程记录 %s 失败: %w", entry.Name(), err)
		}
		if record.Jail != "" {
			if err := recoverBastilleRecord(record); err != nil {
				return fmt.Errorf("恢复 Bastille 会话 %s 失败: %w", entry.Name(), err)
			}
		}
		if err := clearRecord(); err != nil {
			return err
		}
	}
	return nil
}
