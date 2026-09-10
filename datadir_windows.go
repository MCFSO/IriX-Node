//go:build windows

// 数据目录盘位选择（Windows）：避免节点把实例文件、日志、账户库与保险库
// 堆到系统盘（通常 C:）。
//
// 背景：数据目录未显式指定时取进程当前工作目录（main.go），从系统盘启动
// 就会把全部运行时数据写进系统盘——系统盘通常是 SSD 且容量紧张，实例世界
// 文件与日志增长快，容易把系统盘填满导致系统异常。这里在系统盘上启动时
// 自动改用可用空间最大的非系统固定盘（<盘>:\IriX-Node-Data）；显式 -data
// 落在系统盘时不擅自改路径，只在启动日志告警。
//
// 其他平台无盘符概念，见 datadir_other.go（恒返回原目录）。

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	ddKernel32  = syscall.NewLazyDLL("kernel32.dll")
	ddLogical   = ddKernel32.NewProc("GetLogicalDrives")
	ddDriveType = ddKernel32.NewProc("GetDriveTypeW")
	ddDiskFree  = ddKernel32.NewProc("GetDiskFreeSpaceExW")
)

// driveFixed GetDriveTypeW 的 DRIVE_FIXED（固定磁盘）。
const driveFixed = 3

// systemDrive 返回 Windows 系统盘盘符（如 "C:"）；无法确定时返回空串。
// 优先读 SystemDrive 环境变量，回退 SystemRoot（如 C:\Windows）的盘符。
func systemDrive() string {
	if v := strings.TrimSpace(os.Getenv("SystemDrive")); v != "" {
		if vol := filepath.VolumeName(v + `\`); vol != "" {
			return strings.ToUpper(vol)
		}
	}
	if root := strings.TrimSpace(os.Getenv("SystemRoot")); root != "" {
		if vol := filepath.VolumeName(root); vol != "" {
			return strings.ToUpper(vol)
		}
	}
	return ""
}

// fixedDrives 枚举固定磁盘盘符（如 ["C:", "D:"]），排除 exclude 指定的盘。
func fixedDrives(exclude string) []string {
	mask, _, _ := ddLogical.Call()
	if mask == 0 {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		vol := string(rune('A'+i)) + ":"
		if strings.EqualFold(vol, exclude) {
			continue
		}
		p, err := syscall.UTF16PtrFromString(vol + `\`)
		if err != nil {
			continue
		}
		t, _, _ := ddDriveType.Call(uintptr(unsafe.Pointer(p)))
		if uint32(t) != driveFixed {
			continue // 光驱/网络驱动器/可移动设备不用于存放节点数据
		}
		out = append(out, vol)
	}
	return out
}

// driveFreeBytes 返回卷的可用字节数（GetDiskFreeSpaceExW）；失败返回 0。
func driveFreeBytes(vol string) uint64 {
	p, err := syscall.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return 0
	}
	var avail, total, totalFree uint64
	r, _, _ := ddDiskFree.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0
	}
	return avail
}

// onSystemDrive 判断路径是否位于系统盘（盘符相同即视为是）。
func onSystemDrive(path string) bool {
	sys := systemDrive()
	if sys == "" {
		return false
	}
	return strings.EqualFold(filepath.VolumeName(path), sys)
}

// avoidSystemDrive 数据目录避开系统盘：dir 不在系统盘时原样返回；
// 在系统盘时改用可用空间最大的非系统固定盘下的 IriX-Node-Data。
// 返回 (选定目录, 是否发生了改判)；无非系统固定盘可用时保持原目录。
func avoidSystemDrive(dir string) (string, bool) {
	if !onSystemDrive(dir) {
		return dir, false
	}
	var (
		best     string
		bestFree uint64
	)
	for _, vol := range fixedDrives(systemDrive()) {
		if free := driveFreeBytes(vol); free > bestFree {
			best, bestFree = vol, free
		}
	}
	if best == "" {
		return dir, false // 只有系统盘可用（如单盘机器），保持原行为
	}
	return filepath.Join(best+`\`, "IriX-Node-Data"), true
}
