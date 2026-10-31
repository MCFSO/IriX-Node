// DDoS/CC 防御管理接口（防御本身恒开，这些端点只做观测与人工封禁）。
//
//   - GET    /api/defense/status  查看阈值、实时统计、封禁表、UA 排行与最近事件
//   - POST   /api/defense/ban     人工封禁 IP 或 User-Agent（仅管理员）
//   - DELETE /api/defense/ban     解除封禁（仅管理员）
//
// 人工封禁持久化到 {data}/defense.json，重启后仍生效；自动封禁是运行期状态，
// 到期自动解除。这里没有「关闭防御」的端点或参数——防御不可关闭。

package main

import (
	"net/http"
	"strings"
	"time"
)

// registerDefenseRoutes 注册防御管理路由（纳入权限目录「防御」组）。
func (d *Daemon) registerDefenseRoutes(mux *http.ServeMux) {
	perm("防御", "GET /api/defense/status", "查看 DDoS/CC 防御状态")
	mux.HandleFunc("GET /api/defense/status", d.auth(d.handleDefenseStatus))
	perm("防御", "POST /api/defense/ban", "手动封禁 IP 或 User-Agent")
	mux.HandleFunc("POST /api/defense/ban", d.auth(d.handleDefenseBanAdd))
	perm("防御", "DELETE /api/defense/ban", "解除 IP 或 User-Agent 封禁")
	mux.HandleFunc("DELETE /api/defense/ban", d.auth(d.handleDefenseBanRemove))
}

// handleDefenseStatus 返回防御状态快照。
// GET /api/defense/status
func (d *Daemon) handleDefenseStatus(w http.ResponseWriter, r *http.Request) {
	if d.defense == nil {
		writeError(w, http.StatusServiceUnavailable, "防御子系统未就绪")
		return
	}
	writeOK(w, d.defense.snapshot())
}

// defenseBanRequest 人工封禁请求体。
type defenseBanRequest struct {
	Type    string `json:"type"`    // ip | ua
	Value   string `json:"value"`   // IP 地址或 User-Agent
	Minutes int    `json:"minutes"` // 封禁分钟数；0 = 永久
	Reason  string `json:"reason"`  // 原因（可选）
}

// defenseManualBanMaxMinutes 人工封禁时长上限（一年）：永久封禁用 minutes=0。
const defenseManualBanMaxMinutes = 365 * 24 * 60

// handleDefenseBanAdd 人工封禁 IP/User-Agent（仅管理员）。
// POST /api/defense/ban body: {type, value, minutes, reason}
func (d *Daemon) handleDefenseBanAdd(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if d.defense == nil {
		writeError(w, http.StatusServiceUnavailable, "防御子系统未就绪")
		return
	}
	var body defenseBanRequest
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.Minutes < 0 || body.Minutes > defenseManualBanMaxMinutes {
		writeError(w, http.StatusBadRequest, "封禁时长无效（0-525600 分钟）")
		return
	}
	var ttl time.Duration
	if body.Minutes > 0 {
		ttl = time.Duration(body.Minutes) * time.Minute
	}
	ban, err := d.defense.addManualBan(body.Type, body.Value, ttl, body.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{
		"type":      ban.Kind,
		"value":     ban.Value,
		"minutes":   body.Minutes,
		"permanent": ban.Until == 0,
		"until":     ban.Until,
		"reason":    ban.Reason,
		"source":    ban.Source,
	})
}

// handleDefenseBanRemove 解除封禁（仅管理员）。
// DELETE /api/defense/ban?type=ua&value=BadBot
func (d *Daemon) handleDefenseBanRemove(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if d.defense == nil {
		writeError(w, http.StatusServiceUnavailable, "防御子系统未就绪")
		return
	}
	kind := strings.ToLower(queryParam(r, "type"))
	value := strings.TrimSpace(queryParam(r, "value"))
	if (kind != "ip" && kind != "ua") || value == "" {
		writeError(w, http.StatusBadRequest, "需要 type（ip|ua）与 value 参数")
		return
	}
	if !d.defense.removeBan(kind, value) {
		writeError(w, http.StatusNotFound, "未找到该封禁记录")
		return
	}
	writeOK(w, map[string]any{"removed": true, "type": kind, "value": value})
}
