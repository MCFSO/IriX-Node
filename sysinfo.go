// 系统信息采集：内存、运行时间、CPU 使用率。
// 平台相关的采集逻辑拆到 sysinfo_windows.go / sysinfo_linux.go /
// sysinfo_freebsd.go / sysinfo_other.go，本文件只保留通用逻辑。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// uptimeSeconds 系统运行秒数。
func uptimeSeconds() float64 {
	return osUptime()
}

// systemMem 返回总内存与可用内存（字节）。
func systemMem() (total, free uint64) {
	return osMem()
}

// totalMem 总内存（字节）。
func totalMem() uint64 {
	total, _ := systemMem()
	return total
}

// freeMem 可用内存（字节）。
func freeMem() uint64 {
	_, free := systemMem()
	return free
}

// memUsage 内存使用率 (0-1)。
func memUsage() float64 {
	total, free := systemMem()
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total)
}

// cpuUsage CPU 使用率（Linux 下采样 500ms 的近似值，其他平台返回 0）。
func cpuUsage() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	prev := cpuJiffies()
	if prev == 0 {
		return 0
	}
	time.Sleep(500 * time.Millisecond)
	curr := cpuJiffies()
	if curr < prev {
		return 0
	}
	idle := 500.0 * float64(runtime.NumCPU())
	return (curr - prev) / (curr - prev + idle)
}

// cpuJiffies 读取 /proc/stat 的 CPU 忙碌 jiffies。
func cpuJiffies() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.Fields(string(data))
	if len(line) < 5 || line[0] != "cpu" {
		return 0
	}
	var total uint64
	for _, v := range line[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
	}
	return float64(total)
}

// diskInfo 返回路径所在文件系统的 (总容量, 已用, 使用率 0-1)。
// 路径通常取数据目录：实例文件都在该盘上，与 MCSM 的磁盘统计口径一致。
func diskInfo(path string) (total, used uint64, usage float64) {
	t, f := osDisk(path)
	if t == 0 {
		return 0, 0, 0
	}
	if t >= f {
		used = t - f
	}
	return t, used, float64(used) / float64(t)
}

// netRates 采样网络收发速率（字节/秒）。
// 两次读取网卡计数器、间隔约 300ms，差值除以实际耗时。
func netRates() (down, up float64) {
	rx1, tx1 := netCounters()
	start := time.Now()
	time.Sleep(300 * time.Millisecond)
	rx2, tx2 := netCounters()
	dt := time.Since(start).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	if rx2 >= rx1 {
		down = float64(rx2-rx1) / dt
	}
	if tx2 >= tx1 {
		up = float64(tx2-tx1) / dt
	}
	return down, up
}

// parseNetstatIB 解析 netstat -ib 输出（FreeBSD/OpenBSD/macOS 通用）：
// 从表头定位 Ibytes / Obytes 列，累加所有非回环接口的收发字节。
func parseNetstatIB(out string) (rx, tx uint64) {
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	headers := strings.Fields(lines[0])
	iRx, iTx := -1, -1
	for i, h := range headers {
		switch h {
		case "Ibytes":
			iRx = i
		case "Obytes":
			iTx = i
		}
	}
	if iRx < 0 || iTx < 0 {
		return 0, 0
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) <= iTx || fields[0] == "Name" {
			continue
		}
		if strings.HasPrefix(fields[0], "lo") {
			continue
		}
		r, _ := strconv.ParseUint(fields[iRx], 10, 64)
		t, _ := strconv.ParseUint(fields[iTx], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// processAlloc 当前进程堆内存占用（字节）。
func processAlloc() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Alloc
}

// hostInfo 返回 (系统类型, 平台, 发行版本)。
func hostInfo() (osType, platform, release string) {
	if runtime.GOOS == "windows" {
		return "Windows_NT", "win32", osVersion()
	}
	// 非 Windows 由平台相关函数给出 osType/platform（如 OpenHarmony 区别于 linux），
	// release 统一用 osVersion()。
	p := osTypePlatform()
	return p, p, osVersion()
}
