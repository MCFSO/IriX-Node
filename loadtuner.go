// 负载自适应调谐器：守护进程根据自身负载动态调整 Go 调度器与 GC 参数。
//
// 后台 goroutine 周期采样节点自身负载（进程 CPU 占比、goroutine 数、堆内存），
// 状态机在 idle / normal / busy 三态间切换（连续 N 次采样确认才切换，防抖动）：
//   - busy  ：GOMAXPROCS 恢复全部核，GOGC 调高（GC 频率低，吞吐优先）
//   - normal：GOMAXPROCS 全部核，GOGC 默认
//   - idle  ：GOMAXPROCS 降到 1，GOGC 调低（GC 积极，及时回收内存）
//
// 当前状态与采样经 GET /api/load 暴露（带认证）。

package main

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"
)

// loadTuneInterval 负载采样周期。
const loadTuneInterval = 5 * time.Second

// loadTuneStable 状态切换防抖：连续 N 次采样同候选状态才切换。
const loadTuneStable = 3

// 负载阈值（依据 CPU 占比 0-1 与 goroutine 数判定）。
const (
	busyCPU       = 0.60 // 进程 CPU 占比超过即视为高负载
	busyGoroutine = 2000 // goroutine 数超过即视为高负载
	idleCPU       = 0.05 // CPU 占比低于且 goroutine 少视为低负载
	idleGoroutine = 20
)

// 各状态下的 GOGC 参数。
const (
	busyGCPercent   = 400 // 高负载：GC 频率低，吞吐优先
	normalGCPercent = 100
	idleGCPercent   = 50 // 低负载：GC 积极，及时回收
)

// loadState 负载状态。
type loadState int

const (
	loadIdle loadState = iota
	loadNormal
	loadBusy
)

func (s loadState) String() string {
	switch s {
	case loadIdle:
		return "idle"
	case loadBusy:
		return "busy"
	}
	return "normal"
}

// evaluateLoad 纯判定函数：根据采样值给出候选负载状态（可单测，无副作用）。
func evaluateLoad(cpu float64, goroutines int) loadState {
	switch {
	case cpu >= busyCPU || goroutines >= busyGoroutine:
		return loadBusy
	case cpu <= idleCPU && goroutines <= idleGoroutine:
		return loadIdle
	}
	return loadNormal
}

// loadTuner 负载自适应调谐器。
type loadTuner struct {
	mu sync.Mutex

	state      loadState
	stateSince time.Time
	candidate  loadState // 当前候选切换状态
	stable     int       // 候选状态连续确认次数

	maxProcs  int // busy/normal 时的 GOMAXPROCS（= 启动时核数）
	gcPercent int // 当前 GOGC

	// memLimit 平台内存感知软上限（GOMEMLIMIT，Go 1.19+ 的 debug.SetMemoryLimit）。
	// 它是唯一在 GOGC=off 时仍生效的 GC 控制：本调谐器启动即 SetGCPercent(-1)
	// （GC 关），首轮状态切换前（最多约 15s）无 GC 兜底，memLimit 可在此空窗期
	// 防堆爆炸→OOM，对 64–256MB 的 MIPS 路由器/小内存开发板尤为关键。
	memLimit       uint64 // 0 = 未设（回退 Go 默认「无上限」）
	memLimitActive bool   // 是否由本调谐器主动设置

	// 最近采样（供 /api/load 展示）
	cpuBusy    float64 // 进程 CPU 占比（0-1）
	goroutines int
	heapAlloc  uint64
	numCPU     int
	startedAt  time.Time

	// applyFn 状态切换时执行的动作（实际实现改全局参数；测试注入假函数防副作用）
	applyFn func(loadState)
}

// newLoadTuner 创建调谐器（读当前 GOMAXPROCS/GOGC 作为基线）。
//
// 注意：此处**不能**关闭 GC（SetGCPercent(-1)）。此前实现在初始化即关 GC，
// 期望「首轮状态切换（约 15s）后由三态机接管」；但 tick() 在候选状态等于
// 当前状态时直接 return，不调用 applyFn——而初始状态就是 loadNormal，
// 节点长期处于 normal 区间（goroutine 21~1999、CPU 5%~60%，跑着实例的
// 节点几乎必然落在此区间）时状态永不切换，GC 也就永远不会被重新打开，
// 堆只增不减直至 OOM。因此这里只读取基线值、保持 GC 正常工作，
// 三态机仅在状态切换时调整 GOGC（低内存设备另有 GOMEMLIMIT 兜底）。
func newLoadTuner() *loadTuner {
	t := &loadTuner{
		state:      loadNormal,
		stateSince: time.Now(),
		maxProcs:   runtime.GOMAXPROCS(0),
		gcPercent:  currentGCPercent(),
		numCPU:     runtime.NumCPU(),
		startedAt:  time.Now(),
	}
	t.applyFn = t.applyReal
	t.initMemoryLimit()
	return t
}

// currentGCPercent 读取当前 GOGC 取值（只读，不修改运行时状态）。
// 指标不可用或取值异常（如 GOGC=off 时的极大值）时回退 normalGCPercent。
// 注：Go 1.24+ 的 metrics.Read 对单元素数组会返回 KindBad，必须使用与
// metrics.All() 对齐的完整样本数组（同 processCPUUsage 的处理）。
func currentGCPercent() int {
	const key = "/gc/gogc:percent"
	descs := metrics.All()
	samples := make([]metrics.Sample, len(descs))
	for i := range samples {
		samples[i].Name = descs[i].Name
	}
	metrics.Read(samples)
	for i, d := range descs {
		if d.Name != key || samples[i].Value.Kind() != metrics.KindUint64 {
			continue
		}
		v := int(samples[i].Value.Uint64())
		// GOGC=off 时该指标为极大值，视为不可用
		if v > 0 && v <= 1000 {
			return v
		}
		return normalGCPercent
	}
	return normalGCPercent
}

// initMemoryLimit 按平台内存容量设置 GOMEMLIMIT 软上限。
// 仅在嵌入式/低资源档位（自动检测或显式 -low-resource）下主动设限，
// 留出物理内存的 10% 余量给子进程与内核，避免 OOM。非嵌入式档位不设，
// 回退 Go 默认（无上限，由 GOGC 三态机控制）。
func (t *loadTuner) initMemoryLimit() {
	if !embedded.isEmbedded() {
		return
	}
	total, _ := systemMem()
	if total == 0 {
		return
	}
	// 留 10% 余量（至少 8MB），上限不超过物理内存。
	const headroom = 0.10
	limit := uint64(float64(total) * (1 - headroom))
	const minLimit = 8 * 1024 * 1024
	if limit < minLimit {
		limit = minLimit
	}
	debug.SetMemoryLimit(int64(limit))
	t.memLimit = limit
	t.memLimitActive = true
	alog.Printf("负载调谐：嵌入式档位已设内存软上限 GOMEMLIMIT=%dMB（物理内存 %dMB，留 10%% 余量）",
		limit>>20, total>>20)
}

// tuner 全局调谐器实例（main 启动 loop）。
var tuner = newLoadTuner()

// loop 周期采样与调整（后台 goroutine，随进程退出结束）。
func (t *loadTuner) loop() {
	for {
		time.Sleep(loadTuneInterval)
		t.tick()
	}
}

// tick 单轮采样、判定与状态机推进。
func (t *loadTuner) tick() {
	cpu, gos, heap := t.sample()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.cpuBusy, t.goroutines, t.heapAlloc = cpu, gos, heap

	cand := evaluateLoad(cpu, gos)
	if cand == t.state {
		t.candidate = t.state
		t.stable = 0
		return
	}
	if cand == t.candidate {
		t.stable++
	} else {
		t.candidate = cand
		t.stable = 1
	}
	if t.stable >= loadTuneStable {
		t.applyFn(cand)
	}
}

// applyReal 真正切换状态并应用调度器/GC 参数（调用方须持有 mu）。
func (t *loadTuner) applyReal(s loadState) {
	if s == t.state {
		return
	}
	t.state = s
	t.stateSince = time.Now()
	t.stable = 0
	t.candidate = s
	switch s {
	case loadBusy:
		runtime.GOMAXPROCS(t.maxProcs)
		t.gcPercent = busyGCPercent
	case loadNormal:
		runtime.GOMAXPROCS(t.maxProcs)
		t.gcPercent = normalGCPercent
	case loadIdle:
		runtime.GOMAXPROCS(1)
		t.gcPercent = idleGCPercent
	}
	debug.SetGCPercent(t.gcPercent)
	alog.Printf("负载调谐：状态切换为 %s（GOMAXPROCS=%d, GOGC=%d, CPU=%.0f%%, goroutine=%d）",
		s.String(), runtime.GOMAXPROCS(0), t.gcPercent, t.cpuBusy*100, t.goroutines)
}

// cpuSample 进程 CPU 时间采样点（两次调用差值换算占比）。
// 注意：Go 1.24+ 的 metrics.Read 对单元素数组会返回 KindBad，
// 必须使用与 metrics.All() 对齐的完整样本数组（遥测实现已实测验证）。
var cpuSample = struct {
	mu      sync.Mutex
	descs   []metrics.Description
	samples []metrics.Sample
	lastS   float64
	lastT   time.Time
}{}

// processCPUUsage 采样进程 CPU 占比（0-1）：
// 读取 runtime/metrics 的 /cpu/classes/total:cpu-seconds（Go 1.22+），
// 相邻两次采样的 CPU 秒差值 ÷ 墙钟间隔 ÷ GOMAXPROCS。
// 首次调用或指标不可用时返回 0。
func processCPUUsage() float64 {
	const key = "/cpu/classes/total:cpu-seconds"
	cpuSample.mu.Lock()
	defer cpuSample.mu.Unlock()

	if cpuSample.descs == nil {
		cpuSample.descs = metrics.All()
		cpuSample.samples = make([]metrics.Sample, len(cpuSample.descs))
		for i := range cpuSample.samples {
			cpuSample.samples[i].Name = cpuSample.descs[i].Name
		}
	}
	metrics.Read(cpuSample.samples)
	var cur float64
	found := false
	for i, d := range cpuSample.descs {
		if d.Name == key && cpuSample.samples[i].Value.Kind() == metrics.KindFloat64 {
			cur = cpuSample.samples[i].Value.Float64()
			found = true
			break
		}
	}
	if !found {
		return 0
	}
	now := time.Now()
	if cpuSample.lastT.IsZero() {
		cpuSample.lastS, cpuSample.lastT = cur, now
		return 0
	}
	dt := now.Sub(cpuSample.lastT).Seconds()
	dcpu := cur - cpuSample.lastS
	cpuSample.lastS, cpuSample.lastT = cur, now
	if dt <= 0 || dcpu <= 0 {
		return 0
	}
	usage := dcpu / dt / float64(runtime.GOMAXPROCS(0))
	if usage > 1 {
		usage = 1
	}
	return usage
}

// sample 采样一次负载：进程 CPU 占比、goroutine 数、堆内存。
func (t *loadTuner) sample() (float64, int, uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return processCPUUsage(), runtime.NumGoroutine(), ms.HeapAlloc
}

// handleLoad 当前负载状态与调谐参数。
// GET /api/load → {state, since, gomaxprocs, gcPercent, cpuBusy, goroutines, heapAlloc, numCPU, memLimit, memLimitActive}
func (d *Daemon) handleLoad(w http.ResponseWriter, r *http.Request) {
	tuner.mu.Lock()
	defer tuner.mu.Unlock()
	writeOK(w, map[string]any{
		"state":          tuner.state.String(),
		"since":          tuner.stateSince.UnixMilli(),
		"gomaxprocs":     runtime.GOMAXPROCS(0),
		"gcPercent":      tuner.gcPercent,
		"cpuBusy":        tuner.cpuBusy,
		"goroutines":     tuner.goroutines,
		"heapAlloc":      tuner.heapAlloc,
		"numCPU":         tuner.numCPU,
		"memLimit":       tuner.memLimit,       // 内存软上限字节数（0 = 未设）
		"memLimitActive": tuner.memLimitActive, // 是否由本调谐器主动设置
	})
}
