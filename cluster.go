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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// clusterMtimeFormat 同步条目 mtime 的字符串格式（与文档示例一致）。
const clusterMtimeFormat = "2006-01-02 15:04:05"

// maxClusterEvents 集群事件保留条数。
const maxClusterEvents = 100

// clusterRoot 同步区根目录（绝对路径）。
// DataDir 为相对路径（如 -data dev-node-data）时必须 Abs 化：
// 否则 clusterPath 返回相对路径，再经 listDir → NormalizePath 相对拼接，
// 会把根拼接两次（dev-node-data\mirrors\dev-node-data\mirrors…）。
func (d *Daemon) clusterRoot() string {
	root := filepath.Join(d.DataDir, "mirrors")
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
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
	perm("集群", "GET /api/cluster/files/list", "列出同步区文件")
	mux.HandleFunc("GET /api/cluster/files/list", d.auth(d.handleClusterFileList))
	perm("集群", "POST /api/cluster/files/mkdir", "创建同步区目录")
	mux.HandleFunc("POST /api/cluster/files/mkdir", d.auth(d.handleClusterMkdir))
	perm("集群", "DELETE /api/cluster/files", "删除同步区文件")
	mux.HandleFunc("DELETE /api/cluster/files", d.auth(d.handleClusterDelete))
	perm("集群", "POST /api/cluster/files/download", "申请同步区下载票据")
	mux.HandleFunc("POST /api/cluster/files/download", d.auth(d.handleClusterDownload))
	perm("集群", "POST /api/cluster/files/upload", "申请同步区上传票据")
	mux.HandleFunc("POST /api/cluster/files/upload", d.auth(d.handleClusterUpload))

	// P1 增量同步原语
	perm("集群", "GET /api/cluster/sync/list", "同步区文件树快照")
	mux.HandleFunc("GET /api/cluster/sync/list", d.auth(d.handleClusterSyncList))
	perm("实例", "GET /api/instance/sync/list", "实例文件树快照")
	mux.HandleFunc("GET /api/instance/sync/list", d.auth(d.handleInstanceSyncList))
	// 注意：POST /api/instance/snapshot|restore 已由 backup.go 任务化实现
	// （docs/irix-node-local-parity.md §4.5），集群迁移经 runTransfer 走
	// 「任务化快照 → 备份下载票据 → 直连下载」流程，此处不再重复注册。

	// P2 集群协调
	perm("集群", "GET /api/cluster/status", "查看集群状态")
	mux.HandleFunc("GET /api/cluster/status", d.auth(d.handleClusterStatus))
	perm("集群", "POST /api/cluster/heartbeat", "上报集群心跳")
	mux.HandleFunc("POST /api/cluster/heartbeat", d.auth(d.handleClusterHeartbeat))
	perm("集群", "POST /api/cluster/events", "上报集群事件")
	mux.HandleFunc("POST /api/cluster/events", d.auth(d.handleClusterEvents))
	perm("集群", "GET /api/cluster/peers", "查看对等节点")
	mux.HandleFunc("GET /api/cluster/peers", d.auth(d.handleClusterPeers))
	perm("集群", "POST /api/cluster/transfer", "发起节点间直传")
	mux.HandleFunc("POST /api/cluster/transfer", d.auth(d.handleClusterTransfer))
	perm("集群", "GET /api/cluster/transfer", "查看直传任务状态")
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
	// 集群票据为目录范围票据（file 为空）：下载路径按同步区根解析，
	// 兼容 /mirrors/... 虚拟前缀（见 handleDirectDownload）。
	password := tickets.CreateDownload("cluster", d.clusterRoot(), "")
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

// validateTransferTarget 校验集群拉取目标地址，防止认证后 SSRF（审计报告 #5）：
//   - 只允许 http/https 协议（拒绝 file/gopher 等任意协议）；
//   - 解析出的 IP 不得是环回 / 未指定 / 链路本地 / 组播 / 广播地址；
//   - 不得指向本守护进程自身网卡（自我回打）；
//   - 解析失败同样拒绝，避免依赖「校验后 / 连接前」解析时机的 rebinding 手法。
func (d *Daemon) validateTransferTarget(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("不支持的协议（仅允许 http/https）: %s", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("无效地址: %v", err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("地址缺少主机名: %s", raw)
	}
	if d.transferAllowLoopback {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("无法解析主机 %s: %v", host, err)
	}
	for _, ip := range ips {
		if err := d.checkTransferIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// checkTransferIP 校验单个目标 IP：拒绝环回/未指定/链路本地/组播/广播及本机地址；
// RFC1918 内网（10/8、172.16/12、192.168/16，含 IPv6 ULA fc00::/7）默认同样拒绝，
// 集群 LAN 直传需通过 -transfer-allow-cidr 显式放行（见 parseTransferAllowCIDR）。
// 硬性拒绝项（环回/链路本地/本机等）任何配置都不可放行。
func (d *Daemon) checkTransferIP(ip net.IP) error {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return fmt.Errorf("禁止访问的地址: %s", ip)
	}
	if d.isLocalIP(ip) {
		return fmt.Errorf("禁止访问本节点自身: %s", ip)
	}
	if isRFC1918(ip) && !d.ipInAllowCIDR(ip) {
		return fmt.Errorf("禁止访问内网地址（集群 LAN 直传需配置 -transfer-allow-cidr）: %s", ip)
	}
	return nil
}

// isRFC1918 判断 IP 是否为私网地址：IPv4 RFC1918（10/8、172.16/12、192.168/16）
// 与 IPv6 ULA（fc00::/7）。环回/链路本地由调用方另行拒绝。
func isRFC1918(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		}
		return false
	}
	ip6 := ip.To16()
	if ip6 == nil {
		return false
	}
	return ip6[0]&0xfe == 0xfc // fc00::/7
}

// parseTransferAllowCIDR 解析 -transfer-allow-cidr 配置（逗号分隔 CIDR 列表），
// 供 checkTransferIP 放行内网地址；非法 CIDR 返回错误（启动时直接失败）。
// 测试中直接设置 d.transferAllowCIDR 后调用本方法。
func (d *Daemon) parseTransferAllowCIDR() error {
	d.transferAllowNets = nil
	for _, c := range strings.Split(d.transferAllowCIDR, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("无效 CIDR %q: %v", c, err)
		}
		d.transferAllowNets = append(d.transferAllowNets, ipNet)
	}
	return nil
}

// ipInAllowCIDR 判断 IP 是否落在 -transfer-allow-cidr 放行的网段内。
func (d *Daemon) ipInAllowCIDR(ip net.IP) bool {
	for _, n := range d.transferAllowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isLocalIP 判断 IP 是否为本机任一网卡地址（防自我回打）。
func (d *Daemon) isLocalIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var nip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			nip = v.IP
		case *net.IPAddr:
			nip = v.IP
		}
		if nip != nil && nip.Equal(ip) {
			return true
		}
	}
	return false
}

// transferDialContext 构造集群拉取专用拨号器：每次拨号前重新解析主机并逐个
// 校验目标 IP，只连接通过校验的地址 —— 防止 DNS rebinding 在「校验后、连接前」
// 把域名换绑到内网地址。IP 字面量直接参与校验。
func (d *Daemon) transferDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if !d.transferAllowLoopback {
				if err := d.checkTransferIP(ip); err != nil {
					lastErr = err
					continue
				}
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err != nil {
				lastErr = err
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("无可用目标地址")
		}
		return nil, lastErr
	}
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
	// SSRF 防护：申请任务前先校验来源地址（与 runTransfer 中的校验一致）
	addr := body.Source.Address
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	if err := d.validateTransferTarget(addr); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	// 传输用客户端：拨号前按目标 IP 校验（防 DNS rebinding），
	// 重定向逐跳校验（防「先放行、再跳到内网」的跳板式 SSRF）。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.transferDialContext()
	client := &http.Client{
		Timeout:   30 * time.Minute,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := d.validateTransferTarget(req.URL.String()); err != nil {
				return fmt.Errorf("重定向目标被拒绝: %w", err)
			}
			return nil
		},
	}
	if err := d.validateTransferTarget(base); err != nil {
		fail(fmt.Errorf("来源地址被拒绝: %w", err))
		return
	}
	auth := ""
	if apikey != "" {
		auth = "?apikey=" + url.QueryEscape(apikey)
	}
	// 1. 申请远端实例快照任务（任务化，docs/irix-node-local-parity.md §4.5）
	snap, err := postJSON(client, base+"/api/instance/snapshot"+auth, map[string]any{
		"uuid": uuid, "daemonId": daemonID,
	})
	if err != nil {
		fail(fmt.Errorf("申请远端快照失败: %w", err))
		return
	}
	snapJobID, _ := snap["jobId"].(string)
	if snapJobID == "" {
		fail(fmt.Errorf("远端快照响应缺少 jobId: %v", snap))
		return
	}
	// 轮询快照进度直到完成（大世界备份可能耗时，上限 15 分钟）
	var archivePath string
	pollDeadline := time.Now().Add(15 * time.Minute)
	progressURL := base + "/api/instance/snapshot-progress?jobId=" + url.QueryEscape(snapJobID)
	if apikey != "" {
		progressURL += "&apikey=" + url.QueryEscape(apikey) // 已有查询参数，用 & 拼接
	}
	for {
		prog, pErr := getJSON(client, progressURL)
		if pErr != nil {
			fail(fmt.Errorf("查询快照进度失败: %w", pErr))
			return
		}
		switch prog["status"] {
		case "done":
			archivePath, _ = prog["archivePath"].(string)
			if archivePath == "" {
				archivePath, _ = prog["path"].(string) // 兼容旧字段名
			}
			if archivePath == "" {
				fail(fmt.Errorf("快照完成但缺少归档路径: %v", prog))
				return
			}
		case "failed":
			msg, _ := prog["message"].(string)
			fail(fmt.Errorf("远端快照失败: %s", msg))
			return
		default:
			if time.Now().After(pollDeadline) {
				fail(fmt.Errorf("远端快照超时（15 分钟）"))
				return
			}
			time.Sleep(time.Second)
			continue
		}
		break
	} // 2. 申请备份下载票据（绑定该归档文件）
	tick, err := postJSON(client, base+"/api/instance/backups/download"+auth, map[string]any{
		"path": archivePath,
		"uuid": uuid,
	})
	if err != nil {
		fail(fmt.Errorf("申请备份下载票据失败: %w", err))
		return
	}
	password, _ := tick["password"].(string)
	snapAddr, _ := tick["addr"].(string)
	if password == "" {
		fail(fmt.Errorf("远端票据响应缺少 password: %v", tick))
		return
	}
	if snapAddr == "" {
		snapAddr = base
	} else if !strings.HasPrefix(snapAddr, "http://") && !strings.HasPrefix(snapAddr, "https://") {
		snapAddr = "http://" + snapAddr
	}
	// 直连下载地址来自远端响应（攻击者可伪造）：同样必须通过目标校验
	if err := d.validateTransferTarget(snapAddr); err != nil {
		fail(fmt.Errorf("下载地址被拒绝: %w", err))
		return
	}
	// 3. 直连下载归档到本地临时文件
	tmpZip := filepath.Join(d.clusterRoot(), ".transfer", jobID+".zip")
	if err := os.MkdirAll(filepath.Dir(tmpZip), 0o755); err != nil {
		fail(err)
		return
	}
	dlURL := snapAddr + "/download/" + url.PathEscape(password) + "/" + url.PathEscape(filepath.Base(archivePath))
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
	// 4. 解压到目标同步区目录
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
// client 由调用方提供（集群拉取使用带 SSRF 校验的传输客户端）。
func postJSON(client *http.Client, url string, body any) (map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(url, "application/json; charset=utf-8", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return decodeJSONResponse(resp)
}

// getJSON 发送 GET 请求并解析 MCSM 风格响应 {status, data}。
func getJSON(client *http.Client, url string) (map[string]any, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	return decodeJSONResponse(resp)
}

// decodeJSONResponse 解析 MCSM 风格响应 {status, data}，非 200 报错。
func decodeJSONResponse(resp *http.Response) (map[string]any, error) {
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
