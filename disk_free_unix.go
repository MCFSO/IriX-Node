//go:build linux || darwin || freebsd

// disk_free_unix.go — Unix 磁盘余量探测（syscall.Statfs，标准库）。
// 这些平台的 Statfs_t 字段名为 Bsize/Bavail（OpenBSD 用 F_ 前缀，见
// disk_free_openbsd.go）。探测失败或字段异常时返回 ok=false，迁移仅告警。

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
