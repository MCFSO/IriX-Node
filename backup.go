// 实例备份/恢复（docs/irix-node-local-parity.md §4.5，任务化）。
//
// POST   /api/instance/snapshot      body: {uuid, daemonId} → {jobId}
// GET    /api/instance/snapshot-progress?jobId → {status, percent, archivePath}
// POST   /api/instance/restore       body: {uuid, daemonId, archivePath} → {jobId}
// GET    /api/instance/backups?uuid&daemonId → {items: [...]}
// DELETE /api/instance/backups?uuid&daemonId body: {paths: [...]}
// POST   /api/instance/backups/download?uuid&daemonId body: {path} → {password, addr}
//
// 快照：实例 cwd 打成 zip（排除 .irix-trash/ 回收站、日志、临时文件），
// 存入节点备份区 {data}/backups/<uuid>/<ts>.zip；
// 恢复：先自动停止实例（运行中则停），解压覆盖 cwd，恢复后保持停止。
// 备份下载：走现有直连票据（票据绑定单个备份文件）。

package main

import (
	"archive/zip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotTimeFormat 备份文件名时间格式（2026-08-20-10-00-00.zip）。
const snapshotTimeFormat = "2006-01-02-15-04-05"

// backupsDirOf 实例备份目录（{data}/backups/<uuid>）。
func (d *Daemon) backupsDirOf(uuid string) string {
	return filepath.Join(d.DataDir, "backups", uuid)
}

// snapshotEntry 待备份文件。
type snapshotEntry struct {
	abs string // 绝对路径
	rel string // 相对 cwd 路径（zip 内路径）
}

// isExcludedFromSnapshot 备份排除规则：回收站、日志、临时文件。
func isExcludedFromSnapshot(base string, isDir bool) bool {
	if isDir {
		return base == ".irix-trash" || base == ".git"
	}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".tmp") {
		return true
	}
	if strings.Contains(lower, ".part-") {
		return true
	}
	return false
}

// snapshotFiles 遍历 cwd 收集待备份文件（应用排除规则）。
func snapshotFiles(cwd string) ([]snapshotEntry, int64, error) {
	var files []snapshotEntry
	var total int64
	err := filepath.WalkDir(cwd, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if de.IsDir() {
			if path != cwd && isExcludedFromSnapshot(de.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcludedFromSnapshot(de.Name(), false) {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		files = append(files, snapshotEntry{abs: path, rel: rel})
		total += info.Size()
		return nil
	})
	return files, total, err
}

// createSnapshotZip 将文件列表打包到 dest（带进度，按字节推进）。
func createSnapshotZip(dest, cwd string, files []snapshotEntry, total int64, task *task) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	var done int64
	for _, e := range files {
		w, err := zw.Create(filepath.ToSlash(e.rel))
		if err != nil {
			return err
		}
		src, err := os.Open(e.abs)
		if err != nil {
			return err
		}
		n, cerr := io.Copy(w, src)
		src.Close()
		if cerr != nil {
			return cerr
		}
		done += n
		if task != nil && total > 0 {
			task.set(taskStatusRunning, 0.05+0.9*float64(done)/float64(total),
				"压缩中…", "")
		}
	}
	return nil
}

// handleInstanceSnapshot 发起实例快照任务。
// POST /api/instance/snapshot body: {uuid, daemonId}
func (d *Daemon) handleInstanceSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string `json:"uuid"`
		DaemonID string `json:"daemonId"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	inst := d.Find(body.UUID)
	if inst == nil {
		writeError(w, http.StatusBadRequest, "实例不存在")
		return
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if cwd == "" {
		writeError(w, http.StatusBadRequest, "实例工作目录为空")
		return
	}
	taskID, task := d.newTask()
	go d.runSnapshot(taskID, task, body.UUID, cwd)
	writeOK(w, map[string]any{"jobId": taskID})
}

// runSnapshot 执行快照任务。
func (d *Daemon) runSnapshot(taskID string, task *task, uuid, cwd string) {
	fail := func(err error) {
		task.setError(err)
		alog.Printf("实例 %s 备份失败: %v", uuid, err)
	}
	task.set(taskStatusRunning, 0.01, "统计文件…", "")
	files, total, err := snapshotFiles(cwd)
	if err != nil {
		fail(err)
		return
	}
	if len(files) == 0 {
		fail(errors.New("目录为空或无可备份文件"))
		return
	}
	dest := filepath.Join(d.backupsDirOf(uuid), time.Now().Format(snapshotTimeFormat)+".zip")
	if err := createSnapshotZip(dest, cwd, files, total, task); err != nil {
		fail(err)
		return
	}
	task.set(taskStatusDone, 1, "备份完成", dest)
	alog.Printf("实例 %s 备份完成: %s（%s，%d 个文件）", uuid, dest, FormatSize(total), len(files))
}

// handleInstanceRestore 发起实例恢复任务（先停实例，解压覆盖，保持停止）。
// POST /api/instance/restore body: {uuid, daemonId, archivePath}
func (d *Daemon) handleInstanceRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID        string `json:"uuid"`
		DaemonID    string `json:"daemonId"`
		ArchivePath string `json:"archivePath"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	inst := d.Find(body.UUID)
	if inst == nil {
		writeError(w, http.StatusBadRequest, "实例不存在")
		return
	}
	archive, err := filepath.Abs(body.ArchivePath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "备份路径无效")
		return
	}
	backupDir := d.backupsDirOf(body.UUID)
	if !pathWithin(backupDir, archive) {
		writeError(w, http.StatusBadRequest, "备份文件不在本实例备份区")
		return
	}
	if info, err := os.Stat(archive); err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "备份文件不存在: "+body.ArchivePath)
		return
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if cwd == "" {
		writeError(w, http.StatusBadRequest, "实例工作目录为空")
		return
	}

	taskID, task := d.newTask()
	go func() {
		fail := func(err error) {
			task.setError(err)
			alog.Printf("实例 %s 恢复失败: %v", body.UUID, err)
		}
		// 先停止实例（未运行则跳过）；stopInstance 内部管理 Busy
		if err := d.stopInstance(inst); err != nil && !errors.Is(err, errNotRunning) {
			fail(err)
			return
		}
		// 停止完成后置忙：恢复期间禁止启动/再次操作
		inst.mu.Lock()
		inst.Busy = true
		inst.mu.Unlock()
		defer func() {
			inst.mu.Lock()
			inst.Busy = false
			inst.mu.Unlock()
		}()

		task.set(taskStatusRunning, 0.05, "解压中…", "")
		if err := unzip(archive, cwd); err != nil {
			fail(err)
			return
		}
		inst.SetStatus(StatusStopped)
		task.set(taskStatusDone, 1, "恢复完成，实例保持停止", "")
		alog.Printf("实例 %s 恢复完成（%s）", body.UUID, archive)
	}()
	writeOK(w, map[string]any{"jobId": taskID})
}

// writeSnapshotStatus 快照/恢复任务进度（字段名对齐文档 §4.5：archivePath）。
// GET /api/instance/snapshot-progress?jobId
func (d *Daemon) writeSnapshotStatus(w http.ResponseWriter, r *http.Request) {
	taskID := queryParam(r, "jobId")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "缺少 jobId 参数")
		return
	}
	t := d.tasks.get(taskID)
	if t == nil {
		writeError(w, http.StatusBadRequest, "任务不存在或已过期")
		return
	}
	snap := t.snapshot()
	if p, ok := snap["path"].(string); ok && p != "" {
		snap["archivePath"] = p
		delete(snap, "path")
	}
	writeOK(w, snap)
}

// handleBackupsList 列出实例备份。
// GET /api/instance/backups?uuid&daemonId
// 响应 items: [{fileName, size, mtime, path}]（按时间新→旧）。
func (d *Daemon) handleBackupsList(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	if uuid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uuid 参数")
		return
	}
	dir := d.backupsDirOf(uuid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeOK(w, map[string]any{"items": []any{}})
			return
		}
		writeError(w, http.StatusInternalServerError, "读取备份目录失败: "+err.Error())
		return
	}
	var items []map[string]any
	type backupMeta struct {
		fileName string
		size     int64
		mtime    time.Time
		path     string
	}
	var metas []backupMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		metas = append(metas, backupMeta{
			fileName: e.Name(),
			size:     info.Size(),
			mtime:    info.ModTime(),
			path:     filepath.Join(dir, e.Name()),
		})
	}
	// 时间新→旧；同秒内按文件名倒序（文件名含时间戳，保证排序稳定）
	sort.Slice(metas, func(i, j int) bool {
		if !metas[i].mtime.Equal(metas[j].mtime) {
			return metas[i].mtime.After(metas[j].mtime)
		}
		return metas[i].fileName > metas[j].fileName
	})
	for _, m := range metas {
		items = append(items, map[string]any{
			"fileName": m.fileName,
			"size":     m.size,
			"mtime":    m.mtime.Format(fileTimeFormat),
			"path":     m.path,
		})
	}
	writeOK(w, map[string]any{"items": items})
}

// handleBackupsDelete 删除指定备份。
// DELETE /api/instance/backups?uuid&daemonId body: {paths: [...]}
func (d *Daemon) handleBackupsDelete(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	if uuid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uuid 参数")
		return
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	dir := d.backupsDirOf(uuid)
	for _, p := range body.Paths {
		abs, err := filepath.Abs(p)
		if err != nil || !pathWithin(dir, abs) {
			writeError(w, http.StatusBadRequest, "备份路径越界: "+p)
			return
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			writeError(w, http.StatusInternalServerError, "删除备份失败: "+err.Error())
			return
		}
	}
	writeOK(w, true)
}

// handleBackupDownloadTicket 申请备份下载票据（绑定单个备份文件）。
// POST /api/instance/backups/download?uuid&daemonId body: {path, uuid?}
// uuid 优先取查询参数，其次取请求体（集群迁移等调用方不拼 query）。
func (d *Daemon) handleBackupDownloadTicket(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	var body struct {
		UUID string `json:"uuid"`
		Path string `json:"path"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if uuid == "" {
		uuid = body.UUID
	}
	if uuid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uuid 参数")
		return
	}
	abs, err := filepath.Abs(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "备份路径无效")
		return
	}
	dir := d.backupsDirOf(uuid)
	if !pathWithin(dir, abs) {
		writeError(w, http.StatusBadRequest, "备份文件不在本实例备份区")
		return
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "备份文件不存在")
		return
	}
	password := tickets.CreateDownload(uuid, dir, abs)
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "下载票据已满，请稍后重试")
		return
	}
	writeOK(w, map[string]any{
		"password": password,
		"addr":     d.publicAddr(),
	})
}
