// 节点端内网穿透（docs/irix-node-local-parity.md §4.7）。
//
// GET    /api/frp/status                     frpc 二进制状态与隧道列表
// POST   /api/frp/tunnels                    {name, provider, config} → {tunnelId}
// POST   /api/frp/tunnels/{id}/start|stop    启停单隧道
// DELETE /api/frp/tunnels/{id}               删除隧道（停止 + 删配置）
// GET    /api/frp/tunnels/{id}/logs?tail=100 隧道运行日志
// POST   /api/frp/binary                     multipart 上传 frpc 二进制
//
// 隧道在节点上运行（frpc 由节点管理），客户端只下发配置与查看状态——
// 隧道出口与实例同机。配置存 {data}/frp/tunnels/<id>.toml，
// 隧道列表持久化到 {data}/frp/tunnels.json；frpc 二进制
// 优先 {data}/frp/frpc(.exe|.bat)，其次 PATH。
//
// provider 说明：
//   self      自建 frps：config.toml 为完整 frpc 配置（原样下发）
//   openfrp   config: node, serverPort?, token, name?, localPort, remotePort?,
//             localIP?(默认 127.0.0.1), type?(tcp/udp, 默认 tcp), customDomains?
//   sakura    同 openfrp 字段（user 可选，Sakura 访问密钥拆 user/token 时用）

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// frpDirName FRP 配置目录（{data}/frp）。
const frpDirName = "frp"

// frpTunnel 隧道记录（运行态进程不持久化，重启后为停止态）。
type frpTunnel struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Provider string         `json:"provider"` // openfrp | sakura | self
	Config   map[string]any `json:"config"`
	Status   string         `json:"status"` // running | stopped | failed

	proc *Process   // 运行中进程（内存态，不持久化）
	log  *LogBuffer // 运行日志（内存环形）
}

// frpDir 返回 FRP 配置目录。
func (d *Daemon) frpDir() string {
	return filepath.Join(d.DataDir, frpDirName)
}

// frpTunnelsFile 隧道列表持久化文件。
func (d *Daemon) frpTunnelsFile() string {
	return filepath.Join(d.frpDir(), "tunnels.json")
}

// frpcBinaryPath 定位 frpc 可执行文件：优先节点自管（{data}/frp/frpc*），
// 其次 PATH；找不到返回空串。
func (d *Daemon) frpcBinaryPath() string {
	names := []string{"frpc"}
	if runtime.GOOS == "windows" {
		names = []string{"frpc.exe", "frpc.bat", "frpc.cmd"}
	}
	for _, n := range names {
		p := filepath.Join(d.frpDir(), n)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("frpc"); err == nil {
		return p
	}
	return ""
}

// frpcVersion 读取 frpc 版本（-v 输出首行；3 秒超时，失败返回空串）。
func frpcVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-v").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0])
}

// frpLoad 加载持久化隧道列表（启动时调用；进程态重置为停止）。
func (d *Daemon) frpLoad() {
	data, err := os.ReadFile(d.frpTunnelsFile())
	if err != nil {
		return
	}
	var list []*frpTunnel
	if err := json.Unmarshal(data, &list); err != nil {
		alog.Printf("警告: frp 隧道列表损坏（%s），按空列表处理: %v", d.frpTunnelsFile(), err)
		return
	}
	d.frpMu.Lock()
	d.frpTunnels = list
	for _, t := range list {
		t.Status = "stopped"
	}
	d.frpMu.Unlock()
}

// frpSave 持久化隧道列表（原子写）。
func (d *Daemon) frpSave() error {
	d.frpMu.Lock()
	defer d.frpMu.Unlock()
	if err := os.MkdirAll(d.frpDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d.frpTunnels, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.frpTunnelsFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.frpTunnelsFile())
}

// buildFRPConfig 按 provider 生成 frpc.toml 内容。
func buildFRPConfig(t *frpTunnel) (string, error) {
	if t.Provider == "self" {
		raw, _ := t.Config["toml"].(string)
		if strings.TrimSpace(raw) == "" {
			return "", errors.New("self 类型需要 config.toml（完整 frpc 配置）")
		}
		return raw, nil
	}
	if t.Provider != "openfrp" && t.Provider != "sakura" {
		return "", fmt.Errorf("不支持的 provider: %s", t.Provider)
	}
	get := func(k string) string {
		v, _ := t.Config[k].(string)
		return strings.TrimSpace(v)
	}
	num := func(k string) int {
		switch v := t.Config[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	node := get("node")
	if node == "" {
		return "", errors.New("缺少 node（frps 服务器地址）")
	}
	localPort := num("localPort")
	if localPort <= 0 {
		return "", errors.New("缺少 localPort（本地服务端口）")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "serverAddr = %q\n", node)
	if p := num("serverPort"); p > 0 {
		fmt.Fprintf(&sb, "serverPort = %d\n", p)
	}
	if token := get("token"); token != "" {
		fmt.Fprintf(&sb, "auth.token = %q\n", token)
	}
	if user := get("user"); user != "" {
		fmt.Fprintf(&sb, "auth.user = %q\n", user)
	}
	name := firstNonEmpty(get("name"), t.Name, "tunnel")
	sb.WriteString("\n[[proxies]]\n")
	fmt.Fprintf(&sb, "name = %q\n", name)
	fmt.Fprintf(&sb, "type = %q\n", firstNonEmpty(get("type"), "tcp"))
	fmt.Fprintf(&sb, "localIP = %q\n", firstNonEmpty(get("localIP"), "127.0.0.1"))
	fmt.Fprintf(&sb, "localPort = %d\n", localPort)
	if p := num("remotePort"); p > 0 {
		fmt.Fprintf(&sb, "remotePort = %d\n", p)
	}
	if cd := get("customDomains"); cd != "" {
		fmt.Fprintf(&sb, "customDomains = [%q]\n", cd)
	}
	return sb.String(), nil
}

// frpStart 启动隧道进程。
func (d *Daemon) frpStart(t *frpTunnel) error {
	binary := d.frpcBinaryPath()
	if binary == "" {
		return errors.New("未找到 frpc 二进制（请上传到节点或安装到 PATH）")
	}
	cfg, err := buildFRPConfig(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.frpDir(), 0o755); err != nil {
		return err
	}
	cfgPath := filepath.Join(d.frpDir(), "tunnels", t.ID+".toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return err
	}
	proc, err := startProcess(binary+" -c "+quotePath(cfgPath), d.frpDir(), nil)
	if err != nil {
		return fmt.Errorf("启动 frpc 失败: %w", err)
	}
	t.proc = proc
	t.log = proc.Log
	t.Status = "running"
	// 退出监听：更新状态
	go func(t *frpTunnel, proc *Process) {
		<-proc.done
		d.frpMu.Lock()
		if t.proc == proc {
			t.Status = "failed"
		}
		d.frpMu.Unlock()
	}(t, proc)
	return nil
}

// quotePath 简单加引号（路径含空格时）。
func quotePath(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return p
}

// frpStop 停止隧道进程（frpc 无 stdin 停止命令，直接终止）。
func (d *Daemon) frpStop(t *frpTunnel) error {
	d.frpMu.Lock()
	proc := t.proc
	t.proc = nil
	if proc != nil && proc.IsRunning() {
		d.frpMu.Unlock()
		if err := proc.Kill(); err != nil {
			d.frpMu.Lock()
			t.proc = proc
			d.frpMu.Unlock()
			return err
		}
		d.frpMu.Lock()
	}
	t.Status = "stopped"
	d.frpMu.Unlock()
	return nil
}

// frpTunnelStatus 汇总隧道状态快照（供 status 接口）。
func frpTunnelStatus(t *frpTunnel) map[string]any {
	return map[string]any{
		"id":       t.ID,
		"name":     t.Name,
		"provider": t.Provider,
		"status":   t.Status,
		"config":   t.Config,
	}
}

// handleFRPStatus 获取 frpc 状态。
// GET /api/frp/status
func (d *Daemon) handleFRPStatus(w http.ResponseWriter, r *http.Request) {
	binary := d.frpcBinaryPath()
	binaryInfo := map[string]any{"present": false}
	if binary != "" {
		binaryInfo = map[string]any{
			"present": true,
			"path":    binary,
			"version": frpcVersion(binary),
		}
	}
	d.frpMu.Lock()
	tunnels := make([]map[string]any, 0, len(d.frpTunnels))
	for _, t := range d.frpTunnels {
		tunnels = append(tunnels, frpTunnelStatus(t))
	}
	d.frpMu.Unlock()
	writeOK(w, map[string]any{
		"binary":  binaryInfo,
		"tunnels": tunnels,
	})
}

// handleFRPCreate 创建隧道（写配置并启动）。
// POST /api/frp/tunnels body: {name, provider, config}
func (d *Daemon) handleFRPCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string         `json:"name"`
		Provider string         `json:"provider"`
		Config   map[string]any `json:"config"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	t := &frpTunnel{
		ID:       newUUID(),
		Name:     body.Name,
		Provider: body.Provider,
		Config:   body.Config,
		Status:   "stopped",
	}
	if _, err := buildFRPConfig(t); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.frpMu.Lock()
	d.frpTunnels = append(d.frpTunnels, t)
	d.frpMu.Unlock()
	if err := d.frpSave(); err != nil {
		writeError(w, http.StatusInternalServerError, "保存隧道失败: "+err.Error())
		return
	}
	// 创建即启动（文档：节点写入配置并启动）
	if err := d.frpStart(t); err != nil {
		writeError(w, http.StatusInternalServerError, "隧道已创建但启动失败: "+err.Error())
		return
	}
	alog.Printf("FRP 隧道 %s（%s/%s）已启动", t.Name, t.Provider, t.ID)
	writeOK(w, map[string]any{"tunnelId": t.ID})
}

// findFRPTunnel 按 id 查找隧道。
func (d *Daemon) findFRPTunnel(id string) *frpTunnel {
	d.frpMu.Lock()
	defer d.frpMu.Unlock()
	for _, t := range d.frpTunnels {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// handleFRPStart 启动隧道。
// POST /api/frp/tunnels/{id}/start
func (d *Daemon) handleFRPStart(w http.ResponseWriter, r *http.Request) {
	t := d.findFRPTunnel(r.PathValue("id"))
	if t == nil {
		writeError(w, http.StatusBadRequest, "隧道不存在")
		return
	}
	d.frpMu.Lock()
	alreadyRunning := t.proc != nil && t.proc.IsRunning()
	d.frpMu.Unlock()
	if alreadyRunning {
		writeError(w, http.StatusBadRequest, "隧道已在运行")
		return
	}
	if err := d.frpStart(t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"tunnelId": t.ID})
}

// handleFRPStop 停止隧道。
// POST /api/frp/tunnels/{id}/stop
func (d *Daemon) handleFRPStop(w http.ResponseWriter, r *http.Request) {
	t := d.findFRPTunnel(r.PathValue("id"))
	if t == nil {
		writeError(w, http.StatusBadRequest, "隧道不存在")
		return
	}
	if err := d.frpStop(t); err != nil {
		writeError(w, http.StatusInternalServerError, "停止失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"tunnelId": t.ID})
}

// handleFRPDelete 删除隧道。
// DELETE /api/frp/tunnels/{id}
func (d *Daemon) handleFRPDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.frpMu.Lock()
	var target *frpTunnel
	keep := d.frpTunnels[:0]
	for _, t := range d.frpTunnels {
		if t.ID == id {
			target = t
			continue
		}
		keep = append(keep, t)
	}
	if target == nil {
		d.frpMu.Unlock()
		writeError(w, http.StatusBadRequest, "隧道不存在")
		return
	}
	d.frpTunnels = keep
	d.frpMu.Unlock()
	_ = d.frpStop(target)
	_ = os.Remove(filepath.Join(d.frpDir(), "tunnels", id+".toml"))
	_ = d.frpSave()
	writeOK(w, true)
}

// handleFRPLogs 隧道运行日志。
// GET /api/frp/tunnels/{id}/logs?tail=<KB>
func (d *Daemon) handleFRPLogs(w http.ResponseWriter, r *http.Request) {
	t := d.findFRPTunnel(r.PathValue("id"))
	if t == nil {
		writeError(w, http.StatusBadRequest, "隧道不存在")
		return
	}
	tail := atoiDefault(queryParam(r, "tail"), 100)
	if tail < 0 {
		tail = 0
	}
	if tail > 2048 {
		tail = 2048
	}
	d.frpMu.Lock()
	log := t.log
	d.frpMu.Unlock()
	if log == nil {
		writeOK(w, "")
		return
	}
	writeOK(w, log.Tail(tail))
}

// handleFRPUploadBinary 上传 frpc 二进制。
// POST /api/frp/binary multipart: file
func (d *Daemon) handleFRPUploadBinary(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "解析上传失败: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少 file 字段: "+err.Error())
		return
	}
	defer file.Close()
	if err := os.MkdirAll(d.frpDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
		return
	}
	name := "frpc"
	if runtime.GOOS == "windows" {
		name = "frpc.exe"
	}
	dest := filepath.Join(d.frpDir(), name)
	tmp := dest + ".upload"
	out, err := os.Create(tmp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建文件失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o755)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "就位失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"path":    dest,
		"version": frpcVersion(dest),
	})
}

// frpStopAll 停止全部隧道（守护进程退出前调用）。
func (d *Daemon) frpStopAll() {
	d.frpMu.Lock()
	list := make([]*frpTunnel, len(d.frpTunnels))
	copy(list, d.frpTunnels)
	d.frpMu.Unlock()
	for _, t := range list {
		_ = d.frpStop(t)
	}
}
