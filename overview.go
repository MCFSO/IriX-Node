// 概览 API：系统信息、进程信息、守护进程列表。
// 响应结构对齐 MCSManager（见 apis/api_dashboard.md 与 apis/get_apikey.md）。

package main

import (
	"net/http"
	"os"
	"runtime"
	"time"
)

// handleOverview 获取节点概览。
// GET /api/overview
func (d *Daemon) handleOverview(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	osType, platform, release := hostInfo()

	now := time.Now()
	diskTotal, diskUsed, diskUsage := diskInfo(d.DataDir)
	netDown, netUp := netRates()
	system := map[string]any{
		"type":            osType,
		"hostname":        hostname,
		"platform":        platform,
		"release":         release,
		"version":         osDistro(), // 发行版版本号（如 "22.04"）；空则由应用侧回退 release
		"uptime":          uptimeSeconds(),
		"totalmem":        totalMem(),
		"freemem":         freeMem(),
		"cpuUsage":        cpuUsage(),
		"memUsage":        memUsage(),
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
		"memory": processAlloc(),
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
