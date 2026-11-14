// MSLX 面板依赖的实例信息 / FRP 列表等只读接口。
//
// 面板控制台会读取「实例详情」与「通用设置」两套结构（InstanceInfoModel /
// ServerInfo），两者都要求相当完整的字段：`java`、`core`、`minM/maxM`、`args`、
// 以及从 server.properties 解析出来的 `mcConfig`（端口/难度/模式/地图/正版验证）。
// 少一个字段前端就会直接抛异常（例如 `res.java.startsWith(...)`），
// 表现为「获取配置失败: Cannot read properties of undefined」。
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// handleFRP FRP 相关接口：本节点不提供 FRP 隧道，列表按空处理。
//
// 面板控制台加载时会拉 /api/frp/list；501 会弹出红色错误提示，
// 而空列表在语义上就是「没有 FRP 隧道」，页面正常渲染。
func (ms *mslxCompat) handleFRP(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 || segs[0] == "list" {
		mslxOK(w, []any{}, "获取成功")
		return
	}
	mslxFail(w, http.StatusNotImplemented,
		"irix-node 兼容层不提供 MSLX FRP 隧道管理（列表按空返回）；请求："+strings.Join(segs, "/"))
}

// mslxServerInfo 生成面板所需的完整实例信息（InstanceInfoModel + ServerInfo 超集）。
func (ms *mslxCompat) mslxServerInfo(inst *Instance, id uint32) map[string]any {
	inst.mu.Lock()
	cfg := inst.Config
	status := inst.Status
	uptime := ""
	if started := inst.Started; started > 0 {
		uptime = strconv.Itoa(started)
	}
	inst.mu.Unlock()

	javaBin, core, args := parseJavaCommand(cfg.StartCommand)
	minMB, maxMB := parseMemLimits(cfg.StartCommand)
	props := readServerProperties(cfg.Cwd)

	return map[string]any{
		// InstanceInfoModel（控制台「实例概览」）
		"id":             id,
		"name":           cfg.Nickname,
		"basePath":       cfg.Cwd,
		"java":           javaBin,
		"args":           args,
		"core":           core,
		"enablePty":      false,
		"minM":           minMB,
		"maxM":           maxMB,
		"status":         mslxStatus(status),
		"statusText":     mslxStatusText(status),
		"uptime":         uptime,
		"monitorPlayers": false,
		"mcConfig":       props,
		// ServerInfo（「实例设置」通用页）
		"ID":                     id,
		"Name":                   cfg.Nickname,
		"Base":                   cfg.Cwd,
		"Java":                   javaBin,
		"Core":                   core,
		"MinM":                   minMB,
		"MaxM":                   maxMB,
		"Args":                   args,
		"StopCommand":            firstNonEmpty(cfg.StopCommand, "stop"),
		"ExpireTime":             nil,
		"ForceExitDelay":         10,
		"YggdrasilApiAddr":       "",
		"BackupMaxCount":         20,
		"BackupDelay":            10,
		"BackupPath":             "MSLX://Backup/Instance",
		"AllowOriginASCIIColors": true,
		"EnablePty":              false,
		"MonitorPlayers":         false,
		"AutoRestart":            cfg.EventTask.AutoRestart,
		"ForceAutoRestart":       true,
		"RunOnStartup":           cfg.EventTask.AutoStart,
		"IgnoreEula":             false,
		"ForceJvmUTF8":           false,
		"InputEncoding":          firstNonEmpty(cfg.IE, "utf-8"),
		"OutputEncoding":         firstNonEmpty(cfg.OE, "utf-8"),
		"FileEncoding":           "utf-8",
		"ServerPropertiesPath":   "server.properties",
		"PluginsPath":            "plugins",
		"ModsPath":               "mods",
		"WorldPath":              "world",
		"RegionPath":             "region",
		"BindFrpId":              nil,
		"RconMode":               "off",
	}
}

// parseJavaCommand 从启动命令里拆出 java / core / args。
//
// 支持两种形态：`java -Xms.. -Xmx.. [args] -jar core.jar nogui` 与
// MCDR 的 `python -m mcdreforged`（此时 core 保持 server.jar、java 置 "java"，
// 由面板的设置页按 MCDR 分支处理）。
func parseJavaCommand(cmd string) (javaBin, core, args string) {
	fields := splitCommandLoose(cmd)
	if len(fields) == 0 {
		return "java", "server.jar", ""
	}
	javaBin = fields[0]
	core = "server.jar"
	var rest []string
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if f == "-jar" && i+1 < len(fields) {
			core = filepath.Base(fields[i+1])
			i++
			continue
		}
		if strings.HasPrefix(f, "-Xms") || strings.HasPrefix(f, "-Xmx") {
			continue
		}
		rest = append(rest, f)
	}
	args = strings.TrimSpace(strings.Join(rest, " "))
	return javaBin, core, args
}

// splitCommandLoose 宽松切分命令行（保留引号内容）。
func splitCommandLoose(cmd string) []string {
	var out []string
	var cur strings.Builder
	inQuote := rune(0)
	for _, r := range cmd {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			inQuote = r
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseMemLimits 从启动命令里解析 -Xms/-Xmx（返回 MB）。
func parseMemLimits(cmd string) (minMB, maxMB int) {
	re := regexp.MustCompile(`-Xm[sx](\d+)([kKmMgG]?)`)
	for _, m := range re.FindAllStringSubmatch(cmd, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "g":
			n *= 1024
		case "k":
			n /= 1024
		}
		if strings.Contains(strings.ToLower(m[0]), "-xms") {
			if minMB == 0 {
				minMB = n
			}
		} else {
			if maxMB == 0 {
				maxMB = n
			}
		}
	}
	return minMB, maxMB
}

// readServerProperties 读取实例目录的 server.properties，返回面板要的 mcConfig。
func readServerProperties(cwd string) map[string]any {
	out := map[string]any{
		"difficulty":             "easy",
		"levelName":              "world",
		"gamemode":               "survival",
		"serverPort":             "25565",
		"onlineMode":             "true",
		"serverPropertiesPath":   "server.properties",
		"serverPropertiesExists": false,
	}
	path := filepath.Join(cwd, "server.properties")
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	out["serverPropertiesExists"] = true
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "difficulty":
			out["difficulty"] = v
		case "level-name":
			out["levelName"] = v
		case "gamemode":
			out["gamemode"] = v
		case "server-port":
			out["serverPort"] = v
		case "online-mode":
			out["onlineMode"] = v
		}
	}
	return out
}
