// Bastille jail 文件管理 API（NODE_API.md §6.1）：对 jail 内路径（如 /data）
// 提供列表 / 文本读写 / 删除 / 新建 / 上传 / 下载。
//
// 路径解析入口为 bastilleResolve：生产实现是平台函数 bastilleJailHostPath
// （FreeBSD 真实解析，其余平台返回「不支持」）；解析逻辑本身平台无关
// （resolveJailHostPath），测试可用任意目录做替身验证完整流程。

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// errJailPath jail 路径参数错误哨兵（越界 / 无效 / jail 不存在），映射为 HTTP 400。
var errJailPath = errors.New("jail 路径无效")

// bastilleResolve jail 内路径 → 宿主机绝对路径的解析入口。
// 测试可替换为指向临时目录的假实现，以在任意平台验证文件操作逻辑。
var bastilleResolve = bastilleJailHostPath

// resolveJailHostPath 平台无关的 jail 路径解析：jailPath 以 / 开头表示 root
// 内的绝对路径（如 /data），相对路径按 root 解析；.. 越界与符号链接指向
// root 外均被拒绝。返回的宿主机路径已按 root 的符号链接解析。
func resolveJailHostPath(root, jailPath string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: 根目录为空", errJailPath)
	}
	evalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: 解析 jail 根目录失败: %v", errJailPath, err)
	}
	rel := filepath.Clean(strings.TrimPrefix(strings.TrimSpace(jailPath), "/"))
	host := evalRoot
	if rel != "" && rel != "." {
		host = filepath.Join(evalRoot, rel)
	}
	if !pathWithin(evalRoot, host) {
		return "", fmt.Errorf("%w: 路径越界: %s", errJailPath, jailPath)
	}
	// 符号链接越界：解析已存在部分的最深祖先（写入新文件时父目录可能是越界链接）
	if err := checkSymlinkEscape(evalRoot, host); err != nil {
		return "", fmt.Errorf("%w: %v", errJailPath, err)
	}
	return host, nil
}

// checkSymlinkEscape 校验 path 已存在部分解析符号链接后仍在 root 内：
// 目标不存在时逐级向上解析到已存在祖先，再校验其落在 root 内。
func checkSymlinkEscape(root, path string) error {
	cur := path
	for {
		evaled, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if !pathWithin(root, evaled) {
				return errors.New("符号链接指向 jail 外")
			}
			return nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("解析路径失败: %s", path)
		}
		cur = parent
	}
}

// bastilleFilePath 解析 jail 内路径的统一出口：
// 参数类错误（越界 / jail 不存在）→ 400，平台不支持 → 501，其余 → 500。
func (d *Daemon) bastilleFilePath(w http.ResponseWriter, name, jailPath string) (string, bool) {
	host, err := bastilleResolve(name, jailPath)
	if err == nil {
		return host, true
	}
	if errors.Is(err, errJailPath) {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	containerErr(w, err)
	return "", false
}

// jailVirtualDir 计算 jailPath 的 jail 内规范化路径（条目 path 字段与 absolutePath）。
// 空路径 / "." / "data/.." 等均归一为根 "/"。
func jailVirtualDir(jailPath string) string {
	p := filepath.ToSlash(filepath.Clean(strings.TrimSpace(strings.TrimPrefix(jailPath, "/"))))
	if p == "" || p == "." {
		return "/"
	}
	return "/" + p
}

// jailVirtualJoin 拼出 jail 内完整路径（"/" 分隔）。
func jailVirtualJoin(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// handleBastilleFilesList jail 内目录列表。
// GET /api/bastille/jails/{name}/files?path&page&page_size
// 条目: {name, path, isDir, size, mtime}（path 为 jail 内绝对路径，mtime 为
// "2006-01-02 15:04:05" 字符串），目录在前按名称排序。
func (d *Daemon) handleBastilleFilesList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	page := atoiDefault(queryParam(r, "page"), 1)
	pageSize := atoiDefault(queryParam(r, "page_size"), 100)
	dir := queryParam(r, "path")
	host, ok := d.bastilleFilePath(w, name, dir)
	if !ok {
		return
	}
	entries, err := os.ReadDir(host)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取目录失败: "+err.Error())
		return
	}
	vdir := jailVirtualDir(dir)
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, map[string]any{
			"name":  e.Name(),
			"path":  jailVirtualJoin(vdir, e.Name()),
			"isDir": info.IsDir(),
			"size":  info.Size(),
			"mtime": info.ModTime().Format(clusterMtimeFormat),
		})
	}
	// 目录在前，按名称排序
	sort.Slice(items, func(a, b int) bool {
		da, db := items[a]["isDir"].(bool), items[b]["isDir"].(bool)
		if da != db {
			return da
		}
		return items[a]["name"].(string) < items[b]["name"].(string)
	})
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	start := (page - 1) * pageSize
	if start > len(items) {
		start = len(items)
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	writeOK(w, map[string]any{
		"items":        items[start:end],
		"page":         page - 1,
		"pageSize":     pageSize,
		"total":        len(items),
		"absolutePath": vdir,
	})
}

// handleBastilleFilesContentRead 读取 jail 内文本文件。
// GET /api/bastille/jails/{name}/files/content?path=...
// 与实例文件读写接口一致：上限 8 MiB，超限提示改用下载接口。
func (d *Daemon) handleBastilleFilesContentRead(w http.ResponseWriter, r *http.Request) {
	p := queryParam(r, "path")
	if p == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), p)
	if !ok {
		return
	}
	info, err := os.Stat(host)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取文件失败: "+err.Error())
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "目标为目录，无法按文本读取")
		return
	}
	if info.Size() > maxTextReadBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"文件过大（%s），文本读取上限为 %s，请改用下载接口",
			FormatSize(info.Size()), FormatSize(maxTextReadBytes)))
		return
	}
	data, err := os.ReadFile(host)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取文件失败: "+err.Error())
		return
	}
	writeOK(w, string(data))
}

// handleBastilleFilesContentWrite 写入 jail 内文本文件（覆盖写，父目录自动创建）。
// PUT /api/bastille/jails/{name}/files/content body: {path, content}
func (d *Daemon) handleBastilleFilesContentWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), body.Path)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建父目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(host, []byte(body.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleBastilleFilesDelete 删除 jail 内文件/目录（递归，幂等）。
// DELETE /api/bastille/jails/{name}/files?path=...
// jail 根目录（path 为空、"/" 或规范化后为根）禁止删除。
func (d *Daemon) handleBastilleFilesDelete(w http.ResponseWriter, r *http.Request) {
	p := queryParam(r, "path")
	if jailVirtualDir(p) == "/" {
		writeError(w, http.StatusBadRequest, "不允许删除 jail 根目录")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), p)
	if !ok {
		return
	}
	if err := os.RemoveAll(host); err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleBastilleFilesMkdir 在 jail 内新建目录（递归创建）。
// POST /api/bastille/jails/{name}/files/mkdir body: {path}
func (d *Daemon) handleBastilleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), body.Path)
	if !ok {
		return
	}
	if err := os.MkdirAll(host, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleBastilleFilesTouch 在 jail 内新建空文件（已存在时不覆盖内容）。
// POST /api/bastille/jails/{name}/files/touch body: {path}
func (d *Daemon) handleBastilleFilesTouch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), body.Path)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	f, err := os.OpenFile(host, os.O_CREATE, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	_ = f.Close()
	writeOK(w, true)
}

// handleBastilleFilesUpload 上传文件到 jail 内目录（multipart，字段名 file）。
// POST /api/bastille/jails/{name}/files/upload?path=<目录，默认 />
// 只取文件名、丢弃客户端携带的路径部分；响应 {path: jail 内保存路径}。
func (d *Daemon) handleBastilleFilesUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dir := queryParam(r, "path")
	if dir == "" {
		dir = "/"
	}
	hostDir, ok := d.bastilleFilePath(w, name, dir)
	if !ok {
		return
	}
	// 32MB 为内存阈值，超出部分由标准库落临时文件，内存占用恒定
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少 file 字段: "+err.Error())
		return
	}
	defer file.Close()
	// 只取文件名，丢弃客户端可能携带的路径部分，再做越界校验
	fname := filepath.Base(filepath.FromSlash(header.Filename))
	if fname == "." || fname == string(filepath.Separator) || fname == "" {
		writeError(w, http.StatusBadRequest, "文件名无效")
		return
	}
	dest := filepath.Join(hostDir, fname)
	if !pathWithin(hostDir, dest) {
		writeError(w, http.StatusBadRequest, "文件名越界")
		return
	}
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	out, err := os.Create(dest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建文件失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		_ = os.Remove(dest)
		writeError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		writeError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"path": jailVirtualJoin(jailVirtualDir(dir), fname)})
}

// handleBastilleFilesDownload 下载 jail 内文件（二进制流，不走 JSON 信封）。
// GET /api/bastille/jails/{name}/files/download?path=...
func (d *Daemon) handleBastilleFilesDownload(w http.ResponseWriter, r *http.Request) {
	p := queryParam(r, "path")
	if p == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	host, ok := d.bastilleFilePath(w, r.PathValue("name"), p)
	if !ok {
		return
	}
	info, err := os.Stat(host)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "文件不存在: "+p)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(host)))
	http.ServeFile(w, r, host)
}
