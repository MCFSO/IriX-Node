// 插件/Mod 元数据（docs/irix-node-local-parity.md §4.4）。
//
// GET /api/instance/plugins?uuid&daemonId
// 响应 data: {items: [{type: plugin|mod, path, fileName, size, name,
//   description, version, iconBase64?, configDir?}]}
//
// 节点端解析 jar 内 plugin.yml / paper-plugin.yml / fabric.mod.json /
// META-INF/mods.toml（解析逻辑对齐客户端 jar_metadata_service.dart），
// mods 目录递归（版本子目录），iconBase64 为图标文件 base64（PNG）。
// 上传/删除/下载沿用现有文件 API，本接口只补元数据。

package main

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxJarMetaBytes jar 内元数据文件读取上限（超大文件跳过解析）。
const maxJarMetaBytes = 512 << 10

// maxIconBytes 图标文件大小上限（防超大图标 base64 撑爆响应）。
const maxIconBytes = 256 << 10

// maxJarsScanned 单次扫描的 jar 数量上限（防超大实例目录拖垮请求）。
const maxJarsScanned = 500

// pluginItem 插件/Mod 元数据条目。
type pluginItem struct {
	Type        string `json:"type"`        // plugin | mod
	Path        string `json:"path"`        // 相对 cwd 的路径（/ 开头）
	FileName    string `json:"fileName"`    // 文件名
	Size        int64  `json:"size"`        // 字节
	Name        string `json:"name"`        // 显示名
	Description string `json:"description"` // 描述（可空）
	Version     string `json:"version"`     // 版本（可空）
	IconBase64  string `json:"iconBase64,omitempty"`
	ConfigDir   string `json:"configDir,omitempty"`
}

// handleInstancePlugins 获取实例的插件/Mod 元数据。
// GET /api/instance/plugins?uuid&daemonId
func (d *Daemon) handleInstancePlugins(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if cwd == "" {
		writeError(w, http.StatusBadRequest, "实例工作目录为空")
		return
	}

	var items []pluginItem
	// 插件：仅 plugins/ 根目录一层（避免把子目录里的开发包当插件）
	pluginDir := filepath.Join(cwd, "plugins")
	if entries, err := os.ReadDir(pluginDir); err == nil {
		for _, e := range entries {
			if len(items) >= maxJarsScanned {
				break
			}
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
				continue
			}
			if item := d.parsePluginJar(cwd, pluginDir, e.Name(), "plugin"); item != nil {
				items = append(items, *item)
			}
		}
	}
	// Mods：递归（版本子目录）
	modDir := filepath.Join(cwd, "mods")
	if err := filepath.Walk(modDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(items) >= maxJarsScanned {
			return filepath.SkipAll
		}
		if info.IsDir() {
			// 跳过隐藏目录
			if strings.HasPrefix(info.Name(), ".") && path != modDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".jar") {
			return nil
		}
		if item := d.parsePluginJar(cwd, filepath.Dir(path), info.Name(), "mod"); item != nil {
			items = append(items, *item)
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "扫描 mods 目录失败: "+err.Error())
		return
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "plugin" // plugin 在前
		}
		return items[i].FileName < items[j].FileName
	})
	writeOK(w, map[string]any{"items": items})
}

// parsePluginJar 解析单个 jar 的元数据；无法解析或非有效插件返回 nil。
func (d *Daemon) parsePluginJar(cwd, dir, name, kind string) *pluginItem {
	path := filepath.Join(dir, name)
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer zr.Close()

	info, _ := os.Stat(path)
	item := &pluginItem{
		Type:     kind,
		Path:     slashRel(cwd, path),
		FileName: name,
		Size:     info.Size(),
	}
	// 按优先级尝试各元数据文件
	var (
		yamlData  []byte
		jsonData  []byte
		tomlData  []byte
		metaFound bool
	)
	iconFile := ""
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		switch {
		case lower == "plugin.yml" || lower == "paper-plugin.yml":
			if yamlData == nil {
				yamlData, _ = readZipEntry(f, maxJarMetaBytes)
			}
		case lower == "fabric.mod.json":
			if jsonData == nil {
				jsonData, _ = readZipEntry(f, maxJarMetaBytes)
			}
		case lower == "meta-inf/mods.toml":
			if tomlData == nil {
				tomlData, _ = readZipEntry(f, maxJarMetaBytes)
			}
		}
		if yamlData != nil && jsonData != nil && tomlData != nil {
			break
		}
	}

	// plugin.yml / paper-plugin.yml（YAML 顶层标量）
	if len(yamlData) > 0 {
		meta := parseSimpleYAML(yamlData)
		item.Name = firstNonEmpty(meta["name"], strings.TrimSuffix(name, ".jar"))
		item.Description = meta["description"]
		item.Version = meta["version"]
		iconFile = firstNonEmpty(meta["icon"], meta["logo"], meta["iconfile"])
		metaFound = true
	}
	// fabric.mod.json
	if len(jsonData) > 0 {
		var fm struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Version     string `json:"version"`
			Icon        string `json:"icon"`
		}
		if err := json.Unmarshal(jsonData, &fm); err == nil {
			item.Name = firstNonEmpty(fm.Name, fm.ID, item.Name)
			item.Description = firstNonEmpty(fm.Description, item.Description)
			item.Version = firstNonEmpty(fm.Version, item.Version)
			if fm.Icon != "" {
				iconFile = fm.Icon
			}
			metaFound = true
		}
	}
	// META-INF/mods.toml
	if len(tomlData) > 0 {
		mods := parseModsTOML(tomlData)
		if len(mods) > 0 {
			m := mods[0]
			item.Name = firstNonEmpty(m["displayname"], m["modid"], item.Name)
			item.Description = firstNonEmpty(m["description"], item.Description)
			item.Version = firstNonEmpty(m["version"], item.Version)
			if m["logofile"] != "" {
				iconFile = m["logofile"]
			}
			metaFound = true
		}
	}
	if !metaFound {
		return nil // 无任何元数据：不是可识别的插件/Mod
	}

	// 图标：从 jar 内读取 → base64
	if iconFile != "" {
		for _, f := range zr.File {
			if !strings.EqualFold(f.Name, iconFile) {
				continue
			}
			if data, err := readZipEntry(f, maxIconBytes); err == nil && len(data) > 0 {
				item.IconBase64 = base64.StdEncoding.EncodeToString(data)
			}
			break
		}
	}
	// 配置目录：按 name/文件名匹配
	if cd := findPluginConfigDir(cwd, item.Name, strings.TrimSuffix(name, ".jar")); cd != "" {
		item.ConfigDir = cd
	}
	return item
}

// readZipEntry 读取 zip 条目内容（带上限）。
func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if int64(f.UncompressedSize64) > limit {
		return nil, io.ErrUnexpectedEOF
	}
	return io.ReadAll(io.LimitReader(rc, limit))
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseSimpleYAML 极简 YAML 顶层标量解析（plugin.yml 足够）：
// 只取缩进为 0 的 `key: value` 行，去注释/列表/多行块；值去引号。
func parseSimpleYAML(data []byte) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue // 仅顶层键
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.TrimSpace(line[i+1:])
		if val == "" || strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">") {
			continue // 块标量忽略
		}
		if strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
			continue // 列表/映射忽略
		}
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out
}

// parseModsTOML 极简 mods.toml 解析：提取 [[mods]] 表的标量键值。
// 多行描述（”'...”'）只取首行，足够展示用途。
func parseModsTOML(data []byte) []map[string]string {
	var mods []map[string]string
	var cur map[string]string
	inMods := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[[mods]]") {
			inMods = true
			cur = map[string]string{}
			mods = append(mods, cur)
			continue
		}
		if strings.HasPrefix(line, "[") {
			inMods = false // 其他表/嵌套表
			continue
		}
		if !inMods {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		if key != "" {
			cur[key] = val
		}
	}
	return mods
}

// slashRel 返回 path 相对 cwd 的路径（/ 开头、/ 分隔，如 /plugins/x.jar）。
func slashRel(cwd, path string) string {
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		return ""
	}
	return "/" + filepath.ToSlash(rel)
}

// findPluginConfigDir 按 name / 文件名匹配配置目录（可空）。
// 候选：config/<name>、plugins/<name>、mods/<name>；
// 含常见后缀变体（EssentialsX → Essentials、Sodium → sodium）以对齐
// 真实插件目录命名习惯。
func findPluginConfigDir(cwd, name, fileNameBase string) string {
	bases := []string{name, fileNameBase}
	for _, s := range []string{"X", "Mod"} {
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			bases = append(bases, strings.TrimSuffix(name, s))
		}
	}
	if lower := strings.ToLower(name); lower != name {
		bases = append(bases, lower)
	}
	seen := map[string]bool{}
	for _, base := range bases {
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		for _, sub := range []string{"config", "plugins", "mods"} {
			dir := filepath.Join(cwd, sub, base)
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				return slashRel(cwd, dir)
			}
		}
	}
	return ""
}
