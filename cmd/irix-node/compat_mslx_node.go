// MSLX 子节点链接协议（Protocol V1）。
//
// MSLX WebPanel 的「设置 → 节点管理 → 添加节点」不是普通 REST 登录，而是
// 主节点（面板）与子节点之间的**配对链接**：
//
//	1. 主节点 GET  {nodeUrl}/api/node/protocol            → 读协议号（必须是 1）
//	2. 主节点 POST {nodeUrl}/api/node/accept-link          → 带 LinkKey/PairingId/MasterUrl
//	3. 子节点校验 LinkKey（自身 SlaveLinkKey）、配对标识格式与子节点模式，
//	   再反向 GET {masterUrl}/api/node/protocol?pairingId=… 确认主节点认可本次配对
//	4. 子节点保存关系并返回 {nodeId, commsKey}；主节点据此登记节点
//	5. 之后子节点每 2 秒 POST {masterUrl}/api/node/report-stats（头 x-api-key: commsKey）
//	   上报 CPU/内存；主节点回 410 表示关系已撤销
//
// 本文件实现子节点侧的全部环节：配对状态持久化、接受链接、更新/解除链接、
// 以及 2 秒一次的状态上报循环。协议语义与 MSLX 子节点一致，实现为本项目自有 Go 代码。

package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"irix/node/internal/sysinfo"
	"irix/node/internal/timesys"
)

// mslxNodeProtocolVersion 节点协议版本（与 MSLX JwtUtils.NodeProtocolVersion 一致）。
const mslxNodeProtocolVersion = 1

// mslxNodeStateFile 链接关系持久化文件名（0600）。
const mslxNodeStateFile = "compat-mslx-node.json"

// mslxNodeStatsInterval 状态上报间隔（与 MSLX SystemMonitorWorker 一致：2 秒）。
const mslxNodeStatsInterval = 2 * time.Second

// mslxNodeLink 子节点与主节点的链接关系。
type mslxNodeLink struct {
	NodeID    string `json:"nodeId"`    // 本节点标识（稳定，跨多次链接不变）
	CommsKey  string `json:"commsKey"`  // 本次链接的通讯密钥（上报时作 x-api-key）
	MasterID  string `json:"masterId"`  // 主节点标识
	MasterURL string `json:"masterUrl"` // 主节点地址（用于上报与反向探测）
	LinkedAt  int64  `json:"linkedAt"`  // 链接建立时间（Unix 秒）
}

// mslxNodeState mslxCompat 上的节点链接状态。
type mslxNodeState struct {
	mu    sync.Mutex
	link  *mslxNodeLink // nil = 尚未链接
	id    string        // 稳定的本节点标识（跨多次链接复用）
	shown bool          // 上报失败告警去重
}

// nodeStatePath 链接关系文件路径。
func (ms *mslxCompat) nodeStatePath() string {
	dir := strings.TrimSpace(ms.d.DataDir)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, mslxNodeStateFile)
}

// loadNodeLink 读取持久化的链接关系（不存在/损坏时返回 nil）。
func (ms *mslxCompat) loadNodeLink() *mslxNodeLink {
	data, err := os.ReadFile(ms.nodeStatePath())
	if err != nil {
		return nil
	}
	var link mslxNodeLink
	if err := json.Unmarshal(data, &link); err != nil {
		return nil
	}
	if link.NodeID == "" || link.CommsKey == "" || link.MasterURL == "" {
		return nil
	}
	return &link
}

// saveNodeLink 原子写入链接关系（0600）。
func (ms *mslxCompat) saveNodeLink(link *mslxNodeLink) error {
	dir := filepath.Dir(ms.nodeStatePath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	data, err := json.MarshalIndent(link, "", "  ")
	if err != nil {
		return err
	}
	path := ms.nodeStatePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// currentNodeLink 取当前链接关系副本。
func (ms *mslxCompat) currentNodeLink() *mslxNodeLink {
	ms.node.mu.Lock()
	defer ms.node.mu.Unlock()
	if ms.node.link == nil {
		return nil
	}
	cp := *ms.node.link
	return &cp
}

// setNodeLink 更新内存中的链接关系（nil = 解除）。
func (ms *mslxCompat) setNodeLink(link *mslxNodeLink) {
	ms.node.mu.Lock()
	ms.node.link = link
	ms.node.shown = false
	ms.node.mu.Unlock()
}

// ---------------------------------------------------------------------------
// HTTP 处理：/api/node/*
// ---------------------------------------------------------------------------

// handleNode 处理 /api/node/* 下的全部子节点侧接口。
func (ms *mslxCompat) handleNode(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) != 1 {
		http.NotFound(w, r)
		return
	}
	switch segs[0] {
	case "protocol":
		ms.handleNodeProtocol(w, r)
	case "accept-link":
		ms.handleNodeAcceptLink(w, r)
	case "update-link":
		ms.handleNodeUpdateLink(w, r)
	case "remove-link":
		ms.handleNodeRemoveLink(w, r)
	case "status":
		ms.handleNodeStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleNodeProtocol 协议探测（匿名）。主节点链接前会先读这个接口。
func (ms *mslxCompat) handleNodeProtocol(w http.ResponseWriter, r *http.Request) {
	pairingID := strings.TrimSpace(r.URL.Query().Get("pairingId"))
	// 携带 pairingId 的探测是「主节点确认配对」的语义：本节点不作为主节点，
	// 不持有配对请求，按 MSLX 的行为明确拒绝（子节点只在反向探测主节点时用到）。
	if pairingID != "" {
		mslxFail(w, http.StatusBadRequest, "此节点没有对应的配对请求")
		return
	}
	mslxOK(w, map[string]any{
		"protocolVersion": mslxNodeProtocolVersion,
		"name":            "irix-node",
		"version":         Version,
		"slaveMode":       ms.slaveMode,
		"linked":          ms.currentNodeLink() != nil,
		"features": map[string]any{
			"pluginMultiNode":     true,
			"nodeSync":            true,
			"nodeApiKeySession":   true,
			"irixNodeCompatLayer": true,
		},
	}, "成功")
}

// handleNodeAcceptLink 接受主节点的链接请求。
func (ms *mslxCompat) handleNodeAcceptLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var req struct {
		ProtocolVersion int    `json:"protocolVersion"`
		MasterID        string `json:"masterId"`
		MasterURL       string `json:"masterUrl"`
		LinkKey         string `json:"linkKey"`
		PairingID       string `json:"pairingId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体不是有效的 JSON")
		return
	}
	// 1) 协议版本
	if req.ProtocolVersion != mslxNodeProtocolVersion {
		mslxFail(w, http.StatusBadRequest,
			fmt.Sprintf("节点协议不兼容（本节点 %d，请求 %d），请一起升级", mslxNodeProtocolVersion, req.ProtocolVersion))
		return
	}
	// 2) 必须是子节点模式（与 MSLX 一致）
	if !ms.slaveMode {
		mslxFail(w, http.StatusBadRequest, "当前并非子节点模式，拒绝链接请求")
		return
	}
	// 3) 配对密钥（定长比较，避免时序侧信道）
	if ms.linkKey == "" || subtle.ConstantTimeCompare([]byte(ms.linkKey), []byte(req.LinkKey)) != 1 {
		alog.Printf("MSLX 兼容层：拒绝链接，配对密钥不匹配（主节点 %s）", req.MasterURL)
		mslxFail(w, http.StatusUnauthorized, "子节点 LinkKey 错误")
		return
	}
	// 4) 配对标识：32 位十六进制（guid "N" 形式）
	if !isHex32(req.PairingID) {
		mslxFail(w, http.StatusBadRequest, "配对标识无效")
		return
	}
	// 5) 主节点地址规范化
	masterURL, err := normalizeMasterURL(req.MasterURL)
	if err != nil {
		mslxFail(w, http.StatusBadRequest, "子节点连接主节点通讯失败："+err.Error())
		return
	}
	// 6) 反向探测主节点，确认它认可本次配对
	if msg := probeMasterProtocol(masterURL, req.PairingID); msg != "" {
		alog.Printf("MSLX 兼容层：%s", msg)
		mslxFail(w, http.StatusBadRequest, msg)
		return
	}
	// 7) 建立关系：nodeId 稳定，commsKey 每次链接重新生成
	link := &mslxNodeLink{
		NodeID:    ms.nodeID(),
		CommsKey:  randomToken(compatSecretLen),
		MasterID:  strings.TrimSpace(req.MasterID),
		MasterURL: masterURL,
		LinkedAt:  timesys.Now().Unix(),
	}
	if err := ms.saveNodeLink(link); err != nil {
		mslxFail(w, http.StatusInternalServerError, "保存链接关系失败: "+err.Error())
		return
	}
	ms.setNodeLink(link)
	alog.Printf("MSLX 兼容层：已与主节点 %s 建立链接（nodeId=%s）", masterURL, link.NodeID)
	// 立即上报一次：主节点以「10 秒内有上报」判定节点在线，等 2 秒窗口
	// 会让面板先闪一下离线。
	if status, err := ms.reportNodeStats(&http.Client{Timeout: 3 * time.Second}, link); err != nil {
		alog.Printf("MSLX 兼容层：首次状态上报失败（HTTP %d, %v），将继续每 2 秒重试", status, err)
	}
	mslxOK(w, map[string]any{"nodeId": link.NodeID, "commsKey": link.CommsKey}, "链接成功")
}

// handleNodeUpdateLink 主节点同步地址变更（用 nodeId + commsKey 校验）。
func (ms *mslxCompat) handleNodeUpdateLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var req struct {
		MasterURL string `json:"masterUrl"`
		CommsKey  string `json:"commsKey"`
		NodeID    string `json:"nodeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体不是有效的 JSON")
		return
	}
	link := ms.currentNodeLink()
	if link == nil || link.NodeID != strings.TrimSpace(req.NodeID) ||
		subtle.ConstantTimeCompare([]byte(link.CommsKey), []byte(req.CommsKey)) != 1 {
		mslxFail(w, http.StatusUnauthorized, "节点关系已变化或通讯密钥无效")
		return
	}
	masterURL, err := normalizeMasterURL(req.MasterURL)
	if err != nil {
		mslxFail(w, http.StatusBadRequest, err.Error())
		return
	}
	link.MasterURL = masterURL
	if err := ms.saveNodeLink(link); err != nil {
		mslxFail(w, http.StatusInternalServerError, "保存链接关系失败: "+err.Error())
		return
	}
	ms.setNodeLink(link)
	alog.Printf("MSLX 兼容层：主节点地址已更新为 %s", masterURL)
	mslxOK(w, map[string]any{"masterUrl": masterURL}, "同步成功")
}

// handleNodeRemoveLink 主节点解除链接。
func (ms *mslxCompat) handleNodeRemoveLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var req struct {
		NodeID   string `json:"nodeId"`
		CommsKey string `json:"commsKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体不是有效的 JSON")
		return
	}
	link := ms.currentNodeLink()
	if link != nil && link.NodeID == strings.TrimSpace(req.NodeID) &&
		subtle.ConstantTimeCompare([]byte(link.CommsKey), []byte(req.CommsKey)) == 1 {
		_ = os.Remove(ms.nodeStatePath())
		ms.setNodeLink(nil)
		alog.Printf("MSLX 兼容层：主节点已解除链接")
	}
	// 与 MSLX 一致：清理是幂等的，无论此前是否存在关系都返回成功
	mslxOK(w, true, "已清理链接")
}

// noteStatusProbe 记录面板探活（每个节点标识每分钟最多一条），
// 便于运维确认「面板到底有没有打到本节点」。
func (ms *mslxCompat) noteStatusProbe(nodeID string) {
	ms.probeMu.Lock()
	defer ms.probeMu.Unlock()
	now := timesys.Now()
	if ms.probeAt.IsZero() || now.Sub(ms.probeAt) > time.Minute || ms.probeNode != nodeID {
		ms.probeAt = now
		ms.probeNode = nodeID
		alog.Printf("MSLX 兼容层：面板探活命中（x-node-id=%s）", nodeID)
	}
}

// handleNodeStatus 本节点在 MSLX 侧的链接状态（面板/运维排查用，需登录）。
func (ms *mslxCompat) handleNodeStatus(w http.ResponseWriter, r *http.Request) {
	link := ms.currentNodeLink()
	out := map[string]any{
		"slaveMode":       ms.slaveMode,
		"protocolVersion": mslxNodeProtocolVersion,
		"linkKeySet":      ms.linkKey != "",
		"linked":          link != nil,
	}
	if link != nil {
		out["nodeId"] = link.NodeID
		out["masterUrl"] = link.MasterURL
		out["linkedAt"] = link.LinkedAt
		out["commsKeyMasked"] = maskSecret(link.CommsKey)
	}
	mslxOK(w, out, "获取成功")
}

// nodeID 返回稳定的本节点标识（无链接时也生成并持久化，供下一次链接复用）。
func (ms *mslxCompat) nodeID() string {
	ms.node.mu.Lock()
	defer ms.node.mu.Unlock()
	if ms.node.link != nil && ms.node.link.NodeID != "" {
		return ms.node.link.NodeID
	}
	if ms.node.id == "" {
		// randomToken(16) 已是 32 位十六进制，正好是 guid "N" 形式
		ms.node.id = randomToken(16)
	}
	return ms.node.id
}

// isHex32 判断是否为 32 位十六进制字符串（guid "N" 形式）。
func isHex32(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// normalizeMasterURL 校验并规范化主节点地址（与 MSLX 的规则一致）。
func normalizeMasterURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("主节点地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("主节点地址必须为有效 HTTP/HTTPS 地址")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("主节点地址不能包含账号、查询串或锚点")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// probeMasterProtocol 反向探测主节点是否认可本次配对（5 秒超时）。
func probeMasterProtocol(masterURL, pairingID string) string {
	const prefix = "子节点连接主节点通讯失败"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(masterURL + "/api/node/protocol?pairingId=" + url.QueryEscape(pairingID))
	if err != nil {
		return prefix + "：" + masterURL + "，" + err.Error() + "（请检查子节点能否访问此地址、端口及 HTTP/HTTPS 配置；localhost 指向子节点自身）"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("%s（HTTP %d）：%s，请检查该地址是否指向当前主节点，以及端口和反向代理配置",
			prefix, resp.StatusCode, masterURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return prefix + "：读取主节点响应失败"
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			ProtocolVersion int    `json:"protocolVersion"`
			PairingID       string `json:"pairingId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return prefix + "：" + masterURL + "，响应不是有效的 MSLX 接口数据，请检查地址、端口及反向代理配置"
	}
	if env.Code != 200 || env.Data.ProtocolVersion != mslxNodeProtocolVersion || env.Data.PairingID != pairingID {
		return prefix + "：" + masterURL + " 未确认本次配对，请填写子节点能够访问的当前主节点地址"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 状态上报
// ---------------------------------------------------------------------------

// startNodeReporter 启动子节点状态上报循环（每 2 秒一次）。
//
// 上报体与 MSLX NodeStatsPayload 同形：{cpu, memTotal, memUsed, memUsage, timestamp}，
// 头部 x-api-key 为本次链接的 commsKey。主节点返回 410 表示关系已撤销，
// 此时清除本地链接（与 MSLX 行为一致）。
func (ms *mslxCompat) startNodeReporter() {
	safeGo("MSLX 兼容层状态上报", func() {
		client := &http.Client{Timeout: 3 * time.Second}
		t := time.NewTicker(mslxNodeStatsInterval)
		defer t.Stop()
		for {
			select {
			case <-ms.d.workers.ctx.Done():
				return
			case <-t.C:
			}
			link := ms.currentNodeLink()
			if link == nil {
				continue
			}
			status, err := ms.reportNodeStats(client, link)
			if err != nil {
				ms.node.mu.Lock()
				first := !ms.node.shown
				ms.node.shown = true
				ms.node.mu.Unlock()
				if first {
					alog.Printf("MSLX 兼容层：向主节点 %s 上报状态失败（%v），将继续重试", link.MasterURL, err)
				}
				continue
			}
			ms.node.mu.Lock()
			ms.node.shown = false
			ms.node.mu.Unlock()
			if status == http.StatusGone {
				alog.Printf("MSLX 兼容层：主节点 %s 已撤销链接，清除本地链接关系", link.MasterURL)
				_ = os.Remove(ms.nodeStatePath())
				ms.setNodeLink(nil)
			}
		}
	})
}

// reportNodeStats 上报一次状态，返回 HTTP 状态码。
func (ms *mslxCompat) reportNodeStats(client *http.Client, link *mslxNodeLink) (int, error) {
	total, _, memUsage := sysinfo.MemSnapshot()
	cpu := sysinfo.CachedSysCPUUsage()
	payload := map[string]any{
		"cpu":       roundPercent(cpu * 100),
		"memTotal":  total,
		"memUsed":   uint64(float64(total) * memUsage),
		"memUsage":  roundPercent(memUsage * 100),
		"timestamp": timesys.Now().Format("15:04:05"),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, link.MasterURL+"/api/node/report-stats", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", link.CommsKey)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusGone {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("主节点返回 HTTP %d", resp.StatusCode)
}

// roundPercent 百分比保留一位小数（与 MSLX 上报格式一致）。
func roundPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return float64(int(v*10+0.5)) / 10
}

// ---------------------------------------------------------------------------
// 主节点代理请求的鉴权
// ---------------------------------------------------------------------------

// mslxNodeVerifyCache 主节点校验结果的短缓存（key = 令牌），
// 避免面板每个请求都回源主节点（面板会并发打很多接口）。
type mslxNodeVerifyCache struct {
	mu    sync.Mutex
	items map[string]mslxNodeVerifyItem
}

type mslxNodeVerifyItem struct {
	claims  *mslxClaims
	expires time.Time
}

// authorizeNodeRequest 校验「主节点代理」请求。
//
// MSLX 面板在纳管子节点后，会带着 x-node-id + 主节点签发的节点令牌直接请求
// 子节点接口。原生子节点把令牌回源到主节点 /api/node/verify-token（带
// x-api-key = commsKey）验证；我们无法本地验证主节点签名，因此同样回源核对：
// 主节点返回 200 即视为已授权（admin 角色由主节点给出的 role 决定）。
func (ms *mslxCompat) authorizeNodeRequest(r *http.Request) (*mslxClaims, bool) {
	link := ms.currentNodeLink()
	if link == nil {
		return nil, false
	}
	nodeID := strings.TrimSpace(r.Header.Get("x-node-id"))
	if nodeID == "" {
		nodeID = strings.TrimSpace(r.URL.Query().Get("x-node-id"))
	}
	if nodeID == "" || nodeID != link.NodeID {
		return nil, false
	}
	tok := tokenFromRequest(r)
	if tok == "" {
		// 面板探活（/api/status）走的是无令牌的节点请求：已在上游按公开
		// 状态处理；其余接口无令牌一律拒绝。
		return nil, false
	}
	// 命中缓存
	ms.nodeVerify.mu.Lock()
	if ms.nodeVerify.items == nil {
		ms.nodeVerify.items = map[string]mslxNodeVerifyItem{}
	}
	if item, ok := ms.nodeVerify.items[tok]; ok && timesys.Now().Before(item.expires) {
		ms.nodeVerify.mu.Unlock()
		return item.claims, true
	}
	ms.nodeVerify.mu.Unlock()

	claims, ok := verifyNodeTokenAtMaster(link, tok)
	if !ok {
		return nil, false
	}
	ms.nodeVerify.mu.Lock()
	if ms.nodeVerify.items == nil {
		ms.nodeVerify.items = map[string]mslxNodeVerifyItem{}
	}
	// 简单的容量保护：超过 512 条就整体清空（节点侧并发有限，够用）
	if len(ms.nodeVerify.items) > 512 {
		ms.nodeVerify.items = map[string]mslxNodeVerifyItem{}
	}
	ms.nodeVerify.items[tok] = mslxNodeVerifyItem{claims: claims, expires: timesys.Now().Add(30 * time.Second)}
	ms.nodeVerify.mu.Unlock()
	return claims, true
}

// verifyNodeTokenAtMaster 向主节点核对节点令牌。
func verifyNodeTokenAtMaster(link *mslxNodeLink, token string) (*mslxClaims, bool) {
	body, err := json.Marshal(map[string]any{"token": token})
	if err != nil {
		return nil, false
	}
	req, err := http.NewRequest(http.MethodPost, link.MasterURL+"/api/node/verify-token", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", link.CommsKey)
	// 1.5 秒上限：面板对节点接口普遍是 3 秒超时，回源核验不能把调用方拖死。
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			UserID string `json:"userId"`
			Name   string `json:"name"`
			Role   string `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 200 {
		return nil, false
	}
	return &mslxClaims{
		UserID: env.Data.UserID,
		Name:   env.Data.Name,
		Role:   env.Data.Role,
	}, true
}
