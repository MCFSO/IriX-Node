// AI 上下文与实例监控历史（docs/irix-node-local-parity.md §4.8）。
//
// GET /api/instance/logs/query?uuid&keyword&level&windowMin&maxLines
//   结构化日志查询（供 AI 助手分析节点实例日志）。未实现结构化解析，
//   按文档约定退化为 tail 全文 + 关键词过滤。
//
// GET /api/instance/metrics?uuid&minutes=15
//   历史采样 {samples: [{time, cpu, memoryMb, downloadBps, uploadBps}]}。
//   节点每 15 秒采样一次运行中实例，环形保留 60 条（15 分钟）。

package main

import (
	"net/http"
	"strings"
	"time"
)

// metricSample 单次实例指标采样。
type metricSample struct {
	Time        int64   `json:"time"` // unix 毫秒
	CPU         float64 `json:"cpu"`
	MemoryMB    int64   `json:"memoryMb"`
	DownloadBps int64   `json:"downloadBps"`
	UploadBps   int64   `json:"uploadBps"`
}

// metricsSampleInterval 采样间隔（测试可缩短）。
var metricsSampleInterval = 15 * time.Second

// metricsRingSize 环形保留条数（15 秒 × 60 = 15 分钟）。
const metricsRingSize = 60

// sampleAllMetrics 对全部运行中实例采样一次。
func (d *Daemon) sampleAllMetrics() {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		proc := inst.Proc
		inst.mu.Unlock()
		if proc == nil || !proc.IsRunning() {
			continue
		}
		pid := proc.cmd.Process.Pid
		down, up := cachedNetRates()
		s := metricSample{
			Time:        time.Now().UnixMilli(),
			CPU:         round2(proc.sampleCPUPercent()),
			MemoryMB:    procMemoryBytes(pid) >> 20,
			DownloadBps: int64(down),
			UploadBps:   int64(up),
		}
		inst.metricsMu.Lock()
		inst.metrics = append(inst.metrics, s)
		if len(inst.metrics) > metricsRingSize {
			inst.metrics = inst.metrics[len(inst.metrics)-metricsRingSize:]
		}
		inst.metricsMu.Unlock()
	}
}

// metricsLoop 后台采样循环（首次访问 metrics 接口时惰性启动）。
func (d *Daemon) metricsLoop() {
	for {
		time.Sleep(metricsSampleInterval)
		d.sampleAllMetrics()
	}
}

// handleInstanceMetrics 获取实例指标历史。
// GET /api/instance/metrics?uuid&daemonId&minutes=<15>
func (d *Daemon) handleInstanceMetrics(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.metricsOnce.Do(func() { go d.metricsLoop() }) // 循环永不返回，必须 goroutine 启动

	minutes := atoiDefault(queryParam(r, "minutes"), 15)
	if minutes <= 0 {
		minutes = 15
	}
	if minutes > 60 {
		minutes = 60
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).UnixMilli()
	inst.metricsMu.Lock()
	samples := make([]metricSample, 0, len(inst.metrics))
	for _, s := range inst.metrics {
		if s.Time >= since {
			samples = append(samples, s)
		}
	}
	inst.metricsMu.Unlock()
	writeOK(w, map[string]any{"samples": samples})
}

// logQueryMaxLines 日志查询输出上限。
const logQueryMaxLines = 2000

// logQueryScanLines 日志查询扫描上限（关键词可能出现在较老日志，取尾部 N 行）。
const logQueryScanLines = 10000

// handleLogsQuery 结构化日志查询（退化实现：tail 全文 + 关键词过滤）。
// GET /api/instance/logs/query?uuid&daemonId&keyword&level&windowMin&maxLines
// 响应 data: {items: [行...], total}
func (d *Daemon) handleLogsQuery(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(queryParam(r, "keyword")))
	maxLines := atoiDefault(queryParam(r, "maxLines"), 200)
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > logQueryMaxLines {
		maxLines = logQueryMaxLines
	}

	// 日志文件（尾部 logQueryScanLines 行）+ 运行中行缓冲合并
	var lines []string
	paths := d.logFilePaths(inst.InstanceUuid)
	if len(paths) > 0 {
		if all, err := readLogTail(paths, logQueryScanLines); err == nil && all != "" {
			lines = strings.Split(strings.TrimSuffix(all, "\n"), "\n")
		}
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc != nil && proc.lines != nil {
		if recent := proc.lines.tail(1000); recent != "" {
			lines = append(lines, strings.Split(strings.TrimSuffix(recent, "\n"), "\n")...)
		}
	}

	// 从尾部匹配收集（AI 需要最近的上下文）
	matched := make([]string, 0, maxLines)
	for i := len(lines) - 1; i >= 0 && len(matched) < maxLines; i-- {
		if keyword != "" && !strings.Contains(strings.ToLower(lines[i]), keyword) {
			continue
		}
		matched = append(matched, lines[i])
	}
	// 反转回时间顺序
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	if matched == nil {
		matched = []string{}
	}
	writeOK(w, map[string]any{"items": matched, "total": len(matched)})
}
