// vault_migrate.go — 两阶段迁移（M4，A2/D13）。
//
// 阶段一：instances.json → 加密对象 "/system/instances.json"，标记
// migration.instancesDone 后删除明文（崩溃两分支幂等）。
// 阶段二：vaultFiles=true 实例的明文文件树 → 加密对象（逐文件加密 → 索引
// → 删明文 → 游标落盘；重跑幂等：索引条目 size/mtime 一致则跳过）。
//
// 迁移在后台 goroutine 执行：每个文件操作取 v.mu.RLock（与 lock() 互斥），
// 文件间隙检查是否仍解锁，被锁定即中止（下次解锁续跑）。迁移期间数据面
// 返回 403 "vault migrating"（vaultGate）。

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// vaultMigration 迁移状态（持久化于 vault.json）。
type vaultMigration struct {
	InstancesDone bool                `json:"instancesDone"`
	FilesDone     map[string]bool     `json:"filesDone"`
	Cursor        *vaultMigrateCursor `json:"cursor"`
	StartedAt     time.Time           `json:"startedAt"`
	CompletedAt   *time.Time          `json:"completedAt"`
}

// vaultMigrateCursor 阶段二游标（崩溃续迁点）。
type vaultMigrateCursor struct {
	UUID  string `json:"uuid"`
	Path  string `json:"path"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Bytes int64  `json:"bytes"`
}

// systemInstancesPath 实例列表的加密对象逻辑路径。
const systemInstancesPath = "/system/instances.json"

// migrationComplete 迁移是否全部完成。
func (m *vaultMigration) migrationComplete() bool {
	return m != nil && m.InstancesDone && m.CompletedAt != nil
}

// migrationPhase 当前迁移阶段：0 未开始 / 1 阶段一 / 2 阶段二 / 3 已完成。
func (v *vaultState) migrationPhase() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.migrationPhaseLocked()
}

// migrationPhaseLocked 调用方须持有 v.mu。
func (v *vaultState) migrationPhaseLocked() int {
	m := v.migration
	if m == nil {
		return 0
	}
	if m.CompletedAt != nil {
		return 3
	}
	if !m.InstancesDone {
		return 1
	}
	return 2
}

// migrationProgress 迁移进度（供 /api/vault/migrate/status）。
func (v *vaultState) migrationProgress() map[string]any {
	v.mu.RLock()
	defer v.mu.RUnlock()
	m := v.migration
	phase := v.migrationPhaseLocked()
	out := map[string]any{"phase": phase, "done": int64(0), "total": int64(0), "bytes": int64(0), "running": v.migrating}
	if m == nil {
		out["phase"] = 0
		return out
	}
	if m.Cursor != nil {
		out["done"] = m.Cursor.Done
		out["total"] = m.Cursor.Total
		out["bytes"] = m.Cursor.Bytes
	}
	if m.CompletedAt != nil {
		out["completedAt"] = m.CompletedAt.Format(time.RFC3339)
	}
	return out
}

// startMigration 启动/续跑迁移（幂等）。
// 无实质文件迁移工作时同步完成（避免解锁后出现短暂的 403 vault migrating 窗口）；
// 有 vaultFiles 文件树待迁移时后台执行。
func (v *vaultState) startMigration() {
	v.mu.Lock()
	if v.migrating || v.migrationPhaseLocked() == 3 {
		v.mu.Unlock()
		return
	}
	v.migrating = true
	v.mu.Unlock()
	if v.hasPendingFileTrees() {
		go v.runMigration()
	} else {
		v.runMigration()
	}
}

// hasPendingFileTrees 是否存在待迁移的 vaultFiles 实例文件树（调用方须已解锁）。
func (v *vaultState) hasPendingFileTrees() bool {
	v.d.mu.Lock()
	defer v.d.mu.Unlock()
	for _, inst := range v.d.Instances {
		if !inst.Config.VaultFiles {
			continue
		}
		v.mu.RLock()
		done := v.migration != nil && v.migration.FilesDone[inst.InstanceUuid]
		v.mu.RUnlock()
		if !done {
			return true
		}
	}
	return false
}

// runMigration 迁移主循环（后台 goroutine）。
func (v *vaultState) runMigration() {
	defer func() {
		v.mu.Lock()
		v.migrating = false
		v.mu.Unlock()
	}()
	if err := v.migrateInstancesJSON(); err != nil {
		v.d.auditLogf("vault.migrate 阶段一失败: %v", err)
		return
	}
	if err := v.migrateInstanceFiles(); err != nil {
		v.d.auditLogf("vault.migrate 阶段二失败: %v", err)
		return
	}
	now := time.Now()
	v.mu.Lock()
	v.migration.CompletedAt = &now
	err := v.save()
	v.mu.Unlock()
	if err != nil {
		v.d.auditLogf("vault.migrate 完成标记落盘失败: %v", err)
		return
	}
	v.d.auditLogf("vault.migrate 完成")
}

// migrateInstancesJSON 阶段一：instances.json 加密迁移（崩溃两分支幂等）。
func (v *vaultState) migrateInstancesJSON() error {
	v.mu.Lock()
	if v.migration == nil {
		v.migration = &vaultMigration{FilesDone: map[string]bool{}}
	}
	if v.migration.InstancesDone {
		v.mu.Unlock()
		return nil
	}
	v.mu.Unlock()

	plainFile := filepath.Join(v.d.DataDir, "instances.json")
	// 加密（崩溃恢复幂等：对象已存在则跳过；明文不存在则视为空列表）
	if v.store.stat(systemInstancesPath) == nil {
		data, err := os.ReadFile(plainFile)
		if err == nil {
			if werr := v.store.writeFile(v, systemInstancesPath, data); werr != nil {
				return werr
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	// 标记 → 删明文（顺序固定：对象 → 标记 → 删明文；任何一步崩溃都可重跑）
	if err := v.markInstancesDone(); err != nil {
		return err
	}
	if err := os.Remove(plainFile); err != nil && !os.IsNotExist(err) {
		v.d.auditLogf("vault.migrate 删除明文 instances.json 失败: %v", err)
	}
	return nil
}

// markInstancesDone 落盘 instancesDone 标记。
func (v *vaultState) markInstancesDone() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.migration == nil {
		v.migration = &vaultMigration{FilesDone: map[string]bool{}}
	}
	if v.migration.StartedAt.IsZero() {
		v.migration.StartedAt = time.Now()
	}
	v.migration.InstancesDone = true
	return v.save()
}

// migrateInstanceFiles 阶段二：vaultFiles 实例文件树迁移（幂等游标续跑）。
func (v *vaultState) migrateInstanceFiles() error {
	// 候选实例：vaultFiles=true 且未完成
	v.d.mu.Lock()
	type cand struct {
		uuid string
		cwd  string
	}
	var cands []cand
	for _, inst := range v.d.Instances {
		if inst.Config.VaultFiles {
			cands = append(cands, cand{inst.InstanceUuid, inst.Config.Cwd})
		}
	}
	v.d.mu.Unlock()

	// 从游标恢复（跳过已完成的实例与游标之前的文件）
	startUUID := ""
	if v.migration.Cursor != nil {
		startUUID = v.migration.Cursor.UUID
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].uuid < cands[b].uuid })
	begin := false
	if startUUID == "" {
		begin = true
	}
	for _, c := range cands {
		if !begin {
			if c.uuid == startUUID {
				begin = true
			} else {
				continue
			}
		}
		v.mu.RLock()
		done := v.migration.FilesDone[c.uuid]
		v.mu.RUnlock()
		if done {
			continue
		}
		if err := v.migrateOneInstance(c.uuid, c.cwd); err != nil {
			return err
		}
	}
	return nil
}

// migrateOneInstance 迁移单个实例文件树；返回错误时（含被锁定中止）可由游标续跑。
func (v *vaultState) migrateOneInstance(uuid, cwd string) error {
	// 目录不存在：无文件可迁移，直接标记完成
	if _, err := os.Stat(cwd); os.IsNotExist(err) {
		v.mu.Lock()
		v.migration.FilesDone[uuid] = true
		v.migration.Cursor = nil
		saveErr := v.save()
		v.mu.Unlock()
		return saveErr
	}
	// 预检：磁盘余量（平台不支持时仅告警）
	total, err := v.treeBytes(uuid, cwd)
	if err != nil {
		return err
	}
	if free, ok := diskFreeBytes(cwd); ok && free < total {
		return fmt.Errorf("磁盘余量不足：需要约 %s，可用 %s（迁移会先加密后删明文，但预检保守要求全量余量）",
			FormatSize(total), FormatSize(free))
	}
	cursor := &vaultMigrateCursor{UUID: uuid, Total: total}
	// 游标恢复：只处理游标之后的路径
	startPath := ""
	if v.migration.Cursor != nil && v.migration.Cursor.UUID == uuid {
		cursor = v.migration.Cursor
		startPath = cursor.Path
	}

	var files []string
	err = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for i, path := range files {
		if startPath != "" {
			if path < startPath {
				continue // 游标之前的文件已迁移过（幂等续跑）
			}
			startPath = "" // 到达游标点
		}
		// 每文件间隙检查解锁状态（被锁定 → 中止，下次解锁续跑）
		v.mu.RLock()
		unlocked := v.masterKey != nil
		v.mu.RUnlock()
		if !unlocked {
			return errors.New("保险库已锁定，迁移中止（解锁后自动续跑）")
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return err
		}
		logical := logicalPath(uuid, filepath.ToSlash(rel))
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		// 幂等：索引条目 size/mtime 一致 → 跳过（重跑不重复加密）
		if e := v.store.stat(logical); e != nil && e.Size == fi.Size() && e.MTime == fi.ModTime().Unix() {
			cursor.Done = int64(i + 1)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := v.store.writeFile(v, logical, data); err != nil {
			return err
		}
		cursor.Bytes += fi.Size()
		cursor.Done = int64(i + 1)
		cursor.Path = path
		// 每 indexFlushBatch 个文件落盘一次游标 + 强制索引落盘
		if (i+1)%indexFlushBatch == 0 {
			if err := v.store.flush(); err != nil {
				return err
			}
			v.mu.Lock()
			v.migration.Cursor = cursor
			err = v.save()
			v.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	// 实例完成：删明文 + 标记
	if err := v.removePlainTree(cwd); err != nil {
		v.d.auditLogf("vault.migrate 删除明文目录失败（%s）: %v", uuid, err)
	}
	v.mu.Lock()
	v.migration.FilesDone[uuid] = true
	v.migration.Cursor = nil
	err = v.save()
	v.mu.Unlock()
	if err != nil {
		return err
	}
	_ = v.store.flush()
	v.d.auditLogf("vault.migrate 实例 %s 文件树迁移完成", uuid)
	return nil
}

// treeBytes 统计明文文件树总大小。
func (v *vaultState) treeBytes(uuid, cwd string) (int64, error) {
	var total int64
	err := filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// removePlainTree 删除明文文件树（自底向上删目录）。
func (v *vaultState) removePlainTree(cwd string) error {
	var files []string
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	for _, f := range files {
		_ = os.Remove(f)
	}
	var dirs []string
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return nil
}

// plainTreeDirty 明文目录是否残留文件（崩溃恢复判定：存在任何普通文件即 dirty）。
func plainTreeDirty(cwd string) bool {
	if cwd == "" {
		return false
	}
	dirty := false
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			dirty = true
			return errors.New("stop")
		}
		return nil
	})
	return dirty
}

// ensureVaultDir 确保 vault 数据目录存在（0700）。
func ensureVaultDir(path string) error {
	if path == "" {
		return errors.New("vault 目录为空")
	}
	return os.MkdirAll(path, 0o700)
}
