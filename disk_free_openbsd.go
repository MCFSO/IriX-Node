//go:build openbsd

// disk_free_openbsd.go — OpenBSD 磁盘余量探测。
// OpenBSD 的 syscall.Statfs_t 使用 F_ 前缀字段（F_bsize/F_bavail），
// 与 linux/darwin/freebsd 的 Bsize/Bavail 不同，单独文件承载。

package main

import "syscall"

// diskFreeBytes 返回 path 所在文件系统的可用字节数；探测失败时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.F_bavail) * int64(st.F_bsize), true
}
