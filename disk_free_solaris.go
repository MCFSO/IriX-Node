//go:build solaris || illumos

// 磁盘余量探测（golang.org/x/sys/unix.Statvfs，Solaris/illumos 标准接口；
// 标准库 syscall 未暴露 statvfs，故用 x/sys/unix）。
// statvfs 用 Frsize 作为基本块大小，Bavail 为可分配给非特权用户的可用块数。
// 探测失败或字段异常时返回 ok=false，迁移仅告警。

package main

import "golang.org/x/sys/unix"

// diskFreeBytes 返回 path 所在文件系统的可用字节数；平台不支持时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) {
	var st unix.Statvfs_t
	if err := unix.Statvfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Frsize), true
}
