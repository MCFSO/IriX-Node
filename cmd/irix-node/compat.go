// 第三方 Daemon 协议兼容层（config.json 的 compat 块）。
//
// 目标：让现成的第三方控制面板可以把本节点当成它们自己的 Daemon 使用——
//   - MCSManager 面板：按「节点」连接（socket.io 事件通道 + 直连文件通道）；
//   - MSLX WebPanel：按「本机/节点 Daemon」连接（REST api/... + SignalR 控制台）。
//
// 两条兼容层可以同时开启：各自挂在独立子路径前缀下（默认 /mcsm 与 /mslx），
// 互不干扰，也不与节点自身 API 冲突。
//
// 实现约束（必须遵守）：
//   - 底层处理逻辑全部为 irix-node 自有代码，只对齐线上协议与字段语义，
//     **不得引入**这两个项目的源码（MCSManager 为 Apache-2.0、MSLX 另有许可证，
//     直接挪用会造成许可证冲突）；
//   - 兼容层复用本节点既有能力（d.startInstance/stopInstance、tickets、
//     NormalizePath/safeResolve、sysinfo、timesys 等），不新增依赖；
//   - 认证不在节点账户体系内：MCSM 用节点密钥、MSLX 用 JWT，均只做协议侧校验，
//     来源可另用 allowPanelIps 限制。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"math"

	"irix/node/internal/sysinfo"
	"irix/node/internal/timesys"
)

// 兼容层默认监听端口（对齐各自 Daemon 的默认端口，面板直接照抄即可）。
const (
	defaultMCSMCompatPort = 24444 // MCSManager daemon 默认端口
	defaultMSLXCompatPort = 7891  // MSLX daemon 默认端口
)

// 兼容层视图使用的默认值（对齐 MCSM InstanceConfig 的缺省字段）。
const (
	mcDefaultPTYCol        = 164
	mcDefaultPTYRow        = 40
	mcDefaultStopTimeoutMS = 0
)

// mcsmCompat MCSManager Daemon 兼容层状态。
type mcsmCompat struct {
	d     *Daemon
	key   string
	bind  string // 监听地址（已解析，host:port 形式见 addr）
	port  int    // 监听端口（0 = 由系统分配，测试用）
	addr  string // 实际监听地址（startCompatServers 成功后填入）
	allow []*net.IPNet
	// version 上报给面板的版本号（空 = 用本节点 Version）；
	// 面板按主次版本号比较，配成面板期望值可消除「有更新」红字。
	version string
	// pingIv / pingTo engine.io 心跳参数（0 = 用默认 25s/20s）。
	// 挂在实例上而非包级变量：测试可压低间隔覆盖心跳分支，且不会与
	// 其它会话的心跳 goroutine 产生数据竞争。
	pingIv time.Duration
	pingTo time.Duration

	mu       sync.Mutex
	sessions map[string]*eioSession   // sid → 会话
	missions map[string]*mcMission    // 一次性通行证（下载/上传/控制台）
	uploads  map[string]*mcUploadTask // 分片上传会话
	urlDown  map[string]*mcURLDown    // URL 下载任务
	seq      uint64                   // 任务 id 序号

	watchOnce  sync.Once      // 状态监视只启动一次
	stateMu    sync.Mutex     // 状态快照互斥
	lastStatus map[string]int // 上一轮实例状态（uuid → status）

	// 系统曲线（对齐 MCSM VisualDataSubsystem）：200 格环形队列，
	// 每 3 秒推入 {cpu, mem} 的整数百分比；面板节点卡片画 10 分钟曲线。
	chartMu   sync.Mutex
	chart     []mcChartPoint
	chartOnce sync.Once
}

// mcChartPoint 系统曲线上的一个采样点（字段名与 MCSM 完全一致）。
type mcChartPoint struct {
	CPU int `json:"cpu"` // 0-100
	Mem int `json:"mem"` // 0-100
}

// mcChartSize 曲线保留的采样点数（MCSM 固定 200 格）。
const mcChartSize = 200

// mcChartInterval 采样间隔（MCSM 固定 3 秒）。
const mcChartInterval = 3 * time.Second

// ensureChart 初始化曲线队列并启动采样循环（惰性，只启动一次）。
func (mc *mcsmCompat) ensureChart() {
	mc.chartOnce.Do(func() {
		mc.chartMu.Lock()
		mc.chart = make([]mcChartPoint, mcChartSize) // 与 MCSM 一致：初始全 0
		mc.chartMu.Unlock()
		safeGo("MCSM 兼容层系统曲线", func() {
			// 先采一次，避免刚启动时 CPU 恒为 0
			_ = sysinfo.CachedSysCPUUsage()
			t := time.NewTicker(mcChartInterval)
			defer t.Stop()
			for {
				select {
				case <-mc.d.workers.ctx.Done():
					return
				case <-t.C:
				}
				mc.pushChart()
			}
		})
	})
}

// pushChart 采样一次并推入曲线（满则丢弃最旧，与 MCSM LineQueue 一致）。
func (mc *mcsmCompat) pushChart() {
	_, _, memUsage := sysinfo.MemSnapshot()
	point := mcChartPoint{
		CPU: int(math.Round(sysinfo.CachedSysCPUUsage() * 100)),
		Mem: int(math.Round(memUsage * 100)),
	}
	mc.chartMu.Lock()
	if len(mc.chart) == 0 {
		mc.chart = make([]mcChartPoint, mcChartSize)
	}
	mc.chart = append(mc.chart[1:], point)
	mc.chartMu.Unlock()
}

// chartSnapshot 返回曲线快照（复制，避免与采样循环竞态）。
func (mc *mcsmCompat) chartSnapshot() []mcChartPoint {
	mc.chartMu.Lock()
	defer mc.chartMu.Unlock()
	if len(mc.chart) == 0 {
		return make([]mcChartPoint, mcChartSize)
	}
	out := make([]mcChartPoint, len(mc.chart))
	copy(out, mc.chart)
	return out
}

// mslxCompat MSLX Daemon 兼容层状态。
type mslxCompat struct {
	d     *Daemon
	bind  string
	port  int
	addr  string
	allow []*net.IPNet

	user   string
	pass   string
	secret []byte // JWT HS256 签名密钥

	// 登录失败限速（按来源 IP）：5 分钟内 10 次失败即封禁 60 分钟。
	mu        sync.Mutex
	failCount map[string]int
	failSince map[string]time.Time
	banned    map[string]time.Time

	// 子节点链接（MSLX 节点管理）：配对密钥 + 子节点模式 + 链接状态
	linkKey   string
	slaveMode bool
	node      mslxNodeState
	// nodeVerify 主节点签发令牌的核对缓存（key = 令牌，30 秒）
	nodeVerify mslxNodeVerifyCache
	// 面板探活诊断（每个节点标识每分钟最多记一条日志）
	probeMu   sync.Mutex
	probeAt   time.Time
	probeNode string

	ids *mslxIDMap // 实例 uuid ↔ 整数 id 映射（持久化于 {data}/compat-mslx-ids.json）
	// tasks 面板任务中心（创建/删除等后台任务，内存保存）
	tasks mslxTaskRegistry
	// uploads 面板分片上传会话（创建实例时上传核心/整合包）
	uploads mslxUploadRegistry
	// cron 面板「定时任务」表（持久化于 {data}/compat-mslx-tasks.json）
	cron mslxCronStore
	// hubTickets SignalR negotiate 发放的一次性连接凭据（2 分钟有效）。
	hubTickets map[string]*mslxHubTicket
}

// compatListener 一条兼容层独立监听（随节点一起优雅关停）。
type compatListener struct {
	name string // 面板名（日志用）
	srv  *http.Server
	ln   net.Listener
	addr string
}

// compatSecretFile 兼容层自动生成凭据的持久化文件（{data} 下，0600 原子写）。
const compatSecretFile = "compat-secrets.json"

// compatSecretLen 自动生成凭据的长度（十六进制字符数）。
const compatSecretLen = 24

// compatSecrets 兼容层自动生成的凭据集合。
// 配置里留空时使用这里的值；首次启动随机生成并回写配置文件，
// 保证「配置里始终有东西」，同时重启后凭据保持不变（面板不必重配）。
type compatSecrets struct {
	MCSMKey       string `json:"mcsmKey"`       // MCSManager 节点密钥
	MSLXPassword  string `json:"mslxPassword"`  // MSLX 登录密码
	MSLXJWTSecret string `json:"mslxJwtSecret"` // MSLX JWT 签名密钥
	MSLXLinkKey   string `json:"mslxLinkKey"`   // MSLX 子节点配对密钥（面板「配对密钥」）
}

// compatSecretPath 自动凭据文件路径。
func (d *Daemon) compatSecretPath() string {
	dir := strings.TrimSpace(d.DataDir)
	if dir == "" {
		dir = "."
	}
	return dir + string(os.PathSeparator) + compatSecretFile
}

// loadCompatSecrets 读取自动凭据文件（不存在或损坏时返回空结构）。
func (d *Daemon) loadCompatSecrets() *compatSecrets {
	out := &compatSecrets{}
	if data, err := os.ReadFile(d.compatSecretPath()); err == nil {
		_ = json.Unmarshal(data, out)
	}
	// 兼容旧版单文件密钥（v1.5.x 之前的 {data}/compat-mslx-secret）
	if out.MSLXJWTSecret == "" {
		legacy := strings.TrimSpace(d.DataDir) + string(os.PathSeparator) + "compat-mslx-secret"
		if data, err := os.ReadFile(legacy); err == nil {
			if s := strings.TrimSpace(string(data)); len(s) >= 16 {
				out.MSLXJWTSecret = s
			}
		}
	}
	return out
}

// saveCompatSecrets 原子写回自动凭据文件（0600，仅属主可读）。
func (d *Daemon) saveCompatSecrets(s *compatSecrets) error {
	if s == nil {
		return nil
	}
	dir := strings.TrimSpace(d.DataDir)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := d.compatSecretPath()
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

// maskSecret 日志展示用打码（仅露头尾各 4 位）。
func maskSecret(s string) string {
	if len(s) <= 10 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// resolveCompatSecrets 解析两层兼容层实际使用的凭据。
//
// 优先级：配置文件里显式填写的值 > {data}/compat-secrets.json > 现场随机生成。
// 无论兼容层是否启用都会执行（用户要求「就算没开也要填一个东西在那里」），
// 生成结果会持久化并尽力回写到配置文件对应字段。
// 返回本轮新生成（需要提示用户）的字段说明。
func (d *Daemon) resolveCompatSecrets(opts *nodeOptions) (mcKey, mslxPass string, jwtSecret []byte, mslxLinkKey string, notes []string) {
	sec := d.loadCompatSecrets()
	changed := false

	mcKey = strings.TrimSpace(opts.CompatMCSMKey)
	if mcKey == "" {
		if sec.MCSMKey == "" {
			sec.MCSMKey = randomToken(compatSecretLen)
			changed = true
			notes = append(notes, "已自动生成 compat.mcsm.key（面板节点密钥）")
			patchCompatConfigFile(opts.ConfigPath, "mcsm", "key", sec.MCSMKey)
		}
		mcKey = sec.MCSMKey
	} else if sec.MCSMKey != mcKey {
		// 配置优先：同步进自动凭据文件，保持两处一致
		sec.MCSMKey = mcKey
		changed = true
	}

	mslxPass = strings.TrimSpace(opts.CompatMSLXPass)
	if mslxPass == "" {
		if sec.MSLXPassword == "" {
			sec.MSLXPassword = randomToken(compatSecretLen)
			changed = true
			notes = append(notes, "已自动生成 compat.mslx.password（WebPanel 登录密码）")
			patchCompatConfigFile(opts.ConfigPath, "mslx", "password", sec.MSLXPassword)
		}
		mslxPass = sec.MSLXPassword
	} else if sec.MSLXPassword != mslxPass {
		sec.MSLXPassword = mslxPass
		changed = true
	}

	if configured := strings.TrimSpace(opts.CompatMSLXSecret); configured != "" {
		if len(configured) < 16 {
			notes = append(notes, "compat.mslx.jwtSecret 过短（至少 16 个字符），已改用自动生成的密钥")
		} else {
			if sec.MSLXJWTSecret != configured {
				sec.MSLXJWTSecret = configured
				changed = true
			}
			jwtSecret = []byte(configured)
		}
	}
	if jwtSecret == nil {
		if len(sec.MSLXJWTSecret) < 16 {
			sec.MSLXJWTSecret = randomToken(compatSecretLen * 2)
			changed = true
			notes = append(notes, "已自动生成 compat.mslx.jwtSecret（JWT 签名密钥）")
		}
		jwtSecret = []byte(sec.MSLXJWTSecret)
	}

	// MSLX 子节点配对密钥（面板「链接新节点」里填的 LinkKey）：
	// 与其它凭据一样支持自动生成 + 定点回写，保证配置里永远有值。
	mslxLinkKey = strings.TrimSpace(opts.CompatMSLXLinkKey)
	if mslxLinkKey == "" {
		if sec.MSLXLinkKey == "" {
			sec.MSLXLinkKey = randomToken(compatSecretLen)
			changed = true
			notes = append(notes, "已自动生成 compat.mslx.linkKey（子节点配对密钥）")
			patchCompatConfigFile(opts.ConfigPath, "mslx", "linkKey", sec.MSLXLinkKey)
		}
		mslxLinkKey = sec.MSLXLinkKey
	} else if sec.MSLXLinkKey != mslxLinkKey {
		sec.MSLXLinkKey = mslxLinkKey
		changed = true
	}

	if changed {
		if err := d.saveCompatSecrets(sec); err != nil {
			// 落盘失败不影响运行：本轮仍使用内存中的值，仅提示用户
			notes = append(notes, "自动凭据写入 "+d.compatSecretPath()+" 失败: "+err.Error())
		}
	}
	return mcKey, mslxPass, jwtSecret, mslxLinkKey, notes
}

// patchCompatConfigFile 把自动生成的凭据回写到配置文件里仍为空的字段。
//
// 采用保守的「定点文本替换」而不是整体 json.Marshal 重写：
// 配置文件里带大量 _comment_* 说明字段与人工排版，整体重写会打乱顺序、
// 破坏可读性。这里只在 compat.<block> 段内把 "field": "" 替换成具体值，
// 字段已有内容（用户自己填的）时一个字节都不动。
func patchCompatConfigFile(path, block, field, value string) bool {
	p := strings.TrimSpace(path)
	if p == "" || value == "" {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	pos, ok := emptyJSONFieldPos(string(data), block, field)
	if !ok {
		return false
	}
	quoted, err := json.Marshal(value)
	if err != nil {
		return false
	}
	text := string(data)
	updated := text[:pos] + string(quoted) + text[pos+2:]
	// 保持原文件权限（用户可能已设为 0600），不擅自改动
	mode := os.FileMode(0o644)
	if st, serr := os.Stat(p); serr == nil {
		mode = st.Mode().Perm()
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(updated), mode); err != nil {
		return false
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// setupCompat 依据启动选项构建兼容层（两个开关都可同时开启）。
// 凭据缺失时自动生成并回写配置文件；只有前缀非法 / 来源白名单非法时
// 才跳过该层（绝不影响节点主功能启动）。
func (d *Daemon) setupCompat(opts *nodeOptions) {
	if opts == nil {
		return
	}
	// 无论是否启用都先解析凭据：保证配置文件与自动凭据文件里始终有值，
	// 用户开启兼容层时无需自己编密钥。
	mcKey, mslxPass, jwtSecret, mslxLinkKey, notes := d.resolveCompatSecrets(opts)
	for _, n := range notes {
		alog.Printf("兼容层凭据：%s", n)
	}

	if opts.CompatMCSMEnabled {
		if note := deprecatedPrefixNote("compat.mcsm.prefix", opts.CompatMCSMPrefix); note != "" {
			alog.Printf("提示: %s", note)
		}
		if allow, aerr := parseCompatAllow(opts.CompatMCSMIPs); aerr != nil {
			alog.Printf("警告: MCSManager Daemon 兼容层未启用：%v", aerr)
		} else {
			port := opts.CompatMCSMPort
			if port < 0 {
				port = defaultMCSMCompatPort
			}
			d.compatMC = &mcsmCompat{
				d:          d,
				key:        mcKey,
				bind:       opts.CompatMCSMBind,
				port:       port,
				version:    strings.TrimSpace(opts.CompatMCSMVersion),
				allow:      allow,
				sessions:   map[string]*eioSession{},
				missions:   map[string]*mcMission{},
				uploads:    map[string]*mcUploadTask{},
				urlDown:    map[string]*mcURLDown{},
				lastStatus: map[string]int{},
			}
			alog.Printf("MCSManager Daemon 兼容层已启用（面板「节点」填 地址=<本机IP> 端口=%d，密钥 %s）", port, mcKey)
		}
	} else {
		alog.Printf("MCSManager Daemon 兼容层未启用（compat.mcsm.enabled=false）；如需启用，密钥已就绪：%s", maskSecret(mcKey))
	}

	if opts.CompatMSLXEnabled {
		if note := deprecatedPrefixNote("compat.mslx.prefix", opts.CompatMSLXPrefix); note != "" {
			alog.Printf("提示: %s", note)
		}
		if allow, aerr := parseCompatAllow(opts.CompatMSLXIPs); aerr != nil {
			alog.Printf("警告: MSLX Daemon 兼容层未启用：%v", aerr)
		} else {
			user := strings.TrimSpace(opts.CompatMSLXUser)
			if user == "" {
				user = "admin"
			}
			port := opts.CompatMSLXPort
			if port < 0 {
				port = defaultMSLXCompatPort
			}
			d.compatMSLX = &mslxCompat{
				d:         d,
				bind:      opts.CompatMSLXBind,
				port:      port,
				allow:     allow,
				user:      user,
				pass:      mslxPass,
				secret:    jwtSecret,
				failCount: map[string]int{},
				failSince: map[string]time.Time{},
				banned:    map[string]time.Time{},
				linkKey:   mslxLinkKey,
				slaveMode: !opts.CompatMSLXMaster,
			}
			d.compatMSLX.loadIDMap()
			if link := d.compatMSLX.loadNodeLink(); link != nil {
				d.compatMSLX.setNodeLink(link)
				alog.Printf("MSLX 兼容层：已恢复与主节点 %s 的链接（nodeId=%s）", link.MasterURL, link.NodeID)
			}
			alog.Printf("MSLX Daemon 兼容层已启用（WebPanel 后端地址 http://<本机IP>:%d，账号 %s，密码 %s）",
				port, user, mslxPass)
			alog.Printf("MSLX 兼容层：节点探活 /api/status 已开放（匿名仅返回公开信息），配对密钥 %s",
				maskSecret(mslxLinkKey))
		}
	} else {
		alog.Printf("MSLX Daemon 兼容层未启用（compat.mslx.enabled=false）；如需启用，账号 admin、密码已就绪：%s", maskSecret(mslxPass))
	}
}

// deprecatedPrefixNote 提示「prefix 已废弃」：面板只支持地址+端口，
// 兼容层改为独立端口监听，残留的 prefix 配置不再生效。
func deprecatedPrefixNote(field, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return field + " 已废弃（兼容层改为独立端口监听，面板只支持地址+端口），当前值 " +
		value + " 不生效，可从配置里删除"
}

// compatListenAddr 组装兼容层监听地址：地址为空时沿用节点 bind。
func (d *Daemon) compatListenAddr(bind string, port int) string {
	host := strings.TrimSpace(bind)
	if host == "" {
		host = strings.TrimSpace(d.BindHost)
	}
	switch host {
	case "", "localhost":
		host = "127.0.0.1"
	case "[::]", "::":
		host = "::"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// startCompatServers 为已启用的兼容层各自启动一个独立监听端口。
//
// 端口占用/监听失败只告警并跳过该层（节点主功能不受影响）；成功后把实际
// 监听地址记录到层对象上，供启动日志与测试读取。
func (d *Daemon) startCompatServers() {
	if d.compatMC != nil {
		// 系统曲线从启动就开采样（与 MCSM daemon 一致），面板一进来就有历史
		d.compatMC.ensureChart()
		d.startCompatListener("MCSManager", d.compatMC.bind, d.compatMC.port,
			d.compatMC.handler(),
			func(addr string) { d.compatMC.addr = addr },
			func() { d.compatMC = nil })
	}
	if d.compatMSLX != nil {
		// 子节点状态上报（每 2 秒；仅在存在链接关系时真正发送）
		d.compatMSLX.startNodeReporter()
		// 定时任务：加载任务表并启动分钟级调度（面板「定时任务」）
		d.compatMSLX.loadCronTasks()
		d.compatMSLX.startCronScheduler()
		d.startCompatListener("MSLX", d.compatMSLX.bind, d.compatMSLX.port,
			d.compatMSLX.handler(),
			func(addr string) { d.compatMSLX.addr = addr },
			func() { d.compatMSLX = nil })
	}
}

// startCompatListener 启动单个兼容层监听器（带与主服务一致的中间件保护）。
func (d *Daemon) startCompatListener(name, bind string, port int, handler http.Handler,
	setAddr func(string), disable func()) {
	addr := d.compatListenAddr(bind, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		alog.Printf("警告: %s 兼容层未启动：监听 %s 失败（%v）；端口可能被占用，可改 compat 块里的 port",
			name, addr, err)
		disable()
		return
	}
	// 复用主服务的中间件：时间头 → 在途登记 → 恒开防御 → 审计 → CORS
	// → 请求体读空闲超时。兼容层上传走直连通道，故不套 /api/ 的 16MiB 上限。
	var h http.Handler = handler
	h = d.corsMiddleware(h)
	h = d.auditMiddleware(h)
	h = d.defenseMiddleware(h)
	h = d.trackRequests(h)
	h = clockDateMiddleware(h)
	h = idleBodyTimeout(h)

	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	d.compatMu.Lock()
	d.compatServers = append(d.compatServers, &compatListener{
		name: name, srv: srv, ln: ln, addr: ln.Addr().String(),
	})
	d.compatMu.Unlock()
	if setAddr != nil {
		setAddr(ln.Addr().String())
	}
	safeGo(name+" 兼容层监听", func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			alog.Printf("警告: %s 兼容层监听退出：%v", name, err)
		}
	})
	alog.Printf("%s 兼容层监听于 %s", name, ln.Addr().String())
}

// stopCompatServers 关停全部兼容层监听器（可重复调用）。
func (d *Daemon) stopCompatServers(ctx context.Context) error {
	// 先停调度循环，避免关停过程中还去碰实例
	if d.compatMSLX != nil {
		d.compatMSLX.stopCronScheduler()
	}

	d.compatMu.Lock()
	servers := d.compatServers
	d.compatServers = nil
	d.compatMu.Unlock()

	var errs []error
	for _, item := range servers {
		if err := item.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("%s 兼容层关停失败: %w", item.name, err))
			_ = item.srv.Close()
		}
		if err := item.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("%s 兼容层监听器关闭失败: %w", item.name, err))
		}
	}
	return errors.Join(errs...)
}

// compatAddrOf 返回某层实际监听地址（未启动时为空）。
func (d *Daemon) compatAddrOf(layer string) string {
	switch layer {
	case "mcsm":
		if d.compatMC != nil {
			return d.compatMC.addr
		}
	case "mslx":
		if d.compatMSLX != nil {
			return d.compatMSLX.addr
		}
	}
	return ""
}

// pingInterval 返回 engine.io 服务端 ping 间隔。
func (mc *mcsmCompat) pingInterval() time.Duration {
	if mc != nil && mc.pingIv > 0 {
		return mc.pingIv
	}
	return eioPingIntervalDefault
}

// pingTimeout 返回等待客户端 pong 的宽限时间。
func (mc *mcsmCompat) pingTimeout() time.Duration {
	if mc != nil && mc.pingTo > 0 {
		return mc.pingTo
	}
	return eioPingTimeoutDefault
}

// reportVersion 返回上报给 MCSManager 面板的版本号。
//
// 面板的节点卡片用 hasVersionUpdate(panel.specifiedDaemonVersion, node.version)
// 比较**主次版本号**：不一致就显示红色「有更新」。本节点版本号（如 1.5.0）与
// MCSManager daemon 的版本号必然不同，因此这里允许用 compat.mcsm.version
// 覆盖成面板期望的值（例如 4.5.0），让卡片显示为绿色。
func (mc *mcsmCompat) reportVersion() string {
	if mc != nil && mc.version != "" {
		return mc.version
	}
	return Version
}

// compatKillInstance 强制终止实例（两个兼容层共用同一语义）。
//
// 与节点自身 /api/protected_instance/kill 一致：先摘掉进程引用再终止，
// 防止退出监听 goroutine 误判为「意外退出」而触发 AutoRestart。
// 额外处理一种竞态：Kill 期间进程可能已经自行退出（Kill 返回错误但进程
// 确实不在了），此时按成功处理并落 Stopped，而不是把状态回滚成 Running。
func (d *Daemon) compatKillInstance(inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("实例不存在")
	}
	inst.SetStatus(StatusStopping)
	inst.mu.Lock()
	proc := inst.Proc
	inst.Proc = nil
	inst.mu.Unlock()
	if proc != nil && proc.IsRunning() {
		if err := proc.Kill(); err != nil {
			if proc.IsRunning() {
				// 确实没杀掉：恢复现场并如实报错
				inst.mu.Lock()
				if inst.Proc == nil {
					inst.Proc = proc
				}
				inst.mu.Unlock()
				inst.SetStatus(StatusRunning)
				return err
			}
			alog.Printf("兼容层：实例 %s 的进程在强杀期间已自行退出（%v），按已停止处理",
				inst.InstanceUuid, err)
		}
		if err := d.forgetProcessFor(inst.InstanceUuid, proc); err != nil {
			alog.Printf("兼容层：清理实例 %s 进程记录失败: %v", inst.InstanceUuid, err)
		}
	}
	inst.SetStatus(StatusStopped)
	return nil
}

// parseCompatAllow 解析允许的面板来源：支持裸 IP 与 CIDR（换行/逗号分隔）。
func parseCompatAllow(spec string) ([]*net.IPNet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, item := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == '\n' || r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			_, netw, err := net.ParseCIDR(item)
			if err != nil {
				return nil, fmt.Errorf("compat.allowPanelIps 条目 %q 非法: %w", item, err)
			}
			out = append(out, netw)
			continue
		}
		ip := net.ParseIP(item)
		if ip == nil {
			return nil, fmt.Errorf("compat.allowPanelIps 条目 %q 非法（既不是 IP 也不是 CIDR）", item)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// compatClientIP 取请求来源 IP（优先 RemoteAddr，其次 X-Forwarded-For 首项）。
func compatClientIP(r *http.Request) net.IP {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip
		}
	}
	return net.ParseIP(r.RemoteAddr)
}

// allowOK 判定来源是否在允许列表内（空列表 = 不限制）。
func allowOK(r *http.Request, allow []*net.IPNet) bool {
	if len(allow) == 0 {
		return true
	}
	ip := compatClientIP(r)
	if ip == nil {
		return false
	}
	for _, n := range allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// writeCompatJSON 输出 JSON（兼容层不使用节点的 {status,data,time} 信封）。
func writeCompatJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// randomToken 生成 n 字节随机十六进制串（crypto/rand，复用 newUUID 的随机源）。
func randomToken(n int) string {
	if n <= 0 {
		n = 16
	}
	var sb strings.Builder
	for sb.Len() < n*2 {
		sb.WriteString(strings.ReplaceAll(newUUID(), "-", ""))
	}
	return sb.String()[:n*2]
}

// compatNextID 生成兼容层内部任务 id（进程内唯一）。
func (mc *mcsmCompat) nextID(prefix string) string {
	n := atomic.AddUint64(&mc.seq, 1)
	return prefix + strconv.FormatUint(n, 10) + "-" + newUUID()[:8]
}

// ---------------------------------------------------------------------------
// 实例视图映射：本节点 InstanceConfig ↔ MCSM InstanceConfig 字段
// ---------------------------------------------------------------------------

// mcDockerView 返回 MCSM 实例配置里 docker 段的默认值。
// 本节点容器能力独立于 MCSM 的 Docker 实例配置，这里只保证面板渲染不报错。
func mcDockerView() map[string]any {
	return map[string]any{
		"updateCommandImage": "HOST",
		"containerName":      "",
		"image":              "",
		"uploadSpeedLimit":   0,
		"downloadSpeedLimit": 0,
		"ports":              []any{},
		"extraVolumes":       []any{},
		"capAdd":             []any{},
		"capDrop":            []any{},
		"devices":            []any{},
		"privileged":         false,
		"memory":             0,
		"networkMode":        "bridge",
		"networkAliases":     []any{},
		"cpusetCpus":         "",
		"cpuUsage":           0,
		"maxSpace":           0,
		"io":                 0,
		"network":            0,
		"workingDir":         "/data",
		"env":                []any{},
		"changeWorkdir":      true,
		"labels":             []any{},
		"gpuEnabled":         false,
		"gpuCount":           -1,
		"gpuDeviceIds":       []any{},
		"gpuDriver":          "nvidia",
		"deviceReadBps":      []any{},
		"deviceWriteBps":     []any{},
	}
}

// mcConfigView 把本节点实例配置映射为 MCSM 面板可读的完整配置对象。
// 面板会直接读取这些字段（含 terminalOption/docker/eventTask），缺字段会导致
// 前端渲染异常，因此这里把 MCSM 侧的全部字段都补齐默认值。
func mcConfigView(cfg InstanceConfig) map[string]any {
	actions := make([]map[string]any, 0, len(cfg.ActionCommandList))
	for _, item := range cfg.ActionCommandList {
		if strings.TrimSpace(item) == "" {
			continue
		}
		actions = append(actions, map[string]any{"name": item, "command": item})
	}
	tags := cfg.Tag
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"nickname":          cfg.Nickname,
		"startCommand":      cfg.StartCommand,
		"stopCommand":       cfg.StopCommand,
		"stopTimeout":       mcDefaultStopTimeoutMS,
		"cwd":               cfg.Cwd,
		"ie":                cfg.IE,
		"oe":                cfg.OE,
		"createDatetime":    cfg.CreateDatetime,
		"lastDatetime":      cfg.LastDatetime,
		"type":              cfg.Type,
		"tag":               tags,
		"endTime":           cfg.EndTime,
		"fileCode":          cfg.FileCode,
		"processType":       cfg.ProcessType,
		"updateCommand":     cfg.UpdateCommand,
		"runAs":             "",
		"crlf":              cfg.Crlf,
		"category":          0,
		"basePort":          0,
		"enableRcon":        false,
		"rconPassword":      "",
		"rconPort":          0,
		"rconIp":            "",
		"actionCommandList": actions,
		"terminalOption": map[string]any{
			"haveColor":    true,
			"pty":          false,
			"ptyWindowCol": mcDefaultPTYCol,
			"ptyWindowRow": mcDefaultPTYRow,
		},
		"eventTask": map[string]any{
			"autoStart":           cfg.EventTask.AutoStart,
			"autoRestart":         cfg.EventTask.AutoRestart,
			"autoRestartMaxTimes": -1,
			"ignore":              cfg.EventTask.Ignore,
		},
		"java":               map[string]any{"id": ""},
		"docker":             mcDockerView(),
		"pingConfig":         map[string]any{"ip": cfg.PingConfig.IP, "port": cfg.PingConfig.Port, "type": cfg.PingConfig.Type},
		"extraServiceConfig": map[string]any{"openFrpTunnelId": "", "openFrpToken": ""},
	}
}

// mcConfigInput MCSM 面板下发的实例配置（只取本节点支持的字段）。
type mcConfigInput struct {
	Nickname          *string           `json:"nickname"`
	StartCommand      *string           `json:"startCommand"`
	StopCommand       *string           `json:"stopCommand"`
	Cwd               *string           `json:"cwd"`
	IE                *string           `json:"ie"`
	OE                *string           `json:"oe"`
	Type              *string           `json:"type"`
	Tag               []string          `json:"tag"`
	EndTime           *int64            `json:"endTime"`
	FileCode          *string           `json:"fileCode"`
	ProcessType       *string           `json:"processType"`
	UpdateCommand     *string           `json:"updateCommand"`
	Crlf              *int              `json:"crlf"`
	ActionCommandList []json.RawMessage `json:"actionCommandList"`
	EventTask         *struct {
		AutoStart   *bool `json:"autoStart"`
		AutoRestart *bool `json:"autoRestart"`
		Ignore      *bool `json:"ignore"`
	} `json:"eventTask"`
	PingConfig *struct {
		IP   *string `json:"ip"`
		Port *int    `json:"port"`
		Type *int    `json:"type"`
	} `json:"pingConfig"`
}

// apply 把面板下发的字段合并进现有配置（未出现的字段保持原值）。
func (in *mcConfigInput) apply(cfg *InstanceConfig) {
	if in == nil {
		return
	}
	setStr := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setStr(&cfg.Nickname, in.Nickname)
	setStr(&cfg.StartCommand, in.StartCommand)
	setStr(&cfg.StopCommand, in.StopCommand)
	setStr(&cfg.Cwd, in.Cwd)
	setStr(&cfg.IE, in.IE)
	setStr(&cfg.OE, in.OE)
	setStr(&cfg.Type, in.Type)
	setStr(&cfg.FileCode, in.FileCode)
	setStr(&cfg.ProcessType, in.ProcessType)
	setStr(&cfg.UpdateCommand, in.UpdateCommand)
	if in.Tag != nil {
		cfg.Tag = in.Tag
	}
	if in.EndTime != nil {
		cfg.EndTime = *in.EndTime
	}
	if in.Crlf != nil {
		cfg.Crlf = *in.Crlf
	}
	if in.ActionCommandList != nil {
		list := make([]string, 0, len(in.ActionCommandList))
		for _, raw := range in.ActionCommandList {
			// 面板下发的是 {name, command} 对象，也兼容纯字符串。
			var obj struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(raw, &obj); err == nil && obj.Command != "" {
				list = append(list, obj.Command)
				continue
			}
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				list = append(list, s)
			}
		}
		cfg.ActionCommandList = list
	}
	if in.EventTask != nil {
		if in.EventTask.AutoStart != nil {
			cfg.EventTask.AutoStart = *in.EventTask.AutoStart
		}
		if in.EventTask.AutoRestart != nil {
			cfg.EventTask.AutoRestart = *in.EventTask.AutoRestart
		}
		if in.EventTask.Ignore != nil {
			cfg.EventTask.Ignore = *in.EventTask.Ignore
		}
	}
	if in.PingConfig != nil {
		if in.PingConfig.IP != nil {
			cfg.PingConfig.IP = *in.PingConfig.IP
		}
		if in.PingConfig.Port != nil {
			cfg.PingConfig.Port = *in.PingConfig.Port
		}
		if in.PingConfig.Type != nil {
			cfg.PingConfig.Type = *in.PingConfig.Type
		}
	}
}

// mcInstanceInfo 生成 MCSM 面板读取的实例运行信息段。
// 本节点不解析 ping/玩家数据（-1 表示「未知」，面板据此隐藏对应图表）。
func mcInstanceInfo() map[string]any {
	return map[string]any{
		"mcPingOnline":   false,
		"currentPlayers": -1,
		"maxPlayers":     -1,
		"version":        "",
		"fileLock":       0,
		"playersChart":   []any{},
		"openFrpStatus":  false,
		"latency":        0,
		"allocatedPorts": []any{},
	}
}

// mcInstanceDetail 生成 MCSM 面板消费的实例详情对象。
func mcInstanceDetail(inst *Instance) map[string]any {
	if inst == nil {
		return nil
	}
	inst.mu.Lock()
	cfg := inst.Config
	status := inst.Status
	started := inst.Started
	inst.mu.Unlock()
	return map[string]any{
		"instanceUuid":  inst.InstanceUuid,
		"started":       started,
		"autoRestarted": 0,
		"status":        status,
		"config":        mcConfigView(cfg),
		"info":          mcInstanceInfo(),
	}
}

// mcInstanceSection 按 uuid 列表收集实例详情（保持请求顺序）。
func (d *Daemon) mcInstanceSection(uuids []string) []map[string]any {
	out := make([]map[string]any, 0, len(uuids))
	for _, uuid := range uuids {
		if inst := d.Find(uuid); inst != nil {
			out = append(out, mcInstanceDetail(inst))
		}
	}
	return out
}

// mcAllInstanceDetails 返回全部实例详情（含全局实例跳过逻辑由调用方决定）。
func (d *Daemon) mcAllInstanceDetails() []map[string]any {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()
	out := make([]map[string]any, 0, len(insts))
	for _, inst := range insts {
		out = append(out, mcInstanceDetail(inst))
	}
	sort.SliceStable(out, func(a, b int) bool {
		sa, _ := out[a]["status"].(int)
		sb, _ := out[b]["status"].(int)
		if sa != sb {
			return sb < sa
		}
		return fmt.Sprint(out[a]["instanceUuid"]) < fmt.Sprint(out[b]["instanceUuid"])
	})
	return out
}

// mcSystemView 生成 MCSM 面板 info/overview 需要的 system 段。
func mcSystemView(d *Daemon) map[string]any {
	hostname, osType, platform, release, distro := sysinfo.HostStaticInfo()
	diskTotal, diskUsed, diskUsage := sysinfo.CachedDiskInfo(d.DataDir)
	memTotal, memFree, memUse := sysinfo.MemSnapshot()
	netDown, netUp := cachedNetRates()
	return map[string]any{
		"type":            osType,
		"hostname":        hostname,
		"platform":        platform,
		"release":         release,
		"version":         distro,
		"uptime":          sysinfo.UptimeSeconds(),
		"totalmem":        memTotal,
		"freemem":         memFree,
		"cpuUsage":        sysinfo.CachedSysCPUUsage(),
		"memUsage":        memUse,
		"diskusage":       diskUsage,
		"disktotal":       diskTotal,
		"diskused":        diskUsed,
		"networkDownload": netDown,
		"networkUpload":   netUp,
		"time":            timesys.Now().UnixMilli(),
	}
}
