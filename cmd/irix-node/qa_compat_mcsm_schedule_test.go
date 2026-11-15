// MCSManager 面板「计划任务」兼容层的用例（真实面板会走 panel → daemon 的
// schedule/register、schedule/list、schedule/delete 三个事件）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseCronSpecSixFields MCSM 的 node-schedule 表达式是 6 段（含秒）。
func TestParseCronSpecSixFields(t *testing.T) {
	spec, err := parseCronSpec("8 19 14 * * 1,2,3,4")
	if err != nil {
		t.Fatalf("6 段表达式解析失败: %v", err)
	}
	// 2026-11-16 是周一
	if !spec.match(time.Date(2026, 11, 16, 14, 19, 8, 0, time.UTC)) {
		t.Fatal("应命中周一 14:19:08")
	}
	if spec.match(time.Date(2026, 11, 16, 14, 19, 9, 0, time.UTC)) {
		t.Fatal("秒不匹配时不应命中")
	}
	// 2026-11-20 是周五（dow=5），不在 1,2,3,4 内
	if spec.match(time.Date(2026, 11, 20, 14, 19, 8, 0, time.UTC)) {
		t.Fatal("周五不应命中（表达式限定 1,2,3,4）")
	}
	// 5 段仍然可用，7 段报错
	if _, err := parseCronSpec("0 3 * * *"); err != nil {
		t.Fatalf("5 段应继续可用: %v", err)
	}
	if _, err := parseCronSpec("0 0 3 * * * *"); err == nil {
		t.Fatal("7 段应报错")
	}
}

// TestMCMScheduleCRUD 计划任务的注册/列表/删除与字段回显。
func TestMCMScheduleCRUD(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)

	cwd, err := os.MkdirTemp("", "irix-sched-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "s0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}

	// 注册（面板「Add Scheduled Task」提交的形状）
	body := map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"name":         "111",
		"count":        1,
		"time":         "0 30 4 * * *",
		"type":         2,
		"actions":      []map[string]any{{"type": "stop", "payload": ""}},
	}
	status, data := c.requestRaw("schedule/register", "r1", body)
	if status != 200 {
		t.Fatalf("注册计划任务失败: %d %s", status, data)
	}

	// 列表：字段必须齐全（面板按这些字段渲染表格）
	status, data = c.requestRaw("schedule/list", "l1", map[string]any{"instanceUuid": inst.InstanceUuid})
	if status != 200 {
		t.Fatalf("列表失败: %d %s", status, data)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("列表解析失败: %s", data)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 个任务: %s", data)
	}
	for _, field := range []string{"instanceUuid", "name", "count", "time", "actions", "type"} {
		if _, ok := list[0][field]; !ok {
			t.Fatalf("任务缺少字段 %s（面板表格会读它）: %s", field, data)
		}
	}
	if list[0]["name"] != "111" || list[0]["type"] != float64(2) {
		t.Fatalf("字段回显异常: %s", data)
	}

	// 同名重复注册视为更新（面板「编辑」也走 register）
	body["time"] = "*/5 * * * * *" // 6 段：每 5 秒
	if status, data := c.requestRaw("schedule/register", "r2", body); status != 200 {
		t.Fatalf("更新计划任务失败: %d %s", status, data)
	}
	if status, data := c.requestRaw("schedule/list", "l2", map[string]any{"instanceUuid": inst.InstanceUuid}); status != 200 {
		t.Fatalf("列表失败: %d %s", status, data)
	} else {
		var list2 []map[string]any
		_ = json.Unmarshal(data, &list2)
		if len(list2) != 1 || list2[0]["time"] != "*/5 * * * * *" {
			t.Fatalf("同名注册应更新而不是新增: %s", data)
		}
	}

	// 非法输入必须报错
	badCases := []map[string]any{
		{"instanceUuid": inst.InstanceUuid, "name": "x1", "count": 1, "time": "1", "type": 1, "actions": []map[string]any{{"type": "stop"}}},
		{"instanceUuid": inst.InstanceUuid, "name": "x2", "count": 1, "time": "not-cron", "type": 2, "actions": []map[string]any{{"type": "stop"}}},
		{"instanceUuid": inst.InstanceUuid, "name": "x3", "count": 1, "time": "2026-11-15 08:13:50", "type": 3, "actions": []map[string]any{{"type": "boom"}}},
		{"instanceUuid": "no-such-instance", "name": "x4", "count": 1, "time": "10", "type": 1, "actions": []map[string]any{{"type": "stop"}}},
	}
	for i, bad := range badCases {
		if status, _ := c.requestRaw("schedule/register", fmt.Sprintf("bad%d", i), bad); status != 500 {
			t.Fatalf("非法任务 %d 应被拒绝，实际 status=%d", i, status)
		}
	}

	// 删除
	if status, data := c.requestRaw("schedule/delete", "d1", map[string]any{
		"instanceUuid": inst.InstanceUuid, "name": "111",
	}); status != 200 {
		t.Fatalf("删除失败: %d %s", status, data)
	}
	if status, data := c.requestRaw("schedule/list", "l3", map[string]any{"instanceUuid": inst.InstanceUuid}); status != 200 {
		t.Fatalf("列表失败: %d %s", status, data)
	} else {
		var list3 []map[string]any
		_ = json.Unmarshal(data, &list3)
		if len(list3) != 0 {
			t.Fatalf("删除后应为空: %s", data)
		}
	}

	// 持久化文件存在
	if _, err := os.Stat(filepath.Join(d.DataDir, "compat-mcsm-schedules.json")); err != nil {
		t.Fatalf("任务表应落盘: %v", err)
	}
}

// TestMCMScheduleIntervalFires 间隔任务按秒触发（type=1，time 秒）。
func TestMCMScheduleIntervalFires(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)

	cwd, err := os.MkdirTemp("", "irix-sched-fire-")
	if err != nil {
		t.Fatal(err)
	}
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

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "s0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	// 每 3 秒强杀一次，count=1 → 执行后任务自动删除
	status, data := c.requestRaw("schedule/register", "r1", map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"name":         "kill-once",
		"count":        1,
		"time":         "3",
		"type":         1,
		"actions":      []map[string]any{{"type": "kill", "payload": ""}},
	})
	if status != 200 {
		t.Fatalf("注册失败: %d %s", status, data)
	}

	// 直接驱动调度（不必等真实 3 秒），再验证任务被执行且被清理
	d.compatMC.tickSchedules()
	deadline := time.Now().Add(8 * time.Second)
	for {
		inst.mu.Lock()
		running := inst.Proc != nil && inst.Proc.IsRunning()
		inst.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("间隔任务应强杀实例")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// count=1 → 执行后任务应被删除
	var list []map[string]any
	_, raw := c.requestRaw("schedule/list", "l1", map[string]any{"instanceUuid": inst.InstanceUuid})
	_ = json.Unmarshal(raw, &list)
	if len(list) != 0 {
		t.Fatalf("count=1 的任务执行后应自动删除: %s", raw)
	}
}

// TestMCMScheduleSpecifyTime 指定时间任务（type=3）到点执行一次。
func TestMCMScheduleSpecifyTime(t *testing.T) {
	d, _ := newTestDaemon(t)
	base := setupTestMCSM(t, d)

	cwd, _ := os.MkdirTemp("", "irix-sched-at-")
	t.Cleanup(func() { _ = os.RemoveAll(cwd) })
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}

	c := newEIOClient(t, base+"/socket.io/")
	c.connect()
	if status, data := c.requestRaw("auth", "s0", mcsmTestKey); status != 200 {
		t.Fatalf("认证失败: %d %s", status, data)
	}
	past := time.Now().Add(-2 * time.Second).Format("2006-01-02 15:04:05")
	if status, data := c.requestRaw("schedule/register", "r1", map[string]any{
		"instanceUuid": inst.InstanceUuid,
		"name":         "at-once",
		"count":        -1,
		"time":         past,
		"type":         3,
		"actions":      []map[string]any{{"type": "start", "payload": ""}},
	}); status != 200 {
		t.Fatalf("注册指定时间任务失败: %d %s", status, data)
	}

	d.compatMC.tickSchedules()
	deadline := time.Now().Add(5 * time.Second)
	for {
		inst.mu.Lock()
		status := inst.Status
		inst.mu.Unlock()
		if status != StatusStopped {
			break // 已开始启动（start 动作生效）
		}
		if time.Now().After(deadline) {
			t.Fatal("指定时间任务未执行 start 动作")
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Cleanup(func() { stopTestProc(t, inst) })

	// 指定时间任务只跑一次：任务应被移除
	var list []map[string]any
	_, raw := c.requestRaw("schedule/list", "l1", map[string]any{"instanceUuid": inst.InstanceUuid})
	_ = json.Unmarshal(raw, &list)
	if len(list) != 0 {
		t.Fatalf("指定时间任务执行后应移除: %s", raw)
	}
}
