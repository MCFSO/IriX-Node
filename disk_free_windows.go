//go:build windows

// disk_free_windows.go — Windows 磁盘余量探测。
// 标准库 syscall 未暴露 GetDiskFreeSpaceEx，无法无依赖获取余量；
// 返回 ok=false，迁移仅告警（docs/vault-design.md §8.7 预检降级）。

package main

// diskFreeBytes 返回 path 所在卷的可用字节数；平台不支持时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) {
	return 0, false
}
