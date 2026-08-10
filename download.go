// 文件下载/上传直连通道。
// 与 MCSM 一致：先向 /api/files/download 或 /api/files/upload 申请带密码的
// 票据，再通过 /download/{password}/... 或 /upload/{password} 直接传输。

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// transferTicket 下载/上传票据。
type transferTicket struct {
	uuid    string
	cwd     string
	dir     string // 上传目标目录（相对 cwd）
	expires time.Time
}

// ticketStore 票据存储。
type ticketStore struct {
	mu      sync.Mutex
	tickets map[string]*transferTicket
}

// NewTicketStore 创建票据存储。
func NewTicketStore() *ticketStore {
	ts := &ticketStore{tickets: map[string]*transferTicket{}}
	go ts.cleanupLoop()
	return ts
}

// maxTickets 票据上限，防止恶意刷票据耗尽内存。
const maxTickets = 10000

// Create 创建票据，返回密码；票据已满时返回空字符串。
func (ts *ticketStore) Create(uuid, cwd, dir string) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.tickets) >= maxTickets {
		// 先尝试回收过期票据
		now := time.Now()
		for k, v := range ts.tickets {
			if now.After(v.expires) {
				delete(ts.tickets, k)
			}
		}
		if len(ts.tickets) >= maxTickets {
			return ""
		}
	}
	password := newUUID()
	ts.tickets[password] = &transferTicket{
		uuid:    uuid,
		cwd:     cwd,
		dir:     dir,
		expires: time.Now().Add(10 * time.Minute),
	}
	return password
}

// Get 获取票据并校验有效期。
func (ts *ticketStore) Get(password string) *transferTicket {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.tickets[password]
	if t == nil || time.Now().After(t.expires) {
		return nil
	}
	return t
}

// cleanupLoop 定期清理过期票据。
func (ts *ticketStore) cleanupLoop() {
	for {
		time.Sleep(time.Minute)
		ts.mu.Lock()
		now := time.Now()
		for k, v := range ts.tickets {
			if now.After(v.expires) {
				delete(ts.tickets, k)
			}
		}
		ts.mu.Unlock()
	}
}

// tickets 全局票据存储。
var tickets = NewTicketStore()

// handleFileDownloadTicket 申请下载票据。
// POST /api/files/download?file_name&daemonId&uuid
// 响应: {password, addr}
func (d *Daemon) handleFileDownloadTicket(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	cwd, err := d.CwdOf(uuid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fileName := queryParam(r, "file_name")
	if fileName == "" {
		writeError(w, http.StatusBadRequest, "缺少 file_name 参数")
		return
	}
	path, err := NormalizePath(cwd, fileName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "文件不存在: "+fileName)
		return
	}
	password := tickets.Create(uuid, cwd, "")
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "下载票据已满，请稍后重试")
		return
	}
	writeOK(w, map[string]any{
		"password": password,
		"addr":     d.publicAddr(),
	})
}

// handleFileUploadTicket 申请上传票据。
// POST /api/files/upload?upload_dir&daemonId&uuid
// 响应: {password, addr}
func (d *Daemon) handleFileUploadTicket(w http.ResponseWriter, r *http.Request) {
	uuid := queryParam(r, "uuid")
	cwd, err := d.CwdOf(uuid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	uploadDir := queryParam(r, "upload_dir")
	if uploadDir == "" {
		uploadDir = "/"
	}
	dir, err := NormalizePath(cwd, uploadDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	password := tickets.Create(uuid, cwd, dir)
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "上传票据已满，请稍后重试")
		return
	}
	writeOK(w, map[string]any{
		"password": password,
		"addr":     d.publicAddr(),
	})
}

// handleDirectDownload 直连下载。
// GET /download/{password}/{path...}
func (d *Daemon) handleDirectDownload(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/download/")
	seg := strings.SplitN(rest, "/", 2)
	if len(seg) != 2 || seg[0] == "" || seg[1] == "" {
		http.Error(w, "无效的下载链接", http.StatusBadRequest)
		return
	}
	t := tickets.Get(seg[0])
	if t == nil {
		http.Error(w, "下载票据无效或已过期", http.StatusForbidden)
		return
	}
	filePath, err := NormalizePath(t.cwd, seg[1])
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(filePath)))
	http.ServeFile(w, r, filePath)
}

// handleDirectUpload 直连上传（multipart）。
// POST /upload/{password}  form field: file
func (d *Daemon) handleDirectUpload(w http.ResponseWriter, r *http.Request) {
	password := strings.TrimPrefix(r.URL.Path, "/upload/")
	t := tickets.Get(password)
	if t == nil {
		http.Error(w, "上传票据无效或已过期", http.StatusForbidden)
		return
	}
	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	dest, err := NormalizePath(t.dir, header.Filename)
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, "创建文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, "写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "OK")
}

// publicAddr 返回票据中使用的地址（客户端据此拼接下载/上传 URL）。
func (d *Daemon) publicAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", d.Port)
}
