//go:build !linux && !darwin && !freebsd && !dragonfly && !netbsd && !openbsd && !solaris && !illumos && !windows

// 其他平台的磁盘余量探测兜底：暂无移植的 statfs/statvfs 采集路径，
// 一律返回 ok=false。调用方（vault.go / vault_migrate.go）在 ok=false 时
// 仅跳过余量检查，不影响功能。覆盖 aix / plan9 / js / wasip1 等平台。

package main

// diskFreeBytes 返回 path 所在文件系统的可用字节数；平台不支持时 ok=false。
func diskFreeBytes(path string) (free int64, ok bool) { return 0, false }
