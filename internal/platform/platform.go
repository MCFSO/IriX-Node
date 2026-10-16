// Package platform 汇集与操作系统强相关的进程级适配：
//
//   - restrict_{openbsd,other}.go — OpenBSD pledge(2)/unveil(2) 权限自限制；
//   - rlimit_{unix,other}.go     — 启动时把 RLIMIT_NOFILE 软上限拉到硬上限（高并发必需）；
//   - signals_{default,js}.go    — 平台信号装配（忽略 SIGHUP、注册优雅关停信号）。
//
// 本包不反向依赖主包：需要输出日志时由主包在启动时通过 SetLogf 注入，
// 未注入时静默，便于本包独立编译与测试。

package platform

// logf 日志输出函数（主包注入；未注入时静默丢弃）。
var logf = func(format string, args ...any) {}

// SetLogf 注入日志输出函数，通常传主包的异步日志器（alog.Printf）。
// 传 nil 时忽略，保持默认的静默实现。
func SetLogf(f func(format string, args ...any)) {
	if f != nil {
		logf = f
	}
}
