// 集群节点 API（docs/cluster-node-api.md）：
// - P0：节点级文件存储（同步区 /mirrors，不依赖具体实例）
// - P1：递归快照（sync/list）+ 整目录快照/恢复（snapshot/restore）
// - P2：集群协调基础版（status/heartbeat/events/peers/transfer）
//
// 同步区根目录为 {data}/mirrors，所有路径经 NormalizePath 防越界；
// 直连下载/上传复用既有 /download/、/upload/ 票据通道。

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clusterMtimeFormat 同步条目 mtime 的字符串格式（与文档示例一致）。
const clusterMtimeFormat = "2006-01-02 15:04:05"

// maxClusterEvents 集群事件保留条数。
const maxClusterEvents = 100

// clusterRoot 同步区根目录。
func (d *Daemon) clusterRoot() string {
	return filepath.Join(d.DataDir, "mirrors")
}

// clusterPath 把 API 路径规范化为同步区绝对路径。
// 文档约定路径形如 /mirrors/<instanceId>/...：/mirrors 是同步区的虚拟前缀，
// 剥掉后映射到同步区根；无前缀的相对路径同样按同步区根解析。
func (d *Daemon) clusterPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == "/" || p == "/mirrors" || p == "mirrors":
		return d.clusterRoot(), nil
	case strings.HasPrefix(p, "/mirrors/"):
		p = strings.TrimPrefix(p, "/mirrors/")
	case strings.HasPrefix(p, "mirrors/"):
		p = strings.TrimPrefix(p, "mirrors/")
	}
	return NormalizePath(d.clusterRoot(), p)
}

// fileSHA256 计算文件 SHA-256 十六进制摘要；目录或读取失败返回空串。
func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// syncEntry 递归快照条目（path 为相对根目录的 / 前缀路径）。
type syncEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mtime  string `json:"mtime"`
	SHA256 string `json:"sha256"`
	Type   int    `json:"type"`
}

// walkEntries 递归枚举 root 下全部条目（不含 root 自身；不跟随符号链接）。
func walkEntries(root string) ([]syncEntry, error) {
	entries := make([]syncEntry, 0, 64)
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		e := syncEntry{
			Path:  "/" + filepath.ToSlash(rel),
			Size:  info.Size(),
			Mtime: info.ModTime().Format(clusterMtimeFormat),
			Type:  1,
		}
		if info.IsDir() {
			e.Type = 0
			e.Size = 0
		} else {
			e.SHA256 = fileSHA256(path)
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// zipDir 把 src 目录内容打包为 zip（条目路径不含顶层目录名）。
func zipDir(src, zipPath string) error {
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
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := addToZip(zw, filepath.Join(src, e.Name()), e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// registerClusterRoutes 注册集群节点 API 路由。
func (d *Daemon) registerClusterRoutes(mux *http.ServeMux) {
	// P0 节点级文件存储
	mux.HandleFunc("GET /api/cluster/files/list", d.auth(d.handleClusterFileList))
	mux.HandleFunc("POST /api/cluster/files/mkdir", d.auth(d.handleClusterMkdir))
	mux.HandleFunc("DELETE /api/cluster/files", d.auth(d.handleClusterDelete))
	mux.HandleFunc("POST /api/cluster/files/download", d.auth(d.handleClusterDownload))
	mux.HandleFunc("POST /api/cluster/files/upload", d.auth(d.handleClusterUpload))

	// P1 增量同步原语
	mux.HandleFunc("GET /api/cluster/sync/list", d.auth(d.handleClusterSyncList))
	mux.HandleFunc("GET /api/instance/sync/list", d.auth(d.handleInstanceSyncList))
	mux.HandleFunc("POST /api/instance/snapshot", d.auth(d.handleInstanceSnapshot))
	mux.HandleFunc("POST /api/instance/restore", d.auth(d.handleInstanceRestore))

	// P2 集群协调
	mux.HandleFunc("GET /api/cluster/status", d.auth(d.handleClusterStatus))
	mux.HandleFunc("POST /api/cluster/heartbeat", d.auth(d.handleClusterHeartbeat))
	mux.HandleFunc("POST /api/cluster/events", d.auth(d.handleClusterEvents))
	mux.HandleFunc("GET /api/cluster/peers", d.auth(d.handleClusterPeers))
	mux.HandleFunc("POST /api/cluster/transfer", d.auth(d.handleClusterTransfer))
	mux.HandleFunc("GET /api/cluster/transfer", d.auth(d.handleTransferStatus))
}

// handleClusterFileList 列出同步区目录。
// GET /api/cluster/files/list?path&page&page_size
func (d *Daemon) handleClusterFileList(w http.ResponseWriter, r *http.Request) {
	dir, err := d.clusterPath(queryParam(r, "path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, total, abs, err := listDir(d.clusterRoot(), dir,
		atoiDefault(queryParam(r, "page"), 1), atoiDefault(queryParam(r, "page_size"), 100))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{
		"items":        items,
		"total":        total,
		"absolutePath": "/mirrors" + abs, // 补回虚拟前缀，供客户端直接作 path 参数复用
	})
}

// handleClusterMkdir 创建同步区目录。
// POST /api/cluster/files/mkdir body: {path}
func (d *Daemon) handleClusterMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	dir, err := d.clusterPath(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleClusterDelete 删除同步区文件/目录。
// DELETE /api/cluster/files body: {targets: [...]}
func (d *Daemon) handleClusterDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Targets []string `json:"targets"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	for _, t := range body.Targets {
		path, err := d.clusterPath(t)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := os.RemoveAll(path); err != nil {
			writeError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
	}
	writeOK(w, true)
}

// handleClusterDownload 申请同步区下载票据。
// POST /api/cluster/files/download body: {path} → {password, addr}
func (d *Daemon) handleClusterDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	path, err := d.clusterPath(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, "文件不存在: "+body.Path)
		return
	}
	password := tickets.Create("cluster", d.clusterRoot(), "")
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "下载票据已满，请稍后重试")
		return
	}
	writeOK(w, map[string]any{"password": password, "addr": d.publicAddr()})
}

// handleClusterUpload 申请同步区上传票据。
// POST /api/cluster/files/upload body: {upload_dir} → {password, addr, upload_dir}
func (d *Daemon) handleClusterUpload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UploadDir string `json:"upload_dir"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	uploadDir := body.UploadDir
	if uploadDir == "" {
		uploadDir = "/"
	}
	dir, err := d.clusterPath(uploadDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	password := tickets.Create("cluster", d.clusterRoot(), dir)
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

// handleClusterSyncList 递归枚举同步区目录（单次返回整树清单）。
// GET /api/cluster/sync/list?path
func (d *Daemon) handleClusterSyncList(w http.ResponseWriter, r *http.Request) {
	dir, err := d.clusterPath(queryParam(r, "path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSyncList(w, dir)
}

// handleInstanceSyncList 递归枚举实例工作目录（单次返回整树清单）。
// GET /api/instance/sync/list?uuid&daemonId
func (d *Daemon) handleInstanceSyncList(w http.ResponseWriter, r *http.Request) {
	cwd, err := d.CwdOf(queryParam(r, "uuid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSyncList(w, cwd)
}

// writeSyncList 输出递归快照响应 {items, total, root}。
func writeSyncList(w http.ResponseWriter, root string) {
	entries, err := walkEntries(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "枚举失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"items": entries,
		"total": len(entries),
		"root":  "/",
	})
}

// handleInstanceSnapshot 把实例工作目录打成 zip 存入同步区，返回下载票据。
// POST /api/instance/snapshot body: {uuid, daemonId}
// 响应: {password, addr, fileName}（fileName 相对同步区根，如 ".snapshots/xxx.zip"）
func (d *Daemon) handleInstanceSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string `json:"uuid"`
		DaemonID string `json:"daemonId"`
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
	snapDir := filepath.Join(d.clusterRoot(), ".snapshots")
	zipName := body.UUID + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".zip"
	zipPath := filepath.Join(snapDir, zipName)
	if err := zipDir(cwd, zipPath); err != nil {
		writeError(w, http.StatusInternalServerError, "快照失败: "+err.Error())
		return
	}
	password := tickets.Create("snapshot", d.clusterRoot(), "")
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "下载票据已满，请稍后重试")
		return
	}
	writeOK(w, map[string]any{
		"password": password,
		"addr":     d.publicAddr(),
		"fileName": ".snapshots/" + zipName,
	})
}

// handleInstanceRestore 从同步区归档解压到实例工作目录。
// POST /api/instance/restore body: {uuid, daemonId, fileName}
func (d *Daemon) handleInstanceRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string `json:"uuid"`
		DaemonID string `json:"daemonId"`
		FileName string `json:"fileName"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.FileName == "" {
		writeError(w, http.StatusBadRequest, "缺少 fileName 参数")
		return
	}
	cwd, err := d.CwdOf(body.UUID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	zipPath, err := d.clusterPath(body.FileName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := unzip(zipPath, cwd); err != nil {
		writeError(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// handleClusterStatus 集群状态。
// GET /api/cluster/status → {monitorNodeId, role, peers, self: {id, address}}
func (d *Daemon) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	d.clusterMu.Lock()
	defer d.clusterMu.Unlock()
	writeOK(w, map[string]any{
		"monitorNodeId": d.clusterMonitor,
		"role":          d.clusterRole,
		"peers":         d.clusterPeers,
		"self": map[string]any{
			"id":      d.UUID,
			"address": "http://" + d.publicAddr(),
		},
	})
}

// handleClusterHeartbeat 上报资源快照 + 运行实例 + 待处理事件。
// POST /api/cluster/heartbeat body: {resource, instances, events, id?, address?}
// id/address 存在时自动登记/更新对等节点列表。
func (d *Daemon) handleClusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body["time"] = time.Now().UnixMilli()
	d.clusterMu.Lock()
	d.clusterHeartbeat = body
	if id, _ := body["id"].(string); id != "" {
		addr, _ := body["address"].(string)
		found := false
		for _, p := range d.clusterPeers {
			if p["id"] == id {
				p["address"] = addr
				p["available"] = true
				found = true
				break
			}
		}
		if !found {
			d.clusterPeers = append(d.clusterPeers, map[string]any{
				"id": id, "address": addr, "available": true,
			})
		}
	}
	d.clusterMu.Unlock()
	writeOK(w, true)
}

// handleClusterEvents 上报事件（崩溃 / 资源不足 / 同步完成 / 迁移）。
// POST /api/cluster/events body: {type, ...}
func (d *Daemon) handleClusterEvents(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body["time"] = time.Now().UnixMilli()
	d.clusterMu.Lock()
	d.clusterEvents = append(d.clusterEvents, body)
	if len(d.clusterEvents) > maxClusterEvents {
		d.clusterEvents = d.clusterEvents[len(d.clusterEvents)-maxClusterEvents:]
	}
	d.clusterMu.Unlock()
	writeOK(w, true)
}

// handleClusterPeers 获取已登记的对等节点列表。
// GET /api/cluster/peers
func (d *Daemon) handleClusterPeers(w http.ResponseWriter, r *http.Request) {
	d.clusterMu.Lock()
	defer d.clusterMu.Unlock()
	writeOK(w, d.clusterPeers)
}

// transferJob 节点间数据拉取任务。
type transferJob struct {
	mu     sync.Mutex
	status string // running | done | failed
	bytes  int64
	err    error
}

// handleClusterTransfer 指示节点从对等节点拉取实例数据到本地同步区（节点间直传）。
// POST /api/cluster/transfer body: {instanceId, source: {address, apikey, uuid, daemonId}, dest}
func (d *Daemon) handleClusterTransfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstanceID string `json:"instanceId"`
		Source     struct {
			Address  string `json:"address"`
			APIKey   string `json:"apikey"`
			UUID     string `json:"uuid"`
			DaemonID string `json:"daemonId"`
		} `json:"source"`
		Dest string `json:"dest"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.InstanceID == "" || body.Source.Address == "" || body.Source.UUID == "" {
		writeError(w, http.StatusBadRequest, "缺少 instanceId/source.address/source.uuid 参数")
		return
	}
	dest, err := d.clusterPath(body.Dest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	jobID := newUUID()
	job := &transferJob{status: "running"}
	d.clusterMu.Lock()
	d.transfers[jobID] = job
	d.clusterMu.Unlock()
	go d.runTransfer(jobID, job, body.InstanceID, body.Source.Address,
		body.Source.APIKey, body.Source.UUID, body.Source.DaemonID, dest)
	writeOK(w, map[string]any{"jobId": jobID})
}

// handleTransferStatus 查询拉取任务状态。
// GET /api/cluster/transfer?jobId → {status: running|done|failed, bytes}
func (d *Daemon) handleTransferStatus(w http.ResponseWriter, r *http.Request) {
	jobID := queryParam(r, "jobId")
	d.clusterMu.Lock()
	job := d.transfers[jobID]
	d.clusterMu.Unlock()
	if job == nil {
		writeError(w, http.StatusBadRequest, "任务不存在或已过期")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	writeOK(w, map[string]any{"status": job.status, "bytes": job.bytes})
}

// runTransfer 执行节点间拉取：source 快照 → 直连下载 → 解压到本地同步区。
func (d *Daemon) runTransfer(jobID string, job *transferJob, instanceID, addr, apikey, uuid, daemonID, dest string) {
	fail := func(err error) {
		job.mu.Lock()
		job.status = "failed"
		job.err = err
		job.mu.Unlock()
		alog.Printf("集群拉取 %s 失败: %v", instanceID, err)
	}
	// 1. 申请 source 节点实例快照票据
	base := addr
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	auth := ""
	if apikey != "" {
		auth = "?apikey=" + url.QueryEscape(apikey)
	}
	snap, err := postJSON(base+"/api/instance/snapshot"+auth, map[string]any{
		"uuid": uuid, "daemonId": daemonID,
	})
	if err != nil {
		fail(fmt.Errorf("申请远端快照失败: %w", err))
		return
	}
	password, _ := snap["password"].(string)
	fileName, _ := snap["fileName"].(string)
	snapAddr, _ := snap["addr"].(string)
	if password == "" || fileName == "" {
		fail(fmt.Errorf("远端快照响应缺少 password/fileName: %v", snap))
		return
	}
	if snapAddr == "" {
		snapAddr = base
	} else if !strings.HasPrefix(snapAddr, "http://") && !strings.HasPrefix(snapAddr, "https://") {
		snapAddr = "http://" + snapAddr
	}
	// 2. 直连下载归档到本地临时文件
	tmpZip := filepath.Join(d.clusterRoot(), ".transfer", jobID+".zip")
	if err := os.MkdirAll(filepath.Dir(tmpZip), 0o755); err != nil {
		fail(err)
		return
	}
	dlURL := snapAddr + "/download/" + url.PathEscape(password) + "/" + strings.TrimPrefix(fileName, "/")
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(dlURL)
	if err != nil {
		fail(fmt.Errorf("下载归档失败: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("下载归档返回 %d", resp.StatusCode))
		return
	}
	out, err := os.Create(tmpZip)
	if err != nil {
		fail(err)
		return
	}
	bytesN, err := io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		fail(fmt.Errorf("写入归档失败: %w", err))
		return
	}
	// 3. 解压到目标同步区目录
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fail(err)
		return
	}
	if err := unzip(tmpZip, dest); err != nil {
		fail(fmt.Errorf("解压失败: %w", err))
		return
	}
	_ = os.Remove(tmpZip)
	job.mu.Lock()
	job.status = "done"
	job.bytes = bytesN
	job.mu.Unlock()
	alog.Printf("集群拉取 %s 完成（%d 字节 → %s）", instanceID, bytesN, dest)
}

// postJSON 发送 JSON 请求并解析 MCSM 风格响应 {status, data}。
func postJSON(url string, body any) (map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: cliLongTimeout}
	resp, err := client.Post(url, "application/json; charset=utf-8", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != 200 {
		return nil, fmt.Errorf("远端返回 %d: %s", out.Status, strings.TrimSpace(string(out.Data)))
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		return nil, err
	}
	return data, nil
}
