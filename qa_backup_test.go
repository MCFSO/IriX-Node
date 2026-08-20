// 实例备份/恢复测试：快照端到端（排除规则）、恢复（自动停服/覆盖/保持停止）、
// 备份列表/删除、下载票据、路径越界。

package main

import (
	"archive/zip"
	"encoding/json"
	"io"
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

// startTask 发起任务并轮询到 done/failed，返回最终快照。
func startTask(t *testing.T, srvURL, path, body string) map[string]any {
	t.Helper()
	resp, err := testClient.Post(srvURL+path+"?apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("发起任务失败: %v", err)
	}
	var out struct {
		Status int `json:"status"`
		Data   struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		t.Fatalf("响应解析失败: %v", err)
	}
	resp.Body.Close()
	if out.Status != 200 || out.Data.JobID == "" {
		t.Fatalf("未返回 jobId: %+v", out)
	}
	jobID := out.Data.JobID

	// 进度轮询：snapshot-progress 与 restore 共用同一进度语义
	deadline := time.Now().Add(30 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, res := doJSONReq(t, http.MethodGet,
			srvURL+"/api/instance/snapshot-progress?jobId="+jobID+"&apikey=test-key")
		if code != 200 {
			t.Fatalf("进度查询状态码: %d", code)
		}
		last, _ = res["data"].(map[string]any)
		if status, _ := last["status"].(string); status == "done" || status == "failed" {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("任务超时: %v", last)
	return nil
}

// setupServerWorld 构造一个带世界文件、回收站、日志的实例目录。
func setupServerWorld(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{
		"server.jar", "eula.txt", "world/region/r.0.0.mca", "plugins/x/config.yml",
		".irix-trash/old-file.txt", "server.log", "debug.log", "tmp.part-abc",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte("content-"+p), 0o644); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
}

// TestSnapshotEndToEnd 快照任务完成，zip 内容正确且应用排除规则。
func TestSnapshotEndToEnd(t *testing.T) {
	d, dir := newTestDaemon(t)
	world := filepath.Join(dir, "mc")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	setupServerWorld(t, world)
	inst := NewInstance("snap-uuid", InstanceConfig{Nickname: "备份", Cwd: world})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	body := `{"uuid":"snap-uuid","daemonId":"local"}`
	last := startTask(t, srv.URL, "/api/instance/snapshot", body)
	if last["status"] != "done" {
		t.Fatalf("快照未完成: %v", last)
	}
	archivePath, _ := last["archivePath"].(string)
	if !strings.HasSuffix(archivePath, ".zip") {
		t.Fatalf("备份路径异常: %s", archivePath)
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("打开备份失败: %v", err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	for _, want := range []string{"server.jar", "eula.txt", "world/region/r.0.0.mca", "plugins/x/config.yml"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("备份缺少 %s（实际 %v）", want, names)
		}
	}
	for _, bad := range []string{".irix-trash", "server.log", "debug.log", "tmp.part-abc"} {
		for _, n := range names {
			if strings.Contains(n, bad) {
				t.Fatalf("排除项不应出现在备份中: %s（%v）", bad, names)
			}
		}
	}
	t.Logf("[验证] 快照完成（%d 项，排除规则生效）: %s", len(names), archivePath)
}

// TestRestoreEndToEnd 恢复覆盖实例目录并保持停止（Windows 预杀进程）。
func TestRestoreEndToEnd(t *testing.T) {
	d, dir := newTestDaemon(t)
	world := filepath.Join(dir, "mc")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	// 可被 stop 命令终止的进程（Linux：sh 读到 stop 退出；Windows：预杀）
	cmd := "sh -c 'while read l; do [ \"$l\" = stop ] && exit; done'"
	if runtime.GOOS == "windows" {
		cmd = "cmd /c more"
	}
	inst := NewInstance("restore-uuid", InstanceConfig{
		Nickname: "恢复", Cwd: world, StartCommand: cmd,
	})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	setupServerWorld(t, world)
	body := `{"uuid":"restore-uuid","daemonId":"local"}`
	last := startTask(t, srv.URL, "/api/instance/snapshot", body)
	if last["status"] != "done" {
		t.Fatalf("快照未完成: %v", last)
	}
	archivePath, _ := last["archivePath"].(string)

	// 启动实例（验证恢复会先停服；Windows 下 more 不吃 stop 命令，
	// 恢复前先强杀，Linux 用可被 stop 终止的 sh 循环）
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		t.Fatalf("实例未运行")
	}
	if runtime.GOOS == "windows" {
		_ = proc.Kill()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && proc.IsRunning() {
			time.Sleep(50 * time.Millisecond)
		}
	}
	worldFile := filepath.Join(world, "world", "region", "r.0.0.mca")
	if err := os.WriteFile(worldFile, []byte("MODIFIED"), 0o644); err != nil {
		t.Fatalf("修改世界文件失败: %v", err)
	}

	// 恢复
	restoreBody := `{"uuid":"restore-uuid","daemonId":"local","archivePath":` +
		jsonMarshal(archivePath) + `}`
	last = startTask(t, srv.URL, "/api/instance/restore", restoreBody)
	if last["status"] != "done" {
		t.Fatalf("恢复未完成: %v", last)
	}
	// 文件被还原
	data, err := os.ReadFile(worldFile)
	if err != nil || string(data) != "content-world/region/r.0.0.mca" {
		t.Fatalf("恢复后文件内容错误: %q (%v)", data, err)
	}
	// 实例保持停止
	inst.mu.Lock()
	status := inst.Status
	inst.mu.Unlock()
	if status != StatusStopped {
		t.Fatalf("恢复后实例应保持停止: %d", status)
	}
	t.Logf("[验证] 恢复覆盖文件、自动停服、保持停止")
}

// jsonMarshal 编码 JSON 字符串（测试辅助）。
func jsonMarshal(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestBackupsListDelete 备份列表与删除。
func TestBackupsListDelete(t *testing.T) {
	d, dir := newTestDaemon(t)
	world := filepath.Join(dir, "mc")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	_ = os.WriteFile(filepath.Join(world, "server.jar"), []byte("x"), 0o644)
	inst := NewInstance("list-uuid", InstanceConfig{Nickname: "列表", Cwd: world})
	d.Instances = append(d.Instances, inst)
	srv := newTestServer(d)
	defer srv.Close()

	// 造两个备份（显式 mtime 区分新旧）
	backupDir := d.backupsDirOf("list-uuid")
	_ = os.MkdirAll(backupDir, 0o755)
	oldZip := filepath.Join(backupDir, "2026-08-20-09-00-00.zip")
	newZip := filepath.Join(backupDir, "2026-08-20-10-00-00.zip")
	_ = os.WriteFile(oldZip, []byte("b1"), 0o644)
	_ = os.WriteFile(newZip, []byte("b2"), 0o644)
	_ = os.Chtimes(oldZip, time.Unix(1787216400, 0), time.Unix(1787216400, 0))
	_ = os.Chtimes(newZip, time.Unix(1787220000, 0), time.Unix(1787220000, 0))

	code, out := doJSONReq(t, http.MethodGet, srv.URL+"/api/instance/backups?uuid=list-uuid&apikey=test-key")
	if code != 200 {
		t.Fatalf("列表状态码: %d", code)
	}
	items, _ := out["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("备份列表应有 2 项: %v", items)
	}
	first := items[0].(map[string]any)
	if first["fileName"] != "2026-08-20-10-00-00.zip" {
		t.Fatalf("应按时间新→旧排序: %v", items)
	}
	// 删除其中一个
	delBody := `{"paths":[` + jsonMarshal(first["path"].(string)) + `]}`
	req, _ := http.NewRequest(http.MethodDelete,
		srv.URL+"/api/instance/backups?uuid=list-uuid&apikey=test-key",
		strings.NewReader(delBody))
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除状态码: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "2026-08-20-10-00-00.zip")); err == nil {
		t.Fatalf("备份未被删除")
	}
	t.Logf("[验证] 备份列表排序与删除正确")
}

// TestBackupDownloadTicket 备份下载票据：绑定单文件、可下载、越界拒绝。
func TestBackupDownloadTicket(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()

	backupDir := d.backupsDirOf(inst.InstanceUuid)
	_ = os.MkdirAll(backupDir, 0o755)
	zipPath := filepath.Join(backupDir, "a.zip")
	_ = os.WriteFile(zipPath, []byte("backup-content"), 0o644)

	// 同步端口到 httptest 端口，使票据 addr 指向测试服务器
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	d.Port = port

	// 申请票据
	body := `{"path":` + jsonMarshal(zipPath) + `}`
	resp, err := testClient.Post(srv.URL+"/api/instance/backups/download?uuid="+inst.InstanceUuid+"&apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("申请票据失败: %v", err)
	}
	var out struct {
		Data struct {
			Password string `json:"password"`
			Addr     string `json:"addr"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	resp.Body.Close()
	if out.Data.Password == "" {
		t.Fatalf("票据为空")
	}
	// 直连下载
	dlResp, err := testClient.Get("http://" + out.Data.Addr + "/download/" + out.Data.Password + "/a.zip")
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	data, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()
	if string(data) != "backup-content" {
		t.Fatalf("下载内容错误: %q", data)
	}

	// 越界路径拒绝
	bad := `{"path":` + jsonMarshal(filepath.Join(dir, "instances.json")) + `}`
	resp2, err := testClient.Post(srv.URL+"/api/instance/backups/download?uuid="+inst.InstanceUuid+"&apikey=test-key",
		"application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatalf("越界请求失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("越界路径应 400: %d", resp2.StatusCode)
	}
	t.Logf("[验证] 备份下载票据绑定单文件、直连下载、越界拒绝")
}

// TestRestorePathTraversal 恢复路径不在备份区 → 400。
func TestRestorePathTraversal(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()

	evil := filepath.Join(dir, "instances.json")
	body := `{"uuid":"` + inst.InstanceUuid + `","archivePath":` + jsonMarshal(evil) + `}`
	resp, err := testClient.Post(srv.URL+"/api/instance/restore?apikey=test-key",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("越界恢复应 400: %d", resp.StatusCode)
	}
	t.Logf("[验证] 备份区外路径恢复被拒绝")
}
