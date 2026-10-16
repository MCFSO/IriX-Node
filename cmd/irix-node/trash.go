// 实例级回收站（docs/irix-node-local-parity.md §4.6）。
//
// POST /api/files/trash        body: {uuid, daemonId, targets}     删除进回收站
// GET  /api/files/trash/list   ?uuid&daemonId                       列出回收站
// POST /api/files/trash/restore body: {uuid, daemonId, ids}         恢复（冲突改名）
// POST /api/files/trash/empty  body: {uuid, daemonId, ids?}         永久删除（ids 空=全部）
//
// 语义与本地 TrashStore 对齐：删除 → 移入实例内 <cwd>/.irix-trash/<id>-<name>；
// 元数据（原始路径、删除时间）由节点维护（{data}/trash/<uuid>.json），
// 客户端只做展示。备份（§4.5）自动排除 .irix-trash。

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// trashDirName 回收站目录名（备份排除项一致）。
const trashDirName = ".irix-trash"

// trashItem 回收站条目。
type trashItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	OriginalPath string `json:"originalPath"` // 相对 cwd（/ 开头）
	TrashPath    string `json:"trashPath"`    // 相对 cwd（/ 开头）
	Size         int64  `json:"size"`
	DeletedAt    int64  `json:"deletedAt"` // unix 毫秒
}

// trashFile 实例回收站元数据文件。
func (d *Daemon) trashFile(uuid string) string {
	return filepath.Join(d.DataDir, "trash", uuid+".json")
}

// loadTrash 读取实例回收站元数据（调用方须持 trashMu）。
func (d *Daemon) loadTrash(uuid string) []trashItem {
	data, err := os.ReadFile(d.trashFile(uuid))
	if err != nil {
		return nil
	}
	var items []trashItem
	if err := json.Unmarshal(data, &items); err != nil {
		alog.Printf("警告: 回收站元数据损坏（%s），按空列表处理: %v", d.trashFile(uuid), err)
		return nil
	}
	return items
}

// saveTrash 持久化回收站元数据（原子写；调用方须持 trashMu）。
func (d *Daemon) saveTrash(uuid string, items []trashItem) error {
	if items == nil {
		items = []trashItem{}
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	path := d.trashFile(uuid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleFileTrash 删除到回收站。
// POST /api/files/trash body: {uuid, daemonId, targets: [...]}
func (d *Daemon) handleFileTrash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string   `json:"uuid"`
		DaemonID string   `json:"daemonId"`
		Targets  []string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if len(body.Targets) == 0 {
		writeError(w, http.StatusBadRequest, "缺少 targets 参数")
		return
	}
	cwd, err := d.CwdOf(body.UUID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	trashDir := filepath.Join(cwd, trashDirName)

	d.trashMu.Lock()
	defer d.trashMu.Unlock()
	items := d.loadTrash(body.UUID)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建回收站目录失败: "+err.Error())
		return
	}
	for _, t := range body.Targets {
		src, err := NormalizePath(cwd, t)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if pathWithin(trashDir, src) {
			writeError(w, http.StatusBadRequest, "回收站内的内容不能再次删除")
			return
		}
		info, err := os.Stat(src)
		if err != nil {
			writeError(w, http.StatusBadRequest, "目标不存在: "+t)
			return
		}
		size := info.Size()
		if info.IsDir() {
			if sz, err := DirSize(src); err == nil {
				size = sz
			}
		}
		id := newUUID()[:8]
		name := filepath.Base(src)
		dst := filepath.Join(trashDir, id+"-"+name)
		if err := os.Rename(src, dst); err != nil {
			writeError(w, http.StatusInternalServerError, "移入回收站失败: "+err.Error())
			return
		}
		items = append(items, trashItem{
			ID:           id,
			Name:         name,
			OriginalPath: slashRel(cwd, src),
			TrashPath:    slashRel(cwd, dst),
			Size:         size,
			DeletedAt:    time.Now().UnixMilli(),
		})
	}
	if err := d.saveTrash(body.UUID, items); err != nil {
		writeError(w, http.StatusInternalServerError, "保存回收站元数据失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleTrashList 列出回收站内容。
// GET /api/files/trash/list?uuid&daemonId
// 响应 items: [{id, name, originalPath, trashPath, size, deletedAt}]（新→旧）。
func (d *Daemon) handleTrashList(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	if uuid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uuid 参数")
		return
	}
	d.trashMu.Lock()
	defer d.trashMu.Unlock()
	items := d.loadTrash(uuid)
	sort.Slice(items, func(i, j int) bool {
		return items[i].DeletedAt > items[j].DeletedAt
	})
	if items == nil {
		items = []trashItem{}
	}
	writeOK(w, map[string]any{"items": items})
}

// findTrashItem 按 id 查找回收站条目（返回下标，-1 未找到）。
func findTrashItem(items []trashItem, id string) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// uniqueRestorePath 目标已存在时生成不冲突路径：name (1).ext、name (2).ext …
func uniqueRestorePath(dest string) string {
	base := filepath.Base(dest)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(filepath.Dir(dest), fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// handleTrashRestore 恢复回收站条目（目标冲突时自动改名）。
// POST /api/files/trash/restore body: {uuid, daemonId, ids: [...]}
func (d *Daemon) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string   `json:"uuid"`
		DaemonID string   `json:"daemonId"`
		IDs      []string `json:"ids"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "缺少 ids 参数")
		return
	}
	cwd, err := d.CwdOf(body.UUID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.trashMu.Lock()
	defer d.trashMu.Unlock()
	items := d.loadTrash(body.UUID)
	restored := map[string]string{} // id → 实际恢复路径
	for _, id := range body.IDs {
		idx := findTrashItem(items, id)
		if idx < 0 {
			writeError(w, http.StatusBadRequest, "回收站条目不存在: "+id)
			return
		}
		it := items[idx]
		src, err := NormalizePath(cwd, it.TrashPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		dest, err := NormalizePath(cwd, it.OriginalPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := os.Stat(src); err != nil {
			writeError(w, http.StatusBadRequest, "回收站内容已不存在（可能被手动删除）: "+it.Name)
			return
		}
		if _, err := os.Stat(dest); err == nil {
			dest = uniqueRestorePath(dest) // 冲突自动改名
		}
		if err := os.Rename(src, dest); err != nil {
			writeError(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
			return
		}
		restored[id] = slashRel(cwd, dest)
		items = append(items[:idx], items[idx+1:]...)
	}
	if err := d.saveTrash(body.UUID, items); err != nil {
		writeError(w, http.StatusInternalServerError, "保存回收站元数据失败: "+err.Error())
		return
	}
	writeOK(w, restored)
}

// handleTrashEmpty 永久删除回收站内容。
// POST /api/files/trash/empty body: {uuid, daemonId, ids?}（ids 空 = 全部清空）
func (d *Daemon) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string   `json:"uuid"`
		DaemonID string   `json:"daemonId"`
		IDs      []string `json:"ids"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	cwd, err := d.CwdOf(body.UUID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.trashMu.Lock()
	defer d.trashMu.Unlock()
	items := d.loadTrash(body.UUID)
	keep := items[:0]
	for _, it := range items {
		if len(body.IDs) > 0 && !containsStr(body.IDs, it.ID) {
			keep = append(keep, it)
			continue
		}
		src, err := NormalizePath(cwd, it.TrashPath)
		if err == nil {
			_ = os.RemoveAll(src) // 内容缺失也继续清理元数据
		}
	}
	if err := d.saveTrash(body.UUID, keep); err != nil {
		writeError(w, http.StatusInternalServerError, "保存回收站元数据失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// containsStr 判断切片是否包含目标字符串。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
