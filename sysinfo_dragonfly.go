//go:build dragonfly

// DragonFly BSD 磁盘容量采集（statfs，标准库 syscall）。
// DragonFly 的 Statfs_t 字段与 FreeBSD 同名（Bsize/Bavail），
// 与 sysinfo_bsd.go 的其余采集（sysctl/netstat）配套使用。

package main

import "syscall"

// osDisk 返回路径所在文件系统总容量与可用容量（字节）。
// 注意 DragonFly 的 Statfs_t 字段为有符号 int64（与 FreeBSD 一致，OpenBSD
// 则为无符号 F_ 前缀），统一显式转换，非负值下安全。
func osDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize)
}
