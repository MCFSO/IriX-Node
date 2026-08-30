// 实例级运行指标（docs/irix-node-local-parity.md §4.3）。
//
// GET /api/instance/stats?uuid&daemonId
//
// 响应 data:
//   pid, cpuPercent, memoryMb, networkDownloadBps, networkUploadBps,
//   uptimeSec（进程级）；players/maxPlayers/tps 从服务器输出解析，
//   解析失败时省略该字段（客户端显示「—」）。
//
// 平台差异：CPU/内存采样拆到 process_stats_{linux,windows,other}.go；
// 网络速率为节点全局网卡速率缓存（后台采样，Linux 2s / 其他平台 5s）。

package main

import (
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// instanceStats 从服务器输出解析的运行指标（受 Process.statsMu 保护）。
type instanceStats struct {
	players    int     // 当前在线玩家数（-1 = 未知）
	maxPlayers int     // 最大玩家数（-1 = 未知）
	tps        float64 // 最近 TPS（-1 = 未知）
}

// 服务器输出解析模式（与客户端 jar_metadata 对齐的启发式）。
var (
	rePlayersVanilla = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online`)
	rePlayersSimple  = regexp.MustCompile(`^.*?\b(\d+) players online:`)
	reTPSTriple      = regexp.MustCompile(`TPS from last 1m, 5m, 15m: ([0-9]+(?:\.[0-9]+)?)`)
	reTPSSimple      = regexp.MustCompile(`\bTPS: ([0-9]+(?:\.[0-9]+)?)`)
)

// parseServerLine 从一行服务器输出解析玩家数/TPS（启发式，失败静默）。
// 性能：MC 服务器日志绝大多数行与玩家/TPS 无关，先用 strings.Contains 廉价
// 守门（O(n) 无回溯），仅在命中关键词时才跑对应正则（回溯 VM，慢核上成本高），
// 避免对每条日志行做 4 次正则回溯。
func (p *Process) parseServerLine(line string) {
	var players, maxPlayers int
	var tps float64
	gotPlayers, gotTPS := false, false
	// 玩家数正则只需在无 "players" 子串时跳过；大小写不敏感匹配（日志常含 "Players"）。
	if strings.Contains(strings.ToLower(line), "players") {
		if m := rePlayersVanilla.FindStringSubmatch(line); m != nil {
			players, _ = strconv.Atoi(m[1])
			maxPlayers, _ = strconv.Atoi(m[2])
			gotPlayers = true
		} else if m := rePlayersSimple.FindStringSubmatch(line); m != nil {
			players, _ = strconv.Atoi(m[1])
			maxPlayers = -1
			gotPlayers = true
		}
	}
	// TPS 正则只需在无 "tps" 子串时跳过。
	if strings.Contains(strings.ToLower(line), "tps") {
		if m := reTPSTriple.FindStringSubmatch(line); m != nil {
			tps, _ = strconv.ParseFloat(m[1], 64)
			gotTPS = true
		} else if m := reTPSSimple.FindStringSubmatch(line); m != nil {
			tps, _ = strconv.ParseFloat(m[1], 64)
			gotTPS = true
		}
	}
	if !gotPlayers && !gotTPS {
		return
	}
	p.statsMu.Lock()
	if gotPlayers {
		p.srvStats.players = players
		p.srvStats.maxPlayers = maxPlayers
	}
	if gotTPS {
		p.srvStats.tps = tps
	}
	p.statsMu.Unlock()
}

// statsSnapshot 返回已解析的玩家/TPS 指标（未解析为 -1）。
func (p *Process) statsSnapshot() (players, maxPlayers int, tps float64) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	return p.srvStats.players, p.srvStats.maxPlayers, p.srvStats.tps
}

// handleInstanceStats 获取实例运行指标。
// GET /api/instance/stats?uuid&daemonId
func (d *Daemon) handleInstanceStats(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()

	out := map[string]any{
		"pid":                0,
		"cpuPercent":         0,
		"memoryMb":           0,
		"networkDownloadBps": 0,
		"networkUploadBps":   0,
		"uptimeSec":          0,
	}
	if proc != nil && proc.IsRunning() {
		pid := proc.cmd.Process.Pid
		out["pid"] = pid
		out["uptimeSec"] = int64(time.Since(proc.started).Seconds())
		out["cpuPercent"] = round2(proc.sampleCPUPercent())
		out["memoryMb"] = procMemoryBytes(pid) >> 20
		down, up := cachedNetRates()
		out["networkDownloadBps"] = int64(down)
		out["networkUploadBps"] = int64(up)
		players, maxPlayers, tps := proc.statsSnapshot()
		if players >= 0 {
			out["players"] = players
		}
		if maxPlayers >= 0 {
			out["maxPlayers"] = maxPlayers
		}
		if tps >= 0 {
			out["tps"] = round2(tps)
		}
	}
	writeOK(w, out)
}

// round2 保留两位小数。
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ---------------------------------------------------------------------------
// CPU 采样（平台单位由 process_stats_*.go 提供）
// ---------------------------------------------------------------------------

// cpuSamplePoint 进程 CPU 采样点（单位：procTicksPerSec 的时钟滴答）。
type cpuSamplePoint struct {
	at  time.Time
	cpu uint64
}

// sampleCPUPercent 计算进程 CPU 使用率（0~100×核数归一化）。
// 两次采样间隔不足 500ms 时复用上次结果；首次采样仅建立基线返回 0。
func (p *Process) sampleCPUPercent() float64 {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	pid := p.cmd.Process.Pid
	now := time.Now()
	cpu, ok := procCPUTicks(pid)
	if !ok {
		return 0
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if p.cpuLast != nil && now.Sub(p.cpuLast.at) < 500*time.Millisecond {
		return p.cpuPercent
	}
	if p.cpuLast == nil || cpu < p.cpuLast.cpu {
		p.cpuLast = &cpuSamplePoint{at: now, cpu: cpu}
		p.cpuPercent = 0
		return 0
	}
	wall := now.Sub(p.cpuLast.at).Seconds()
	if wall <= 0 {
		return p.cpuPercent
	}
	percent := float64(cpu-p.cpuLast.cpu) / wall / procTicksPerSec() * 100
	percent /= float64(runtime.NumCPU()) // 归一化到单核 0~100
	p.cpuPercent = percent
	p.cpuLast = &cpuSamplePoint{at: now, cpu: cpu}
	return percent
}

// ---------------------------------------------------------------------------
// 节点网络速率缓存（实例指标接口复用，避免每请求 sleep 采样）
// ---------------------------------------------------------------------------

// netRateCache 全局网络速率缓存（后台采样）。
var netRateCache = struct {
	mu       sync.Mutex
	down, up float64
}{}

var netRateOnce sync.Once

// startNetRateLoop 后台采样网络速率：Linux 读 /proc 开销小用 2s；
// 其他平台 netstat 外部进程开销大用 5s。
func startNetRateLoop() {
	interval := 2 * time.Second
	if runtime.GOOS != "linux" {
		interval = 5 * time.Second
	}
	rx1, tx1 := netCounters()
	t1 := time.Now()
	for {
		time.Sleep(interval)
		rx2, tx2 := netCounters()
		t2 := time.Now()
		dt := t2.Sub(t1).Seconds()
		if dt > 0 {
			netRateCache.mu.Lock()
			if rx2 >= rx1 {
				netRateCache.down = float64(rx2-rx1) / dt
			}
			if tx2 >= tx1 {
				netRateCache.up = float64(tx2-tx1) / dt
			}
			netRateCache.mu.Unlock()
		}
		rx1, tx1, t1 = rx2, tx2, t2
	}
}

// cachedNetRates 返回缓存的网络收发速率（字节/秒，惰性启动后台采样）。
func cachedNetRates() (down, up float64) {
	netRateOnce.Do(func() { go startNetRateLoop() }) // 循环永不返回，必须 goroutine 启动
	netRateCache.mu.Lock()
	defer netRateCache.mu.Unlock()
	return netRateCache.down, netRateCache.up
}
