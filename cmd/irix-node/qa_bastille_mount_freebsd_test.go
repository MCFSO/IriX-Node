//go:build freebsd

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBastilleMountAddNullfsMkdirAll 验证：nullfs 挂载前确保 jail 内挂载点
// 目录存在（卸载后目录可能消失，再次挂载会因 mountpoint 不存在而失败）。
// 仅在 FreeBSD 下编译运行（bastilleMountAdd 实际挂载逻辑在 freebsd 构建标签内）。
func TestBastilleMountAddNullfsMkdirAll(t *testing.T) {
	_, name, restore := setupTestJail(t)
	defer restore()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("创建源目录失败: %v", err)
	}
	// 挂载点目录初始不存在
	dst := "/data"
	hostPath := filepath.Join(bastilleJailRoot(name), "data")
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Fatalf("前置条件失败：挂载点不应存在")
	}

	// bastilleMountAdd 内部会先 MkdirAll(hostPath) 再调 cliRun(bastille mount)；
	// 若 bastille 命令缺失则 cliRun 失败，但挂载点目录应已被创建（MkdirAll 兜底）。
	_ = bastilleMountAdd(name, src, dst, "nullfs", "rw")

	if _, err := os.Stat(hostPath); err != nil {
		t.Errorf("nullfs 挂载分支应预先创建挂载点目录 %s: %v", hostPath, err)
	}
}
