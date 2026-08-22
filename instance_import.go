// 导入目录创建实例（docs/irix-node-local-parity.md §4.2.4）。
//
// POST /api/instance/import body: {daemonId, path, nickname?} → {instanceUuid}
//
// 节点校验目录存在 → 扫描内容（根目录存在 *.jar / eula.txt / server.properties
// 等服务端特征即判定为可导入）→ 自动创建实例（cwd=该目录，启动命令留空由
// 用户在配置页填写），与客户端本地「导入实例」体验对齐。

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleInstanceImport 导入目录创建实例。
func (d *Daemon) handleInstanceImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DaemonID string `json:"daemonId"`
		Path     string `json:"path"`
		Nickname string `json:"nickname"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	abs, err := normalizeCwd(body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "目录不存在: "+body.Path)
		return
	}

	// 目录不得已被其他实例占用（同一 cwd 只允许一个实例）
	d.mu.Lock()
	for _, inst := range d.Instances {
		inst.mu.Lock()
		cwd := inst.Config.Cwd
		inst.mu.Unlock()
		if cwd != "" && sameFilePath(cwd, abs) {
			d.mu.Unlock()
			writeError(w, http.StatusBadRequest, "该目录已被实例使用，请勿重复导入")
			return
		}
	}
	d.mu.Unlock()

	// 特征扫描：无明显服务端特征时拒绝导入，避免把任意目录建成空实例
	if !importableDir(abs) {
		writeError(w, http.StatusBadRequest,
			"目录中未发现服务端特征（根目录 *.jar、eula.txt、server.properties 等），无法导入")
		return
	}

	nickname := strings.TrimSpace(body.Nickname)
	if nickname == "" {
		nickname = filepath.Base(abs)
	}
	cfg := InstanceConfig{
		Nickname: nickname,
		Cwd:      abs,
		Type:     "universal",
	}
	cfg.FillDefaults()
	d.applyVaultDefault(&cfg)
	cfg.CreateDatetime = time.Now().UnixMilli()
	cfg.LastDatetime = cfg.CreateDatetime
	inst := NewInstance("", cfg)
	if err := d.Add(inst); err != nil {
		writeError(w, http.StatusInternalServerError, "保存实例失败: "+err.Error())
		return
	}
	alog.Printf("导入目录创建实例成功: %s（%s，cwd=%s）", inst.InstanceUuid, nickname, abs)
	writeOK(w, map[string]any{"instanceUuid": inst.InstanceUuid})
}

// importableDir 判断目录是否包含可导入的服务端特征。
// 判定规则（启发式，任一命中即可导入）：
//   - 根目录存在任意 .jar（服务端/插件核心）
//   - 存在常见服务端标志文件：eula.txt / server.properties / bukkit.yml /
//     spigot.yml / paper.yml / purpur.yml / bungee.yml / velocity.toml / version.json
func importableDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if e.IsDir() {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".jar"):
			return true
		case name == "eula.txt", name == "server.properties",
			name == "bukkit.yml", name == "spigot.yml", name == "paper.yml",
			name == "purpur.yml", name == "bungee.yml", name == "velocity.toml",
			name == "version.json":
			return true
		}
	}
	return false
}
