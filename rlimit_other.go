//go:build windows || openbsd

// Windows / OpenBSD 平台：无可移植的 RLIMIT_NOFILE 提升路径。
//
// Windows 无类 Unix 的 per-process fd 软/硬上限概念，连接数受非分页池与
// 端口范围制约，应用层无法直接调高（需部署侧调整 TCP 注册表参数）。
// OpenBSD 上 pledge(2) 已收敛 syscall 集，额外 Setrlimit 与权限模型
// 配合复杂，保持空操作交由部署侧 ulimit -n 控制。
// 留空实现保持调用点统一。

package main

// raiseFDLimit 在 Windows/OpenBSD 上为空操作，返回 0 与 nil。
func raiseFDLimit() (soft uint64, err error) {
	return 0, nil
}
