// time_handlers.go — 时间系统 API（docs/time-system.md）。
//
// 时间系统是节点唯一的墙上时间来源：GET /api/time 暴露当前质量、偏差、
// 漂移校正与各服务器状态；POST /api/time/sync 立即触发一轮 Roughtime 同步
// （权限目录「时间系统」组，管理员默认可用）。
package main

import (
	"context"
	"net/http"
	"time"
)

// timeSyncTimeout 手动触发同步的请求超时（需覆盖一轮多服务器查询）。
const timeSyncTimeout = 15 * time.Second

// handleTimeStatus 返回时间系统状态。
// GET /api/time
func (d *Daemon) handleTimeStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, d.timeStatus())
}

// handleTimeSync 立即执行一轮 Roughtime 同步并返回最新状态。
// POST /api/time/sync
func (d *Daemon) handleTimeSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), timeSyncTimeout)
	defer cancel()
	if err := d.syncTimeNow(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "时间同步失败: "+err.Error())
		return
	}
	st := d.timeStatus()
	// 同步成功但时间仍不可信（例如实际采用的源只有一台且未达一致性门槛）
	// 视为业务失败，便于调用方（面板/脚本）据此告警。
	if !st.Trustworthy {
		writeError(w, http.StatusServiceUnavailable,
			"时间同步未取得可信结果（质量 "+string(st.Quality)+"）："+st.LastError)
		return
	}
	writeOK(w, st)
}
