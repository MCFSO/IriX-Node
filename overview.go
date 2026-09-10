// 概览 API：系统信息、进程信息、守护进程列表。
// 响应结构对齐 MCSManager（见 apis/api_dashboard.md 与 apis/get_apikey.md）。

package main

import (
	"net/http"
	"runtime"
	"time"
)

// handleOverview 获取节点概览。
// GET /api/overview
func (d *Daemon) handleOverview(w http.ResponseWriter, r *http.Request) {
	// 系统静态信息（主机名/系统类型/平台/内核版本/发行版）进程内只采集一次：
	// 此前每次请求都要读磁盘取版本信息（Windows 读 C:/Windows/System32/Release.txt，
	// Linux 各读一次 /etc/os-release），而这些值运行期内恒定不变。
	hostname, osType, platform, release, distro := hostStaticInfo()

	now := time.Now()
	// 磁盘容量与内存均带短 TTL 缓存：仪表盘秒级刷新无需每次 statfs /
	// 重复采样（原先 totalMem/freeMem/memUsage 各自采样一次，共三遍）。
	diskTotal, diskUsed, diskUsage := cachedDiskInfo(d.DataDir)
	memTotal, memFree, memUse := memSnapshot()
	// 网络速率与系统 CPU 使用率复用后台采样缓存（cachedNetRates /
	// cachedSysCPUUsage），避免每请求同步 sleep 数百毫秒——对慢速
	// 顺序核（MIPS 路由器、老 ARM）减负明显，仪表盘轮询不再占用 CPU。
	netDown, netUp := cachedNetRates()
	system := map[string]any{
		"type":            osType,
		"hostname":        hostname,
		"platform":        platform,
		"release":         release,
		"version":         distro, // 发行版版本号（如 "22.04"）；空则由应用侧回退 release
		"uptime":          uptimeSeconds(),
		"totalmem":        memTotal,
		"freemem":         memFree,
		"cpuUsage":        cachedSysCPUUsage(),
		"memUsage":        memUse,
		"diskusage":       diskUsage,
		"disktotal":       diskTotal,
		"diskused":        diskUsed,
		"networkDownload": netDown,
		"networkUpload":   netUp,
		"processCpu":      0,
		"processMem":      0,
		"node":            runtime.Version(),
		"time":            now.UnixMilli(),
		"cwd":             d.DataDir,
	}

	processInfo := map[string]any{
		"cpu":    0,
		"memory": processAllocCached(), // 缓存版：ReadMemStats 会 STW，不能每请求调用
		"cwd":    d.DataDir,
	}

	instances := d.List()
	running := d.CountRunning()
	// vault 启用且未解锁/未初始化时脱敏：不泄露实例数量与运行状态
	// （docs/vault-design.md §7.3，/api/overview 为门禁豁免路径）。
	if d.vault != nil && d.vault.enabled && !d.vault.unlockedSafe() {
		instances = nil
		running = 0
	}

	writeOK(w, map[string]any{
		"version":                Version,
		"specifiedDaemonVersion": Version,
		"process":                processInfo,
		"record": map[string]any{
			"logined":       0,
			"illegalAccess": 0,
			"banips":        0,
			"loginFailed":   0,
		},
		"system": system,
		"chart": map[string]any{
			"system":  []any{},
			"request": []any{},
		},
		"remoteCount": map[string]any{
			"available": 1,
			"total":     1,
		},
		"remote": []map[string]any{
			{
				"version": Version,
				"process": processInfo,
				"instance": map[string]any{
					"running": running,
					"total":   len(instances),
				},
				"system":      system,
				"cpuMemChart": []any{},
				"uuid":        d.UUID,
				"ip":          "127.0.0.1",
				"port":        d.Port,
				"prefix":      "",
				"available":   true,
				"remarks":     "本地节点",
			},
		},
	})
}
