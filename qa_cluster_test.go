// 集群节点 API（P0-P2）与容器能力测试。
// 覆盖：同步区文件存储、递归快照、快照/恢复、集群协调基础版、
// 节点间直传（双节点端到端）、容器能力探测（非 Linux/FreeBSD 平台不可用）。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// apiPost 发送带 apikey 的 JSON POST 请求。
func apiPost(t *testing.T, u string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// apiDelete 发送带 apikey 的 JSON DELETE 请求。
func apiDelete(t *testing.T, u string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodDelete, u, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// decodeData 解析统一响应体，返回 data 字段。
func decodeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Status int            `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应失败: %v（%s）", err, body)
	}
	return resp.Data
}

// TestClusterFileStore P0 节点级文件存储：mkdir/list/票据/删除/越界防护。
func TestClusterFileStore(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	// 同步端口到 httptest 端口，使票据 addr 指向测试服务器
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	d.Port = port
	base := srv.URL + "/api/cluster/files"

	// mkdir
	code, body := apiPost(t, base+"/mkdir?apikey=test-key", map[string]any{"path": "/mirrors/i-abcd/world"})
	if code != http.StatusOK {
		t.Fatalf("mkdir 失败: %d %s", code, body)
	}
	// 写入文件（直连上传票据）
	code, body = apiPost(t, base+"/upload?apikey=test-key", map[string]any{"upload_dir": "/mirrors/i-abcd/world"})
	if code != http.StatusOK {
		t.Fatalf("申请上传票据失败: %d %s", code, body)
	}
	up := decodeData(t, body)
	pw, _ := up["password"].(string)
	addr, _ := up["addr"].(string)
	if pw == "" || addr == "" {
		t.Fatalf("上传票据缺少 password/addr: %v", up)
	}
	// multipart 上传
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "level.dat")
	_, _ = fw.Write([]byte("hello world"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/upload/"+pw, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("直连上传失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("直连上传返回 %d", resp.StatusCode)
	}

	// list：校验条目字段（含 sha256/mtime）
	code, body = doReq(t, base+"/list?apikey=test-key&path=/mirrors/i-abcd/world")
	if code != http.StatusOK {
		t.Fatalf("list 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("同步区应 1 个文件, 实际 %d", len(items))
	}
	it := items[0].(map[string]any)
	if it["name"] != "level.dat" || it["type"] != float64(1) {
		t.Fatalf("条目字段错误: %v", it)
	}
	if it["sha256"] != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Fatalf("sha256 错误: %v", it["sha256"])
	}
	if it["mtime"] == "" {
		t.Fatalf("mtime 缺失: %v", it)
	}
	if data["absolutePath"] != "/mirrors/i-abcd/world" {
		t.Fatalf("absolutePath 错误: %v", data["absolutePath"])
	}

	// 下载票据
	code, body = apiPost(t, base+"/download?apikey=test-key", map[string]any{"path": "/mirrors/i-abcd/world/level.dat"})
	if code != http.StatusOK {
		t.Fatalf("申请下载票据失败: %d %s", code, body)
	}
	dl := decodeData(t, body)
	dpw, _ := dl["password"].(string)
	daddr, _ := dl["addr"].(string)
	code, body = doReq(t, "http://"+daddr+"/download/"+dpw+"/mirrors/i-abcd/world/level.dat")
	if code != http.StatusOK || string(body) != "hello world" {
		t.Fatalf("直连下载失败: %d %s", code, body)
	}

	// 越界防护：../ 逃逸同步区必须拒绝（业务状态 400）
	code, body = apiPost(t, base+"/mkdir?apikey=test-key", map[string]any{"path": "/mirrors/../../escape"})
	var mkResp struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(body, &mkResp)
	if mkResp.Status != http.StatusBadRequest {
		t.Fatalf("越界路径未被拒绝: %d %s", mkResp.Status, body)
	}

	// delete
	code, body = apiDelete(t, base+"?apikey=test-key", map[string]any{"targets": []string{"/mirrors/i-abcd"}})
	if code != http.StatusOK {
		t.Fatalf("delete 失败: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(d.clusterRoot(), "i-abcd")); !os.IsNotExist(err) {
		t.Fatalf("同步区目录未被删除")
	}
}

// TestClusterSyncList P1 递归快照：单次枚举整树，sha256/path/type 正确。
func TestClusterSyncList(t *testing.T) {
	d, _ := newTestDaemon(t)
	root := filepath.Join(d.clusterRoot(), "i-abcd", "world")
	if err := os.MkdirAll(filepath.Join(root, "region"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "level.dat"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "region", "r.0.0.mca"), []byte("region"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/cluster/sync/list?apikey=test-key&path=/mirrors/i-abcd")
	if code != http.StatusOK {
		t.Fatalf("sync/list 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	items, _ := data["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("应枚举 4 个条目, 实际 %d: %v", len(items), data["items"])
	}
	byPath := map[string]map[string]any{}
	for _, it := range items {
		byPath[it.(map[string]any)["path"].(string)] = it.(map[string]any)
	}
	dir := byPath["/world/region"]
	if dir == nil || dir["type"] != float64(0) || dir["sha256"] != "" {
		t.Fatalf("目录条目错误: %v", dir)
	}
	file := byPath["/world/level.dat"]
	if file == nil || file["type"] != float64(1) {
		t.Fatalf("文件条目错误: %v", file)
	}
	if file["sha256"] == "" || file["mtime"] == "" {
		t.Fatalf("文件条目缺 sha256/mtime: %v", file)
	}
}

// TestInstanceSyncList P1 实例级递归快照。
func TestInstanceSyncList(t *testing.T) {
	d, dir := newTestDaemon(t)
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte("a=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, dir)
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/instance/sync/list?apikey=test-key&uuid="+inst.InstanceUuid)
	if code != http.StatusOK {
		t.Fatalf("sync/list 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	if data["total"] != float64(1) {
		t.Fatalf("应枚举 1 个条目: %v", data)
	}
}

// TestInstanceSnapshotRestore P1 快照/恢复端到端：快照→下载→恢复到另一实例。
func TestInstanceSnapshotRestore(t *testing.T) {
	d, _ := newTestDaemon(t)
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "world.dat"), []byte("snapshot-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := sampleInst(1, srcDir)
	d.Instances = append(d.Instances, src)
	srv := newTestServer(d)
	defer srv.Close()
	// 同步端口到 httptest 端口，使票据 addr 指向测试服务器
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	d.Port = port

	// 快照
	code, body := apiPost(t, srv.URL+"/api/instance/snapshot?apikey=test-key",
		map[string]any{"uuid": src.InstanceUuid, "daemonId": d.UUID})
	if code != http.StatusOK {
		t.Fatalf("snapshot 失败: %d %s", code, body)
	}
	snap := decodeData(t, body)
	pw, _ := snap["password"].(string)
	addr, _ := snap["addr"].(string)
	fileName, _ := snap["fileName"].(string)
	if pw == "" || addr == "" || fileName == "" {
		t.Fatalf("快照响应不完整: %v", snap)
	}
	// 下载归档
	code, zipBody := doReq(t, "http://"+addr+"/download/"+pw+"/"+fileName)
	if code != http.StatusOK {
		t.Fatalf("下载快照失败: %d %s", code, zipBody)
	}
	// 目标实例：先把归档放入其同步区（模拟「目标节点收到归档」）
	dstDir := t.TempDir()
	dst := sampleInst(2, dstDir)
	d.Instances = append(d.Instances, dst)
	stage := filepath.Join(d.clusterRoot(), "i-incoming.zip")
	if err := os.WriteFile(stage, zipBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// restore
	code, body = apiPost(t, srv.URL+"/api/instance/restore?apikey=test-key",
		map[string]any{"uuid": dst.InstanceUuid, "daemonId": d.UUID, "fileName": "/i-incoming.zip"})
	if code != http.StatusOK {
		t.Fatalf("restore 失败: %d %s", code, body)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "world.dat"))
	if err != nil || string(got) != "snapshot-content" {
		t.Fatalf("恢复内容错误: %v %q", err, got)
	}
}

// TestClusterCoordination P2 集群协调基础版：status/heartbeat/events/peers。
func TestClusterCoordination(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/cluster"

	// status
	code, body := doReq(t, base+"/status?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("status 失败: %d %s", code, body)
	}
	st := decodeData(t, body)
	self, _ := st["self"].(map[string]any)
	if self["id"] != d.UUID {
		t.Fatalf("self.id 错误: %v", st)
	}

	// heartbeat（带 id/address 自动登记为对等节点）
	code, body = apiPost(t, base+"/heartbeat?apikey=test-key", map[string]any{
		"id":        "n-remote",
		"address":   "http://192.168.1.6:12346",
		"resource":  map[string]any{"cpuUsage": 0.4, "memUsage": 0.6},
		"instances": []any{map[string]any{"uuid": "u1", "status": 3}},
	})
	if code != http.StatusOK {
		t.Fatalf("heartbeat 失败: %d %s", code, body)
	}
	// events
	code, body = apiPost(t, base+"/events?apikey=test-key", map[string]any{"type": "crash", "instanceUuid": "u1", "count": 2})
	if code != http.StatusOK {
		t.Fatalf("events 失败: %d %s", code, body)
	}
	// peers：heartbeat 已登记
	code, body = doReq(t, base+"/peers?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("peers 失败: %d %s", code, body)
	}
	var peersResp struct {
		Status int              `json:"status"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &peersResp); err != nil {
		t.Fatalf("解析 peers 失败: %v（%s）", err, body)
	}
	found := false
	for _, p := range peersResp.Data {
		if p["id"] == "n-remote" {
			found = true
		}
	}
	if !found {
		t.Fatalf("对等节点未登记: %v", peersResp.Data)
	}
}

// TestClusterTransfer P2 节点间直传端到端：源节点快照 → 目标节点拉取解压。
func TestClusterTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("压力/规模测试在 -short 模式下跳过（CI 的 race 用 -short 跑核心并发/安全测试）")
	}
	// 源节点：有实例与数据
	src, _ := newTestDaemon(t)
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "world", "level.dat"), []byte("transfer-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, srcDir)
	src.Instances = append(src.Instances, inst)
	srcSrv := newTestServer(src)
	defer srcSrv.Close()
	su, _ := url.Parse(srcSrv.URL)
	sport, _ := strconv.Atoi(su.Port())
	src.Port = sport // 使快照票据 addr 指向源节点测试服务器

	// 目标节点：发起 transfer
	dst, _ := newTestDaemon(t)
	// 测试源为 httptest（127.0.0.1 环回）：显式放行环回走通端到端链路，
	// 生产默认禁止（防认证后 SSRF），SSRF 拒绝路径见 TestClusterTransferSSRF
	dst.transferAllowLoopback = true
	dstSrv := newTestServer(dst)
	defer dstSrv.Close()
	base := dstSrv.URL + "/api/cluster"

	code, body := apiPost(t, base+"/transfer?apikey=test-key", map[string]any{
		"instanceId": "i-abcd",
		"source": map[string]any{
			"address": srcSrv.URL, "apikey": "test-key",
			"uuid": inst.InstanceUuid, "daemonId": src.UUID,
		},
		"dest": "/mirrors/i-abcd",
	})
	if code != http.StatusOK {
		t.Fatalf("transfer 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	jobID, _ := data["jobId"].(string)
	if jobID == "" {
		t.Fatalf("transfer 无 jobId: %v", data)
	}
	// 轮询任务状态
	deadline := time.Now().Add(15 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		code, body = doReq(t, base+"/transfer?apikey=test-key&jobId="+jobID)
		if code != http.StatusOK {
			t.Fatalf("查询任务失败: %d %s", code, body)
		}
		st := decodeData(t, body)
		status, _ = st["status"].(string)
		if status == "done" || status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("任务未完成: %s %s", status, body)
	}
	got, err := os.ReadFile(filepath.Join(dst.clusterRoot(), "i-abcd", "world", "level.dat"))
	if err != nil || string(got) != "transfer-me" {
		t.Fatalf("拉取内容错误: %v %q", err, got)
	}
}

// TestClusterTransferSSRF 认证后 SSRF 防护：环回/未指定/链路本地/任意协议目标必须被拒。
// （审计报告 #5：此前 /api/cluster/transfer 可打 127.0.0.1:12399 完整成功）
func TestClusterTransferSSRF(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/cluster/transfer?apikey=test-key"

	blocked := map[string]string{
		"环回 IPv4":         "127.0.0.1:12399",
		"环回 IPv6":         "[::1]:12399",
		"环回不带协议":          "127.0.0.1:12399",
		"未指定地址":           "0.0.0.0:12399",
		"链路本地（云元数据）":      "169.254.169.254",
		"RFC1918 内网":      "192.168.1.1:12346",
		"RFC1918 内网 10/8": "10.0.0.5:12346",
		"任意协议 file":       "file:///etc/passwd",
		"任意协议 gopher":     "gopher://127.0.0.1:6379/_x",
	}
	for name, addr := range blocked {
		code, body := apiPost(t, base, map[string]any{
			"instanceId": "i-ssrf",
			"source": map[string]any{
				"address": addr, "apikey": "test-key",
				"uuid": "u-1", "daemonId": "d-1",
			},
			"dest": "/mirrors/i-ssrf",
		})
		var resp struct {
			Status int `json:"status"`
		}
		_ = json.Unmarshal(body, &resp)
		if resp.Status != http.StatusBadRequest || code != http.StatusBadRequest {
			t.Errorf("[SSRF] %s（%q）应被拒绝（HTTP/业务 400），实际 HTTP=%d 业务=%d %s",
				name, addr, code, resp.Status, body)
		}
		t.Logf("[SSRF] %s（%q）已被拒绝: HTTP %d", name, addr, code)
	}

	// -transfer-allow-cidr 显式放行内网后，RFC1918 地址可被受理（集群 LAN 直传）
	d.transferAllowCIDR = "192.168.0.0/16,10.0.0.0/8"
	if err := d.parseTransferAllowCIDR(); err != nil {
		t.Fatalf("解析 allow-cidr 失败: %v", err)
	}
	for name, addr := range map[string]string{
		"放行后 192.168/16": "http://192.168.1.7:12346",
		"放行后 10/8":       "http://10.0.1.9:12346",
	} {
		code, body := apiPost(t, base, map[string]any{
			"instanceId": "i-lan",
			"source": map[string]any{
				"address": addr, "apikey": "test-key",
				"uuid": "u-3", "daemonId": "d-3",
			},
			"dest": "/mirrors/i-lan",
		})
		if code != http.StatusOK {
			t.Errorf("[SSRF] %s（%q）配置放行后应被受理，实际 %d %s", name, addr, code, body)
		}
		t.Logf("[SSRF] %s（%q）配置 -transfer-allow-cidr 后已受理", name, addr)
	}
	// 硬性拒绝项不因 allow-cidr 放行：环回仍被拒
	code, body := apiPost(t, base, map[string]any{
		"instanceId": "i-lo",
		"source": map[string]any{
			"address": "127.0.0.1:12399", "apikey": "test-key",
			"uuid": "u-4", "daemonId": "d-4",
		},
		"dest": "/mirrors/i-lo",
	})
	var resp struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Status != http.StatusBadRequest {
		t.Errorf("[SSRF] 配置放行后环回仍应被拒（硬性拒绝），实际 %d %s", resp.Status, body)
	}

	// 合法地址（公网，非本机）通过入口校验：不要求任务成功，只要求被受理
	code, body = apiPost(t, base, map[string]any{
		"instanceId": "i-ok",
		"source": map[string]any{
			"address": "http://203.0.113.10:12346", "apikey": "test-key",
			"uuid": "u-2", "daemonId": "d-2",
		},
		"dest": "/mirrors/i-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("合法地址应被受理: %d %s", code, body)
	}
	data := decodeData(t, body)
	if data["jobId"] == "" {
		t.Fatalf("受理响应缺 jobId: %v", data)
	}
}

// TestParseTransferAllowCIDR -transfer-allow-cidr 配置解析：逗号分隔、去空白、
// 空配置、非法 CIDR 报错。
func TestParseTransferAllowCIDR(t *testing.T) {
	d, _ := newTestDaemon(t)

	// 空配置：不报错、不放行任何网段
	if err := d.parseTransferAllowCIDR(); err != nil {
		t.Fatalf("空配置应可解析: %v", err)
	}
	if len(d.transferAllowNets) != 0 {
		t.Fatalf("空配置不应有放行网段: %v", d.transferAllowNets)
	}

	// 正常解析（含空白分隔）
	d.transferAllowCIDR = " 192.168.0.0/16 , 10.0.0.0/8,"
	if err := d.parseTransferAllowCIDR(); err != nil {
		t.Fatalf("正常配置应可解析: %v", err)
	}
	if len(d.transferAllowNets) != 2 {
		t.Fatalf("应解析出 2 个网段，实际 %d", len(d.transferAllowNets))
	}
	if !d.ipInAllowCIDR(net.ParseIP("192.168.5.5")) || d.ipInAllowCIDR(net.ParseIP("172.16.3.3")) {
		t.Fatalf("网段匹配错误")
	}

	// 非法 CIDR：报错
	d.transferAllowCIDR = "192.168.0.0/16,not-a-cidr"
	if err := d.parseTransferAllowCIDR(); err == nil {
		t.Fatalf("非法 CIDR 应报错")
	}
}

// TestContainerUnavailable 容器能力探测与平台行为一致性：
// - 本机运行时不可用 → available=false，Docker 操作端点 501
// - 本机运行时可用（如 CI 的 Linux runner 自带 Docker）→ available=true
// - Bastille 端点在非 FreeBSD 平台恒定 501
func TestContainerUnavailable(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/container/info?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("container/info 失败: %d %s", code, body)
	}
	info := decodeData(t, body)
	_, _, _, runtimeOK := containerRuntimeInfo()
	if runtimeOK {
		if info["available"] != true {
			t.Fatalf("运行时可用但探测不可用: %v", info)
		}
	} else {
		if info["available"] != false {
			t.Fatalf("运行时不可用但探测可用: %v", info)
		}
	}
	// Bastille 端点在非 FreeBSD 平台恒 501（HTTP 与 body.status 一致）
	if runtime.GOOS != "freebsd" {
		for _, p := range []string{
			"/api/bastille/releases?apikey=test-key",
			"/api/bastille/jails?apikey=test-key",
			"/api/bastille/templates?apikey=test-key",
		} {
			code, body := doReq(t, srv.URL+p)
			if code != http.StatusNotImplemented {
				t.Fatalf("%s HTTP 应 501, 实际 %d %s", p, code, body)
			}
			var resp struct {
				Status int `json:"status"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("解析失败: %v（%s）", err, body)
			}
			if resp.Status != http.StatusNotImplemented {
				t.Fatalf("%s 业务状态应 501, 实际 %d %s", p, resp.Status, body)
			}
		}
		// POST 型 Bastille 端点同样应 501
		for _, p := range []struct {
			path string
			body map[string]any
		}{
			{"/api/bastille/setup", map[string]any{"mode": "default"}},
			{"/api/bastille/jails/x/clone", map[string]any{"newName": "y"}},
			{"/api/bastille/jails/x/export", nil},
			{"/api/bastille/jails/import", map[string]any{"file": "/a.tar.gz"}},
			{"/api/bastille/jails/x/limits", map[string]any{"memoryMb": 512}},
			{"/api/bastille/jails/x/mounts", map[string]any{"source": "/s", "dest": "/d"}},
			{"/api/bastille/jails/create", map[string]any{"name": "y", "release": "14.1-RELEASE"}},
		} {
			code, body := apiPost(t, srv.URL+p.path+"?apikey=test-key", p.body)
			if code != http.StatusNotImplemented {
				t.Fatalf("%s HTTP 应 501, 实际 %d %s", p.path, code, body)
			}
			var resp struct {
				Status int `json:"status"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("解析失败: %v（%s）", err, body)
			}
			if resp.Status != http.StatusNotImplemented {
				t.Fatalf("%s 业务状态应 501, 实际 %d %s", p.path, resp.Status, body)
			}
		}
	}
	// Docker 端点：本机运行时不可用（或非 Linux）时 501
	if !runtimeOK || runtime.GOOS != "linux" {
		for _, p := range []string{
			"/api/container/ps?apikey=test-key",
			"/api/image/list?apikey=test-key",
			"/api/volume/list?apikey=test-key",
			"/api/network/list?apikey=test-key",
		} {
			code, body := doReq(t, srv.URL+p)
			if code != http.StatusNotImplemented {
				t.Fatalf("%s HTTP 应 501, 实际 %d %s", p, code, body)
			}
			var resp struct {
				Status int `json:"status"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("解析失败: %v（%s）", err, body)
			}
			if resp.Status != http.StatusNotImplemented {
				t.Fatalf("%s 业务状态应 501, 实际 %d %s", p, resp.Status, body)
			}
		}
		for _, p := range []struct {
			path string
			body map[string]any
		}{
			{"/api/container/x/clone", map[string]any{"name": "y"}},
			{"/api/container/x/export", nil},
			{"/api/container/x/limits", map[string]any{"memoryMb": 512}},
			{"/api/image/import", map[string]any{"fileName": "/a.tar", "name": "img"}},
		} {
			code, body := apiPost(t, srv.URL+p.path+"?apikey=test-key", p.body)
			if code != http.StatusNotImplemented {
				t.Fatalf("%s HTTP 应 501, 实际 %d %s", p.path, code, body)
			}
			var resp struct {
				Status int `json:"status"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("解析失败: %v（%s）", err, body)
			}
			if resp.Status != http.StatusNotImplemented {
				t.Fatalf("%s 业务状态应 501, 实际 %d %s", p.path, resp.Status, body)
			}
		}
	}
}

// TestFileListHasSyncFields 实例级文件列表条目含 mtime/sha256（集群文档 §4 要求）。
func TestFileListHasSyncFields(t *testing.T) {
	d, dir := newTestDaemon(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("sync"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, dir)
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/files/list?apikey=test-key&uuid="+inst.InstanceUuid)
	if code != http.StatusOK {
		t.Fatalf("files/list 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应 1 个条目: %v", data)
	}
	it := items[0].(map[string]any)
	if it["mtime"] == "" || it["sha256"] == "" {
		t.Fatalf("条目缺 mtime/sha256: %v", it)
	}
	if it["sha256"] != "75c75efe327a8ef35a072f25117961f5b99e35035dc9bd86493dd29fd7bc07eb" {
		t.Fatalf("sha256 错误: %v", it["sha256"])
	}
	if !strings.Contains(string(body), "mtime") {
		t.Fatalf("响应缺少 mtime 字段: %s", body)
	}
}

// TestOverviewSyncFields 概览响应含磁盘/网络/version 字段（§2 契约）。
func TestOverviewSyncFields(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := doReq(t, srv.URL+"/api/overview?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("overview 失败: %d %s", code, body)
	}
	var resp struct {
		Data struct {
			System map[string]any `json:"system"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, k := range []string{"version", "diskusage", "disktotal", "diskused", "networkDownload", "networkUpload"} {
		if _, ok := resp.Data.System[k]; !ok {
			t.Fatalf("system 缺字段 %s: %v", k, resp.Data.System)
		}
	}
}

// TestUploadTicketHasUploadDir 上传票据响应含 upload_dir（§4 契约）。
func TestUploadTicketHasUploadDir(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	code, body := apiPost(t, srv.URL+"/api/files/upload?apikey=test-key&uuid="+inst.InstanceUuid+"&upload_dir=/sub",
		map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("upload 失败: %d %s", code, body)
	}
	data := decodeData(t, body)
	if data["upload_dir"] != "/sub" {
		t.Fatalf("upload_dir 错误: %v", data)
	}
	if data["password"] == "" || data["addr"] == "" {
		t.Fatalf("票据不完整: %v", data)
	}
}

// TestContainerJobStore 长任务注册表：创建/读取/行数上限。
func TestContainerJobStore(t *testing.T) {
	id := jobs.create()
	if id == "" {
		t.Fatal("任务创建失败")
	}
	job := jobs.get(id)
	if job == nil || job.status != "building" {
		t.Fatalf("任务状态错误: %v", job)
	}
	col := &logCollector{job: job}
	for i := 0; i < jobLogMaxLines+50; i++ {
		_, _ = col.Write([]byte(fmt.Sprintf("line-%d\n", i)))
	}
	job.mu.Lock()
	n := len(job.log)
	job.mu.Unlock()
	if n != jobLogMaxLines {
		t.Fatalf("日志行数应截断到 %d, 实际 %d", jobLogMaxLines, n)
	}
}

// TestNormalizeRelease 客户端可能传 "name:version" 标签，服务端剥离冒号后缀。
func TestNormalizeRelease(t *testing.T) {
	cases := map[string]string{
		"15.0-RELEASE:15.0-RELEASE": "15.0-RELEASE",
		"14.2-RELEASE:14.2":         "14.2-RELEASE",
		"14.2-RELEASE":              "14.2-RELEASE",
		"":                          "",
		":weird":                    ":weird", // 前缀冒号无意义，原样保留由后续校验处理
	}
	for in, want := range cases {
		if got := normalizeRelease(in); got != want {
			t.Fatalf("normalizeRelease(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestSplitPorts docker ps 的 Ports 字符串拆分（客户端契约要求数组）。
func TestSplitPorts(t *testing.T) {
	cases := map[string][]string{
		"0.0.0.0:25565->25565/tcp, :::25565->25565/tcp": {"0.0.0.0:25565->25565/tcp", ":::25565->25565/tcp"},
		"":                {},
		"25565/tcp":       {"25565/tcp"},
		"80/tcp, 443/tcp": {"80/tcp", "443/tcp"},
	}
	for in, want := range cases {
		got := splitPorts(in)
		if len(got) != len(want) {
			t.Fatalf("splitPorts(%q) = %v, 期望 %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("splitPorts(%q) = %v, 期望 %v", in, got, want)
			}
		}
	}
}

// TestDockerTime docker 时间格式转 ISO-8601。
func TestDockerTime(t *testing.T) {
	cases := map[string]string{
		"2026-08-14 12:34:56 +0000 UTC": "2026-08-14T12:34:56Z",
		"2026-01-01 00:00:00 +0000 UTC": "2026-01-01T00:00:00Z",
		"garbage":                       "garbage", // 解析失败原样返回
	}
	for in, want := range cases {
		if got := dockerTime(in); got != want {
			t.Fatalf("dockerTime(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestParseDockerSize 容量字符串解析。
func TestParseDockerSize(t *testing.T) {
	cases := map[string]uint64{
		"1.2MiB":  1 << 20 * 12 / 10,
		"187MB":   187 * 1000 * 1000,
		"3.4kB":   3400,
		"512B":    512,
		"1GiB":    1 << 30,
		"bad":     0,
		"":        0,
		" 2.5GB ": 25 * 1000 * 1000 * 1000 / 10,
	}
	for in, want := range cases {
		if got := parseDockerSize(in); got != want {
			t.Fatalf("parseDockerSize(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

// TestParseRdrLine 按 Bastille 官方文档的 rdr list 实例输出解析端口。
func TestParseRdrLine(t *testing.T) {
	// 官方输出形如: rdr on em0 inet proto tcp from any to any port = 2001 -> 10.17.89.1 port 22
	h, j := parseRdrLine("rdr on em0 inet proto tcp from any to any port = 2001 -> 10.17.89.1 port 22")
	if h != 2001 || j != 22 {
		t.Fatalf("解析错误: hostPort=%d jailPort=%d", h, j)
	}
	// pass 行（无 ->）应解析出 jailPort=0 从而被过滤
	_, j2 := parseRdrLine("pass in on bge0 inet proto tcp from any to 10.17.89.1 port = 22 flags S/SA keep state")
	if j2 != 0 {
		t.Fatalf("pass 行不应解析出 jailPort: %d", j2)
	}
}
