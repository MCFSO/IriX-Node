// CORS 中间件：允许 Web 面板（浏览器跨源）直接调用节点 API。
//
// 节点默认监听 127.0.0.1，前端面板通常运行在另一来源（端口），
// 浏览器会以跨源请求访问节点 API；响应无 CORS 头时 fetch/XHR 会被浏览器拦截。
//
// 设计要点（红队审计 中-1 修复：不再反射任意来源）：
//   - **来源白名单**：只有 corsOriginAllowed 放行的来源才下发
//     Access-Control-Allow-Origin 与 Allow-Credentials；其余来源不下发任何
//     CORS 头，浏览器侧直接拒绝读取响应。放行范围见 corsOriginAllowed
//     （显式配置的来源 / 环回来源 / 与请求同主机名的来源）。
//   - 预检（OPTIONS + Access-Control-Request-Method）直接返回 204，
//     不进入业务路由与保险库门禁（保险库锁定时前端仍需完成预检，
//     且 403/503 等错误响应同样必须带 CORS 头，否则前端读不到错误信息）；
//     未放行来源的预检同样 204 但不带 Allow-* 头（浏览器判定预检失败）。
//   - Allow-Headers 使用**固定清单**，不再回显 Access-Control-Request-Headers
//     （回显等于把发起方声明的任意请求头宣称成「服务端接受」）。
//   - 直连下载/上传通道（/download/ /upload/）与 WebSocket 控制台同样经过本中间件。

package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// corsAllowedHeaders 固定放行的跨源请求头（节点实际读取的全部请求头）。
const corsAllowedHeaders = "Authorization, Content-Type, X-Auth-Token, X-Vault-Token"

// setCorsOrigins 解析额外的跨源来源白名单（-cors-origins / 配置文件 corsOrigins，
// 逗号分隔，形如 https://panel.example.com，可带端口；大小写不敏感）。
func (d *Daemon) setCorsOrigins(raw string) {
	m := map[string]bool{}
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		// 容忍误加的结尾斜杠（浏览器 Origin 头不带路径）
		o = strings.TrimRight(o, "/")
		m[strings.ToLower(o)] = true
	}
	d.corsOrigins = m
}

// corsOriginAllowed 判断请求来源是否放行。
//
// 放行三类（其余一律不下发 CORS 头）：
//  1. 显式配置的来源（-cors-origins / 配置文件 corsOrigins，精确匹配）；
//  2. 环回来源：本机面板（localhost / 127.0.0.0/8 / ::1），端口不限；
//  3. 与本次请求 Host 同主机名的来源（端口不限）——同机面板、反代同域场景。
//
// 明确拒绝 "null" 来源：沙箱 iframe 与 file:// 页面都会发出该值，
// 放行它等于对任意本地文件/沙箱页面开放。
func (d *Daemon) corsOriginAllowed(r *http.Request, origin string) bool {
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if d.corsOrigins[strings.ToLower(u.Scheme+"://"+u.Host)] {
		return true
	}
	if isLoopbackHostname(u.Hostname()) {
		return true
	}
	// 与请求 Host 同主机名（忽略端口）：面板与节点同机/同域反代时同样需要放行。
	reqHost := r.Host
	if h, _, splitErr := net.SplitHostPort(reqHost); splitErr == nil {
		reqHost = h
	}
	return reqHost != "" && strings.EqualFold(u.Hostname(), reqHost)
}

// isLoopbackHostname 判断主机名是否为环回地址（含 localhost）。
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// corsMiddleware 按白名单追加 CORS 头，并拦截预检请求。
func (d *Daemon) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := d.corsOriginAllowed(r, origin)
		if origin != "" {
			// 无论是否放行都标记 Vary: Origin，避免共享缓存把带 CORS 头的
			// 响应复用给其他来源。
			w.Header().Add("Vary", "Origin")
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		// 浏览器读取下载文件名等自定义响应头需要显式暴露
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")

		// CORS 预检：不进入业务路由（OPTIONS 无对应路由，会 405）
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			// 未放行来源：仅终结预检、不给出任何 Allow-* 头（浏览器判定失败）
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
