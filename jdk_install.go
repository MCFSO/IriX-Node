// 节点端 JDK 安装（docs/irix-node-local-parity.md §4.2.2，任务化）。
//
// POST   /api/runtime/java/install          body: {major: 21} → {jobId}
// GET    /api/runtime/java/install-progress ?jobId → {status, percent, message, path}
// DELETE /api/runtime/java                  ?major=21 → 卸载
//
// 节点直连 Adoptium API 下载对应大版本的 JDK（客户端不中转字节），
// 解压安装到 {data}/jdk/jdk-<major>/，完成后 path 指向 bin/java。
// 进度：0~0.9 下载（按字节），0.9~0.95 解压（按文件数），完成后 1.0。

package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// adoptiumAPI Adoptium API 基础地址（测试可覆盖为本地服务）。
var adoptiumAPI = "https://api.adoptium.net/v3"

// jdkInstallMu 串行化 JDK 安装：同大版本并发安装互斥，避免目录互相覆盖。
var jdkInstallMu sync.Mutex

// jdkInstallTimeout 安装总超时（下载 + 解压）。
const jdkInstallTimeout = 30 * time.Minute

// handleInstallJava 发起 JDK 安装任务。
// POST /api/runtime/java/install body: {major: 21}
func (d *Daemon) handleInstallJava(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Major int `json:"major"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Major < 8 || body.Major > 30 {
		writeError(w, http.StatusBadRequest, "大版本号无效（支持 8~30）: "+fmt.Sprint(body.Major))
		return
	}
	taskID, task := d.newTask()
	go d.runInstallJDK(taskID, task, body.Major)
	writeOK(w, map[string]any{"jobId": taskID})
}

// handleUninstallJava 卸载指定版本 JDK。
// DELETE /api/runtime/java?major=21
func (d *Daemon) handleUninstallJava(w http.ResponseWriter, r *http.Request) {
	major := atoiDefault(queryParam(r, "major"), 0)
	if major <= 0 {
		writeError(w, http.StatusBadRequest, "缺少 major 参数")
		return
	}
	target := filepath.Join(d.DataDir, "jdk", fmt.Sprintf("jdk-%d", major))
	if _, err := os.Stat(target); err != nil {
		writeError(w, http.StatusNotFound, "该版本 JDK 未安装")
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeError(w, http.StatusInternalServerError, "卸载失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// adoptiumOS 当前平台对应的 Adoptium os 参数。
func adoptiumOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "mac"
	case "linux":
		return "linux"
	case "freebsd":
		return "freebsd"
	case "aix":
		return "aix"
	case "solaris":
		return "solaris"
	}
	return "linux"
}

// adoptiumArch 当前 CPU 架构对应的 Adoptium architecture 参数。
func adoptiumArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x32"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	case "riscv64":
		return "riscv64"
	}
	return "x64"
}

// adoptiumAsset Adoptium API 资产响应的最小结构。
type adoptiumAsset struct {
	Binary struct {
		Package struct {
			Link     string `json:"link"`
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Checksum string `json:"checksum"`
		} `json:"package"`
		JavaVersion string `json:"java_version"`
	} `json:"binary"`
}

// validateDownloadURL 校验下载链接：仅允许 https；
// 环回地址例外（测试用本地服务，生产不受影响）。
func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("下载链接无效: %v", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return nil
	}
	return fmt.Errorf("仅允许 https 下载链接: %s", raw)
}

// jdkHTTPClient 构造安装下载用客户端（重定向逐跳校验 https）。
func jdkHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return validateDownloadURL(req.URL.String())
		},
	}
}

// runInstallJDK 执行 JDK 安装任务：查 API → 下载 → 解压 → 就位 → 验证。
func (d *Daemon) runInstallJDK(taskID string, task *task, major int) {
	jdkInstallMu.Lock()
	defer jdkInstallMu.Unlock()

	fail := func(err error) {
		task.setError(err)
		alog.Printf("JDK %d 安装失败: %v", major, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), jdkInstallTimeout)
	defer cancel()

	// 1. 查询 Adoptium API 获取下载链接
	task.set(taskStatusRunning, 0.01, "查询 Adoptium 下载信息…", "")
	apiURL := fmt.Sprintf("%s/assets/latest/%d/hotspot?architecture=%s&image_type=jdk&os=%s&vendor=eclipse",
		adoptiumAPI, major, adoptiumArch(), adoptiumOS())
	client := jdkHTTPClient(30 * time.Second)
	resp, err := client.Get(apiURL)
	if err != nil {
		fail(fmt.Errorf("查询 Adoptium API 失败: %w", err))
		return
	}
	var assets []adoptiumAsset
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&assets)
	resp.Body.Close()
	if decodeErr != nil || resp.StatusCode != http.StatusOK || len(assets) == 0 {
		fail(fmt.Errorf("Adoptium 无该版本 JDK 或响应异常（HTTP %d）", resp.StatusCode))
		return
	}
	link := assets[0].Binary.Package.Link
	if err := validateDownloadURL(link); err != nil {
		fail(err)
		return
	}
	ver := assets[0].Binary.JavaVersion
	task.set(taskStatusRunning, 0.02, fmt.Sprintf("已获取 JDK %s 下载信息", ver), "")

	// 2. 下载到临时文件
	jdkRoot := filepath.Join(d.DataDir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		fail(fmt.Errorf("创建 jdk 目录失败: %w", err))
		return
	}
	tmpDir := filepath.Join(jdkRoot, ".install-"+taskID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		fail(err)
		return
	}
	defer os.RemoveAll(tmpDir) // 失败/成功均清理临时目录

	archName := assets[0].Binary.Package.Name
	if archName == "" {
		archName = fmt.Sprintf("jdk-%d", major)
	}
	archive := filepath.Join(tmpDir, archName)
	dlCtx, dlCancel := context.WithTimeout(ctx, 25*time.Minute)
	defer dlCancel()
	task.set(taskStatusRunning, 0.05, "开始下载…", "")
	if err := downloadFile(dlCtx, client, link, archive, task, 0.05, 0.90); err != nil {
		fail(fmt.Errorf("下载失败: %w", err))
		return
	}

	// 3. 解压到独立子目录（下载的归档与解压根目录分开，避免混淆）
	task.set(taskStatusRunning, 0.90, "解压中…", "")
	extractDir := filepath.Join(tmpDir, "x")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		fail(err)
		return
	}
	if err := extractJDKArchive(archive, extractDir, task); err != nil {
		fail(fmt.Errorf("解压失败: %w", err))
		return
	}

	// 4. 归档根目录就位为 jdk-<major>
	entries, err := os.ReadDir(extractDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		fail(fmt.Errorf("JDK 归档结构异常（期望单个根目录）"))
		return
	}
	target := filepath.Join(jdkRoot, fmt.Sprintf("jdk-%d", major))
	if err := os.RemoveAll(target); err != nil {
		fail(fmt.Errorf("清理旧目录失败: %w", err))
		return
	}
	if err := os.Rename(filepath.Join(extractDir, entries[0].Name()), target); err != nil {
		fail(fmt.Errorf("移动安装目录失败: %w", err))
		return
	}

	// 5. 验证 bin/java 存在
	javaPath := filepath.Join(target, "bin", "java")
	if runtime.GOOS == "windows" {
		javaPath += ".exe"
	}
	if _, err := os.Stat(javaPath); err != nil {
		fail(fmt.Errorf("安装后找不到 java 可执行文件: %v", err))
		return
	}
	task.set(taskStatusDone, 1, fmt.Sprintf("JDK %d 安装完成（%s）", major, ver), javaPath)
	alog.Printf("JDK %d 安装完成: %s", major, javaPath)
}

// downloadFile 下载到文件并更新任务进度（percent 区间 [lo, hi] 按字节推进）。
func downloadFile(ctx context.Context, client *http.Client, url, dest string, task *task, lo, hi float64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total > 1<<30 {
		return fmt.Errorf("下载内容过大: %s", FormatSize(total))
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if total > 0 {
				task.set(taskStatusRunning, lo+(hi-lo)*float64(written)/float64(total),
					fmt.Sprintf("下载中 %s / %s", FormatSize(written), FormatSize(total)), "")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// extractJDKArchive 按扩展名解压 JDK 归档（zip / tar.gz）。
func extractJDKArchive(archive, destDir string, task *task) error {
	lower := strings.ToLower(archive)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archive, destDir, task)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archive, destDir, task)
	}
	return fmt.Errorf("不支持的归档格式: %s", archive)
}

// extractZip 解压 zip 到 destDir（防路径穿越；按文件数推进 0.9~0.95）。
func extractZip(archive, destDir string, task *task) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	total := len(zr.File)
	for i, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(destDir, name)
		if !pathWithin(destDir, target) {
			return fmt.Errorf("解压路径越界: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if cerr != nil {
			return cerr
		}
		if task != nil && total > 0 {
			task.set(taskStatusRunning, 0.9+0.05*float64(i+1)/float64(total), "解压中…", "")
		}
	}
	return nil
}

// safeSymlinkTarget 判断在 destDir 内创建指向 linkname 的符号链接是否安全：
// 相对链接按链接所在目录解析，绝对链接直接拒绝；解析结果必须落在 destDir 内，
// 否则视为越界（防止归档内恶意符号链接指向解压目录外的敏感文件）。
func safeSymlinkTarget(destDir, linkPath, linkname string) bool {
	if linkname == "" {
		return false
	}
	// 绝对路径链接一律拒绝（即便指向 destDir 内，跨平台语义不一致且高风险）
	if filepath.IsAbs(filepath.FromSlash(linkname)) {
		return false
	}
	dir := filepath.Dir(linkPath)
	resolved := filepath.Clean(filepath.Join(dir, filepath.FromSlash(linkname)))
	return pathWithin(destDir, resolved)
}

// extractTarGz 解压 tar.gz 到 destDir（防路径穿越；保留可执行权限与符号链接）。
func extractTarGz(archive, destDir string, task *task) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(destDir, name)
		if !pathWithin(destDir, target) {
			return fmt.Errorf("解压路径越界: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			out.Close()
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink:
			if runtime.GOOS == "windows" {
				continue // Windows 的 JDK zip 无符号链接；tar 包内链接跳过
			}
			// 符号链接目标必须解析进 destDir 内：拒绝绝对路径与指向
			// destDir 外的相对链接，防止 tar 内恶意链接逃逸解压目录
			// （CodeQL 审计 #12/#13）。
			if !safeSymlinkTarget(destDir, target, hdr.Linkname) {
				return fmt.Errorf("符号链接目标越界: %s → %s", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			continue // 设备/管道等特殊项忽略
		}
		n++
		if task != nil && n%25 == 0 {
			task.set(taskStatusRunning, 0.9+0.05*float64(n%100)/100.0, "解压中…", "")
		}
	}
	return nil
}
