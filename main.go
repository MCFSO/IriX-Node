// IriX Node Daemon
// IriX 本地节点守护进程（Go 实现）。
//
// 提供与 MCSManager 面板一致风格的 HTTP API（见 apis/node_api.md），
// 使得 IriX 客户端可以用同一套客户端代码同时管理 MCSM 节点与本节点。
// 本守护进程使用纯标准库实现，无任何外部依赖，单文件 go build 即可运行。

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		port    = flag.Int("port", 12346, "监听端口")
		dataDir = flag.String("data", "", "数据目录（实例配置等，默认当前目录）")
		apiKey  = flag.String("apikey", "", "可选固定 API 密钥；留空则启用配对码机制（首次启动生成 20 位随机配对码，仅显示一次）")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "IriX Node Daemon - 本地节点服务\n\n")
		fmt.Fprintf(os.Stderr, "用法: irix-node [选项]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  irix-node\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data -apikey mykey\n")
	}
	flag.Parse()

	if *dataDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatalf("无法获取当前目录: %v", err)
		}
		*dataDir = wd
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("无法创建数据目录 %s: %v", *dataDir, err)
	}

	d := NewDaemon(*dataDir, *apiKey)
	d.Port = *port
	if err := d.Load(); err != nil {
		log.Fatalf("加载实例数据失败: %v", err)
	}
	// 自动启动标记了 AutoStart 的实例（异步，不阻塞 HTTP 服务就绪）
	for _, inst := range d.Instances {
		if inst.Config.EventTask.AutoStart {
			go func(uuid string) {
				if err := d.startInstance(uuid); err != nil {
					log.Printf("自动启动实例 %s 失败: %v", uuid, err)
				}
			}(inst.InstanceUuid)
		}
	}
	if *apiKey == "" {
		code, isNew, err := d.LoadPairing()
		if err != nil {
			log.Fatalf("初始化配对码失败: %v", err)
		}
		if isNew {
			log.Printf("======================================================")
			log.Printf("首次启动：已生成配对码（仅此一次显示，请立即记录）")
			log.Printf("")
			log.Printf("  配对码: %s", code)
			log.Printf("")
			log.Printf("后续所有 API 请求必须携带配对码：")
			log.Printf("  ?apikey=%s 或请求头 X-Api-Key: %s", code, code)
			log.Printf("======================================================")
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	if strings.EqualFold(os.Getenv("IRIX_NODE_BIND_ALL"), "1") {
		addr = fmt.Sprintf("0.0.0.0:%d", *port)
	}

	mux := http.NewServeMux()
	d.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:    addr,
		Handler: logMiddleware(limitAPIBody(mux)),
		// 只限制读取请求头与空闲连接：防 slowloris 占用连接。
		// 不设 ReadTimeout/WriteTimeout，否则大文件上传/下载会被中途切断。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("IriX Node Daemon 已启动 (version %s, go %s)", Version, runtime.Version())
	log.Printf("数据目录: %s", *dataDir)
	log.Printf("监听地址: http://%s/api/overview", addr)
	if *apiKey == "" {
		log.Printf("已启用配对码认证：所有 API 请求需携带配对码（apikey 参数或 X-Api-Key 头）")
	}

	// 优雅关停：停止接受新请求，等待在途请求，再关停子进程避免孤儿进程
	stopped := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signals
		log.Printf("收到信号 %v，开始优雅关停…", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("HTTP 关停未在超时内完成: %v", err)
		}
		// 关停实例：先发送停止命令，超时后强杀，避免留下无人管理的孤儿进程
		d.StopAll(30 * time.Second)
		close(stopped)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
	<-stopped
	log.Printf("已退出")
}

// maxAPIBodyBytes API 请求体上限（不含 /upload/ 直连通道）。
// 文件写入接口把整个正文读进内存并做 JSON 解码，无上限时单请求可放大数倍内存。
const maxAPIBodyBytes = 16 << 20 // 16 MiB

// limitAPIBody 为 /api/ 路由的请求体设置大小上限。
// /upload/ 直连通道不限制：它通过 multipart 落盘，内存占用恒定。
func limitAPIBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && strings.HasPrefix(r.URL.Path, "/api/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// logMiddleware 记录所有 API 请求。
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}()
		next.ServeHTTP(w, r)
	})
}

// Version 守护进程版本号。
const Version = "1.0.0"

// NormalizePath 将 API 传入的路径规范化为相对目录的绝对路径。
//
// 路径以 '/' 开头表示实例工作目录（cwd）的根；Windows 盘符路径（如 C:\x）
// 直接使用。任何试图逃逸 cwd 的路径（.. 越界）都会被拒绝。
func NormalizePath(cwd, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return cwd, nil
	}
	var full string
	if filepath.IsAbs(target) {
		full = filepath.Clean(target)
	} else {
		full = filepath.Clean(filepath.Join(cwd, target))
	}
	if !pathWithin(cwd, full) {
		return "", fmt.Errorf("路径越界: %s", target)
	}
	return full, nil
}

// pathWithin 判断 p 是否位于 base 之内（允许相等）。
func pathWithin(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// SplitCommand 将启动命令字符串拆分为参数列表，支持单/双引号。
func SplitCommand(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// FormatSize 将字节数格式化为人类可读字符串。
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
