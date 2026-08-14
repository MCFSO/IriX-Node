// 文件管理 API：列表/读写/删除/移动/复制/压缩/新建目录/新建文件。
// 路由与响应格式对齐 MCSManager（见 apis/api_fileManager.md）。
// 所有文件操作均限定在实例的工作目录（cwd）内。

package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fileTimeFormat 与 MCSM 返回的时间字符串格式保持一致。
const fileTimeFormat = "Mon Jan 02 2006 15:04:05 GMT+0800 (中国标准时间)"

// entryType 文件类型：0 = 文件夹, 1 = 文件。
func entryType(info os.FileInfo) int {
	if info.IsDir() {
		return 0
	}
	return 1
}

// handleFileList 获取文件列表。
// GET /api/files/list?daemonId&uuid&target&page&page_size
// 条目含 name/size/time/mode/type，以及增量同步用的 mtime/sha256
// （sha256 为文件内容摘要，目录为空串；详见 docs/cluster-node-api.md §4）。
func (d *Daemon) handleFileList(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	cwd, err := d.CwdOf(uuid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, total, abs, err := listDir(cwd, queryParam(r, "target"),
		atoiDefault(queryParam(r, "page"), 1), atoiDefault(queryParam(r, "page_size"), 100))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{
		"items":        items,
		"page":         atoiDefault(queryParam(r, "page"), 1) - 1,
		"pageSize":     atoiDefault(queryParam(r, "page_size"), 100),
		"total":        total,
		"absolutePath": abs,
	})
}

// listDir 列出目录内容：条目含 name/size/time/mode/type/mtime/sha256，
// 目录在前按名称排序，按 page/pageSize 分页。
// 返回 (当前页条目, 总条目数, 相对 cwd 的绝对路径, 错误)。
func listDir(cwd, target string, page, pageSize int) ([]map[string]any, int, string, error) {
	dir, err := NormalizePath(cwd, target)
	if err != nil {
		return nil, 0, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, "", err
	}

	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		item := map[string]any{
			"name":  e.Name(),
			"size":  info.Size(),
			"time":  info.ModTime().Format(fileTimeFormat),
			"mtime": info.ModTime().Format(clusterMtimeFormat),
			"mode":  int(info.Mode().Perm()),
			"type":  entryType(info),
		}
		if info.IsDir() {
			item["sha256"] = ""
		} else {
			item["sha256"] = fileSHA256(full)
		}
		items = append(items, item)
	}
	// 目录在前，按名称排序
	sort.Slice(items, func(a, b int) bool {
		ta, tb := items[a]["type"].(int), items[b]["type"].(int)
		if ta != tb {
			return ta < tb
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

	abs := strings.ReplaceAll(strings.TrimPrefix(dir, cwd), "\\", "/")
	if abs == "" {
		abs = "/"
	}
	return items[start:end], len(items), abs, nil
}

// maxTextReadBytes 文本读取接口的单文件上限。
// 文件内容会整体进内存并转成 string 后再 JSON 编码，必须设上限防止 OOM；
// 超限文件应走 /api/files/download 票据通道（流式，内存恒定）。
const maxTextReadBytes = 8 << 20 // 8 MiB

// handleFileReadWrite 读取或写入文件内容。
// PUT /api/files/?daemonId&uuid  body: {target, text?}
//   - 仅含 target：读取文件内容，返回字符串
//   - 含 text：写入文件内容（可为空字符串），返回 true
func (d *Daemon) handleFileReadWrite(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	cwd, err := d.CwdOf(uuid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 一次性读入后解码到类型化结构：比 map[string]any 少一层装箱，
	// 也比 json.Decoder 少一轮内部缓冲倍增拷贝（实测大载荷分配更低）。
	// text 用指针区分「未提供」（读取）与「提供空串」（写入空文件）。
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	var body struct {
		Target string  `json:"target"`
		Text   *string `json:"text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	raw = nil // 尽早允许回收请求体副本
	path, err := NormalizePath(cwd, body.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if body.Text != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(path, []byte(*body.Text), 0o644); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, true)
		return
	}

	// 读取前先查大小：整个文件会被读进内存并转成 string（约两倍占用），
	// 无上限时单个大文件即可打爆守护进程内存。
	info, err := os.Stat(path)
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
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取文件失败: "+err.Error())
		return
	}
	writeOK(w, string(data))
}

// handleFileDelete 删除文件/目录。
// DELETE /api/files?daemonId&uuid  body: {targets: [...]}
func (d *Daemon) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	cwd, uuid, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Targets []string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	for _, t := range body.Targets {
		path, err := NormalizePath(cwd, t)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := os.RemoveAll(path); err != nil {
			writeError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
	}
	_ = uuid
	writeOK(w, true)
}

// handleFileMove 移动或重命名。
// PUT /api/files/move?daemonId&uuid  body: {targets: [[src, dst], ...]}
func (d *Daemon) handleFileMove(w http.ResponseWriter, r *http.Request) {
	cwd, _, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Targets [][]string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	for _, pair := range body.Targets {
		if len(pair) != 2 {
			continue
		}
		src, err := NormalizePath(cwd, pair[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		dst, err := NormalizePath(cwd, pair[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := os.Rename(src, dst); err != nil {
			writeError(w, http.StatusInternalServerError, "移动失败: "+err.Error())
			return
		}
	}
	writeOK(w, true)
}

// handleFileCopy 复制文件/目录。
// POST /api/files/copy?daemonId&uuid  body: {targets: [[src, dst], ...]}
func (d *Daemon) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	cwd, _, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Targets [][]string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	for _, pair := range body.Targets {
		if len(pair) != 2 {
			continue
		}
		src, err := NormalizePath(cwd, pair[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		dst, err := NormalizePath(cwd, pair[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := copyPath(src, dst); err != nil {
			writeError(w, http.StatusInternalServerError, "复制失败: "+err.Error())
			return
		}
	}
	writeOK(w, true)
}

// handleFileCompress 压缩/解压。
// POST /api/files/compress?daemonId&uuid
// body: {type: 1=压缩, 2=解压, code: "utf-8", source: 压缩包路径, targets: 目标文件列表 或 解压目录}
func (d *Daemon) handleFileCompress(w http.ResponseWriter, r *http.Request) {
	cwd, _, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Type    int      `json:"type"`
		Code    string   `json:"code"`
		Source  string   `json:"source"`
		Targets []string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	switch body.Type {
	case 1: // 压缩
		zipPath, err := NormalizePath(cwd, body.Source)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := createZip(zipPath, cwd, body.Targets); err != nil {
			writeError(w, http.StatusInternalServerError, "压缩失败: "+err.Error())
			return
		}
	case 2: // 解压
		zipPath, err := NormalizePath(cwd, body.Source)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var dest string
		if len(body.Targets) > 0 {
			dest, err = NormalizePath(cwd, body.Targets[0])
		} else {
			dest = filepath.Dir(zipPath)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := unzip(zipPath, dest); err != nil {
			writeError(w, http.StatusInternalServerError, "解压失败: "+err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "type 仅支持 1=压缩, 2=解压")
		return
	}
	writeOK(w, true)
}

// handleFileMkdir 创建文件夹。
// POST /api/files/mkdir?daemonId&uuid  body: {target}
func (d *Daemon) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	cwd, _, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	path, err := NormalizePath(cwd, body.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleFileTouch 新建空文件。
// POST /api/files/touch?daemonId&uuid  body: {target}
func (d *Daemon) handleFileTouch(w http.ResponseWriter, r *http.Request) {
	cwd, _, ok := d.fileScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	path, err := NormalizePath(cwd, body.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	_ = f.Close()
	writeOK(w, true)
}

// fileScope 解析实例工作目录与 uuid 公共逻辑。
func (d *Daemon) fileScope(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	uuid := queryParam(r, "uuid")
	cwd, err := d.CwdOf(uuid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	return cwd, uuid, true
}

// copyPath 递归复制文件或目录。
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(src, path)
			target := filepath.Join(dst, rel)
			if fi.IsDir() {
				return os.MkdirAll(target, fi.Mode().Perm())
			}
			return copyFile(path, target)
		})
	}
	return copyFile(src, dst)
}

// copyFile 复制单个文件。
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// createZip 将 cwd 下的 targets 打包到 zipPath。
func createZip(zipPath, cwd string, targets []string) error {
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	for _, t := range targets {
		path, err := NormalizePath(cwd, t)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), strings.TrimSuffix(filepath.ToSlash(cwd), "/"))
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = filepath.Base(path)
		}
		if err := addToZip(zw, path, rel); err != nil {
			return err
		}
	}
	return nil
}

// addToZip 递归将文件/目录加入 zip。
func addToZip(zw *zip.Writer, path, rel string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := addToZip(zw, filepath.Join(path, e.Name()), filepath.Join(rel, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	w, err := zw.Create(filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// unzip 解压 zip 到 dest 目录（防路径穿越）。
func unzip(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(dest, name)
		if !pathWithin(dest, target) {
			return fmt.Errorf("解压路径越界: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if cerr != nil {
			return cerr
		}
	}
	return nil
}
