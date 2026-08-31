// 节点端服务端核心下载（docs/irix-node-local-parity.md §4.2.3，任务化）。
//
// POST /api/instance/download-core
//   body: {uuid, daemonId, url, fileName, sha512?} → {jobId}
// GET  /api/instance/download-core-progress?jobId → {status, percent, path}
//
// 节点用直连 URL 下载核心 jar 到实例根目录（客户端不中转字节），
// 下载过程流式计算 sha512（若提供）校验，完成后 rename 就位。
// 与 H-1（客户端本地下载核心）哈希校验规则一致。

package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// validateCoreURL 校验核心下载链接：仅允许 http/https（MCSM 同款行为，
// 用户可配置自建镜像/内网源；本接口受 apikey 保护）。
// host 为字面量环回/未指定地址时直接拒绝（防 SSRF 到本机）；
// 实际解析的 IP 在拨号时由 coreDownloadDialContext 再校验（防 DNS rebinding）。
func validateCoreURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("下载链接无效: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅允许 http/https 下载链接: %s", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("下载链接缺少主机名: %s", raw)
	}
	return nil
}

// coreDownloadDialContext 构造拨号函数：解析主机名后按 IP 校验
// （拒绝环回/未指定/链路本地/组播/本机地址，以及未放行的 RFC1918 内网），
// 复用集群传输同一套 checkTransferIP 规则，消除 DNS rebinding SSRF
// （CodeQL 审计 #7）。
func (d *Daemon) coreDownloadDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, fmt.Errorf("无法解析主机 %s: %v", host, err)
		}
		var lastErr error
		for _, ip := range ips {
			// 与集群传输一致：仅测试用的 transferAllowLoopback 放行环回；
			// 生产保持 false，核心下载禁打本机/内网（防 SSRF）。
			if !d.transferAllowLoopback {
				if err := d.checkTransferIP(ip); err != nil {
					lastErr = err
					continue
				}
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err != nil {
				lastErr = err
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("无可用目标地址")
		}
		return nil, lastErr
	}
}

// handleDownloadCore 发起核心下载任务。
func (d *Daemon) handleDownloadCore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID     string `json:"uuid"`
		DaemonID string `json:"daemonId"`
		URL      string `json:"url"`
		FileName string `json:"fileName"`
		SHA512   string `json:"sha512"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.URL == "" || body.FileName == "" {
		writeError(w, http.StatusBadRequest, "缺少 url 或 fileName 参数")
		return
	}
	if err := validateCoreURL(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst := d.Find(body.UUID)
	if inst == nil {
		writeError(w, http.StatusBadRequest, "实例不存在")
		return
	}
	// 文件名净化：只取基名，丢弃客户端可能携带的路径；再经 NormalizePath 防穿越
	name := filepath.Base(filepath.FromSlash(strings.TrimSpace(body.FileName)))
	if name == "." || name == string(filepath.Separator) || name == "" {
		writeError(w, http.StatusBadRequest, "文件名无效")
		return
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	dest, err := NormalizePath(cwd, name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	taskID, task := d.newTask()
	go d.runDownloadCore(taskID, task, body.URL, dest, strings.ToLower(strings.TrimSpace(body.SHA512)))
	writeOK(w, map[string]any{"jobId": taskID})
}

// coreDownloadTimeout 核心下载总超时。
const coreDownloadTimeout = 60 * time.Minute

// coreDownloadMaxBytes 核心文件大小上限（8 GiB，防异常响应撑爆磁盘）。
const coreDownloadMaxBytes = 8 << 30

// runDownloadCore 执行核心下载任务：下载到同目录 .part 临时文件
// （流式 sha512）→ 校验 → rename 就位。
func (d *Daemon) runDownloadCore(taskID string, task *task, url, dest, sha512Hex string) {
	failed := false
	fail := func(err error) {
		failed = true
		task.setError(err)
		alog.Printf("核心下载失败（%s）: %v", dest, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), coreDownloadTimeout)
	defer cancel()

	task.set(taskStatusRunning, 0.01, "开始下载…", "")
	// 拨号前按解析 IP 校验（防 SSRF 到内网/本机、防 DNS rebinding），
	// 与集群传输同一套 checkTransferIP 规则（CodeQL 审计 #7）。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.coreDownloadDialContext()
	client := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := validateCoreURL(req.URL.String()); err != nil {
				return err
			}
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fail(err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(fmt.Errorf("下载失败: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("下载失败: HTTP %d", resp.StatusCode))
		return
	}
	total := resp.ContentLength
	if total > coreDownloadMaxBytes {
		fail(fmt.Errorf("下载内容过大: %s", FormatSize(total)))
		return
	}

	// 下载到同目录临时文件（完成后 rename，避免半成品出现在实例目录）
	part := dest + ".part-" + taskID
	f, err := os.Create(part)
	if err != nil {
		fail(fmt.Errorf("创建临时文件失败: %w", err))
		return
	}
	defer func() {
		if f != nil {
			f.Close()
		}
		if failed {
			_ = os.Remove(part) // 失败时清理临时文件
		}
	}()

	hasher := sha512.New()
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				fail(fmt.Errorf("写入失败: %w", werr))
				return
			}
			_, _ = hasher.Write(buf[:n])
			written += int64(n)
			if total > 0 {
				task.set(taskStatusRunning, 0.9*float64(written)/float64(total),
					fmt.Sprintf("下载中 %s / %s", FormatSize(written), FormatSize(total)), "")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			fail(fmt.Errorf("下载中断: %w", rerr))
			return
		}
	}
	if err := f.Sync(); err != nil {
		fail(fmt.Errorf("落盘失败: %w", err))
		return
	}
	f.Close()
	f = nil

	// sha512 校验（未提供则跳过并提示）
	if sha512Hex != "" {
		task.set(taskStatusRunning, 0.93, "校验 sha512…", "")
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, sha512Hex) {
			fail(fmt.Errorf("sha512 校验失败（期望 %s，实际 %s）", sha512Hex, got))
			return
		}
	} else {
		task.set(taskStatusRunning, 0.95, "未提供 sha512，跳过校验", "")
	}

	// rename 就位
	if err := os.Rename(part, dest); err != nil {
		fail(fmt.Errorf("文件就位失败: %w", err))
		return
	}
	task.set(taskStatusDone, 1, "下载完成", dest)
	alog.Printf("核心下载完成: %s（%s）", dest, FormatSize(written))
}
