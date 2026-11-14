//go:build !linux && !android && !windows

// CPU 累计时间采样：其余平台暂无可靠的标准库接口。
//
// 返回 ok=false，调用方据此不上报 CPU（面板显示 0%，与改造前一致），
// 但不会影响其它指标与功能。
package sysinfo

// sysCPUTimes 其余平台暂不支持，返回 ok=false。
func sysCPUTimes() (busy, idle uint64, ok bool) {
	return 0, 0, false
}
