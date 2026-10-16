//go:build !windows

// 数据目录盘位选择（非 Windows）：无盘符/系统盘概念，恒保持原目录。
// Windows 实现见 datadir_windows.go。

package sysinfo

// SystemDrive 非 Windows 无系统盘概念，恒返回空串。
func SystemDrive() string { return "" }

// OnSystemDrive 非 Windows 恒返回 false。
func OnSystemDrive(path string) bool { return false }

// AvoidSystemDrive 非 Windows 恒返回原目录（未改判）。
func AvoidSystemDrive(dir string) (string, bool) { return dir, false }
