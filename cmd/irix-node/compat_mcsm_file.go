// MCSManager Daemon 兼容层：文件管理事件与直连下载/上传通道。
//
// 所有路径都必须经过 safeResolve（= NormalizePath + 软链与越界校验），
// 与节点自身文件 API 使用同一套边界检查，兼容层不放宽任何限制。

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// mcFileItem 文件列表条目（对齐 MCSM 的 IFile：name/size/time/type/mode）。
type mcFileItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time string `json:"time"`
	Type int    `json:"type"` // 0 = 目录，1 = 文件
	Mode int    `json:"mode"`
}

// mcFileListResult 文件列表响应。
type mcFileListResult struct {
	Items        []mcFileItem `json:"items"`
	Page         int          `json:"page"`
	PageSize     int          `json:"pageSize"`
	Total        int          `json:"total"`
	AbsolutePath string       `json:"absolutePath"`
}

// mcJSDateString 生成与 JS Date.toString() 同形的字符串，便于面板直接展示。
func mcJSDateString(t time.Time) string {
	return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700")
}

// mcFileMode 取文件权限末三位（与 MCSM 一致）。
func mcFileMode(mode os.FileMode) int {
	perm := int(mode.Perm())
	// 与 MCSM 的 parseInt(octal).slice(-3) 等价：返回八进制末三位数值
	return (perm / 64 % 8 * 100) + (perm / 8 % 8 * 10) + perm%8
}

// mcResolveTarget 把面板下发的相对路径解析为工作目录内的绝对路径。
func mcResolveTarget(cwd, target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		t = "/"
	}
	return safeResolve(cwd, t)
}

// mcEventFileList 列出实例工作目录下的文件。
func mcEventFileList(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Target       string `json:"target"`
		FileName     string `json:"fileName"`
		Page         int    `json:"page"`
		PageSize     int    `json:"pageSize"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	dir, err := mcResolveTarget(cwd, req.Target)
	if err != nil {
		return nil, err
	}
	page := req.Page
	if page < 0 {
		page = 0
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 40
	}
	if pageSize > 100 {
		pageSize = 100
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	keyword := strings.ToLower(strings.TrimSpace(req.FileName))
	items := make([]mcFileItem, 0, len(entries))
	for _, e := range entries {
		if keyword != "" && !strings.Contains(strings.ToLower(e.Name()), keyword) {
			continue
		}
		info, ierr := e.Info()
		item := mcFileItem{Name: e.Name(), Type: 1}
		if ierr == nil {
			if info.IsDir() {
				item.Type = 0
				item.Size = 0
			} else {
				item.Size = info.Size()
			}
			item.Time = mcJSDateString(info.ModTime())
			item.Mode = mcFileMode(info.Mode())
		} else {
			item.Time = mcJSDateString(timesys.Now())
		}
		// 符号链接按目标类型归类（与 MCSM 一致的容错）
		if item.Type == 0 && !e.IsDir() {
			if st, serr := os.Stat(filepath.Join(dir, e.Name())); serr == nil && !st.IsDir() {
				item.Type = 1
				item.Size = st.Size()
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Type != items[b].Type {
			return items[a].Type < items[b].Type
		}
		return strings.ToLower(items[a].Name) < strings.ToLower(items[b].Name)
	})
	total := len(items)
	start := page * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return mcFileListResult{Items: items[start:end], Page: page, PageSize: pageSize, Total: total, AbsolutePath: dir}, nil
}

// mcEventFileStatus 文件管理状态（本节点无后台压缩/下载队列，返回空计数）。
func mcEventFileStatus(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return map[string]any{
		"instanceFileTask":        0,
		"globalFileTask":          0,
		"downloadFileFromURLTask": 0,
		"downloadTasks":           []any{},
		"platform":                runtime.GOOS,
		"isGlobalInstance":        false,
		"disks":                   []any{},
	}, nil
}

// mcFileTarget 取 {instanceUuid, target} 并解析为绝对路径。
func mcFileTarget(mc *mcsmCompat, data json.RawMessage) (cwd, abs string, err error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Target       string `json:"target"`
	}
	if err := decode(data, &req); err != nil {
		return "", "", fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err = mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return "", "", err
	}
	abs, err = mcResolveTarget(cwd, req.Target)
	return cwd, abs, err
}

// mcEventFileTouch 新建空文件。
func mcEventFileTouch(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	_, abs, err := mcFileTarget(mc, data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}
	_ = f.Close()
	return true, nil
}

// mcEventFileMkdir 新建目录。
func mcEventFileMkdir(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	_, abs, err := mcFileTarget(mc, data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}
	return true, nil
}

// mcEventFileDelete 删除文件/目录。
func mcEventFileDelete(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string   `json:"instanceUuid"`
		Targets      []string `json:"targets"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	for _, target := range req.Targets {
		abs, err := mcResolveTarget(cwd, target)
		if err != nil {
			return nil, err
		}
		if abs == cwd {
			return nil, errors.New("不能删除实例工作目录本身")
		}
		if err := os.RemoveAll(abs); err != nil {
			return nil, fmt.Errorf("删除 %s 失败: %w", target, err)
		}
	}
	return true, nil
}

// mcEventFileEdit 编辑文本文件。
func mcEventFileEdit(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		Target       string `json:"target"`
		Text         string `json:"text"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	abs, err := mcResolveTarget(cwd, req.Target)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(abs, []byte(req.Text), 0o644); err != nil {
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}
	return true, nil
}

// mcEventFileCopy 复制文件/目录（targets 为 [源, 目标] 数组）。
func mcEventFileCopy(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	return mcFileTransfer(mc, data, false)
}

// mcEventFileMove 移动文件/目录。
func mcEventFileMove(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	return mcFileTransfer(mc, data, true)
}

// mcFileTransfer 复制/移动的公共实现。
func mcFileTransfer(mc *mcsmCompat, data json.RawMessage, move bool) (any, error) {
	var req struct {
		InstanceUUID string      `json:"instanceUuid"`
		Targets      [][2]string `json:"targets"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	for _, pair := range req.Targets {
		src, err := mcResolveTarget(cwd, pair[0])
		if err != nil {
			return nil, err
		}
		dst, err := mcResolveTarget(cwd, pair[1])
		if err != nil {
			return nil, err
		}
		if src == cwd {
			return nil, errors.New("不能对实例工作目录本身执行该操作")
		}
		if move {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			if err := os.Rename(src, dst); err != nil {
				// 跨卷时退化为「复制 + 删除」
				if cerr := copyPath(cwd, src, dst, 0); cerr != nil {
					return nil, fmt.Errorf("移动 %s → %s 失败: %w", pair[0], pair[1], err)
				}
				_ = os.RemoveAll(src)
			}
			continue
		}
		if err := copyPath(cwd, src, dst, 0); err != nil {
			return nil, fmt.Errorf("复制 %s → %s 失败: %w", pair[0], pair[1], err)
		}
	}
	return true, nil
}

// mcEventFileCompress 压缩（type=1）或解压（type=0）。
func mcEventFileCompress(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string   `json:"instanceUuid"`
		Source       string   `json:"source"`
		Targets      []string `json:"targets"`
		Type         int      `json:"type"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	src, err := mcResolveTarget(cwd, req.Source)
	if err != nil {
		return nil, err
	}
	if req.Type == 1 {
		if len(req.Targets) == 0 {
			return nil, errors.New("压缩至少需要一个目标")
		}
		return true, createZip(src, cwd, req.Targets)
	}
	dest := "."
	if len(req.Targets) > 0 {
		dest = req.Targets[0]
	}
	destAbs, err := mcResolveTarget(cwd, dest)
	if err != nil {
		return nil, err
	}
	return true, unzip(src, destAbs)
}

// mcEventFileChmodUnsupported 权限修改本节点不支持（跨平台不一致）。
func mcEventFileChmodUnsupported(_ *mcsmCompat, _ *eioSession, _ json.RawMessage) (any, error) {
	return nil, errors.New("本节点不支持通过 MCSManager 面板修改文件权限")
}

// ---------------------------------------------------------------------------
// URL 下载（面板「从链接下载」）
// ---------------------------------------------------------------------------

// mcURLDown URL 下载任务状态。
type mcURLDown struct {
	id      string
	path    string
	total   int64
	current int64
	status  int // 0 进行中 / 1 完成 / 2 失败
	errText string
	cancel  func()
}

// mcEventFileDownloadFromURL 后台下载链接到实例目录。
func mcEventFileDownloadFromURL(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		InstanceUUID string `json:"instanceUuid"`
		URL          string `json:"url"`
		FileName     string `json:"fileName"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	cwd, err := mc.d.CwdOf(req.InstanceUUID)
	if err != nil {
		return nil, err
	}
	dest, err := mcResolveTarget(cwd, req.FileName)
	if err != nil {
		return nil, err
	}
	if err := validateDownloadURL(req.URL); err != nil {
		return nil, err
	}
	id := mc.nextID("dl")
	mc.mu.Lock()
	if mc.urlDown == nil {
		mc.urlDown = map[string]*mcURLDown{}
	}
	mc.urlDown[id] = &mcURLDown{id: id, path: dest}
	mc.mu.Unlock()
	safeGo("MCSM 兼容层 URL 下载", func() {
		defer recoverLog("MCSM 兼容层 URL 下载")
		mc.runURLDownload(id, req.URL, dest)
	})
	return map[string]any{}, nil
}

// runURLDownload 执行下载并更新任务状态。
func (mc *mcsmCompat) runURLDownload(id, url, dest string) {
	fail := func(err error) {
		mc.mu.Lock()
		if t := mc.urlDown[id]; t != nil {
			t.status = 2
			t.errText = err.Error()
		}
		mc.mu.Unlock()
		alog.Printf("MCSManager 兼容层：下载 %s 失败: %v", url, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fail(err)
		return
	}
	client := jdkHTTPClient(0) // 复用节点既有出站客户端（逐跳校验链接）
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fail(err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	mc.mu.Lock()
	if t := mc.urlDown[id]; t != nil {
		t.total = resp.ContentLength
	}
	mc.mu.Unlock()
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		fail(err)
		return
	}
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				_ = os.Remove(tmp)
				fail(werr)
				return
			}
			written += int64(n)
			mc.mu.Lock()
			if t := mc.urlDown[id]; t != nil {
				t.current = written
			}
			mc.mu.Unlock()
		}
		if rerr != nil {
			if rerr != io.EOF {
				f.Close()
				_ = os.Remove(tmp)
				fail(rerr)
				return
			}
			break
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		fail(err)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		fail(err)
		return
	}
	mc.mu.Lock()
	if t := mc.urlDown[id]; t != nil {
		t.status = 1
		t.current = written
		if t.total <= 0 {
			t.total = written
		}
	}
	mc.mu.Unlock()
}

// mcEventFileDownloadFromURLStop 停止 URL 下载任务。
func mcEventFileDownloadFromURLStop(mc *mcsmCompat, _ *eioSession, data json.RawMessage) (any, error) {
	var req struct {
		TaskID string `json:"taskId"`
	}
	if err := decode(data, &req); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	mc.mu.Lock()
	t := mc.urlDown[req.TaskID]
	if t != nil {
		t.status = 2
		t.errText = "已取消"
		if t.cancel != nil {
			t.cancel()
		}
	}
	mc.mu.Unlock()
	if t == nil {
		return false, nil
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// 直连下载 / 上传（票据通行证）
// ---------------------------------------------------------------------------

// handleCompatDownload 直连下载：GET /download/{通行证}/{文件名}
func (mc *mcsmCompat) handleCompatDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/download/")
	seg := strings.SplitN(rest, "/", 2)
	if len(seg) != 2 || seg[0] == "" || seg[1] == "" {
		http.Error(w, "无效的下载链接", http.StatusBadRequest)
		return
	}
	m := mc.takeMission(seg[0], "download")
	if m == nil {
		http.Error(w, "下载通行证无效或已过期", http.StatusForbidden)
		return
	}
	uuid := missionString(m, "instanceUuid")
	fileName := missionString(m, "fileName")
	if uuid == "" || fileName == "" {
		http.Error(w, "下载通行证参数不完整", http.StatusForbidden)
		return
	}
	cwd, err := mc.d.CwdOf(uuid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	abs, err := safeResolve(cwd, fileName)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	// 通行证只允许下载申请时的那个文件名：URL 里的文件名与通行证绑定的
	// 文件名必须一致，防止拿同一条链接去读同目录的其他文件。
	if !strings.EqualFold(seg[1], path.Base(filepath.ToSlash(fileName))) {
		http.Error(w, "下载通行证仅限绑定文件", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(abs)))
	http.ServeFile(w, r, abs)
}

// handleCompatUpload 直连上传：POST /upload/{通行证}（multipart，字段名 file）
func (mc *mcsmCompat) handleCompatUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/upload/")
	key = strings.Trim(key, "/")
	m := mc.takeMission(key, "upload")
	if m == nil {
		http.Error(w, "上传通行证无效或已过期", http.StatusForbidden)
		return
	}
	uuid := missionString(m, "instanceUuid")
	uploadDir := missionString(m, "uploadDir")
	if uuid == "" {
		http.Error(w, "上传通行证参数不完整", http.StatusForbidden)
		return
	}
	cwd, err := mc.d.CwdOf(uuid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	dir, err := mcResolveTarget(cwd, uploadDir)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析上传表单失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	name := filepath.Base(filepath.FromSlash(header.Filename))
	if name == "" || name == "." || name == string(filepath.Separator) {
		http.Error(w, "文件名无效", http.StatusBadRequest)
		return
	}
	dest, err := safeResolve(dir, name)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, "创建文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		http.Error(w, "写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		http.Error(w, "写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK")
}

// mcUploadTask 分片上传会话（MCSM 新文件管理器的分块上传）。
type mcUploadTask struct {
	id     string
	path   string
	size   int64
	unzip  bool
	mu     sync.Mutex
	f      *os.File
	ranges []mcRange
}

// mcRange 已写入的字节区间。
type mcRange struct {
	start int64
	end   int64
}

// covered 返回已覆盖的总字节数。
func (t *mcUploadTask) covered() int64 {
	var n int64
	for _, r := range t.ranges {
		if r.end > r.start {
			n += r.end - r.start
		}
	}
	return n
}

// handleCompatUploadNew 新建分片上传会话：POST /upload-new/{通行证}?filename=&size=
// 响应 {data:{id, received:[{start,end}]}, status:200}
func (mc *mcsmCompat) handleCompatUploadNew(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(strings.TrimPrefix(r.URL.Path, "/upload-new/"), "/")
	m := mc.takeMission(key, "upload")
	if m == nil {
		http.Error(w, "上传通行证无效或已过期", http.StatusForbidden)
		return
	}
	uuid := missionString(m, "instanceUuid")
	uploadDir := missionString(m, "uploadDir")
	cwd, err := mc.d.CwdOf(uuid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	dir, err := mcResolveTarget(cwd, uploadDir)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	name := filepath.Base(filepath.FromSlash(q.Get("filename")))
	if name == "" || name == "." {
		http.Error(w, "缺少 filename 参数", http.StatusBadRequest)
		return
	}
	size := int64(atoiDefault(q.Get("size"), 0))
	dest, err := safeResolve(dir, name)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "创建文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	task := &mcUploadTask{id: mc.nextID("up"), path: dest, size: size, unzip: q.Get("unzip") == "true", f: f}
	if size == 0 {
		task.ranges = append(task.ranges, mcRange{start: 0, end: 0})
	}
	mc.mu.Lock()
	if mc.uploads == nil {
		mc.uploads = map[string]*mcUploadTask{}
	}
	mc.uploads[task.id] = task
	mc.mu.Unlock()
	writeCompatJSON(w, http.StatusOK, map[string]any{
		"status": 200,
		"data": map[string]any{
			"id":       task.id,
			"received": []any{},
		},
	})
}

// handleCompatUploadPiece 写入分片：POST /upload-piece/{id}?offset=
func (mc *mcsmCompat) handleCompatUploadPiece(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/upload-piece/"), "/")
	mc.mu.Lock()
	task := mc.uploads[id]
	mc.mu.Unlock()
	if task == nil {
		http.Error(w, "上传任务不存在", http.StatusForbidden)
		return
	}
	offset := int64(atoiDefault(r.URL.Query().Get("offset"), -1))
	if offset < 0 {
		http.Error(w, "缺少 offset 参数", http.StatusBadRequest)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析上传表单失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128<<20))
	if err != nil {
		http.Error(w, "读取分片失败", http.StatusBadRequest)
		return
	}
	task.mu.Lock()
	_, werr := task.f.WriteAt(data, offset)
	if werr == nil {
		task.ranges = append(task.ranges, mcRange{start: offset, end: offset + int64(len(data))})
	}
	done := task.size > 0 && task.covered() >= task.size
	task.mu.Unlock()
	if werr != nil {
		http.Error(w, "写入分片失败: "+werr.Error(), http.StatusInternalServerError)
		return
	}
	if done {
		mc.finishUpload(task)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK")
}

// finishUpload 收尾分片上传（关闭文件、按需解压）。
func (mc *mcsmCompat) finishUpload(task *mcUploadTask) {
	task.mu.Lock()
	if task.f != nil {
		_ = task.f.Close()
		task.f = nil
	}
	task.mu.Unlock()
	if task.unzip {
		cwd := filepath.Dir(task.path)
		if err := unzip(task.path, cwd); err != nil {
			alog.Printf("MCSManager 兼容层：上传后解压 %s 失败: %v", task.path, err)
		}
	}
	mc.mu.Lock()
	delete(mc.uploads, task.id)
	mc.mu.Unlock()
}
