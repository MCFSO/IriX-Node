// MCSManager 面板「快速安装/异步任务」兼容层的用例。
//
// 面板服务端市场点 Install 走的是：instance/asynchronous（quick_install）
// → instance/query_asynchronous 轮询 → 完成后实例出现在列表里。
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeTemplateZip 构造一个最小模板包（含 mcsmanager-config.json）。
func makeTemplateZip(t *testing.T, name string, cfg map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "template.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	add("server.jar", "FAKE-JAR")
	add("eula.txt", "eula=true\n")
	if cfg != nil {
		data, _ := json.Marshal(cfg)
		add("mcsmanager-config.json", string(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMCSMAsyncQuickInstall 快速安装：下载模板包 → 解压 → 建实例 → 任务完成。
func TestMCSMAsyncQuickInstall(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	// 测试里允许访问环回（生产由 checkTransferIP 拦截）
	d.transferAllowLoopback = true

	pack := makeTemplateZip(t, "paper", map[string]any{
		"nickname":     "模板来的服",
		"startCommand": "java -Xms512M -Xmx1024M -jar server.jar nogui",
		"stopCommand":  "stop",
	})
	packBytes, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(packBytes)))
		_, _ = w.Write(packBytes)
	}))
	defer srv.Close()

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "a0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}

	status, data := c.requestRaw("instance/asynchronous", "t1", map[string]any{
		"instanceUuid": "",
		"taskName":     "quick_install",
		"role":         10,
		"parameter": map[string]any{
			"newInstanceName": "模板安装测试",
			"targetLink":      srv.URL + "/template.zip",
			"setupInfo":       map[string]any{"startCommand": "java -jar server.jar nogui"},
		},
	})
	if status != 200 {
		t.Fatalf("发起快速安装失败: %d %s", status, data)
	}
	var created struct {
		TaskID       string `json:"taskId"`
		InstanceUUID string `json:"instanceUuid"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.TaskID == "" || created.InstanceUUID == "" {
		t.Fatalf("回包应包含 taskId 与 instanceUuid: %s", data)
	}

	// 轮询到任务结束
	deadline := time.Now().Add(20 * time.Second)
	var lastStatus float64 = -99
	for time.Now().Before(deadline) {
		status, data := c.requestRaw("instance/query_asynchronous", "q1", map[string]any{
			"taskName":  "quick_install",
			"parameter": map[string]any{"taskId": created.TaskID},
		})
		if status != 200 {
			t.Fatalf("查询任务失败: %d %s", status, data)
		}
		var env struct {
			TaskID string  `json:"taskId"`
			Status float64 `json:"status"`
			Detail struct {
				InstanceUUID     string `json:"instanceUuid"`
				InstanceStatus   int    `json:"instanceStatus"`
				DownloadProgress struct {
					Percentage int `json:"percentage"`
				} `json:"downloadProgress"`
			} `json:"detail"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("任务回包解析失败: %s", data)
		}
		lastStatus = env.Status
		if env.Status != float64(mcAsyncStatusRunning) {
			if env.Status == float64(mcAsyncStatusError) {
				t.Fatalf("任务失败: %s", data)
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastStatus == float64(mcAsyncStatusRunning) {
		t.Fatalf("任务未在期限内结束（status=%v）", lastStatus)
	}

	// 实例应存在，且模板包内容已解压到实例目录
	inst := d.Find(created.InstanceUUID)
	if inst == nil {
		t.Fatal("快速安装后实例应存在")
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	for _, name := range []string{"server.jar", "eula.txt", "mcsmanager-config.json"} {
		if _, err := os.Stat(filepath.Join(cwd, name)); err != nil {
			t.Fatalf("实例目录缺少 %s: %v", name, err)
		}
	}
}

// TestMCSMAsyncQueryList 不传 taskId 时返回任务列表（面板首屏用）。
func TestMCSMAsyncQueryList(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "a0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	status, data := c.requestRaw("instance/query_asynchronous", "q0", map[string]any{
		"taskName":  "quick_install",
		"parameter": map[string]any{},
	})
	if status != 200 {
		t.Fatalf("查询列表失败: %d %s", status, data)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("列表应为数组: %s", data)
	}
}

// TestMCSMAsyncInstallRejectsBadLink 非法下载地址必须报错且不影响节点。
func TestMCSMAsyncInstallRejectsBadLink(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "a0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	status, data := c.requestRaw("instance/asynchronous", "t1", map[string]any{
		"taskName":  "quick_install",
		"role":      10,
		"parameter": map[string]any{"newInstanceName": "坏地址", "targetLink": "file:///etc/passwd"},
	})
	// 任务会被创建（真实 daemon 也是异步的），但随后失败；这里先确认同步校验拦住明显非法输入
	if status == 200 {
		var created struct {
			TaskID string `json:"taskId"`
		}
		_ = json.Unmarshal(data, &created)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			_, q := c.requestRaw("instance/query_asynchronous", "q1", map[string]any{
				"taskName":  "quick_install",
				"parameter": map[string]any{"taskId": created.TaskID},
			})
			var env struct {
				Status float64 `json:"status"`
			}
			_ = json.Unmarshal(q, &env)
			if env.Status == float64(mcAsyncStatusError) {
				return // 如期失败
			}
			if env.Status == float64(mcAsyncStatusStop) {
				t.Fatalf("非法地址不应成功")
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("非法地址任务应失败: %s", data)
	}
	if !strings.Contains(string(data), "http") {
		t.Fatalf("错误信息应提示只允许 http/https: %s", data)
	}
}

// TestMCSMAsyncUpdateWithoutCommand update 任务在实例没配 updateCommand 时必须明确报错。
func TestMCSMAsyncUpdateWithoutCommand(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)
	cwd, _ := os.MkdirTemp("", "irix-async-upd-")
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "a0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	status, data := c.requestRaw("instance/asynchronous", "t1", map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"taskName":     "update",
		"role":         10,
		"parameter":    map[string]any{},
	})
	if status != 500 {
		t.Fatalf("无 updateCommand 时应报错，实际 %d %s", status, data)
	}
	if !strings.Contains(string(data), "updateCommand") {
		t.Fatalf("错误信息应提到 updateCommand: %s", data)
	}
}
