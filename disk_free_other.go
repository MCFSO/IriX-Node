//go:build !windows

// disk_free_other.go — Unix 磁盘余量探测（syscall.Statfs，标准库）。
// 平台不支持 Statfs 时（如部分 BSD 变体）返回 ok=false，迁移仅告警。

package main

import "syscall"

// diskFreeBytes 返回 path 所在文件系统的可用字节数；平台不支持时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
