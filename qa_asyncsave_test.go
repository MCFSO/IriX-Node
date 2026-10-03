// 异步合并落盘（防抖 Save）语义测试：
//   - 热路径（Add/UpdateInstance/Remove）只改内存并置脏标记，不立即写盘；
//   - FlushDirty 把脏变更一次性落盘并清标记，无变更时空操作；
//   - saveLoop 在一个防抖窗口内自动落盘；
//   - StopAutoSave 后循环退出，FlushDirty 仍可手动兜底。
package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// waitDirtyFlushed 轮询等待 instances.json 出现（saveLoop 异步写盘）。
func waitDirtyFlushed(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "instances.json")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("防抖窗口内 instances.json 未落盘")
}

// 停止信号发出时写盘可能仍在进行，StopAutoSave 必须等待这次写入完成。
func TestAsyncSaveStopWaitsForInFlightWrite(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = time.Millisecond
	d.saveMu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(func() { d.saveMu.Unlock() }) }
	defer unlock()
	inst := NewInstance("", InstanceConfig{Nickname: "停止时写盘", Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	d.StartAutoSave()
	// 脏标记被消费说明 FlushDirty 已进入 Save；saveMu 让实际写入暂时阻塞。
	deadline := time.Now().Add(3 * time.Second)
	for d.dirty.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.dirty.Load() {
		t.Fatal("后台写盘没有开始")
	}
	stopped := make(chan struct{})
	go func() {
		d.StopAutoSave()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("写盘仍受阻时 StopAutoSave 已返回")
	case <-time.After(30 * time.Millisecond):
	}
	unlock()
	lifecycleWait(t, stopped)
	d.StartAutoSave() // 停止后重启为空操作
	d.StopAutoSave()
	restarted := NewDaemon(dir, "test-key")
	if err := restarted.Load(); err != nil || restarted.Find(inst.InstanceUuid) == nil {
		t.Fatalf("停止前已经开始的写入未完成: %v", err)
	}
}

func TestAsyncSaveDirtyAndFlush(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = 0 // 本用例不开后台循环，手动控制 flush

	inst := NewInstance("", InstanceConfig{Nickname: "async", Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if !d.dirty.Load() {
		t.Fatal("Add 后应有脏标记")
	}
	if _, err := os.Stat(d.instanceFile()); !os.IsNotExist(err) {
		t.Fatalf("打脏标记后不应立即写盘（instances.json 不应存在）: %v", err)
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty 失败: %v", err)
	}
	if d.dirty.Load() {
		t.Fatal("FlushDirty 后脏标记应被清除")
	}

	// 落盘后新实例可被重新加载（重启语义保持）
	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if d2.Find(inst.InstanceUuid) == nil {
		t.Fatal("FlushDirty 后重启应能找回新实例")
	}

	// 无变更时 FlushDirty 为空操作（不报错、不重复写盘）
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("无变更时 FlushDirty 应为空操作: %v", err)
	}
}

func TestAsyncSaveUpdateAndRemove(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = 0

	inst := NewInstance("", InstanceConfig{Nickname: "u", Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	_ = d.FlushDirty()

	// 更新：改昵称 → 打脏 → flush → 重启可见新昵称
	cfg := inst.Config
	cfg.Nickname = "u2"
	if err := d.UpdateInstance(inst, cfg); err != nil {
		t.Fatalf("UpdateInstance 失败: %v", err)
	}
	if !d.dirty.Load() {
		t.Fatal("Update 后应有脏标记")
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty 失败: %v", err)
	}
	d2 := NewDaemon(dir, "test-key")
	_ = d2.Load()
	if got := d2.Find(inst.InstanceUuid); got == nil || got.Config.Nickname != "u2" {
		t.Fatalf("重启后昵称应为 u2，实际 %+v", got)
	}

	// 删除：打脏 → flush → 重启后不存在
	if err := d.Remove(inst.InstanceUuid, false); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty 失败: %v", err)
	}
	d3 := NewDaemon(dir, "test-key")
	_ = d3.Load()
	if d3.Find(inst.InstanceUuid) != nil {
		t.Fatal("Remove + FlushDirty 后重启不应再找到该实例")
	}
}

func TestAsyncSaveLoopDebounce(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = 20 * time.Millisecond
	d.StartAutoSave()
	defer d.StopAutoSave()

	inst := NewInstance("", InstanceConfig{Nickname: "loop", Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	waitDirtyFlushed(t, dir)

	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatal(err)
	}
	if d2.Find(inst.InstanceUuid) == nil {
		t.Fatal("saveLoop 落盘后重启应能找回实例")
	}

	// StopAutoSave 后 saveLoop 退出，但 FlushDirty 仍可手动兜底
	d.StopAutoSave()
	if err := d.Add(NewInstance("", InstanceConfig{Nickname: "after-stop", Cwd: dir})); err != nil {
		t.Fatal(err)
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("StopAutoSave 后手动 FlushDirty 失败: %v", err)
	}
}
