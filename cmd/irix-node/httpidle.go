// 请求体「读空闲」超时（红队审计 高-9）。
//
// 背景：节点需要流式承接大文件上传/下载，因此**不能**设置全局
// http.Server.ReadTimeout / WriteTimeout（会按「整个请求」计时，把正常的
// 大文件传输切断）。但这留下一个缺口：外部只要声明一个很大的
// Content-Length，然后每 30 秒发 1 个字节，就能长期占住一个连接与
// goroutine（免认证入口 /api/auth/login 同样适用）；项目在启动时把 fd
// 上限拉到硬上限，会进一步放大成「fd 耗尽 → 合法用户连不上」。
//
// 做法：把「整请求超时」换成「读空闲超时」——每次成功读取后顺延读截止
// 时间，只要对端还在持续发包就不触发；一旦空闲超过窗口即中断该请求。

package main

import (
	"io"
	"net/http"
	"time"
)

// requestBodyIdleTimeout 请求体读空闲窗口。
//
// 声明为 var 而非 const：测试需压低窗口以覆盖超时分支（本包测试不并行）。
var requestBodyIdleTimeout = 60 * time.Second

// idleBodyReadCloser 包装请求体：每次读取前/后都把读截止时间顺延一个窗口，
// 使「持续有数据」的长上传不受影响，而「挂住不发声」的连接会在窗口后失败。
type idleBodyReadCloser struct {
	rc     *http.ResponseController
	body   io.ReadCloser
	window time.Duration
}

// Read 顺延读截止时间后读取；读完后再次顺延，避免长块读取耗尽余量。
func (b *idleBodyReadCloser) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.window))
	n, err := b.body.Read(p)
	if n > 0 {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.window))
	}
	return n, err
}

func (b *idleBodyReadCloser) Close() error { return b.body.Close() }

// idleBodyTimeout 为带请求体的请求套上「读空闲」读截止时间。
//
// 无请求体的请求（含 WebSocket 升级握手）完全不介入：控制台长连接由
// console_ws 自己的心跳看门狗管理。
func idleBodyTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &idleBodyReadCloser{
				rc:     http.NewResponseController(w),
				body:   r.Body,
				window: requestBodyIdleTimeout,
			}
		}
		next.ServeHTTP(w, r)
	})
}
