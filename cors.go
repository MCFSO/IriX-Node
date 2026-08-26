// CORS 中间件：允许 Web 面板（浏览器跨源）直接调用节点 API。
//
// 节点默认监听 127.0.0.1，前端面板通常运行在另一来源（端口），
// 浏览器会以跨源请求访问节点 API；响应无 CORS 头时 fetch/XHR 会被浏览器拦截。
//
// 设计要点：
//   - 预检（OPTIONS + Access-Control-Request-Method）直接返回 204，
//     不进入业务路由与保险库门禁（保险库锁定时前端仍需完成预检，
//     且 403/503 等错误响应同样必须带 CORS 头，否则前端读不到错误信息）；
//   - 允许来源回显 Origin（Vary: Origin），请求头回显 Access-Control-Request-Headers，
//     方法固定放行 GET/POST/PUT/DELETE/PATCH/HEAD/OPTIONS，预检缓存 24 小时；
//   - 认证凭据是 apikey 查询参数 / X-Api-Key 头（非 Cookie），回显来源
//     并允许凭据携带，兼容前端 fetch 的各类凭据模式；
//   - 直连下载/上传通道（/download/ /upload/）与 WebSocket 控制台同样经过本中间件，
//     浏览器直接访问时响应同样带 CORS 头。

package main

import "net/http"

// corsMiddleware 为全部响应追加 CORS 头，并拦截预检请求。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			// 非浏览器请求（curl / 服务间调用）：无 Origin 可回显，放行全部来源
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		// 浏览器读取下载文件名等自定义响应头需要显式暴露
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")

		// CORS 预检：不进入业务路由（OPTIONS 无对应路由，会 405）
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS")
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			}
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
