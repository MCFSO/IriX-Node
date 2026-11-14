// MSLX 实例部署细节：上传文件落位、整合包解压（zip / Modrinth mrpack）、MCDReforged。
//
// 对应 MSLX daemon 的 ServerDeploymentService / CreateServerTask 的行为：
//   - 核心：面板上传的临时文件（coreFileKey）或下载地址（coreUrl）→ 落成 <cwd>/<core>；
//   - 整合包：zip 直接解压到实例目录；.mrpack 先解压，再按 modrinth.index.json
//     下载 files[]，最后把 overrides/ 覆盖到根目录；
//   - MCDReforged：按需 pip install mcdreforged、生成 config.yml，
//     并把实例启动命令改写为 `<python> -m mcdreforged`。
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxPackageEntries 整合包单次解压的条目上限（防 zip 炸弹）。
const maxPackageEntries = 200000

// copyFileTo 复制文件（覆盖目标）。
func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// extractZipInto 把 zip 解压到 root（防目录穿越）。
func extractZipInto(archive, root string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("不是有效的 zip 压缩包: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxPackageEntries {
		return errors.New("压缩包条目过多")
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name == "" {
			continue
		}
		dest, err := safeResolve(root, name)
		if err != nil {
			return fmt.Errorf("压缩包内路径非法（%s）: %w", f.Name, err)
		}
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(rc, 1<<30))
		rc.Close()
		if closeErr := out.Close(); copyErr != nil {
			return copyErr
		} else if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// modrinthIndex Modrinth 整合包的索引（只用得到的字段）。
type modrinthIndex struct {
	Files []struct {
		Path      string   `json:"path"`
		Downloads []string `json:"downloads"`
	} `json:"files"`
}

// applyModrinthPackage 若是 Modrinth 整合包，则按索引下载文件并应用 overrides。
func applyModrinthPackage(cwd string) error {
	indexPath := filepath.Join(cwd, "modrinth.index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil // 普通 zip，无需额外处理
	}
	var idx modrinthIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("modrinth.index.json 解析失败: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, f := range idx.Files {
		if len(f.Downloads) == 0 || strings.TrimSpace(f.Path) == "" {
			continue
		}
		dest, err := safeResolve(cwd, f.Path)
		if err != nil {
			return fmt.Errorf("整合包内路径非法（%s）: %w", f.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		resp, err := client.Get(f.Downloads[0])
		if err != nil {
			return fmt.Errorf("下载整合包文件失败（%s）: %w", f.Path, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("下载整合包文件失败（%s）: HTTP %d", f.Path, resp.StatusCode)
		}
		tmp := dest + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(resp.Body, 1<<30))
		resp.Body.Close()
		if cerr := out.Close(); copyErr != nil {
			_ = os.Remove(tmp)
			return copyErr
		} else if cerr != nil {
			_ = os.Remove(tmp)
			return cerr
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	// overrides/ 与 client-overrides/ 覆盖到实例根目录
	for _, sub := range []string{"overrides", "client-overrides"} {
		srcDir := filepath.Join(cwd, sub)
		if _, err := os.Stat(srcDir); err != nil {
			continue
		}
		if err := copyTreeInto(srcDir, cwd); err != nil {
			return fmt.Errorf("应用 %s 失败: %w", sub, err)
		}
	}
	_ = os.Remove(indexPath)
	return nil
}

// copyTreeInto 把 src 目录内容复制到 dst（覆盖同名文件），用于整合包 overrides。
func copyTreeInto(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFileTo(path, target)
	})
}

// setupMCDR 部署 MCDReforged：安装（可选）→ 写 config.yml → 改写启动命令。
func (ms *mslxCompat) setupMCDR(inst *Instance, req *mslxCreateRequest, cwd string) error {
	python := strings.TrimSpace(req.McdrPython)
	if python == "" {
		python = "python"
	}
	if req.McdrInstall {
		args := []string{"-m", "pip", "install", "--upgrade", "mcdreforged"}
		if mirror := strings.TrimSpace(req.McdrPipMirror); mirror != "" {
			args = append(args, "-i", mirror)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, python, args...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		if err != nil {
			tail := string(out)
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			return fmt.Errorf("安装 MCDReforged 失败（%s %s）: %v\n%s", python, strings.Join(args, " "), err, tail)
		}
	}
	handler := strings.TrimSpace(req.McdrHandler)
	if handler == "" {
		handler = "mcs_handler"
	}
	// config.yml：MCDR 的最小可用配置（start_command 指向真实服务端启动命令）
	startLine := strings.Join(buildJavaCommand(req), " ")
	cfg := "handler: " + handler + "\n" +
		"start_command: " + strconvQuote(startLine) + "\n" +
		"working_directory: .\n" +
		"advanced_console: false\n"
	if err := os.WriteFile(filepath.Join(cwd, "config.yml"), []byte(cfg), 0o644); err != nil {
		return fmt.Errorf("写入 MCDR 配置失败: %w", err)
	}
	// 实例启动命令改为 MCDR
	inst.mu.Lock()
	inst.Config.StartCommand = python + " -m mcdreforged"
	inst.Config.StopCommand = "!!MCDR stop"
	inst.mu.Unlock()
	if err := ms.d.Save(); err != nil {
		alog.Printf("MSLX 兼容层：保存 MCDR 实例启动命令失败：%v", err)
	}
	alog.Printf("MSLX 兼容层：MCDR 部署完成（python=%s, handler=%s, 启动命令=%s -m mcdreforged）",
		python, handler, python)
	return nil
}

// buildJavaCommand 还原未启用 MCDR 时的 java 启动命令（用于 MCDR 的 start_command）。
func buildJavaCommand(req *mslxCreateRequest) []string {
	javaBin := strings.TrimSpace(req.Java)
	if javaBin == "" {
		javaBin = "java"
	}
	parts := []string{javaBin}
	if req.MinM > 0 {
		parts = append(parts, fmt.Sprintf("-Xms%dM", req.MinM))
	}
	if req.MaxM > 0 {
		parts = append(parts, fmt.Sprintf("-Xmx%dM", req.MaxM))
	}
	if extra := strings.TrimSpace(req.Args); extra != "" {
		parts = append(parts, extra)
	}
	core := strings.TrimSpace(req.Core)
	if core == "" {
		core = "server.jar"
	}
	parts = append(parts, "-jar", filepath.Base(core), "nogui")
	return parts
}

// strconvQuote 给 YAML 字符串加引号（保持命令原样）。
func strconvQuote(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
