// MSLX 兼容层定时任务（面板「定时任务」页）的用例。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseCronSpec 校验 5 段 cron 解析与匹配语义。
func TestParseCronSpec(t *testing.T) {
	cases := []struct {
		expr  string
		when  time.Time
		match bool
	}{
		{"* * * * *", time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC), true},
		{"30 4 * * *", time.Date(2026, 3, 4, 4, 30, 0, 0, time.UTC), true},
		{"30 4 * * *", time.Date(2026, 3, 4, 4, 31, 0, 0, time.UTC), false},
		{"*/15 * * * *", time.Date(2026, 3, 4, 4, 45, 0, 0, time.UTC), true},
		{"*/15 * * * *", time.Date(2026, 3, 4, 4, 46, 0, 0, time.UTC), false},
		{"0 0 1 * *", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), true},
		{"0 0 1 * *", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), false},
		// 日与周同时限制时取「或」：每月 1 号或每周一
		{"0 0 1 * mon", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), true}, // 3/2 是周一
		{"0 0 1 * mon", time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), true}, // 周一
		{"0 0 1 * mon", time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), false},
		{"0 0 * jan-mar *", time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), true},
		{"0 0 * jan-mar *", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC), false},
		{"0 9-18/3 * * *", time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC), true},
		{"0 9-18/3 * * *", time.Date(2026, 3, 4, 13, 0, 0, 0, time.UTC), false},
		{"5,35 * * * *", time.Date(2026, 3, 4, 4, 35, 0, 0, time.UTC), true},
		{"5,35 * * * *", time.Date(2026, 3, 4, 4, 36, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		spec, err := parseCronSpec(c.expr)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.expr, err)
		}
		if got := spec.match(c.when); got != c.match {
			t.Fatalf("%q 在 %s 的匹配结果应为 %v，实际 %v", c.expr, c.when, c.match, got)
		}
	}

	// 非法表达式必须报错
	for _, bad := range []string{"", "* * * *", "61 * * * *", "* * * * 9", "*/0 * * * *", "a b c d e", "*,,, * * * *"} {
		if _, err := parseCronSpec(bad); err == nil {
			t.Fatalf("非法表达式 %q 应报错", bad)
		}
	}
}

// TestParseCronSpecNextRun 下一次触发时间的推算。
func TestParseCronSpecNextRun(t *testing.T) {
	spec, err := parseCronSpec("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 4, 4, 0, 0, 0, time.UTC)
	next, ok := spec.nextRun(from)
	if !ok || next != time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC) {
		t.Fatalf("下一次触发应为次日 03:00，实际 %s", next)
	}
}

// TestCompatMSLXCronTaskCRUD 定时任务的增删改查与持久化。
func TestCompatMSLXCronTaskCRUD(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd, _ := os.MkdirTemp("", "irix-cron-")
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	id := d.compatMSLX.idFor(inst.InstanceUuid)

	post := func(path, body string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-user-token", token)
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	// 创建
	createBody := fmt.Sprintf(`{"instanceId":%d,"name":"每天备份","cron":"0 3 * * *","type":"backup","payload":"","enable":true,"runWhenOffline":true}`, id)
	status, raw := post("/api/instance/tasks/create", createBody)
	if status != http.StatusOK {
		t.Fatalf("创建任务应 200: %d %s", status, raw)
	}
	var created struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.Data == "" {
		t.Fatalf("创建任务应返回 id: %s", raw)
	}
	taskID := created.Data

	// 列表（全部 / 按实例）
	for _, path := range []string{"/api/instance/tasks/list", fmt.Sprintf("/api/instance/tasks/list/%d", id)} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("x-user-token", token)
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var env struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s 解析失败: %s", path, raw)
		}
		if len(env.Data) != 1 {
			t.Fatalf("%s 应有 1 个任务: %s", path, raw)
		}
		for _, field := range []string{"id", "instanceId", "name", "type", "cron", "payload", "enable", "runWhenOffline"} {
			if _, ok := env.Data[0][field]; !ok {
				t.Fatalf("%s 返回字段缺少 %s（面板任务表会读它）: %s", path, field, raw)
			}
		}
	}

	// 更新
	updateBody := fmt.Sprintf(`{"id":%q,"instanceId":%d,"name":"改名任务","cron":"*/5 * * * *","type":"command","payload":"say hi","enable":false,"runWhenOffline":false}`, taskID, id)
	if status, raw := post("/api/instance/tasks/update", updateBody); status != http.StatusOK {
		t.Fatalf("更新任务应 200: %d %s", status, raw)
	}
	// 非法 cron 必须被拒绝
	badBody := fmt.Sprintf(`{"id":%q,"instanceId":%d,"name":"x","cron":"not-a-cron","type":"command","payload":"x"}`, taskID, id)
	if status, _ := post("/api/instance/tasks/update", badBody); status != http.StatusBadRequest {
		t.Fatalf("非法 cron 应 400，实际 %d", status)
	}
	// 实例不存在必须被拒绝
	if status, _ := post("/api/instance/tasks/create", `{"instanceId":9999,"name":"x","cron":"* * * * *","type":"command","payload":"x"}`); status != http.StatusBadRequest {
		t.Fatalf("未知实例应 400，实际 %d", status)
	}

	// 持久化：任务表文件存在且包含任务
	data, err := os.ReadFile(filepath.Join(d.DataDir, "compat-mslx-tasks.json"))
	if err != nil {
		t.Fatalf("任务表应落盘: %v", err)
	}
	if !strings.Contains(string(data), "改名任务") {
		t.Fatalf("任务表应包含更新后的名称: %s", data)
	}

	// 删除
	if status, raw := post("/api/instance/tasks/delete/"+taskID, ""); status != http.StatusOK {
		t.Fatalf("删除任务应 200: %d %s", status, raw)
	}
	if status, _ := post("/api/instance/tasks/delete/"+taskID, ""); status != http.StatusBadRequest {
		t.Fatalf("重复删除应 400，实际 %d", status)
	}
}

// TestCompatMSLXCronFires 调度器按 cron 触发任务（直接调用 tickCron 校验）。
func TestCompatMSLXCronFires(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd, _ := os.MkdirTemp("", "irix-cron-fire-")
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	inst.Config.StopCommand = "stop"
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })
	id := d.compatMSLX.idFor(inst.InstanceUuid)

	// 建一个「每分钟」的 shell 任务：写文件作为执行证据
	marker := filepath.Join(cwd, "cron-fired.txt")
	payload := `echo fired > "` + marker + `"`
	body := fmt.Sprintf(`{"instanceId":%d,"name":"写标记","cron":"* * * * *","type":"shell","payload":%q,"enable":true,"runWhenOffline":false}`,
		id, payload)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/instance/tasks/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-user-token", token)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建任务失败: %d %s", resp.StatusCode, raw)
	}

	// 触发一次调度 tick（与真实调度器同一入口）
	d.compatMSLX.tickCron()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cron 任务应被执行并写出标记文件")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 同一分钟内重复 tick 不应重复执行（lastFired 去重）
	_ = os.Remove(marker)
	d.compatMSLX.tickCron()
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("同一分钟内不应重复执行任务")
	}

	// 最后执行时间应被记录
	req, _ = http.NewRequest(http.MethodGet, "/x", nil)
	_ = req
	cronTasks := func() []mslxCronTask {
		d.compatMSLX.cron.mu.Lock()
		defer d.compatMSLX.cron.mu.Unlock()
		out := make([]mslxCronTask, len(d.compatMSLX.cron.items))
		copy(out, d.compatMSLX.cron.items)
		return out
	}()
	if len(cronTasks) != 1 || cronTasks[0].LastRunTime == nil {
		t.Fatalf("任务应记录 lastRunTime: %+v", cronTasks)
	}
}

// TestCompatMSLXCronDisabledSkipped 停用的任务不应被调度。
func TestCompatMSLXCronDisabledSkipped(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMSLX(t, d)
	token := mslxLogin(t, base)

	cwd, _ := os.MkdirTemp("", "irix-cron-off-")
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	inst.Config.StartCommand = echoCommand()
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })
	id := d.compatMSLX.idFor(inst.InstanceUuid)

	marker := filepath.Join(cwd, "disabled-fired.txt")
	body := fmt.Sprintf(`{"instanceId":%d,"name":"停用任务","cron":"* * * * *","type":"shell","payload":%q,"enable":false,"runWhenOffline":false}`,
		id, `echo x > "`+marker+`"`)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/instance/tasks/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-user-token", token)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	d.compatMSLX.tickCron()
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("停用的任务不应执行")
	}
}
