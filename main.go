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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		port          = flag.Int("port", 12346, "监听端口（1-65535）")
		bindHost      = flag.String("bind", "", "监听地址（IP 或主机名，如 127.0.0.1 / 0.0.0.0 / 192.168.1.5 / ::）；留空时读 IRIX_NODE_BIND_ALL 环境变量（=1 则 0.0.0.0），否则默认 127.0.0.1")
		dataDir       = flag.String("data", "", "数据目录（实例配置等，默认当前目录）")
		apiKey        = flag.String("apikey", "", "可选固定 API 密钥；留空则启用配对码机制（首次启动生成 20 位随机配对码，仅显示一次）")
		instanceLog   = flag.Bool("instance-log", true, "将实例输出日志异步落盘到 {data}/logs/（关闭则仅内存环形缓冲）")
		instanceLogMB = flag.Int("instance-log-max", 64, "单实例日志文件轮转上限（MB，超过后轮转为 .1 保留最近一个）")
		auditLog      = flag.Bool("audit-log", true, "将用户操作审计日志异步落盘到 {data}/logs/audit.log（记录每次 API 请求的完整细节）")
		auditLogMB    = flag.Int("audit-log-max", 64, "审计日志文件轮转上限（MB，超过后轮转为 .1 保留最近一个）")
		loadTune      = flag.Bool("load-tune", true, "根据节点自身负载动态调整 GOMAXPROCS 与 GOGC（负载自适应调谐，状态见 GET /api/load）")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "IriX Node Daemon - 本地节点服务\n\n")
		fmt.Fprintf(os.Stderr, "用法: irix-node [选项]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  irix-node\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data -apikey mykey\n")
		fmt.Fprintf(os.Stderr, "  irix-node -bind 0.0.0.0 -port 23333 -apikey mykey  # 监听全部网卡的 23333 端口（局域网可访问）\n")
		fmt.Fprintf(os.Stderr, "  irix-node -bind 192.168.1.5 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -instance-log-max 128 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -audit-log=false  # 关闭审计日志落盘（stderr 仍输出审计行）\n")
	}
	flag.Parse()

	if *port <= 0 || *port > 65535 {
		log.Fatalf("端口无效: %d（须在 1-65535 之间）", *port)
	}
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
	if *loadTune {
		go tuner.loop() // 负载自适应调谐（后台 goroutine 周期采样）
	}
	logDir := filepath.Join(*dataDir, "logs")
	if *instanceLog {
		d.LogDir = logDir
		d.LogMaxBytes = int64(*instanceLogMB) << 20
	}
	if *auditLog {
		d.AuditLog = newFileLogger(logDir, "audit.log", int64(*auditLogMB)<<20)
	}
	if err := d.Load(); err != nil {
		log.Fatalf("加载实例数据失败: %v", err)
	}
	// 自动启动标记了 AutoStart 的实例（异步，不阻塞 HTTP 服务就绪）
	for _, inst := range d.Instances {
		if inst.Config.EventTask.AutoStart {
			go func(inst *Instance) {
				if err := d.startInstance(inst); err != nil {
					alog.Printf("自动启动实例 %s 失败: %v", inst.InstanceUuid, err)
				}
			}(inst)
		}
	}
	if *apiKey == "" {
		code, isNew, err := d.LoadPairing()
		if err != nil {
			log.Fatalf("初始化配对码失败: %v", err)
		}
		if isNew {
			alog.Printf("======================================================")
			alog.Printf("首次启动：已生成配对码（仅此一次显示，请立即记录）")
			alog.Printf("")
			alog.Printf("  配对码: %s", code)
			alog.Printf("")
			alog.Printf("后续所有 API 请求必须携带配对码：")
			alog.Printf("  ?apikey=%s 或请求头 X-Api-Key: %s", code, code)
			alog.Printf("======================================================")
		}
	}

	bind := resolveBind(*bindHost, os.Getenv("IRIX_NODE_BIND_ALL"))
	// 记录实际监听主机：下载/上传票据据此生成客户端可达的直连地址
	d.BindHost = bind
	addr := net.JoinHostPort(bind, strconv.Itoa(*port))

	mux := http.NewServeMux()
	d.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:    addr,
		Handler: d.auditMiddleware(limitAPIBody(mux)),
		// 只限制读取请求头与空闲连接：防 slowloris 占用连接。
		// 不设 ReadTimeout/WriteTimeout，否则大文件上传/下载会被中途切断。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	d.auditLogf("IriX Node Daemon 已启动 (version %s, go %s)", Version, runtime.Version())
	d.auditLogf("数据目录: %s", *dataDir)
	d.auditLogf("监听地址: http://%s/api/overview", addr)
	if d.LogDir != "" {
		alog.Printf("实例日志落盘: %s（单文件上限 %dMB，轮转保留 .1）", d.LogDir, *instanceLogMB)
	}
	if d.AuditLog != nil {
		alog.Printf("审计日志落盘: %s（单文件上限 %dMB，轮转保留 .1）", filepath.Join(logDir, "audit.log"), *auditLogMB)
	}
	if *apiKey == "" {
		alog.Printf("已启用配对码认证：所有 API 请求需携带配对码（apikey 参数或 X-Api-Key 头）")
	}

	// 优雅关停：停止接受新请求，等待在途请求，再关停子进程避免孤儿进程
	stopped := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signals
		alog.Printf("收到信号 %v，开始优雅关停…", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			alog.Printf("HTTP 关停未在超时内完成: %v", err)
		}
		// 关停实例：先发送停止命令，超时后强杀，避免留下无人管理的孤儿进程
		d.StopAll(30 * time.Second)
		close(stopped)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
	<-stopped
	d.auditLogf("已退出")
	// 先排空审计落盘，再排空 stderr 异步日志，等待全部日志写出后退出
	if d.AuditLog != nil {
		d.AuditLog.Close()
	}
	alog.Close()
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

// Version 守护进程版本号。
const Version = "1.0.0"

// resolveBind 解析监听地址：-bind 显式指定优先；留空时读 IRIX_NODE_BIND_ALL
// 环境变量（=1 则 0.0.0.0），否则默认 127.0.0.1。
func resolveBind(flagValue, envValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue)
	}
	if strings.EqualFold(strings.TrimSpace(envValue), "1") {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// NormalizePath 将 API 传入的路径规范化为相对目录的绝对路径。
//
// 路径以 '/' 开头表示实例工作目录（cwd）的根（跨平台一致）；
// 已是文件系统绝对路径且位于 cwd 内（cwd 本身或其子目录）时按原样使用；
// 仅 Windows 盘符绝对路径（如 C:\x）例外——直接使用，越界即拒绝。
// 任何试图逃逸 cwd 的路径（.. 越界）都会被拒绝。
func NormalizePath(cwd, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return cwd, nil
	}
	cleaned := filepath.Clean(target)
	if filepath.IsAbs(cleaned) {
		if isDriveAbs(cleaned) {
			// Windows 盘符绝对路径：只接受位于 cwd 内的，否则拒绝
			if !pathWithin(cwd, cleaned) {
				return "", fmt.Errorf("路径越界: %s", target)
			}
			return cleaned, nil
		}
		// Unix 绝对路径：位于 cwd 内（cwd 本身或其子目录）则原样使用，
		// 否则视为 cwd 根下的相对路径（/uploads 约定，与 Windows 一致）
		if pathWithin(cwd, cleaned) {
			return cleaned, nil
		}
	}
	full := filepath.Clean(filepath.Join(cwd, target))
	if !pathWithin(cwd, full) {
		return "", fmt.Errorf("路径越界: %s", target)
	}
	return full, nil
}

// isDriveAbs 判断是否为 Windows 盘符绝对路径（C:\x 或 C:/x）。
func isDriveAbs(p string) bool {
	return runtime.GOOS == "windows" && len(p) >= 3 && p[1] == ':' &&
		(p[2] == '/' || p[2] == '\\')
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

// atoiDefault 解析整数字符串，非法或为空时返回默认值。
func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}
