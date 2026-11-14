// MSLX Daemon 兼容层：文件管理接口（/api/files/instance/{id}/...）。
//
// 所有路径都经 safeResolve 落到实例工作目录内（软链与 .. 越界一律拒绝），
// 与节点自身文件 API 使用同一套边界检查。

package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"irix/node/internal/timesys"
)

// mslxFileItem MSLX 文件列表条目。
type mslxFileItem struct {
	Name         string `json:"name"`
	Type         string `json:"type"` // folder | file
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
	Permission   string `json:"permission"`
}

// mslxFilePerm 生成八进制权限串（Windows 无 POSIX 权限，统一 777）。
func mslxFilePerm(mode os.FileMode) string {
	if runtime.GOOS == "windows" {
		return "777"
	}
	return fmt.Sprintf("%03o", int(mode.Perm()))
}

// handleFiles 文件管理接口分派。
func (ms *mslxCompat) handleFiles(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		http.NotFound(w, r)
		return
	}
	switch segs[0] {
	case "root", "drives":
		// 主机文件系统浏览：本节点不对外暴露（等价于节点内部权限边界）
		mslxFail(w, http.StatusForbidden, "irix-node 兼容层不提供主机文件系统浏览")
		return
	case "upload":
		mslxFail(w, http.StatusNotImplemented, "请使用 /api/files/instance/{id}/upload 一次性上传")
		return
	case "task":
		mslxFail(w, http.StatusNotFound, "没有该任务（本节点不产生文件任务）")
		return
	}
	if segs[0] != "instance" || len(segs) < 3 {
		http.NotFound(w, r)
		return
	}
	inst, _, err := ms.instanceByID(segs[1])
	if err != nil {
		mslxFail(w, http.StatusNotFound, err.Error())
		return
	}
	inst.mu.Lock()
	base := inst.Config.Cwd
	inst.mu.Unlock()
	action := segs[2]
	switch action {
	case "lists":
		ms.filesList(w, r, base)
	case "content":
		if r.Method == http.MethodGet {
			ms.filesRead(w, r, base)
			return
		}
		ms.filesWrite(w, r, base)
	case "directory":
		ms.filesMkdir(w, r, base)
	case "delete":
		ms.filesDelete(w, r, base)
	case "rename":
		ms.filesRename(w, r, base)
	case "copy", "move":
		ms.filesBatch(w, r, base, action == "move")
	case "upload":
		ms.filesUpload(w, r, base)
	case "download":
		ms.filesDownload(w, r, base)
	case "chmod":
		mslxFail(w, http.StatusNotImplemented, "irix-node 兼容层不支持修改文件权限")
	case "thumbnail":
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// mslxPath 把 MSLX 的相对路径解析为实例目录内的绝对路径。
func mslxPath(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return base, nil
	}
	return safeResolve(base, rel)
}

// filesList 列出目录内容。
func (ms *mslxCompat) filesList(w http.ResponseWriter, r *http.Request, base string) {
	q := r.URL.Query()
	dir, err := mslxPath(base, q.Get("path"))
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		mslxFail(w, http.StatusNotFound, "目录不存在或不可读")
		return
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	items := make([]mslxFileItem, 0, len(entries))
	for _, e := range entries {
		if search != "" && !strings.Contains(strings.ToLower(e.Name()), search) {
			continue
		}
		item := mslxFileItem{Name: e.Name(), Type: "file", Permission: "644"}
		if info, ierr := e.Info(); ierr == nil {
			if info.IsDir() {
				item.Type = "folder"
				item.Size = 0
			} else {
				item.Size = info.Size()
			}
			item.LastModified = info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
			item.Permission = mslxFilePerm(info.Mode())
		} else {
			item.LastModified = timesys.Now().UTC().Format("2006-01-02T15:04:05Z")
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Type != items[b].Type {
			return items[a].Type == "folder"
		}
		if strings.EqualFold(q.Get("sort"), "size") {
			return items[a].Size > items[b].Size
		}
		return strings.ToLower(items[a].Name) < strings.ToLower(items[b].Name)
	})
	// pageSize<=0 表示全量（与 MSLX 一致）
	pageSize := atoiDefault(q.Get("pageSize"), 0)
	if pageSize <= 0 {
		mslxOK(w, items, "获取成功")
		return
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	total := len(items)
	totalPages := (total + pageSize - 1) / pageSize
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	mslxOK(w, map[string]any{
		"items":      items[start:end],
		"totalCount": total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": totalPages,
	}, "获取成功")
}

// filesRead 读取文本文件。
func (ms *mslxCompat) filesRead(w http.ResponseWriter, r *http.Request, base string) {
	rel := r.URL.Query().Get("path")
	if strings.TrimSpace(rel) == "" {
		mslxFail(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	abs, err := mslxPath(base, rel)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		mslxFail(w, http.StatusNotFound, "文件不存在")
		return
	}
	if st.Size() > 8<<20 {
		mslxFail(w, http.StatusRequestEntityTooLarge, "文件过大（上限 8MB），请下载后编辑")
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "读取失败: "+err.Error())
		return
	}
	mslxOK(w, string(data), "读取成功")
}

// filesWrite 写入文本文件。
func (ms *mslxCompat) filesWrite(w http.ResponseWriter, r *http.Request, base string) {
	var req struct {
		Path     string  `json:"path"`
		Content  string  `json:"content"`
		Content2 *string `json:"text"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		mslxFail(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	content := req.Content
	if req.Content2 != nil && req.Content == "" {
		content = *req.Content2
	}
	abs, err := mslxPath(base, req.Path)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		mslxFail(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	mslxOK(w, nil, "保存成功")
}

// filesMkdir 新建目录。
func (ms *mslxCompat) filesMkdir(w http.ResponseWriter, r *http.Request, base string) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		mslxFail(w, http.StatusBadRequest, "缺少目录名 name")
		return
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		mslxFail(w, http.StatusBadRequest, "目录名包含非法字符")
		return
	}
	rel := strings.Trim(req.Path, "/")
	if rel != "" {
		rel += "/"
	}
	abs, err := mslxPath(base, rel+name)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	mslxOK(w, nil, "创建成功")
}

// filesDelete 删除文件/目录。
func (ms *mslxCompat) filesDelete(w http.ResponseWriter, r *http.Request, base string) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if len(req.Paths) == 0 {
		mslxFail(w, http.StatusBadRequest, "至少需要指定一个文件或目录")
		return
	}
	for _, rel := range req.Paths {
		abs, err := mslxPath(base, rel)
		if err != nil {
			mslxFail(w, http.StatusForbidden, "路径越界: "+rel)
			return
		}
		if abs == base {
			mslxFail(w, http.StatusBadRequest, "不能删除实例根目录")
			return
		}
		if err := os.RemoveAll(abs); err != nil {
			mslxFail(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
	}
	mslxOK(w, nil, "删除成功")
}

// filesRename 重命名/移动单个条目。
func (ms *mslxCompat) filesRename(w http.ResponseWriter, r *http.Request, base string) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	src, err := mslxPath(base, req.OldPath)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	dst, err := mslxPath(base, req.NewPath)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	if src == base || dst == base {
		mslxFail(w, http.StatusBadRequest, "不能重命名实例根目录")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	if err := os.Rename(src, dst); err != nil {
		mslxFail(w, http.StatusInternalServerError, "重命名失败: "+err.Error())
		return
	}
	mslxOK(w, nil, "重命名成功")
}

// filesBatch 批量复制/移动。
func (ms *mslxCompat) filesBatch(w http.ResponseWriter, r *http.Request, base string, move bool) {
	var req struct {
		SourcePaths []string `json:"sourcePaths"`
		TargetPath  string   `json:"targetPath"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if len(req.SourcePaths) == 0 {
		mslxFail(w, http.StatusBadRequest, "至少需要选择一个文件或目录")
		return
	}
	targetDir, err := mslxPath(base, req.TargetPath)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "目标路径越界")
		return
	}
	var errs []string
	for _, rel := range req.SourcePaths {
		src, err := mslxPath(base, rel)
		if err != nil {
			errs = append(errs, rel+": 路径越界")
			continue
		}
		if src == base {
			errs = append(errs, rel+": 不能操作实例根目录")
			continue
		}
		dst := filepath.Join(targetDir, filepath.Base(src))
		if move {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				errs = append(errs, rel+": "+err.Error())
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				if cerr := copyPath(base, src, dst, 0); cerr != nil {
					errs = append(errs, rel+": "+err.Error())
					continue
				}
				_ = os.RemoveAll(src)
			}
			continue
		}
		if err := copyPath(base, src, dst, 0); err != nil {
			errs = append(errs, rel+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		writeCompatJSON(w, http.StatusOK, mslxResponse{Code: 200, Message: "部分操作失败", Data: errs})
		return
	}
	msg := "复制成功"
	if move {
		msg = "移动成功"
	}
	mslxOK(w, nil, msg)
}

// filesUpload 一次性上传（multipart，字段名 file）。
func (ms *mslxCompat) filesUpload(w http.ResponseWriter, r *http.Request, base string) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	// 目录来自查询参数或表单字段
	targetRel := r.URL.Query().Get("path")
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		mslxFail(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if targetRel == "" {
		targetRel = r.FormValue("path")
	}
	dir, err := mslxPath(base, targetRel)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "目标路径越界")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		mslxFail(w, http.StatusBadRequest, "缺少 file 字段: "+err.Error())
		return
	}
	defer file.Close()
	name := filepath.Base(filepath.FromSlash(header.Filename))
	if name == "" || name == "." {
		mslxFail(w, http.StatusBadRequest, "文件名无效")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	dest, err := safeResolve(dir, name)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	out, err := os.Create(dest)
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建文件失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		mslxFail(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if err := out.Close(); err != nil {
		mslxFail(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	mslxOK(w, map[string]any{"name": name, "size": header.Size}, "上传成功")
}

// filesDownload 下载文件（inline=true 时按内容类型内联展示）。
func (ms *mslxCompat) filesDownload(w http.ResponseWriter, r *http.Request, base string) {
	rel := r.URL.Query().Get("path")
	if strings.TrimSpace(rel) == "" {
		mslxFail(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	abs, err := mslxPath(base, rel)
	if err != nil {
		mslxFail(w, http.StatusForbidden, "路径越界")
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		mslxFail(w, http.StatusNotFound, "文件不存在")
		return
	}
	inline := r.URL.Query().Get("inline") == "true"
	if inline {
		if ct := mime.TypeByExtension(filepath.Ext(abs)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", filepath.Base(abs)))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	http.ServeFile(w, r, abs)
}
