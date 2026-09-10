// 系统信息采集：内存、运行时间、CPU 使用率。
// 平台相关的采集逻辑拆到 sysinfo_windows.go / sysinfo_linux.go /
// sysinfo_freebsd.go / sysinfo_other.go，本文件只保留通用逻辑。

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// ---------------------------------------------------------------------------
// 系统总体 CPU 使用率缓存（后台采样，供 /api/overview 复用，避免每请求 sleep）
// ---------------------------------------------------------------------------

// sysCPUCache 全局系统 CPU 使用率缓存（后台采样）。
var sysCPUCache = struct {
	mu    sync.Mutex
	usage float64
	valid bool
}{}

var sysCPUOnce sync.Once

// startSysCPULoop 后台采样系统 CPU 使用率：周期读取 /proc/stat 两次取差值，
// 间隔约 1 秒（比单次 500ms 略宽，但不再阻塞 HTTP 请求线程）。非 Linux 平台
// 无 /proc/stat，循环直接置 0 并退出（overview 回退到 0）。
func startSysCPULoop() {
	if runtime.GOOS != "linux" {
		return
	}
	const interval = 1 * time.Second
	for {
		prev := cpuJiffies()
		time.Sleep(interval)
		curr := cpuJiffies()
		if curr < prev {
			continue
		}
		// /proc/stat 第一行 cpu 为各态 jiffies 总和；空闲近似为采样间隔×核数，
		// busy = (curr-prev) - idle，usage = busy/(curr-prev)。
		busy := curr - prev
		idle := interval.Seconds() * float64(runtime.NumCPU())
		usage := busy / (busy + idle)
		if usage > 1 {
			usage = 1
		}
		sysCPUCache.mu.Lock()
		sysCPUCache.usage = usage
		sysCPUCache.valid = true
		sysCPUCache.mu.Unlock()
	}
}

// cachedSysCPUUsage 返回后台采样的系统 CPU 使用率（0-1，惰性启动采样）。
// 首次调用时尚无缓存（后台刚启动）则回退到 0；后续请求即拿到最近一次采样值。
func cachedSysCPUUsage() float64 {
	sysCPUOnce.Do(func() { go startSysCPULoop() })
	sysCPUCache.mu.Lock()
	defer sysCPUCache.mu.Unlock()
	return sysCPUCache.usage
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
// 注意：runtime.ReadMemStats 是 stop-the-world 操作，高频调用会反复暂停
// 世界（堆越大代价越高）。请求路径请用带缓存的 processAllocCached。
func processAlloc() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Alloc
}

// allocCacheTTL 进程堆内存缓存时长（仪表盘展示无需亚秒精度）。
const allocCacheTTL = time.Second

// allocCache 进程堆内存缓存（避免 /api/overview 每次请求都 STW 一次）。
var allocCache = struct {
	mu sync.Mutex
	v  uint64
	at time.Time
}{}

// processAllocCached 返回缓存的进程堆内存（字节），一秒内复用同一次采样。
func processAllocCached() uint64 {
	now := time.Now()
	allocCache.mu.Lock()
	if now.Sub(allocCache.at) < allocCacheTTL {
		v := allocCache.v
		allocCache.mu.Unlock()
		return v
	}
	allocCache.mu.Unlock()
	v := processAlloc() // 采样不持锁：STW 期间不应堵住其他请求
	allocCache.mu.Lock()
	allocCache.v, allocCache.at = v, now
	allocCache.mu.Unlock()
	return v
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

// ---------------------------------------------------------------------------
// /api/overview 高频轮询缓存：静态信息 / 内存 / 磁盘
// ---------------------------------------------------------------------------
//
// /api/overview 是面板轮询最频繁的接口（秒级），此前每次请求都要：
//   - 读磁盘取版本信息（Windows 上 osVersion 读 C:/Windows/System32/Release.txt，
//     Linux 上 osDistro/osTypePlatform 各读一次 /etc/os-release）；
//   - 调三次 systemMem()（totalMem / freeMem / memUsage 各自采样一次）；
//   - 每次 statfs 查磁盘容量。
// 这些值要么进程生命周期内恒定，要么秒级内无意义地重复采样，全部改为缓存。

// hostStatic 进程生命周期内不变的系统静态信息（首次访问时采集一次）。
var hostStatic = struct {
	once     sync.Once
	hostname string
	osType   string
	platform string
	release  string
	distro   string
}{}

// hostStaticInfo 返回缓存的系统静态信息（主机名/系统类型/平台/内核版本/发行版）。
func hostStaticInfo() (hostname, osType, platform, release, distro string) {
	hostStatic.once.Do(func() {
		hostStatic.hostname, _ = os.Hostname()
		hostStatic.osType, hostStatic.platform, hostStatic.release = hostInfo()
		hostStatic.distro = osDistro()
	})
	return hostStatic.hostname, hostStatic.osType, hostStatic.platform, hostStatic.release, hostStatic.distro
}

// memSnapshotTTL 内存快照缓存时长（仪表盘秒级刷新无需更精确）。
const memSnapshotTTL = 2 * time.Second

// memCache 系统内存快照缓存（总/可用），供一次请求内多处复用。
var memCache = struct {
	mu    sync.Mutex
	total uint64
	free  uint64
	at    time.Time
}{}

// memSnapshot 返回系统内存快照（总字节、可用字节、使用率 0-1）。
// 合并原先 totalMem/freeMem/memUsage 各自采样（一次请求三遍 /proc/meminfo）
// 为一次采样三处复用。
func memSnapshot() (total, free uint64, usage float64) {
	now := time.Now()
	memCache.mu.Lock()
	if now.Sub(memCache.at) < memSnapshotTTL {
		t, f := memCache.total, memCache.free
		memCache.mu.Unlock()
		return t, f, memUsageOf(t, f)
	}
	// 采样本身不持锁：osMem 可能读文件/调系统调用，避免拉长临界区
	memCache.mu.Unlock()
	t, f := systemMem()
	memCache.mu.Lock()
	memCache.total, memCache.free, memCache.at = t, f, now
	memCache.mu.Unlock()
	return t, f, memUsageOf(t, f)
}

// memUsageOf 由总内存与可用内存计算使用率（0-1）。
func memUsageOf(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total)
}

// diskSnapshotTTL 磁盘容量缓存时长（容量变化以分钟计，无需每次 statfs）。
const diskSnapshotTTL = 5 * time.Second

// diskCacheStat 磁盘容量缓存（按路径键缓存，通常只有数据目录一个键）。
var diskCacheStat = struct {
	mu          sync.Mutex
	path        string
	total, used uint64
	usage       float64
	at          time.Time
}{}

// cachedDiskInfo 返回路径所在文件系统的容量信息（带 TTL 缓存）。
func cachedDiskInfo(path string) (total, used uint64, usage float64) {
	now := time.Now()
	diskCacheStat.mu.Lock()
	if diskCacheStat.path == path && now.Sub(diskCacheStat.at) < diskSnapshotTTL {
		t, u, g := diskCacheStat.total, diskCacheStat.used, diskCacheStat.usage
		diskCacheStat.mu.Unlock()
		return t, u, g
	}
	diskCacheStat.mu.Unlock()
	t, u, g := diskInfo(path)
	diskCacheStat.mu.Lock()
	diskCacheStat.path, diskCacheStat.total, diskCacheStat.used = path, t, u
	diskCacheStat.usage, diskCacheStat.at = g, now
	diskCacheStat.mu.Unlock()
	return t, u, g
}
