// 实时控制台 WebSocket（docs/irix-node-local-parity.md §4.1.1）。
//
// WS  /api/instance/console/ws?uuid=<u>&daemonId=<d>&apikey=<key>&since=<unix_ms>
//
//   - 服务端 → 客户端：文本帧，一行一条服务器原始输出（保留 ANSI 转义，
//     颜色由客户端 ansi_color.dart 渲染，节点不得剥离）；
//   - 客户端 → 服务端：文本帧为控制台命令（等效 POST /api/protected_instance/command）；
//   - 心跳：客户端每 30s 发送 ping（文本帧或控制帧均可）；节点 90s 未收到
//     任何帧则断开（writeClose 1001 + 关闭连接）；
//   - 断线重连：客户端带 since=<unix_ms> 重连，节点补发该时间点之后的
//     增量日志（运行中走行缓冲，精确到行）。
//
// 兼容性：不支持 WebSocket 的旧节点对升级请求返回非 101，客户端握手失败
// 即回退 outputlog 轮询 + command，不报错。

package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// wsHeartbeatIdle 心跳超时：90 秒未收到任何帧即断开。
const wsHeartbeatIdle = 90 * time.Second

// wsHeartbeatTick 心跳检查周期。
const wsHeartbeatTick = 15 * time.Second

// wsSendLines 将多行文本按行拆成多个文本帧发送（补发场景）。
func (c *wsConn) wsSendLines(s string) error {
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if err := c.writeText(line); err != nil {
			return err
		}
	}
	return nil
}

// handleConsoleWS 实时控制台 WebSocket 入口。
func (d *Daemon) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	conn, err := upgradeWS(w, r)
	if err != nil {
		// 升级失败（如旧代理不支持）：响应已写出，客户端据此回退轮询
		return
	}
	// 审计中间件如实记录 101 状态（hijack 绕过了普通响应路径）
	if arw, ok := w.(*auditResponseWriter); ok {
		arw.code = http.StatusSwitchingProtocols
	}
	defer conn.Close()

	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()

	// 订阅进程输出（进程未运行时仅命令与补发可用，输出通道保持空）
	var (
		subCh    <-chan string
		cancel   func()
		procDone <-chan struct{}
	)
	if proc != nil {
		subCh, cancel = proc.Subscribe()
		defer cancel()
		procDone = proc.done
	}

	// 断线补发：先订阅后补发（补发期间的少量重复可接受，不允许丢失）
	if sinceStr := queryParam(r, "since"); sinceStr != "" && proc != nil && proc.lines != nil {
		since := int64(atoiDefault(sinceStr, 0))
		if s := proc.lines.since(since); s != "" {
			_ = conn.wsSendLines(s)
		}
	}

	done := make(chan struct{})
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixMilli())

	// 写循环：订阅输出 → 文本帧；进程退出 → 通知并关闭；连接结束 → 退出
	writeErr := make(chan struct{})
	go func() {
		defer close(writeErr)
		exitNotified := false
		for {
			select {
			case line := <-subCh:
				if err := conn.writeText(line); err != nil {
					return
				}
			case <-procDone:
				if !exitNotified {
					_ = conn.writeText("[节点] 进程已退出，输出结束")
					_ = conn.writeClose(1000, "进程已退出")
					exitNotified = true
					_ = conn.Close() // 解除读循环阻塞
				}
			case <-done:
				return
			}
		}
	}()

	// 心跳看门狗：90s 无任何帧则断开
	go func() {
		ticker := time.NewTicker(wsHeartbeatTick)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Since(time.UnixMilli(lastRead.Load())) > wsHeartbeatIdle {
					_ = conn.writeClose(1001, "心跳超时")
					_ = conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()

	// 读循环：命令 / ping / pong / close
	for {
		opcode, payload, err := conn.readFrame()
		if err != nil {
			break // 对端关闭或协议错误
		}
		lastRead.Store(time.Now().UnixMilli())
		switch opcode {
		case wsText:
			msg := string(payload)
			if msg == "ping" {
				continue // 心跳文本帧
			}
			inst.mu.Lock()
			p := inst.Proc
			inst.mu.Unlock()
			if p == nil || !p.IsRunning() {
				_ = conn.writeText("[节点] 实例未在运行，命令未发送")
				continue
			}
			if err := p.WriteCommand(msg); err != nil {
				_ = conn.writeText("[节点] 命令发送失败: " + err.Error())
			}
		case wsPing:
			_ = conn.writePong(payload)
		case wsPong:
			// 仅刷新活跃时间
		case wsClose:
			_ = conn.writeClose(1000, "")
			close(done)
			return
		}
	}
	// 读循环退出：通知写循环并关闭连接
	close(done)
	_ = conn.Close()
	<-writeErr
}
