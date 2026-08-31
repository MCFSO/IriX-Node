//go:build !openbsd

// 非 OpenBSD 平台：无 pledge(2)/unveil(2)（OpenBSD 专有，NetBSD/FreeBSD/Linux 均未移植）。
// 留空实现保持调用点统一；权限收敛依赖部署侧（systemd 硬化、降权运行等）。

package main

// restrictPrivileges 在非 OpenBSD 平台为空操作。
func restrictPrivileges(dataDir string) error {
	return nil
}
