// IriX Node Daemon
// IriX 本地节点守护进程（Go 实现）。
//
// 提供与 MCSManager 面板一致风格的 HTTP API（见 apis/node_api.md），
// 使得 IriX 客户端可以用同一套客户端代码同时管理 MCSM 节点与本节点。
// 本守护进程使用纯标准库实现，无任何外部依赖，单文件 go build 即可运行。

package main

import (
	"context"
	"crypto/tls"
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
		configPath     = flag.String("config", "config.json", "配置文件路径（JSON，不存在则首次启动自动生成示例配置）；全部启动参数均可写入配置文件（字段见 config.example.json），命令行显式参数优先级高于配置文件")
		port           = flag.Int("port", 12346, "监听端口（1-65535）")
		bindHost       = flag.String("bind", "", "监听地址（IP 或主机名，如 127.0.0.1 / 0.0.0.0 / 192.168.1.5 / ::）；留空时依次读配置文件 bind、IRIX_NODE_BIND_ALL 环境变量（=1 则 0.0.0.0），否则默认 127.0.0.1")
		dataDir        = flag.String("data", "", "数据目录（实例配置等，默认当前目录）")
		apiKey         = flag.String("apikey", "", "可选固定 API 密钥；留空则启用配对码机制（首次启动生成 20 位随机配对码，仅显示一次）")
		instanceLog    = flag.Bool("instance-log", true, "将实例输出日志异步落盘到 {data}/logs/（关闭则仅内存环形缓冲）")
		instanceLogMB  = flag.Int("instance-log-max", 64, "单实例日志文件轮转上限（MB，超过后轮转为 .1 保留最近一个）")
		logBufferKB    = flag.Int("log-buffer-kb", 0, "每实例内存日志环形缓冲上限（KB，0 = 用默认值；嵌入式档位默认 512KB，否则 2MB）")
		logLines       = flag.Int("log-lines", 0, "每实例断线重连补发行缓冲上限（行，0 = 用默认值；嵌入式档位默认 300，否则 1000）")
		auditLog       = flag.Bool("audit-log", true, "将用户操作审计日志异步落盘到 {data}/logs/audit.log（记录每次 API 请求的完整细节）")
		auditLogMB     = flag.Int("audit-log-max", 64, "审计日志文件轮转上限（MB，超过后轮转为 .1 保留最近一个）")
		loadTune       = flag.Bool("load-tune", true, "根据节点自身负载动态调整 GOMAXPROCS 与 GOGC（负载自适应调谐，状态见 GET /api/load）")
		lowResource    = flag.Bool("low-resource", false, "强制启用嵌入式预设（更小的每实例日志缓冲/内存软上限 GOMEMLIMIT/更低的 PBKDF2 默认迭代）；留空则按 CPU 核数与物理内存自动判定（≤2 核或 ≤512MB 即套用）")
		transferCIDR   = flag.String("transfer-allow-cidr", "", "集群拉取（POST /api/cluster/transfer）额外放行的内网 CIDR 列表（逗号分隔，如 192.168.0.0/16,10.0.0.0/8）；默认拒绝全部 RFC1918 内网地址，集群 LAN 节点间直传需显式配置（环回/链路本地/本机地址任何配置都不可放行）")
		tlsMode        = flag.String("tls-mode", "off", "TLS 模式：off（默认，明文 HTTP，与既有部署兼容）/ auto（自动生成自签证书，启动日志打印指纹，客户端按指纹固定校验）/ manual（使用 -tls-cert / -tls-key 指定的正式证书）；开启加密保险库（-vault）时强制要求 TLS")
		tlsCertFile    = flag.String("tls-cert", "", "manual 模式：TLS 证书文件路径（PEM）")
		tlsKeyFile     = flag.String("tls-key", "", "manual 模式：TLS 私钥文件路径（PEM）")
		vaultEnabled   = flag.Bool("vault", false, "启用加密保险库：数据加密存储，访问需 TOTP+密码+证书签名解锁（见 docs/vault-design.md；开启时强制要求 TLS，否则拒绝启动）")
		vaultIdle      = flag.Int("vault-idle-timeout", 30, "保险库解锁会话空闲超时（分钟），到期自动锁定")
		vaultAttempts  = flag.Int("vault-max-attempts", 5, "保险库 unlock/recovery/初始化验证的统一失败限速阈值（用户+IP 双维度）")
		vaultLockout   = flag.Int("vault-lockout-minutes", 15, "保险库失败限速触发后的锁定时长（分钟）")
		vaultKDFIter   = flag.Int("vault-pbkdf2-iterations", 0, "保险库密码派生 KEK 的 PBKDF2 迭代次数（0 = 按平台架构自适应：arm64 等具硬件 SHA 用 600000，MIPS/armv7 无硬件加速用更低默认）")
		vaultPwMinLen  = flag.Int("vault-password-min-length", 12, "保险库密码最小长度（须含大写、小写与数字）")
		vaultPwExpire  = flag.Int("vault-password-expire-days", 90, "保险库密码有效期（天，0=不过期），到期解锁响应提示 passwordExpired")
		vaultForceExp  = flag.Bool("vault-force-expire", false, "保险库密码到期强制改密：解锁请求必须携带 newPassword 同请求完成解锁+改密")
		vaultBindIP    = flag.Bool("vault-bind-session-ip", false, "保险库会话令牌绑定来源 IP（防令牌跨机器盗用；动态 IP 场景请保持关闭）")
		vaultBlockKB   = flag.Int("vault-block-size-kb", 1024, "保险库密文对象块大小（KB，1-65536；分块随机读的粒度）")
		vaultScrub     = flag.Bool("vault-scrub-on-delete", false, "保险库回收/删除明文前覆盖随机数据（best-effort，防介质残留）")
		vaultFilesDef  = flag.String("vault-default-files-mode", "plaintext", "保险库新实例文件区默认模式：plaintext（明文，默认）/ materialize（启停物化加密）")
		accountsDriver = flag.String("accounts-driver", "sqlite", "账户管理数据库驱动：sqlite（默认，{data}/accounts.db）/ mysql / postgres（需配合 -accounts-dsn 连接串；连接池参数见配置文件 accounts 块）")
		accountsDSN    = flag.String("accounts-dsn", "", "账户管理数据库连接串（sqlite 为文件路径，空 = {data}/accounts.db；mysql 如 user:pass@tcp(127.0.0.1:3306)/irix?charset=utf8mb4；postgres 如 postgres://user:pass@127.0.0.1:5432/irix?sslmode=disable）")
		redisAddr      = flag.String("redis-addr", "", "Redis 地址（如 127.0.0.1:6379；空 = 不启用 Redis 缓存，账户会话与权限直接走数据库）")
		redisPassword  = flag.String("redis-password", "", "Redis 密码（无密码留空）")
		redisDB        = flag.Int("redis-db", 0, "Redis 库号（0-15）")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "IriX Node Daemon - 本地节点服务\n\n")
		fmt.Fprintf(os.Stderr, "用法: irix-node [选项]\n\n")
		fmt.Fprintf(os.Stderr, "所有参数均可写入配置文件（默认 ./config.json，可用 -config 指定路径，\n")
		fmt.Fprintf(os.Stderr, "首次启动无配置文件时自动生成示例配置；字段说明见 config.example.json）。\n")
		fmt.Fprintf(os.Stderr, "优先级：命令行显式参数 > 配置文件 > 默认值。\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  irix-node\n")
		fmt.Fprintf(os.Stderr, "  irix-node -config /etc/irix-node/config.json  # 用配置文件启动（推荐）\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -port 12346 -data C:\\irix-node-data -apikey mykey\n")
		fmt.Fprintf(os.Stderr, "  irix-node -bind 0.0.0.0 -port 23333 -apikey mykey  # 监听全部网卡的 23333 端口（局域网可访问）\n")
		fmt.Fprintf(os.Stderr, "  irix-node -bind 192.168.1.5 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -instance-log-max 128 -data C:\\irix-node-data\n")
		fmt.Fprintf(os.Stderr, "  irix-node -audit-log=false  # 关闭审计日志落盘（stderr 仍输出审计行）\n")
		fmt.Fprintf(os.Stderr, "  irix-node -transfer-allow-cidr 192.168.0.0/16  # 集群 LAN 直传放行内网网段\n")
		fmt.Fprintf(os.Stderr, "  irix-node -tls-mode auto              # 开启 TLS（自签证书，按启动日志指纹校验）\n")
		fmt.Fprintf(os.Stderr, "  irix-node -tls-mode auto -vault       # 开启 TLS 与加密保险库（TOTP+密码+证书解锁）\n")
	}
	flag.Parse()

	// 命令行显式设置的参数名集合：这些参数不被配置文件覆盖
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	// 首次启动无配置文件时自动生成示例配置（生成失败仅告警，不阻断启动）
	if created, err := ensureConfigFile(*configPath); err != nil {
		log.Printf("警告: 自动生成配置文件 %s 失败: %v（将继续按命令行参数与默认值启动）", *configPath, err)
	} else if created {
		log.Printf("未检测到配置文件 %s，已自动生成示例配置（字段说明见文件内注释，按需修改后重启生效）", *configPath)
	}

	// 加载配置文件并合并启动选项（不存在则静默跳过，与纯命令行启动一致）
	cfg, cfgLoaded, err := loadConfigFile(*configPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	opts := &nodeOptions{
		Port:                  *port,
		BindHost:              *bindHost,
		DataDir:               *dataDir,
		APIKey:                *apiKey,
		InstanceLog:           *instanceLog,
		InstanceLogMax:        *instanceLogMB,
		AuditLog:              *auditLog,
		AuditLogMax:           *auditLogMB,
		LoadTune:              *loadTune,
		LogBufferKB:           *logBufferKB,
		LogLines:              *logLines,
		TransferAllowCIDR:     *transferCIDR,
		TLSMode:               *tlsMode,
		TLSCert:               *tlsCertFile,
		TLSKey:                *tlsKeyFile,
		VaultEnabled:          *vaultEnabled,
		VaultIdleTimeout:      *vaultIdle,
		VaultMaxAttempts:      *vaultAttempts,
		VaultLockoutMinutes:   *vaultLockout,
		VaultPBKDF2Iterations: *vaultKDFIter,
		VaultPasswordMinLen:   *vaultPwMinLen,
		VaultPasswordExpire:   *vaultPwExpire,
		VaultForceExpire:      *vaultForceExp,
		VaultBindSessionIP:    *vaultBindIP,
		VaultBlockSizeKB:      *vaultBlockKB,
		VaultScrubOnDelete:    *vaultScrub,
		VaultDefaultFilesMode: *vaultFilesDef,
		AccountsDriver:        *accountsDriver,
		AccountsDSN:           *accountsDSN,
		RedisAddr:             *redisAddr,
		RedisPassword:         *redisPassword,
		RedisDB:               *redisDB,
	}
	opts.applyConfig(cfg, setFlags)

	// 应用嵌入式/低资源预设开关：命令行显式设置时按用户意图强制开/关，
	// 配置文件显式设置时同理；两者都未设则保持自动检测结果（nil）。
	applyLowResourceExplicit := func() {
		var forced *bool
		switch {
		case setFlags["low-resource"]:
			v := *lowResource
			forced = &v
		case cfg != nil && cfg.LowResource != nil:
			forced = cfg.LowResource
		}
		embedded.applyLowResourceOverride(forced)
	}
	applyLowResourceExplicit()
	alog.Printf("嵌入式/低资源档位：%v（原因：%s，CPU=%d 核，内存=%dMB）",
		embedded.isEmbedded(), embedded.reason, embedded.cpu, embedded.mem>>20)

	if opts.Port <= 0 || opts.Port > 65535 {
		log.Fatalf("端口无效: %d（须在 1-65535 之间；请检查命令行参数或配置文件 %s）", opts.Port, *configPath)
	}
	if opts.InstanceLogMax < 1 || opts.AuditLogMax < 1 {
		log.Fatalf("日志轮转上限无效: 实例 %dMB / 审计 %dMB（须 ≥1；请检查命令行参数或配置文件 %s）",
			opts.InstanceLogMax, opts.AuditLogMax, *configPath)
	}
	switch opts.TLSMode {
	case "off", "auto", "manual":
	default:
		log.Fatalf("tls-mode 无效: %q（须为 off / auto / manual；请检查命令行参数或配置文件 %s）", opts.TLSMode, *configPath)
	}
	if opts.TLSMode == "manual" && (opts.TLSCert == "" || opts.TLSKey == "") {
		log.Fatalf("tls-mode=manual 需要同时配置 tls-cert 与 tls-key（请检查命令行参数或配置文件 %s）", *configPath)
	}
	if opts.VaultEnabled && opts.TLSMode == "off" {
		log.Fatalf("启用加密保险库（-vault）必须开启 TLS（tls-mode=auto 或 manual）：密码/TOTP/签名明文传输将完全破坏 Vault 的安全模型")
	}
	if opts.VaultEnabled {
		if opts.VaultIdleTimeout < 1 {
			log.Fatalf("vault-idle-timeout 无效: %d 分钟（须 ≥1）", opts.VaultIdleTimeout)
		}
		if opts.VaultMaxAttempts < 1 {
			log.Fatalf("vault-max-attempts 无效: %d（须 ≥1）", opts.VaultMaxAttempts)
		}
		if opts.VaultLockoutMinutes < 1 {
			log.Fatalf("vault-lockout-minutes 无效: %d 分钟（须 ≥1）", opts.VaultLockoutMinutes)
		}
		if opts.VaultPBKDF2Iterations < 10000 {
			log.Fatalf("vault-pbkdf2-iterations 无效: %d（须 ≥10000）", opts.VaultPBKDF2Iterations)
		}
		if opts.VaultPasswordMinLen < 1 {
			log.Fatalf("vault-password-min-length 无效: %d（须 ≥1）", opts.VaultPasswordMinLen)
		}
		if opts.VaultPasswordExpire < 0 {
			log.Fatalf("vault-password-expire-days 无效: %d（须 ≥0）", opts.VaultPasswordExpire)
		}
		if opts.VaultBlockSizeKB < 1 || opts.VaultBlockSizeKB > 65536 {
			log.Fatalf("vault-block-size-kb 无效: %d（须在 1-65536 之间）", opts.VaultBlockSizeKB)
		}
		switch opts.VaultDefaultFilesMode {
		case "plaintext", "materialize":
		default:
			log.Fatalf("vault-default-files-mode 无效: %q（须为 plaintext 或 materialize）", opts.VaultDefaultFilesMode)
		}
	}
	if opts.DataDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatalf("无法获取当前目录: %v", err)
		}
		opts.DataDir = wd
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		log.Fatalf("无法创建数据目录 %s: %v", opts.DataDir, err)
	}

	d := NewDaemon(opts.DataDir, opts.APIKey)
	d.Port = opts.Port
	d.transferAllowCIDR = opts.TransferAllowCIDR
	if err := d.parseTransferAllowCIDR(); err != nil {
		log.Fatalf("transfer-allow-cidr 配置无效: %v（请检查命令行参数或配置文件 %s）", err, *configPath)
	}
	if opts.LoadTune {
		go tuner.loop() // 负载自适应调谐（后台 goroutine 周期采样）
	}
	logDir := filepath.Join(opts.DataDir, "logs")
	if opts.InstanceLog {
		d.LogDir = logDir
		d.LogMaxBytes = int64(opts.InstanceLogMax) << 20
	}
	// 每实例内存缓冲上限：命令行/配置显式设置优先；未设则沿用 NewDaemon 中的
	// 默认（嵌入式档位已自动调小，见 daemon.go）。
	if opts.LogBufferKB > 0 {
		d.LogBufferKB = opts.LogBufferKB
	}
	if opts.LogLines > 0 {
		d.LogLines = opts.LogLines
	}
	if opts.AuditLog {
		d.AuditLog = newFileLogger(logDir, "audit.log", int64(opts.AuditLogMax)<<20)
		// 审计日志轮转归档（等保二级「审计记录保护与定期备份」）：
		// 每次轮转把将被覆盖的审计段复制到 {data}/backup/audit/
		d.AuditLog.archiveDir = filepath.Join(opts.DataDir, "backup", "audit")
	}
	if err := d.Load(); err != nil {
		log.Fatalf("加载实例数据失败: %v", err)
	}
	d.frpLoad() // 加载 FRP 隧道列表（进程态重置为停止，由用户手动启动）

	// 账户管理初始化（docs/accounts-design.md）：默认 SQLite {data}/accounts.db，
	// 可选 MySQL/PostgreSQL + Redis 热缓存；连接池参数见配置文件 accounts 块。
	if err := d.initAccounts(accountsConfig{
		Driver:             opts.AccountsDriver,
		DSN:                opts.AccountsDSN,
		MaxOpen:            opts.AccountsMaxOpen,
		MaxIdle:            opts.AccountsMaxIdle,
		ConnMaxLifetimeMin: opts.AccountsConnMaxLifetimeMin,
		RedisAddr:          opts.RedisAddr,
		RedisPassword:      opts.RedisPassword,
		RedisDB:            opts.RedisDB,
		RedisPoolSize:      opts.RedisPoolSize,
	}); err != nil {
		log.Fatalf("账户管理初始化失败: %v（请检查 accounts 配置或 -accounts-* / -redis-* 参数）", err)
	}
	redisNote := ""
	if d.accounts.redis != nil {
		redisNote = "，Redis 缓存 " + d.accounts.redis.Options().Addr
	}
	alog.Printf("账户管理已就绪：驱动 %s，连接池上限 %d%s",
		d.accounts.driver, d.accounts.db.Stats().MaxOpenConnections, redisNote)

	// 加密保险库初始化（vault.go）：加载 vault.json、应用调优配置。
	// 数据面门禁（vaultGate）在下方处理链中挂载。
	if opts.VaultEnabled {
		d.vault.enabled = true
		d.vault.idleTimeout = time.Duration(opts.VaultIdleTimeout) * time.Minute
		d.vault.maxAttempts = opts.VaultMaxAttempts
		d.vault.lockoutDuration = time.Duration(opts.VaultLockoutMinutes) * time.Minute
		// PBKDF2 迭代：用户显式传 -vault-pbkdf2-iterations 时按用户意图，
		// 否则保留 newVaultState 中按平台架构设定的默认（arm64 600000；MIPS/armv7 更低）。
		if setFlags["vault-pbkdf2-iterations"] {
			d.vault.pbkdf2Iterations = opts.VaultPBKDF2Iterations
		}
		d.vault.passwordMinLen = opts.VaultPasswordMinLen
		d.vault.passwordExpire = time.Duration(opts.VaultPasswordExpire) * 24 * time.Hour
		d.vault.forceExpire = opts.VaultForceExpire
		d.vault.bindSessionIP = opts.VaultBindSessionIP
		d.vault.scrubOnDelete = opts.VaultScrubOnDelete
		d.vault.defaultFilesMode = opts.VaultDefaultFilesMode
		if opts.VaultBlockSizeKB != 0 {
			d.vault.blockSizeKB = opts.VaultBlockSizeKB
			d.vault.store.blockSize = opts.VaultBlockSizeKB * 1024
		}
		if err := d.vault.load(filepath.Join(opts.DataDir, "vault", "vault.json")); err != nil {
			log.Fatalf("加载保险库状态失败: %v", err)
		}
		if d.vault.initialized {
			alog.Printf("加密保险库已启用（已初始化，当前锁定：请先解锁再访问数据）")
		} else {
			alog.Printf("加密保险库已启用（未初始化：请调用 POST /api/vault/init 完成初始化）")
		}
	}

	// 自动启动标记了 AutoStart 的实例（异步，不阻塞 HTTP 服务就绪）。
	// vault 开启时跳过：重启后保险库处于锁定状态，必须先解锁才能操作实例。
	if opts.VaultEnabled {
		alog.Printf("保险库已启用：跳过实例自动启动（解锁后才能启动实例）")
	} else {
		for _, inst := range d.Instances {
			if inst.Config.EventTask.AutoStart {
				go func(inst *Instance) {
					if err := d.startInstance(inst); err != nil {
						alog.Printf("自动启动实例 %s 失败: %v", inst.InstanceUuid, err)
					}
				}(inst)
			}
		}
	}
	if opts.APIKey == "" {
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

	bind := resolveBind(opts.BindHost, os.Getenv("IRIX_NODE_BIND_ALL"))
	// 记录实际监听主机：下载/上传票据据此生成客户端可达的直连地址
	d.BindHost = bind
	addr := net.JoinHostPort(bind, strconv.Itoa(opts.Port))

	// TLS 初始化：auto 生成自签证书 / manual 加载正式证书 / off 保持明文（打印一行提示）
	var tlsCfg *tls.Config
	tlsFingerprint := ""
	switch opts.TLSMode {
	case "auto":
		tlsCfg, tlsFingerprint, err = ensureSelfSignedTLS(filepath.Join(opts.DataDir, "tls"))
		if err != nil {
			log.Fatalf("TLS 自签证书初始化失败: %v", err)
		}
	case "manual":
		tlsCfg, tlsFingerprint, err = loadManualTLS(opts.TLSCert, opts.TLSKey)
		if err != nil {
			log.Fatalf("%v", err)
		}
	default:
		alog.Printf("TLS 未开启（tls-mode=off），流量为明文传输；等保二级部署请设置 tls-mode=auto 或 manual")
	}

	// 显式监听并把监听器交给 Server：连接层日志需要包装 Accept。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
	// TLS 开启时在监听器上包一层 tls.NewListener（日志包装保持最外层）
	var serveLn net.Listener = &connLogListener{Listener: ln, d: d}
	scheme := "http"
	if tlsCfg != nil {
		scheme = "https"
		serveLn = &connLogListener{Listener: tls.NewListener(ln, tlsCfg), d: d}
		if tlsFingerprint != "" {
			alog.Printf("TLS 已开启（%s 模式），证书指纹（SHA-256）: %s", opts.TLSMode, tlsFingerprint)
		} else {
			alog.Printf("TLS 已开启（%s 模式）", opts.TLSMode)
		}
	}

	mux := http.NewServeMux()
	d.RegisterRoutes(mux)

	srv := &http.Server{
		Addr: addr,
		// 处理链：审计（含预检）→ CORS（预检在此终结，错误响应同样带 CORS 头）→
		// 保险库数据面门禁 → 请求体限额 → 业务路由
		Handler: d.auditMiddleware(corsMiddleware(d.vaultGate(limitAPIBody(mux)))),
		// 只限制读取请求头与空闲连接：防 slowloris 占用连接。
		// 不设 ReadTimeout/WriteTimeout，否则大文件上传/下载会被中途切断。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	d.auditLogf("IriX Node Daemon 已启动 (version %s, go %s)", Version, runtime.Version())
	if cfgLoaded {
		d.auditLogf("已加载配置文件: %s", *configPath)
	}
	d.auditLogf("数据目录: %s", opts.DataDir)
	d.auditLogf("监听地址: %s://%s/api/overview", scheme, addr)
	if d.LogDir != "" {
		alog.Printf("实例日志落盘: %s（单文件上限 %dMB，轮转保留 .1）", d.LogDir, opts.InstanceLogMax)
	}
	if d.AuditLog != nil {
		alog.Printf("审计日志落盘: %s（单文件上限 %dMB，轮转保留 .1）", filepath.Join(logDir, "audit.log"), opts.AuditLogMax)
	}
	if opts.APIKey == "" {
		alog.Printf("已启用配对码认证：所有 API 请求需携带配对码（apikey 参数或 X-Api-Key 头）")
	}

	// 忽略 SIGHUP：SSH/终端前台启动的节点在会话断开时不应被杀
	// （否则端口静默关闭，客户端表现为「网络错误」且服务器无任何日志）。
	// systemd/rc.d 等服务管理器不会发送 SIGHUP，忽略它不影响优雅关停。
	signal.Ignore(syscall.SIGHUP)
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
		d.frpStopAll()    // 停止全部 FRP 隧道进程
		d.closeAccounts() // 关闭账户数据库与 Redis 连接池
		// 保险库：进程退出前落盘加密索引（解锁状态下的最后变更不能丢）
		if d.vault != nil && d.vault.enabled && d.vault.unlockedSafe() {
			if err := d.vault.store.flush(); err != nil {
				alog.Printf("保险库索引落盘失败: %v", err)
			}
			d.auditLogf("保险库索引已落盘（关停）")
		}
		close(stopped)
	}()

	if err := srv.Serve(serveLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// connLogListener 包装 net.Listener：记录每次接受到的连接来源与 Accept 错误。
// 审计中间件只覆盖到达 HTTP 层的请求；连接层失败（SYN 未到达节点、被防火墙
// 丢弃、半开等）在日志中天然不可见，排查「客户端网络错误」时全靠猜。
// 包装后「客户端是否连到节点」在审计日志中一眼可见。
type connLogListener struct {
	net.Listener
	d *Daemon
}

// Accept 接受连接并记录来源；Accept 出错时记录非关闭类错误（如句柄耗尽）。
func (l *connLogListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			l.d.auditLogf("接受连接失败: %v", err)
		}
		return nil, err
	}
	l.d.auditLogf("接受到来自 %s 的连接", c.RemoteAddr())
	return c, err
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
// 1.1.0：新增实时控制台 WebSocket、实例日志持久化查询、Java 检测/安装、
// 核心下载、导入目录（docs/irix-node-local-parity.md M1）。
const Version = "1.1.0"

// resolveBind 解析监听地址：显式 bind（-bind 参数或配置文件 bind 字段，
// 已合并进 flagValue）优先；留空时读 IRIX_NODE_BIND_ALL 环境变量
// （=1 则 0.0.0.0），否则默认 127.0.0.1。
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

// forbiddenCwdDirs 返回系统目录黑名单：实例工作目录位于这些目录内
// （含本身）时拒绝创建/更新。防止认证后把 cwd 指向系统目录，配合文件 API
// 形成全盘读写（审计报告 #4）。
// Windows 含 \Users（各用户 Profile 根）：Profile 内是横向渗透的高价值目标
// （浏览器凭据 / .ssh 密钥 / 启动目录持久化），整树拒绝，仅豁免系统临时目录
// （见 validateCwd）。Unix 的 /home 未列入：Linux 游戏服常驻 /home 且无
// Profile 凭据集中存放问题，敏感子目录（.ssh 等）建议用独立账户/系统策略。
func forbiddenCwdDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{
			`\Windows`, `\Program Files`, `\Program Files (x86)`,
			`\ProgramData`, `\Users`, `\System Volume Information`, `\$Recycle.Bin`,
		}
	}
	return []string{
		"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32",
		"/boot", "/dev", "/proc", "/sys", "/var/log", "/var/run", "/root",
	}
}

// validateCwd 校验实例工作目录：拒绝空目录、文件系统/磁盘根目录与系统目录。
// 相对路径按守护进程当前目录解析为绝对路径后再做黑名单比对。
func validateCwd(cwd string) error {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return errors.New("工作目录不能为空")
	}
	cleaned := filepath.Clean(cwd)
	if cleaned == string(filepath.Separator) {
		return fmt.Errorf("工作目录不能是文件系统根目录: %s", cwd)
	}
	if runtime.GOOS == "windows" {
		if isDriveRoot(cleaned) {
			return fmt.Errorf("工作目录不能是磁盘根目录（含盘符根与 UNC 共享根）: %s", cwd)
		}
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return fmt.Errorf("工作目录解析失败: %v", err)
	}
	for _, bad := range forbiddenCwdDirs() {
		// Windows 黑名单为卷内相对路径（如 \Users）：挂到各盘符卷根后比对；
		// Unix 黑名单为绝对路径，直接比对。
		base := bad
		if runtime.GOOS == "windows" {
			vol := filepath.VolumeName(abs)
			if vol == "" {
				continue
			}
			base = filepath.Clean(vol + bad)
		}
		if pathWithin(base, abs) {
			// \Users 整树拒绝，但豁免系统临时目录（%TEMP%，通常为
			// <用户>\AppData\Local\Temp）：临时目录无浏览器凭据/SSH 密钥/
			// 启动项等敏感数据，且大量部署与测试依赖它（Go 测试临时目录）。
			if runtime.GOOS == "windows" && bad == `\Users` && pathWithin(os.TempDir(), abs) {
				continue
			}
			return fmt.Errorf("工作目录不能位于系统目录内: %s", cwd)
		}
	}
	return nil
}

// normalizeCwd 校验并规范化实例工作目录：相对路径按节点进程当前目录
// （systemd 下为 /）转成绝对路径后返回，供入库保存。
// 此前相对路径被原样保存，同一实例的 cwd 会随节点启动目录漂移，
// 导致进程在错误目录启动（相对 jar 找不到、配置写错位置）。
func normalizeCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", errors.New("工作目录不能为空")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("工作目录解析失败: %v", err)
	}
	abs = filepath.Clean(abs)
	if err := validateCwd(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// isDriveRoot 判断 Windows 路径是否为磁盘根（C:\、C:）或 UNC 共享根（\\server\share）。
// 注意 filepath.Clean("C:") 会得到 "C:."（盘符相对路径），尾点同样视为根。
func isDriveRoot(p string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	if vol == "" {
		return false
	}
	switch strings.TrimPrefix(p, vol) {
	case "", "/", "\\", ".":
		return true
	}
	return false
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
