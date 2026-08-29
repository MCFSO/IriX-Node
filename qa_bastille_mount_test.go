package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 在临时目录构造一个 jail 的 fstab，并把 bastilleRoot 指向该临时根，
// 以便真实触发 bastilleFstabPath / bastilleJailRoot 等路径逻辑。
// 返回 jail 名与还原函数。
func setupTestJail(t *testing.T) (root, jailName string, restore func()) {
	t.Helper()
	tmp := t.TempDir()
	orig := bastilleRoot
	bastilleRoot = tmp
	jailName = "test50"
	jailDir := filepath.Join(tmp, "jails", jailName)
	if err := os.MkdirAll(filepath.Join(jailDir, "root"), 0o755); err != nil {
		t.Fatalf("创建 jail root 失败: %v", err)
	}
	return tmp, jailName, func() { bastilleRoot = orig }
}

// TestBastilleMountListFstabNormalize 验证：fstab 中 mountpoint 可能是
// 宿主绝对路径（jailRoot + jail内路径）或 jail 内路径；bastilleMountList
// 必须统一归一化为 jail 内路径，否则前端按 jail 内路径查找会找不到文件。
func TestBastilleMountListFstabNormalize(t *testing.T) {
	_, name, restore := setupTestJail(t)
	defer restore()

	jailRoot := bastilleJailRoot(name)
	// 模拟 bastille 写入 fstab 时使用宿主绝对路径（jailRoot + /data）
	hostData := filepath.Join(jailRoot, "data")
	fstab := "/data/src " + hostData + " nullfs rw 0 0\n" +
		"proc /proc procfs rw 0 0\n"
	if err := os.WriteFile(bastilleFstabPath(name), []byte(fstab), 0o644); err != nil {
		t.Fatalf("写 fstab 失败: %v", err)
	}

	items, err := bastilleMountListFstab(name)
	if err != nil {
		t.Fatalf("bastilleMountListFstab 失败: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("期望至少 2 条，实际 0 条")
	}
	// 第一条应为 nullfs，dst 归一化为 jail 内路径（不含 jailRoot 前缀）。
	// FreeBSD 下为 /data；Windows 测试环境下因路径分隔符为 \data，
	// 故断言以 data 结尾且不包含 jailRoot 前缀即可，平台无关。
	got := map[string]string{}
	for _, it := range items {
		got[it["fstype"].(string)] = it["dst"].(string)
	}
	dst := got["nullfs"]
	if strings.Contains(dst, jailRoot) || !strings.HasSuffix(dst, "data") {
		t.Errorf("nullfs dst 应归一化为 jail 内路径（不含 %q），实际: %q", jailRoot, dst)
	}
	_ = jailRoot
}

// TestBastilleFstabRemoveDualForm 验证：用户传入 jail 内路径 /data 时，
// 应能匹配并删除 fstab 中以宿主绝对路径（jailRoot+/data）写入的条目，
// 否则卸载后残留会导致下次挂载报「已存在」而再也挂不上。
func TestBastilleFstabRemoveDualForm(t *testing.T) {
	_, name, restore := setupTestJail(t)
	defer restore()

	jailRoot := bastilleJailRoot(name)
	hostMount := filepath.Join(jailRoot, "data")
	fstab := "/data/src " + hostMount + " nullfs rw 0 0\n"
	if err := os.WriteFile(bastilleFstabPath(name), []byte(fstab), 0o644); err != nil {
		t.Fatalf("写 fstab 失败: %v", err)
	}

	// 用户按 jail 内路径卸载
	removed := bastilleFstabRemove(name, "/data", "")
	if !removed {
		t.Fatalf("应按 jail 内路径 /data 匹配并删除宿主绝对路径条目")
	}
	entries, err := readFstab(name)
	if err != nil {
		t.Fatalf("读 fstab 失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("期望删除后 0 条，实际 %d 条", len(entries))
	}
}

// TestBastilleFstabRemoveJailPathForm 验证：fstab 中以 jail 内路径写入时，
// 按 jail 内路径也能正确删除。
func TestBastilleFstabRemoveJailPathForm(t *testing.T) {
	_, name, restore := setupTestJail(t)
	defer restore()

	fstab := "/data/src /data nullfs rw 0 0\n"
	if err := os.WriteFile(bastilleFstabPath(name), []byte(fstab), 0o644); err != nil {
		t.Fatalf("写 fstab 失败: %v", err)
	}
	if !bastilleFstabRemove(name, "/data", "") {
		t.Fatalf("按 jail 内路径 /data 应删除成功")
	}
}

// TestBastilleMountAddNullfsMkdirAll 的 freebsd-only 版本见
// qa_bastille_mount_freebsd_test.go（bastilleMountAdd 挂载逻辑在 freebsd 构建标签内）。
