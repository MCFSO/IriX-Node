// Bastille 挂载相关的纯逻辑（路径解析 / fstab 读写），不依赖 FreeBSD 专属
// 系统调用，故放在无构建标签文件中，使非 FreeBSD 平台也能编译与单元测试
// （docs/irix-node-container-api.md §4.10）。真正的挂载执行（bastille mount /
// umount、系统 mount 命令）仍在 container_bastille.go（//go:build freebsd）。

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bastilleRoot Bastille 数据目录。
// 声明为 var（而非 const）以便测试将其指向临时目录，验证 fstab 解析 /
// 挂载列表等纯逻辑，生产环境仍为固定路径 /usr/local/bastille。
var bastilleRoot = "/usr/local/bastille"

// fstabEntry 单条 fstab 挂载（device mountpoint fstype options dump pass）。
type fstabEntry struct {
	Device  string
	Mount   string
	Fstype  string
	Options string
}

// bastilleFstabPath 返回 jail 的 fstab 路径。
func bastilleFstabPath(name string) string {
	return filepath.Join(bastilleRoot, "jails", name, "fstab")
}

// bastilleJailRoot 返回 jail 根目录的宿主路径（thin jail 为指向 releases 的符号链接）。
func bastilleJailRoot(name string) string {
	return filepath.Join(bastilleRoot, "jails", name, "root")
}

// readFstab 解析 jail 的 fstab 文件为条目列表（跳过注释与空行）。
func readFstab(name string) ([]fstabEntry, error) {
	data, err := os.ReadFile(bastilleFstabPath(name))
	if err != nil {
		return nil, err
	}
	var out []fstabEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		opts := ""
		if len(fields) >= 4 {
			opts = strings.Trim(fields[3], `"'`)
		}
		out = append(out, fstabEntry{Device: fields[0], Mount: fields[1], Fstype: fields[2], Options: opts})
	}
	return out, nil
}

// writeFstab 整文件写回 fstab（保留原有注释与空行结构）。
func writeFstab(name string, lines []string) error {
	return os.WriteFile(bastilleFstabPath(name), []byte(strings.Join(lines, "\n")), 0o644)
}

// bastilleFstabRemove 从 fstab 移除匹配 mount 的条目（可选限定 fstype）；返回是否移除。
// mount 为 jail 内路径（如 /data），但 fstab 中 bastille 写入的 mountpoint 可能是
// 宿主绝对路径（jailRoot + /data）。两种形式都视为匹配，避免卸载时删不掉残留条目
// （删不掉会导致下次挂载报「已存在」而再也挂不上）。
func bastilleFstabRemove(name, mount, fstype string) bool {
	jailRoot := bastilleJailRoot(name)
	hostMount := filepath.Join(jailRoot, strings.TrimPrefix(mount, "/"))
	lines := strings.Split(string(mustReadFstab(name)), "\n")
	var kept []string
	removed := false
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 3 && (fields[1] == mount || fields[1] == hostMount) &&
			(fstype == "" || fields[2] == fstype) {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if removed {
		_ = writeFstab(name, kept)
	}
	return removed
}

// mustReadFstab 读取 fstab 原文（失败返回空串；bastilleFstabRemove 内部使用）。
func mustReadFstab(name string) []byte {
	data, err := os.ReadFile(bastilleFstabPath(name))
	if err != nil {
		return nil
	}
	return data
}

// bastilleMountListFstab 仅从 fstab 构造挂载列表（纯逻辑，不依赖系统 mount 命令）。
// fstab 中 bastille 写入的 mountpoint 可能是宿主绝对路径（jailRoot + jail内路径），
// 也可能是 jail 内路径；统一归一化为 jail 内路径，与前端视角保持一致，
// 否则前端按 jail 内路径查找会找不到文件、卸载时也匹配不上。
// 返回的条目 permanent=true；调用方（freebsd 版 bastilleMountList）再合并当前实际挂载。
func bastilleMountListFstab(name string) ([]map[string]any, error) {
	entries, err := readFstab(name)
	if err != nil {
		return nil, fmt.Errorf("读取 fstab 失败: %w", err)
	}
	items := make([]map[string]any, 0, len(entries))
	jailRoot := bastilleJailRoot(name)
	for _, e := range entries {
		dst := e.Mount
		if strings.HasPrefix(dst, jailRoot) {
			if rel := strings.TrimPrefix(dst, jailRoot); rel != "" {
				dst = rel
			}
		}
		item := map[string]any{
			"dst":       dst,
			"fstype":    e.Fstype,
			"options":   e.Options,
			"permanent": true,
		}
		if e.Device != "proc" && e.Device != "devfs" {
			item["src"] = e.Device
		}
		items = append(items, item)
	}
	return items, nil
}

// parseMountLine 解析 mount 输出行 "device on mountpoint (fstype, options...)"。
func parseMountLine(line string) (dev, mnt, fstype, opts string, ok bool) {
	idx := strings.Index(line, " on ")
	if idx < 0 {
		return "", "", "", "", false
	}
	dev = strings.TrimSpace(line[:idx])
	rest := line[idx+4:]
	pi := strings.Index(rest, " (")
	if pi < 0 {
		return "", "", "", "", false
	}
	mnt = strings.TrimSpace(rest[:pi])
	inner := strings.TrimSuffix(rest[pi+2:], ")")
	parts := strings.SplitN(inner, ",", 2)
	if len(parts) == 0 {
		return "", "", "", "", false
	}
	fstype = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		opts = strings.TrimSpace(parts[1])
	}
	return dev, mnt, fstype, opts, true
}
