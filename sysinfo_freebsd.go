//go:build freebsd

// FreeBSD 磁盘采集（statfs）：Statfs_t 的 Bavail 为 int64，需显式转换。

package main

import "syscall"

// osDisk 返回路径所在文件系统总容量与可用容量（字节，statfs）。
func osDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = st.Blocks * uint64(st.Bsize)
	if st.Bavail > 0 {
		free = uint64(st.Bavail) * uint64(st.Bsize)
	}
	return total, free
}
