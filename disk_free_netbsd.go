//go:build netbsd

// 磁盘余量探测（golang.org/x/sys/unix.Statvfs，NetBSD 标准接口）。
// 标准库 syscall 在 netbsd 下未提供可用的 Statfs_t（定义为 [0]byte），
// 故用 x/sys/unix 的 Statvfs_t：Bavail 为可分配给非特权用户的可用块数，
// Bsize 为文件系统块大小。探测失败时返回 ok=false，迁移仅告警。

package main

import "golang.org/x/sys/unix"

// diskFreeBytes 返回 path 所在文件系统的可用字节数；平台不支持时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) {
	var st unix.Statvfs_t
	if err := unix.Statvfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
