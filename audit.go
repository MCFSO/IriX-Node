// 审计日志：记录每一次用户操作（API 请求）的完整细节。
//
// 审计行格式（单行，| 分隔）：
//
//	2026-08-13 12:00:00.123 | 127.0.0.1 | POST /api/instance?apikey=*** | 200 | 5ms | {请求体前缀}
//
// 设计要点：
//   - 每条请求都记录：时间、来源 IP、方法、路径与查询参数（apikey 一律打码、
//     直连通道 /download/、/upload/ 路径中的票据密码同样打码）、
//     响应状态码、耗时、请求体前 auditBodyMax 字节——用户的「一举一动」可完整回溯；
//   - 请求体只捕获前缀，防大文件上传把审计日志撑爆；控制字符转义，
//     防恶意请求体/参数换行伪造日志行；
//   - 落盘复用 fileLogger（有界队列 + 大小轮转 + Write 永不阻塞），
//     审计写日志绝不影响请求路径；同时输出 stderr 供终端实时观察；
//   - 直连下载/上传通道（/download/ /upload/，票据绕过 API 认证）同样在审计范围内，
//     它们更需要记录来源与用途。

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// auditBodyMax 审计捕获请求体的字节上限（只保留前缀，防大上传撑爆审计日志）。
const auditBodyMax = 2048

// auditBody 请求体捕获包装：旁路记录读过的前 auditBodyMax 字节供审计展示，
// 不改变读取语义（数据原样流出，Close 透传）。
type auditBody struct {
	io.ReadCloser
	kept      []byte
	truncated bool
}

// Read 透传读取并捕获前缀。
func (b *auditBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n <= 0 {
		return n, err
	}
	rest := auditBodyMax - len(b.kept)
	if rest > 0 {
		if n <= rest {
			b.kept = append(b.kept, p[:n]...)
		} else {
			b.kept = append(b.kept, p[:rest]...)
			b.truncated = true
		}
	} else {
		b.truncated = true
	}
	return n, err
}

// auditResponseWriter 包装 ResponseWriter 记录响应状态码。
type auditResponseWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader 记录状态码并透传。
func (w *auditResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write 隐式 200 时补记状态码并透传。
func (w *auditResponseWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Flush 透传底层 Flusher（若有）。
func (w *auditResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIP 提取请求来源 IP（不解析代理头：直连通道同样需审计真实来源）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// redactQuery 将查询串中的 apikey 明文打码（?a=b&apikey=***&...），其余保持原样。
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if strings.HasPrefix(strings.ToLower(p), "apikey=") {
			parts[i] = "apikey=***"
		}
	}
	return "?" + strings.Join(parts, "&")
}

// redactPath 将直连通道 URL 路径中的票据密码打码
// （/download/{password}/...、/upload/{password} → /download/***/...）。
// 票据是 10 分钟有效的免密凭据：审计日志可读者若拿到明文密码，
// 即可在有效期内直接下载目标文件，故路径中的票据段必须与 apikey 同等打码。
// 无票据段的裸路径（/download/、/upload/）原样返回。
func redactPath(p string) string {
	for _, prefix := range []string{"/download/", "/upload/"} {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if rest == "" {
			return p
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return prefix + "***/" + rest[i+1:]
		}
		return prefix + "***"
	}
	return p
}

// sanitizeLog 将控制字符转义为可见形式，防止恶意请求体/参数伪造日志行。
func sanitizeLog(s string) string {
	if !strings.ContainsAny(s, "\r\n\t") {
		return s
	}
	return strings.NewReplacer("\r", "\\r", "\n", "\\n", "\t", "\\t").Replace(s)
}

// auditLogf 写一条审计日志：同时输出 stderr（异步）与审计文件（异步落盘）。
func (d *Daemon) auditLogf(format string, args ...any) {
	line := sanitizeLog(fmt.Sprintf(format, args...))
	alog.Printf("%s", line)
	if d.AuditLog != nil {
		_, _ = d.AuditLog.Write([]byte(line + "\n"))
	}
}

// auditMiddleware 记录每一次请求的审计日志（替换原 logMiddleware）：
//
//	时间 | 来源 IP | 方法 路径+查询（apikey 打码）| 状态码 | 耗时 | 请求体前缀
//
// 请求体只捕获前 auditBodyMax 字节（超长标记截断），审计行恒为单行且有界。
func (d *Daemon) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var ab *auditBody
		if r.Body != nil {
			ab = &auditBody{ReadCloser: r.Body}
			r.Body = ab
		}
		rw := &auditResponseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)

		body := ""
		if ab != nil {
			body = string(ab.kept)
			if ab.truncated {
				body += "…(截断)"
			}
		}
		status := rw.code
		if status == 0 {
			status = http.StatusOK
		}
		d.auditLogf("%s | %s | %s %s%s | %d | %s | %s",
			time.Now().Format("2006-01-02 15:04:05.000"),
			clientIP(r), r.Method, redactPath(r.URL.Path), redactQuery(r.URL.RawQuery),
			status, time.Since(start).Round(time.Millisecond), body)
	})
}
