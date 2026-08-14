//go:build openbsd

// OpenBSD 磁盘采集（statfs）：Statfs_t 字段名为 Fblocks/Fbsize/Fbavail。

package main

import "syscall"

// osDisk 返回路径所在文件系统总容量与可用容量（字节，statfs）。
func osDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = st.F_blocks * uint64(st.F_bsize)
	if st.F_bavail > 0 {
		free = uint64(st.F_bavail) * uint64(st.F_bsize)
	}
	return total, free
}
