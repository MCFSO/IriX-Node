// 文件下载/上传直连通道。
// 与 MCSM 一致：先向 /api/files/download 或 /api/files/upload 申请带密码的
// 票据，再通过 /download/{password}/... 或 /upload/{password} 直接传输。

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ticketKind 票据用途类型：下载与上传严格区分。
// 此前下载票据 dir 为空，被投到 /upload/ 时 NormalizePath 会把空 cwd 当
// 相对路径处理，文件直接写进守护进程自身工作目录 —— 类型校验封死该通道。
type ticketKind int

const (
	ticketDownload ticketKind = iota // 下载票据
	ticketUpload                     // 上传票据
)

// transferTicket 下载/上传票据。
type transferTicket struct {
	uuid    string
	kind    ticketKind
	cwd     string
	dir     string // 上传目标目录（绝对路径）；下载票据为空
	file    string // 下载票据绑定的文件（绝对路径）；为空表示目录范围票据（集群/快照）
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

// Create 创建上传票据，返回密码；票据已满时返回空字符串。
// uuid 为来源实例 id（集群票据为 "cluster"），dir 为上传目标绝对目录。
func (ts *ticketStore) Create(uuid, cwd, dir string) string {
	return ts.create(ticketUpload, uuid, cwd, dir, "")
}

// CreateDownload 创建下载票据，返回密码；票据已满时返回空字符串。
// file 非空时绑定单文件（仅该文件可下载）；为空表示目录范围票据
// （集群同步区 / 快照下载按设计需要整目录范围）。
func (ts *ticketStore) CreateDownload(uuid, cwd, file string) string {
	return ts.create(ticketDownload, uuid, cwd, "", file)
}

// create 创建票据的公共实现。
func (ts *ticketStore) create(kind ticketKind, uuid, cwd, dir, file string) string {
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
		kind:    kind,
		cwd:     cwd,
		dir:     dir,
		file:    file,
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
	// 实例下载票据绑定单文件：票据只能下载申请时的这个文件，
	// 不能用来读取同目录其他文件，也不能触发目录列表浏览整棵 cwd 树。
	password := tickets.CreateDownload(uuid, cwd, path)
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
		"password":   password,
		"addr":       d.publicAddr(),
		"upload_dir": uploadDir,
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
	if t == nil || t.kind != ticketDownload {
		http.Error(w, "下载票据无效或已过期", http.StatusForbidden)
		return
	}
	// 集群票据（uuid 固定为 "cluster"）的 cwd 是同步区根：
	// 兼容客户端按 /mirrors/... 虚拟前缀拼接的下载路径。
	if t.uuid == "cluster" && strings.HasPrefix(seg[1], "mirrors/") {
		seg[1] = strings.TrimPrefix(seg[1], "mirrors/")
	}
	filePath, err := NormalizePath(t.cwd, seg[1])
	if err != nil {
		http.Error(w, "路径越界", http.StatusForbidden)
		return
	}
	// 单文件票据：只允许下载申请时绑定的文件，杜绝同目录越权读取
	// 与 http.ServeFile 的目录列表浏览（请求目录时同样在此被拒）。
	if t.file != "" && !sameFilePath(filePath, t.file) {
		http.Error(w, "下载票据仅限绑定文件", http.StatusForbidden)
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
	// 必须为上传类型且带目标目录：下载票据（dir 为空）被投到 /upload/ 时，
	// NormalizePath("", name) 会把空 cwd 当相对路径，文件将直接写进守护进程
	// 自身工作目录 —— 此处一并拒绝（审计报告 #1）。
	if t == nil || t.kind != ticketUpload || t.dir == "" {
		http.Error(w, "上传票据无效或已过期", http.StatusForbidden)
		return
	}
	// ParseMultipartForm 的错误必须处理：吞掉后只会报「缺少 file 字段」，
	// 掩盖真实原因（非 multipart、边界损坏、超限等）。
	// 32MB 为内存阈值，超出部分由标准库落临时文件，内存占用恒定。
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析上传表单失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll() // 清理标准库落盘的临时文件
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 只取文件名，丢弃客户端可能携带的路径部分，再做越界校验
	name := filepath.Base(filepath.FromSlash(header.Filename))
	if name == "." || name == string(filepath.Separator) || name == "" {
		http.Error(w, "文件名无效", http.StatusBadRequest)
		return
	}
	dest, err := NormalizePath(t.dir, name)
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

// sameFilePath 判断两个路径是否指向同一文件（Windows 忽略大小写）。
func sameFilePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

// publicAddr 返回票据中使用的地址（客户端据此拼接下载/上传 URL）。
// 绑定 0.0.0.0 时不能写死 127.0.0.1，否则远端客户端拿到的直连地址不可用；
// 此时返回本机对外 IP，取不到则退回主机名占位。
func (d *Daemon) publicAddr() string {
	host := d.BindHost
	if host == "" || host == "127.0.0.1" || host == "localhost" {
		return fmt.Sprintf("127.0.0.1:%d", d.Port)
	}
	if host == "0.0.0.0" || host == "::" || host == "[::]" {
		if ip := outboundIP(); ip != "" {
			return net.JoinHostPort(ip, strconv.Itoa(d.Port))
		}
		if name, err := os.Hostname(); err == nil && name != "" {
			return net.JoinHostPort(name, strconv.Itoa(d.Port))
		}
		return fmt.Sprintf("127.0.0.1:%d", d.Port)
	}
	return net.JoinHostPort(host, strconv.Itoa(d.Port))
}

// outboundIP 探测本机对外 IPv4 地址（不产生实际流量，UDP 仅做路由选择）。
func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
		return addr.IP.String()
	}
	return ""
}
