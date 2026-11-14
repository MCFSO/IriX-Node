// time_system.go — 节点自有时间系统的启动编排与 TLS/日志/HTTP 衔接。
//
// 时间系统（internal/timesys）是节点的唯一时间来源：
//   - 业务时间（会话/封禁/审计/实例时间戳）通过 timesys.Now() 获取；
//   - TLS 证书签发与证书链校验使用同一时间源；
//   - 标准库日志与 HTTP Date 响应头也取该时间；
//   - 证书在节点时间上失效时（自签模式）自动续签，保证 TLS 始终围绕时间系统。
//
// 启动顺序：解析配置 → 初始化时间系统并等待首轮同步（有时限）→ 账户/TLS
// 等依赖时间的子系统 → 对外服务。关停时先停同步循环再落盘。
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"irix/node/internal/timesys"
	"irix/node/internal/vaultcrypto"
)

// 时间系统默认值（命令行默认值与配置文件缺省值一致）。
const (
	defaultTimeIntervalSeconds = 300
	defaultTimeTimeoutMillis   = 1500
	defaultTimeSamples         = 3
	defaultTimeMinAgreement    = 2
	defaultTimeMaxSlewPPM      = 500
	defaultTimeMaxRatePPM      = 500
	defaultTimeStepMillis      = 2000
	defaultTimeMaxSlewOffsetMS = 10000
	defaultTimeMinRateSpanMin  = 30
	defaultTimeHoldoverHours   = 24
	defaultTimeMaxUncMillis    = 5000
	defaultTimeDriftPPM        = 100
	defaultTimeBootWaitMillis  = 1500
)

// parseTimeServerList 解析 -time-servers 的值：
// 逗号分隔的 host:port@base64公钥，可重复；名称为地址。
func parseTimeServerList(raw string) ([]timesys.Server, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []timesys.Server
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		addr, key, ok := strings.Cut(item, "@")
		if !ok || strings.TrimSpace(addr) == "" || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("time-servers 条目格式错误: %q（应为 host:port@base64公钥）", item)
		}
		out = append(out, timesys.Server{
			Name:      strings.TrimSpace(addr),
			Address:   strings.TrimSpace(addr),
			PublicKey: strings.TrimSpace(key),
			Profile:   timesys.ProfileAuto,
		})
	}
	return out, nil
}

// encodeTimeServersConfig 把配置文件服务器列表编码为一行一台的可比较字符串
// （name|address|publicKey|protocol）。base64 公钥不含 '|' 与换行，可安全拆分。
func encodeTimeServersConfig(list []TimeServerConfig) string {
	var b strings.Builder
	for i, s := range list {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s|%s|%s|%s",
			strings.TrimSpace(s.Name), strings.TrimSpace(s.Address),
			strings.TrimSpace(s.PublicKey), strings.TrimSpace(s.Protocol))
	}
	return b.String()
}

// decodeTimeServersConfig 解析 encodeTimeServersConfig 的编码。
func decodeTimeServersConfig(raw string) ([]timesys.Server, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []timesys.Server
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			return nil, fmt.Errorf("配置文件 time.servers 条目格式错误: %q", line)
		}
		out = append(out, timesys.Server{
			Name:      parts[0],
			Address:   parts[1],
			PublicKey: parts[2],
			Profile:   timesys.Profile(parts[3]),
		})
	}
	return out, nil
}

// buildTimeConfig 把启动选项（命令行 > 配置文件 > 默认值）转换为时间系统配置。
func buildTimeConfig(o *nodeOptions, dataDir string) (timesys.Config, error) {
	if o == nil {
		return timesys.Config{}, fmt.Errorf("时间系统配置缺失")
	}
	if o.TimeIntervalSeconds < 5 {
		return timesys.Config{}, fmt.Errorf("time-interval 无效: %d 秒（须 ≥5）", o.TimeIntervalSeconds)
	}
	if o.TimeTimeoutMillis < 100 || o.TimeTimeoutMillis > 60000 {
		return timesys.Config{}, fmt.Errorf("time-timeout-ms 无效: %d（须在 100-60000 之间）", o.TimeTimeoutMillis)
	}
	if o.TimeSamples < 1 || o.TimeSamples > 16 {
		return timesys.Config{}, fmt.Errorf("time.samples 无效: %d（须在 1-16 之间）", o.TimeSamples)
	}
	if o.TimeMinAgreement < 1 || o.TimeMinAgreement > 16 {
		return timesys.Config{}, fmt.Errorf("time.minAgreement 无效: %d（须在 1-16 之间）", o.TimeMinAgreement)
	}
	if o.TimeMaxSlewPPM < 1 || o.TimeMaxSlewPPM > 10000 {
		return timesys.Config{}, fmt.Errorf("time-max-slew-ppm 无效: %d（须在 1-10000 之间）", o.TimeMaxSlewPPM)
	}
	if o.TimeMaxRatePPM < 1 || o.TimeMaxRatePPM > 10000 {
		return timesys.Config{}, fmt.Errorf("time.maxRatePPM 无效: %d（须在 1-10000 之间）", o.TimeMaxRatePPM)
	}
	if o.TimeStepMillis < 1 {
		return timesys.Config{}, fmt.Errorf("time-step-ms 无效: %d（须 ≥1）", o.TimeStepMillis)
	}
	servers, err := parseTimeServerList(o.TimeServers)
	if err != nil {
		return timesys.Config{}, err
	}
	if len(servers) == 0 {
		servers, err = decodeTimeServersConfig(o.TimeServersConfig)
		if err != nil {
			return timesys.Config{}, err
		}
	}
	stateFile := strings.TrimSpace(o.TimeStateFile)
	if stateFile == "" {
		stateFile = filepath.Join(dataDir, "timesys.json")
	} else if !filepath.IsAbs(stateFile) {
		// 相对路径按数据目录解析，避免随启动目录漂移。
		stateFile = filepath.Join(dataDir, stateFile)
	}
	return timesys.Config{
		Enabled:      o.TimeEnabled,
		Servers:      servers,
		Interval:     time.Duration(o.TimeIntervalSeconds) * time.Second,
		Timeout:      time.Duration(o.TimeTimeoutMillis) * time.Millisecond,
		Samples:      o.TimeSamples,
		MinAgreement: o.TimeMinAgreement,
		Limits: timesys.Limits{
			MaxStep:          time.Duration(o.TimeStepMillis) * time.Millisecond,
			MaxSlewPPM:       float64(o.TimeMaxSlewPPM),
			MaxRatePPM:       float64(o.TimeMaxRatePPM),
			MaxSlewOffset:    time.Duration(o.TimeMaxSlewOffsetMs) * time.Millisecond,
			MinRateSpan:      time.Duration(o.TimeMinRateSpanMinutes) * time.Minute,
			HoldoverLimit:    time.Duration(o.TimeHoldoverLimitHours) * time.Hour,
			MaxUncertainty:   time.Duration(o.TimeMaxUncMillis) * time.Millisecond,
			HoldoverDriftPPM: float64(o.TimeHoldoverDriftPPM),
		},
		StateFile: stateFile,
		Logf:      alog.Printf,
	}, nil
}

// initTimeSystem 初始化并启动时间系统，同时把它安装为进程唯一时间源。
// bootWait 为等待首轮同步的上限（0 = 不等待，直接继续启动）。
func (d *Daemon) initTimeSystem(cfg timesys.Config, bootWait time.Duration, tlsRequireTrusted bool) error {
	sys, err := timesys.New(cfg)
	if err != nil {
		return err
	}
	d.time = sys
	d.timeRestore = timesys.SetCurrent(sys.Clock())
	d.tlsRequireTrusted = tlsRequireTrusted
	sys.Start()
	if bootWait > 0 && cfg.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), bootWait)
		sys.WaitFirstSync(ctx)
		cancel()
	}
	if !cfg.Enabled {
		alog.Printf("时间系统未启用（-time-sync=false）：全部时间判定回退到系统时钟，TLS 证书仍按该时钟签发/校验")
		return nil
	}
	snap := sys.Clock().Snapshot()
	if snap.Verified {
		alog.Printf("时间系统已就绪：Roughtime 同步成功，质量 %s，不确定度 ±%s，频率校正 %+.1fppm，时钟偏差 %+.3fs",
			snap.Quality, snap.Uncertainty.Round(time.Millisecond), snap.RatePPM, snap.Offset.Seconds())
		return nil
	}
	source := "系统时钟"
	if snap.Locked {
		source = fmt.Sprintf("状态文件恢复的保持锚点（推算偏差 %+.3fs，未经在线验证）", snap.Offset.Seconds())
	}
	if bootWait > 0 {
		alog.Printf("时间系统：首轮 Roughtime 同步尚未完成（等待上限 %s），暂用%s；后台按指数退避重试",
			bootWait, source)
	} else {
		alog.Printf("时间系统：未等待首轮同步，暂用%s；后台按指数退避重试", source)
	}
	return nil
}

// stopTimeSystem 停止时间系统并落盘状态（幂等）。
func (d *Daemon) stopTimeSystem() {
	if d == nil || d.time == nil {
		return
	}
	d.time.Stop()
}

// timeStatus 返回时间系统状态（未初始化时回退到进程当前时间源）。
func (d *Daemon) timeStatus() timesys.Status {
	if d != nil && d.time != nil {
		return d.time.Status()
	}
	snap := timesys.Current().Snapshot()
	return timesys.Status{
		Enabled:           false,
		Quality:           snap.Quality,
		Trustworthy:       snap.Trustworthy,
		Now:               snap.Time,
		UnixMilli:         snap.Time.UnixMilli(),
		LastSync:          snap.LastSync,
		RatePPM:           snap.RatePPM,
		OffsetMillis:      float64(snap.Offset) / float64(time.Millisecond),
		UncertaintyMillis: float64(snap.Uncertainty) / float64(time.Millisecond),
	}
}

// syncTimeNow 立即触发一轮 Roughtime 同步。
func (d *Daemon) syncTimeNow(ctx context.Context) error {
	if d == nil || d.time == nil {
		return fmt.Errorf("时间系统未初始化")
	}
	return d.time.SyncNow(ctx)
}

// setupTLS 加载/生成 TLS 证书并构造围绕时间系统的 tls.Config。
// mode = auto 时 dir 为证书目录（自动续签）；mode = manual 时使用 certFile/keyFile。
func (d *Daemon) setupTLS(mode, certFile, keyFile, dir string) (*tls.Config, string, error) {
	var (
		cert tls.Certificate
		fp   string
		err  error
	)
	switch mode {
	case "auto":
		cert, fp, err = vaultcrypto.EnsureSelfSignedCert(dir)
		if err != nil {
			return nil, "", fmt.Errorf("TLS 自签证书初始化失败: %w", err)
		}
	case "manual":
		cert, fp, err = vaultcrypto.LoadCertFiles(certFile, keyFile)
		if err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("未知的 TLS 模式: %q", mode)
	}
	// 启动时按节点时间检查证书；无效仅告警（自动模式会在后台/握手时重签）。
	now := timesys.Now()
	if verr := vaultcrypto.CertificateValidAt(&cert, now); verr != nil {
		alog.Printf("警告: TLS 证书在节点时间 %s 无效: %v（TLS 握手将按规则拒绝或自动重签）",
			now.Format(time.RFC3339), verr)
	}
	if nb, na, verr := vaultcrypto.CertificateValidity(&cert); verr == nil {
		alog.Printf("TLS 证书有效期（按节点时间）：%s ~ %s",
			nb.Local().Format("2006-01-02 15:04:05"), na.Local().Format("2006-01-02 15:04:05"))
	}

	d.tlsMu.Lock()
	d.tlsMode = mode
	d.tlsDir = dir
	d.tlsCertFile = certFile
	d.tlsKeyFile = keyFile
	d.tlsCert.Store(&cert)
	d.TLSFingerprint = fp
	d.tlsMu.Unlock()

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// 证书链校验（客户端证书 / 未来双向 TLS）同样使用节点时间。
		Time: func() time.Time { return timesys.Now() },
		// 只注册 GetCertificate：每次握手都按节点时间检查/续签。
		GetCertificate: d.getTLSCertificate,
	}
	return cfg, fp, nil
}

// getTLSCertificate 每次握手取证书：
//   - 时间可信时，证书在节点时间无效 → auto 模式重签、manual 模式拒绝握手；
//   - tlsRequireTrusted=true 且时间不可信 → 拒绝握手（默认关闭，避免离线自锁）；
//   - 时间不可信时不强制有效期（用客户端自身时钟校验，避免把管理入口锁死）。
func (d *Daemon) getTLSCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := d.tlsCert.Load()
	if cert == nil {
		return nil, fmt.Errorf("TLS 证书未就绪")
	}
	trusted := timesys.Current().Trustworthy()
	if d.tlsRequireTrusted && !trusted {
		snap := timesys.Current().Snapshot()
		return nil, fmt.Errorf("节点时间不可信（质量 %s，不确定度 ±%s），按 tlsRequireTrusted 配置拒绝 TLS 握手",
			snap.Quality, snap.Uncertainty.Round(time.Millisecond))
	}
	if !trusted {
		return cert, nil
	}
	now := timesys.Now()
	if err := vaultcrypto.CertificateValidAt(cert, now); err == nil {
		return cert, nil
	} else if d.tlsMode != "auto" {
		return nil, err
	}
	// 自动模式：按节点时间重签（证书已过期/尚未生效）。
	if newCert, fp, err := d.renewTLSCert(); err == nil {
		alog.Printf("时间系统：TLS 证书按节点时间自动重签，新指纹（SHA-256）: %s（客户端需重新固定）", fp)
		return newCert, nil
	} else {
		alog.Printf("错误: TLS 证书按节点时间已失效且自动重签失败: %v", err)
		return nil, err
	}
}

// renewTLSCert 重新生成自签证书并原子替换（仅 auto 模式）。
func (d *Daemon) renewTLSCert() (*tls.Certificate, string, error) {
	d.tlsMu.Lock()
	defer d.tlsMu.Unlock()
	if d.tlsMode != "auto" || d.tlsDir == "" {
		return nil, "", fmt.Errorf("当前 TLS 模式不支持自动续签")
	}
	cert, fp, err := vaultcrypto.EnsureSelfSignedCert(d.tlsDir)
	if err != nil {
		return nil, "", fmt.Errorf("自签证书续签失败: %w", err)
	}
	d.tlsCert.Store(&cert)
	d.TLSFingerprint = fp
	return &cert, fp, nil
}

// tlsFingerprintValue 线程安全地读取当前证书指纹。
func (d *Daemon) tlsFingerprintValue() string {
	d.tlsMu.RLock()
	defer d.tlsMu.RUnlock()
	return d.TLSFingerprint
}

// setTLSFingerprint 线程安全地设置证书指纹（启动与续签）。
func (d *Daemon) setTLSFingerprint(fp string) {
	d.tlsMu.Lock()
	d.TLSFingerprint = fp
	d.tlsMu.Unlock()
}

// tlsJanitor 周期检查证书与节点时间的关系：证书在节点时间失效时自动重签
// （auto）或告警（manual）。由 backgroundWorkers 管理生命周期。
func (d *Daemon) tlsJanitor(ctx context.Context) {
	// 启动后先让首轮同步有机会完成，再开始检查。
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		d.checkTLSCert()
		timer.Reset(5 * time.Minute)
	}
}

// checkTLSCert 检查一次证书有效期。
func (d *Daemon) checkTLSCert() {
	cert := d.tlsCert.Load()
	if cert == nil {
		return
	}
	trusted := timesys.Current().Trustworthy()
	now := timesys.Now()
	err := vaultcrypto.CertificateValidAt(cert, now)
	if err == nil {
		if _, na, verr := vaultcrypto.CertificateValidity(cert); verr == nil {
			remaining := na.Sub(now)
			if remaining < 365*24*time.Hour {
				day := now.Format("2006-01-02")
				d.tlsMu.Lock()
				show := d.tlsNoticeDay != day
				if show {
					d.tlsNoticeDay = day
				}
				d.tlsMu.Unlock()
				if show {
					alog.Printf("提示: TLS 证书将于 %s 到期（按节点时间剩余 %s），请准备更换证书",
						na.Local().Format(time.RFC3339), remaining.Round(24*time.Hour))
				}
			}
		}
		return
	}
	if !trusted {
		// 时间尚未可信：不据此重签（否则可能签出错误日期），等同步完成后再处理。
		return
	}
	if d.tlsMode == "auto" {
		if _, fp, rerr := d.renewTLSCert(); rerr != nil {
			alog.Printf("警告: TLS 证书自动续签失败: %v", rerr)
		} else {
			alog.Printf("时间系统：TLS 证书在节点时间无效（%v），已自动重签，新指纹: %s（客户端需重新固定）", err, fp)
		}
		return
	}
	alog.Printf("警告: TLS 证书在节点时间无效: %v（manual 模式不会自动重签，请更换证书）", err)
}

// clockDateMiddleware 用节点时间写 HTTP Date 响应头（Go 只在未设置时自填）。
func clockDateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", timesys.Now().UTC().Format(http.TimeFormat))
		next.ServeHTTP(w, r)
	})
}

// outboundTLSConfig 出站 HTTPS（集群拉取、核心下载）的 TLS 配置：
// 服务器证书链校验同样使用节点时间，避免系统墙钟错误导致外部 TLS 失败/误放行。
func outboundTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		Time:       func() time.Time { return timesys.Now() },
	}
}

// clockLogWriter 用节点时间系统给标准库日志加时间戳。
// log 包在 flags 含 LstdFlags 时用自己的 time.Now()，这里改用 flags=0 +
// 自定义 writer，让日志时间与业务时间来自同一时钟。
type clockLogWriter struct {
	out io.Writer
}

// Write 在每条日志前加节点时间戳。
func (w clockLogWriter) Write(p []byte) (int, error) {
	ts := timesys.Now().Format("2006/01/02 15:04:05")
	if _, err := fmt.Fprintf(w.out, "%s %s", ts, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// setupClockLogging 把标准库 log（含异步日志器 alog）的时间戳接到时间系统。
// 必须在任何日志输出之前调用。
func setupClockLogging() {
	log.SetFlags(0)
	log.SetOutput(clockLogWriter{out: os.Stderr})
}
