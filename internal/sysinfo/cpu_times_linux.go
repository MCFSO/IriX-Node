//go:build linux || android

// CPU 累计时间采样：Linux / Android（读 /proc/stat）。
//
// 返回 (忙碌时间, 空闲时间) 的累计值（jiffies）；两值在区间内的差值之比
// 即为该区间的 CPU 使用率（与 top / MCSManager 的算法一致）。
package sysinfo

import (
	"os"
	"strconv"
	"strings"
)

// sysCPUTimes 读取 /proc/stat 的 cpu 汇总行，返回 (busy, idle) 累计 jiffies。
// idle = idle + iowait；busy = user + nice + system + irq + softirq + steal。
// ok=false 表示读取失败（调用方退化为不上报 CPU）。
func sysCPUTimes() (busy, idle uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		vals := make([]uint64, 0, len(fields)-1)
		for _, f := range fields[1:] {
			n, _ := strconv.ParseUint(f, 10, 64)
			vals = append(vals, n)
		}
		// 字段顺序：user nice system idle iowait irq softirq steal guest guest_nice
		at := func(i int) uint64 {
			if i < len(vals) {
				return vals[i]
			}
			return 0
		}
		idle = at(3) + at(4)
		busy = at(0) + at(1) + at(2) + at(5) + at(6) + at(7)
		return busy, idle, true
	}
	return 0, 0, false
}

// cpuJiffies 读取 /proc/stat 首行 cpu 的各态 jiffies 总和。
//
// 供 ProcTicksPerSec（process_stats_linux.go）实测校准 USER_HZ 使用，
// 语义与 CPU 使用率采样无关，故单独保留。
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
