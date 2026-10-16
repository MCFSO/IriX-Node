package main

import (
	"net/http"
	"time"
)

// newHTTPServer 集中组装路由、中间件和传输限制，供入口与集成测试共用。
func (d *Daemon) newHTTPServer(addr string) *http.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return &http.Server{
		Addr:    addr,
		Handler: d.trackRequests(d.auditMiddleware(corsMiddleware(d.vaultGate(limitAPIBody(mux))))),
		// 只限制请求头与空闲连接，保留大文件流式上传/下载的能力。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// trackRequests 不包装 ResponseWriter，保留控制台所需的 Hijacker/Flusher。
// 请求计数涵盖业务处理与审计写入；关停返回前日志落盘器仍然可用。
func (d *Daemon) trackRequests(next http.Handler) http.Handler {
	// 被关停门禁拒绝的请求保留统一响应与 CORS，避免客户端误报网络错误。
	unavailable := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "节点正在关停，请稍后重试")
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.httpRequests.enter() {
			unavailable.ServeHTTP(w, r)
			return
		}
		defer d.httpRequests.leave()
		next.ServeHTTP(w, r)
	})
}

// registerConsole 登记已劫持的控制台连接。关停与登记使用同一把锁，
// 防止 Shutdown 关闭快照后又出现未被关闭的新连接。
func (d *Daemon) registerConsole(conn *wsConn) bool {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	if d.consoleClosed {
		return false
	}
	d.consoles[conn] = struct{}{}
	return true
}

func (d *Daemon) unregisterConsole(conn *wsConn) {
	d.consoleMu.Lock()
	delete(d.consoles, conn)
	d.consoleMu.Unlock()
}

// closeConsoles 显式关闭 HTTP 已劫持的连接；http.Server.Shutdown 不处理它们。
func (d *Daemon) closeConsoles() {
	d.consoleMu.Lock()
	d.consoleClosed = true
	conns := make([]*wsConn, 0, len(d.consoles))
	for conn := range d.consoles {
		conns = append(conns, conn)
	}
	d.consoleMu.Unlock()
	for _, conn := range conns {
		// 直接解除读写阻塞，避免等待慢客户端接收关闭帧。
		_ = conn.Close()
	}
}
