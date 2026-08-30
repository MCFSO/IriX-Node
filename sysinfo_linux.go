//go:build linux

// Linux 系统信息采集（解析 /proc）。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// osUptime 系统运行秒数。
func osUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// osDisk 返回路径所在文件系统总容量与可用容量（字节，statfs）。
func osDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)
}

// netCounters 读取网络收发总字节数（/proc/net/dev，不含回环接口）。
func netCounters() (rx, tx uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		if strings.TrimSpace(line[:idx]) == "lo" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// osDistro 发行版版本号（/etc/os-release 的 VERSION_ID，如 "22.04"；不可得返回空串）。
func osDistro() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VERSION_ID=") {
			return strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"' \t\r\n")
		}
	}
	return ""
}

// osReleaseName 读取 /etc/os-release 的 NAME（如 "OpenHarmony"、"Ubuntu"）。
// 同时匹配 ID= 行，任一命中即返回，用于区分发行版/系统类型。
// 文件缺失或无对应字段时返回空串。
func osReleaseName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	name := ""
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "NAME="):
			name = strings.Trim(strings.TrimPrefix(line, "NAME="), "\"' \t\r\n")
		case strings.HasPrefix(line, "ID="):
			if id := strings.Trim(strings.TrimPrefix(line, "ID="), "\"' \t\r\n"); id != "" && name == "" {
				name = id
			}
		}
	}
	return name
}

// detectOpenHarmony 判断当前 Linux 用户态是否为 OpenHarmony（鸿蒙原生）。
// OpenHarmony 标准系统内核为 Linux，runtime.GOOS=="linux"，但 /etc/os-release 的
// NAME/ID 含 "OpenHarmony"/"openharmony"。识别失败返回 false，回退普通 Linux。
func detectOpenHarmony() bool {
	name := osReleaseName()
	if name == "" {
		return false
	}
	low := strings.ToLower(name)
	return low == "openharmony" || strings.Contains(low, "openharmony")
}

// osTypePlatform 返回用于 hostInfo 的系统类型/平台标识：OpenHarmony 返回
// "OpenHarmony"，否则回退 "linux"。供 GET /api/overview 区分鸿蒙设备与普通 Linux。
func osTypePlatform() string {
	if detectOpenHarmony() {
		return "OpenHarmony"
	}
	return "linux"
}

// osMem 返回总内存与可用内存（字节）。
func osMem() (total, free uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			free = v * 1024
		}
	}
	return total, free
}

// osVersion 读取 Linux 内核版本。
func osVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return runtime.GOARCH
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return runtime.GOARCH
}
