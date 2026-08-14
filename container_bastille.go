//go:build freebsd

// Bastille 容器能力实现（FreeBSD 节点）：包装系统 bastille CLI（docs/container-support.md §3.3 契约）。
// Bastille 的数据目录固定为 /usr/local/bastille（releases/jails/templates）。
// 命令输出解析均容错：字段缺失 / 格式变化返回默认值，不中断列表。

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bastilleRoot Bastille 数据目录。
const bastilleRoot = "/usr/local/bastille"

// CLI 绝对路径：FreeBSD 上以服务方式启动时 PATH 常不含 /usr/local/bin 与 sbin，
// 依赖 PATH 查找会报 "executable file not found"，全部固定绝对路径。
const (
	bastilleBin = "/usr/local/bin/bastille"
	jlsBin      = "/usr/sbin/jls"
	zfsBin      = "/sbin/zfs"
)

// containerRuntimeInfo 能力探测：FreeBSD 节点 → Bastille。
func containerRuntimeInfo() (runtime, platform, version string, ok bool) {
	v, ok := bastilleAvailable()
	return "bastille", "freebsd", v, ok
}

// bastilleAvailable 探测 bastille CLI 可用性与版本。
func bastilleAvailable() (version string, ok bool) {
	if _, err := os.Stat("/usr/local/bin/bastille"); err != nil {
		return "", false
	}
	// bastille version 输出形如 "0.10.20231013"；失败不阻断可用性判断
	if out, err := cliRun(cliTimeout, bastilleBin, "version"); err == nil {
		return strings.TrimSpace(out), true
	}
	return "", true
}

// releaseSizeEntry 发行版大小缓存条目。
type releaseSizeEntry struct {
	modTime time.Time
	size    int64
}

// releaseSizeCache 发行版目录大小缓存（目录 mtime 未变时复用，避免每次列表全量遍历）。
var releaseSizeCache sync.Map // release 名 → releaseSizeEntry

// bastilleReleaseSize 统计发行版目录总大小（字节，walk 累加普通文件，跳过符号链接）。
func bastilleReleaseSize(name string) int64 {
	dir := filepath.Join(bastilleRoot, "releases", name)
	info, err := os.Stat(dir)
	if err != nil {
		return 0
	}
	if v, ok := releaseSizeCache.Load(name); ok {
		if e := v.(releaseSizeEntry); e.modTime.Equal(info.ModTime()) {
			return e.size
		}
	}
	var size int64
	_ = filepath.WalkDir(dir, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if de.Type().IsRegular() {
			if fi, err := de.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	releaseSizeCache.Store(name, releaseSizeEntry{modTime: info.ModTime(), size: size})
	return size
}

// bastilleReleases 已 bootstrap 的发行版列表（目录名如 "14.1-RELEASE"）。
func bastilleReleases() ([]map[string]any, error) {
	entries, err := os.ReadDir(filepath.Join(bastilleRoot, "releases"))
	if err != nil {
		return nil, fmt.Errorf("读取发行版列表失败: %w", err)
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		info, err := e.Info()
		createdAt := ""
		if err == nil {
			createdAt = info.ModTime().Format("2006-01-02 15:04:05")
		}
		items = append(items, map[string]any{
			"name":      name,
			"version":   name, // 客户端拼标签 name:version，保持完整发行版名
			"sizeBytes": bastilleReleaseSize(name),
			"createdAt": createdAt,
		})
	}
	return items, nil
}

// bastilleBootstrap bootstrap 发行版 → 后台任务 {jobId}。
func bastilleBootstrap(release string) (string, error) {
	jobID := jobs.create()
	if jobID == "" {
		return "", errors.New("任务已满，请稍后重试")
	}
	go runLongJob(jobID, bastilleBin, "bootstrap", release)
	return jobID, nil
}

// bastilleJails jail 列表。
// 名称与 release 从 /usr/local/bastille/jails 目录读取（thin jail 的 root
// 为指向 releases 的符号链接，可据此推断发行版）；运行状态由 jls 判定。
// status 用 "Up"/"Down"（客户端以「含 up」判断运行态）；ports 填充 rdr 规则摘要。
func bastilleJails() ([]map[string]any, error) {
	entries, err := os.ReadDir(filepath.Join(bastilleRoot, "jails"))
	if err != nil {
		return nil, fmt.Errorf("读取 jail 列表失败: %w", err)
	}
	running := bastilleRunningSet()
	rdrRules, _ := bastilleRdrRulesByJail()
	rdrByJail := map[string][]rdrRule{}
	for _, r := range rdrRules {
		rdrByJail[r.Jail] = append(rdrByJail[r.Jail], r)
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		status, state := "Down", "stopped"
		if running[name] {
			status, state = "Up", "running"
		}
		ports := make([]string, 0, 2)
		for _, r := range rdrByJail[name] {
			ports = append(ports, fmt.Sprintf("%s %d -> %d", r.Proto, r.HostPort, r.JailPort))
		}
		items = append(items, map[string]any{
			"name":      name,
			"release":   bastilleJailRelease(name),
			"status":    status,
			"state":     state,
			"ports":     ports,
			"createdAt": "",
		})
	}
	return items, nil
}

// bastilleJailRelease 推断 jail 的发行版：thin jail 的 root 是指向 releases 的符号链接。
func bastilleJailRelease(name string) string {
	link, err := os.Readlink(filepath.Join(bastilleRoot, "jails", name, "root"))
	if err == nil {
		// 形如 ../../releases/14.1-RELEASE/root
		if idx := strings.Index(link, "releases/"); idx >= 0 {
			rest := link[idx+len("releases/"):]
			if slash := strings.IndexByte(rest, '/'); slash > 0 {
				return rest[:slash]
			}
			return rest
		}
	}
	return ""
}

// bastilleRunningSet 返回运行中 jail 名集合（jls 输出的 Hostname 列）。
func bastilleRunningSet() map[string]bool {
	out, err := cliRun(cliTimeout, jlsBin)
	if err != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// jls 表头为 JID/IP/Hostname/Path，首行无数字跳过
		if len(fields) < 3 || fields[0] == "JID" {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err == nil {
			set[fields[2]] = true
		}
	}
	return set
}

// bastilleCreate 创建 jail（docs/container-support.md §3.3 契约）。
// type 映射：thin=默认(无标志) / thick(-T) / clone(-C) / empty(-E, 仅 NAME) / linux(-L)。
// vnetMode：none=共享宿主网络(默认) / vnet(-V INTERFACE，物理网卡) / bridge(-B INTERFACE，桥接网卡)。
// 校验：linux 与任何 VNET 模式互斥；VNET 时 IP 必须含子网掩码、interface 必须非空。
// 创建成功后依次应用：volumes（bastille mount 写 fstab, nullfs）、workdir（exec.start
// 前置 cd）、limits（bastille limits / ZFS 配额）；配置步骤失败记入 warnings 不影响创建结果。
func bastilleCreate(name, release, ip, jtype, vnetMode, iface string,
	volumes []bastilleVolume, workdir string, memoryLimitMb, cpus, diskLimitMb int) (map[string]any, error) {
	if jtype == "" {
		jtype = "thin"
	}
	if vnetMode == "" {
		vnetMode = "none"
	}
	switch jtype {
	case "thin", "thick", "clone", "empty", "linux":
	default:
		return nil, fmt.Errorf("type 仅支持 thin/thick/clone/empty/linux")
	}
	switch vnetMode {
	case "none", "vnet", "bridge":
	default:
		return nil, fmt.Errorf("vnet 仅支持 none/vnet/bridge")
	}
	if jtype == "linux" && vnetMode != "none" {
		return nil, fmt.Errorf("linux 类型与 VNET 模式互斥")
	}
	if vnetMode != "none" {
		if iface == "" {
			return nil, fmt.Errorf("VNET 模式必须提供 interface 参数")
		}
		if !strings.Contains(ip, "/") {
			return nil, fmt.Errorf("VNET 模式 IP 必须含子网掩码（如 10.0.0.2/24）")
		}
	}

	args := []string{"create"}
	switch jtype {
	case "thick":
		args = append(args, "-T")
	case "clone":
		args = append(args, "-C")
	case "empty":
		args = append(args, "-E")
	case "linux":
		args = append(args, "-L")
	}
	// INTERFACE 是位置参数（NAME RELEASE IP [INTERFACE]），不是 -V/-B 的选项参数
	switch vnetMode {
	case "vnet":
		args = append(args, "-V")
	case "bridge":
		args = append(args, "-B")
	}
	args = append(args, name)
	if jtype != "empty" {
		if release == "" {
			return nil, fmt.Errorf("缺少 release 参数")
		}
		if ip == "" {
			return nil, fmt.Errorf("缺少 ip 参数")
		}
		args = append(args, release, ip)
		if vnetMode != "none" {
			args = append(args, iface)
		}
	}
	if _, err := cliRun(cliLongTimeout, bastilleBin, args...); err != nil {
		return nil, err
	}

	// 创建后配置：失败仅告警，不视为创建失败
	var warnings []string
	for _, v := range volumes {
		if v.Source == "" || v.Dest == "" {
			continue
		}
		if err := bastilleMount(name, v.Source, v.Dest); err != nil {
			warnings = append(warnings, fmt.Sprintf("挂载 %s→%s 失败: %v", v.Source, v.Dest, err))
		}
	}
	if workdir != "" {
		if err := bastilleSetWorkdir(name, workdir); err != nil {
			warnings = append(warnings, fmt.Sprintf("设置工作目录失败: %v", err))
		}
	}
	if memoryLimitMb > 0 || cpus > 0 || diskLimitMb > 0 {
		if err := bastilleApplyLimits(name, memoryLimitMb, cpus, diskLimitMb); err != nil {
			warnings = append(warnings, fmt.Sprintf("设置资源限制失败: %v", err))
		}
	}
	return map[string]any{"name": name, "warnings": warnings}, nil
}

// bastilleAction jail 操作（start/stop/restart/destroy）。
// destroy 时：-y 跳过交互确认（必须）；force 附加 -a（摧毁运行中的 jail）。
func bastilleAction(name, action string, force bool) error {
	args := []string{action}
	if action == "destroy" {
		args = append(args, "-y")
		if force {
			args = append(args, "-a")
		}
	}
	args = append(args, name)
	_, err := cliRun(cliTimeout, bastilleBin, args...)
	return err
}

// bastilleLogs jail 控制台日志尾部（bastille logs 输出全部，Go 侧截取尾部 N 行）。
func bastilleLogs(name string, tail int) (string, error) {
	out, err := cliRun(cliTimeout, bastilleBin, "logs", name)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// bastilleCmd jail 内执行命令（经 sh -c 支持 shell 语法）。
func bastilleCmd(name, command string) (string, error) {
	return cliRun(cliTimeout, bastilleBin, "cmd", name, "sh", "-c", command)
}

// bastilleConfig 返回 jail.conf 内容。
func bastilleConfig(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(bastilleRoot, "jails", name, "jail.conf"))
	if err != nil {
		return "", fmt.Errorf("读取 jail.conf 失败: %w", err)
	}
	return string(data), nil
}

// bastilleMounts 返回 fstab 挂载列表（每行一个挂载）。
func bastilleMounts(name string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(bastilleRoot, "jails", name, "fstab"))
	if err != nil {
		return nil, fmt.Errorf("读取 fstab 失败: %w", err)
	}
	mounts := make([]string, 0, 4)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			mounts = append(mounts, line)
		}
	}
	return mounts, nil
}

// bastilleMount 挂载宿主机目录到 jail（nullfs；写入 fstab，运行中则即时挂载）。
// 语法：bastille mount JAIL HOST JAILPATH nullfs
func bastilleMount(name, source, dest string) error {
	_, err := cliRun(cliTimeout, bastilleBin, "mount", name, source, dest, "nullfs")
	return err
}

// bastilleUmount 解除 jail 挂载（按目标路径）。
func bastilleUmount(name, dest string) error {
	_, err := cliRun(cliTimeout, bastilleBin, "umount", name, dest)
	return err
}

// bastilleSetWorkdir 强制 jail 的工作目录：把 exec.start 改写为
// `cd <workdir> && <原 exec.start>`（数据目录挂载后强制 cwd）。
func bastilleSetWorkdir(name, workdir string) error {
	confPath := filepath.Join(bastilleRoot, "jails", name, "jail.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	// 转义单引号（jail.conf 用单引号包裹 exec.start 值）
	esc := strings.ReplaceAll(workdir, `'`, `'\''`)
	content := string(data)
	if idx := strings.Index(content, "exec.start"); idx >= 0 {
		// 找到该行行尾（第一个 ';'）
		if semi := strings.Index(content[idx:], ";"); semi >= 0 {
			lineEnd := idx + semi + 1
			replacement := fmt.Sprintf("exec.start = 'cd %s && /bin/sh /etc/rc';", esc)
			content = content[:idx] + replacement + content[lineEnd:]
		}
	} else {
		// 无 exec.start：插入到配置块末尾（最后一个 } 之前）
		insert := fmt.Sprintf("  exec.start = 'cd %s && /bin/sh /etc/rc';\n", esc)
		if end := strings.LastIndex(content, "}"); end >= 0 {
			content = content[:end] + insert + content[end:]
		} else {
			content += insert
		}
	}
	return os.WriteFile(confPath, []byte(content), 0o644)
}

// bastilleApplyLimits 应用资源限制（docs/container-support.md §3.3）：
// - memoryLimitMb → bastille limits JAIL add memoryuse <N>M
// - cpus → bastille limits JAIL cpu 0..N-1（cpuset，核数换算）
// - diskLimitMb → zfs set quota=<N>M（jail 数据集，按挂载点路径定位）
func bastilleApplyLimits(name string, memoryLimitMb, cpus, diskLimitMb int) error {
	if memoryLimitMb > 0 {
		if _, err := cliRun(cliTimeout, bastilleBin, "limits", name, "add",
			"memoryuse", fmt.Sprintf("%dM", memoryLimitMb)); err != nil {
			return err
		}
	}
	if cpus > 0 {
		// cpuset 为逗号分隔的 CPU 列表（官方：bastille limits TARGET cpu 2,3,4）
		cores := make([]string, 0, cpus)
		for i := 0; i < cpus; i++ {
			cores = append(cores, strconv.Itoa(i))
		}
		if _, err := cliRun(cliTimeout, bastilleBin, "limits", name, "cpu", strings.Join(cores, ",")); err != nil {
			return err
		}
	}
	if diskLimitMb > 0 {
		jailDir := filepath.Join(bastilleRoot, "jails", name)
		// zfs set 接受挂载点路径；非 ZFS 环境将报错（容错：仅记录失败）
		if _, err := cliRun(cliTimeout, zfsBin, "set", fmt.Sprintf("quota=%dM", diskLimitMb), jailDir); err != nil {
			return err
		}
	}
	return nil
}

// bastilleClone 克隆 jail 为新的名字与可选新 IP（bastille clone TARGET NEW_NAME [IP]）。
func bastilleClone(name, newName, newIP string) error {
	args := []string{"clone", name, newName}
	if newIP != "" {
		args = append(args, newIP)
	}
	_, err := cliRun(cliLongTimeout, bastilleBin, args...)
	return err
}

// bastilleExport 导出 jail 为 txz 归档到 bastille/backups/（默认输出目录）。
// 语法：bastille export --txz TARGET PATH；返回 {path}（绝对路径，可直接作 import 的 file）。
func bastilleExport(d *Daemon, name string) (map[string]any, error) {
	backupDir := filepath.Join(bastilleRoot, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return nil, err
	}
	outPath := filepath.Join(backupDir, name+"-"+strconv.FormatInt(time.Now().UnixMilli(), 10)+".txz")
	if _, err := cliRun(cliLongTimeout, bastilleBin, "export", "--txz", name, outPath); err != nil {
		return nil, err
	}
	return map[string]any{"path": outPath}, nil
}

// resolveImportFile 解析 import 的 file 参数：
// 绝对路径仅允许位于 bastille/backups/ 或同步区内；相对路径按同步区根解析。
func (d *Daemon) resolveImportFile(file string) (string, error) {
	if filepath.IsAbs(file) {
		if pathWithin(filepath.Join(bastilleRoot, "backups"), file) ||
			pathWithin(d.clusterRoot(), file) {
			return file, nil
		}
		return "", fmt.Errorf("导入文件路径越界: %s", file)
	}
	return d.clusterPath(file)
}

// bastilleImport 从归档导入 jail（bastille import [-f] FILE [RELEASE]）。
// RELEASE 为「导入到指定发行版」；force(-f) 跳过校验和；返回导入后的 jail 名。
func bastilleImport(d *Daemon, file, release string, force bool) (string, error) {
	filePath, err := d.resolveImportFile(file)
	if err != nil {
		return "", err
	}
	args := []string{"import"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, filePath)
	if release != "" {
		args = append(args, release)
	}
	out, err := cliRun(cliLongTimeout, bastilleBin, args...)
	if err != nil {
		return "", err
	}
	return importJailName(out), nil
}

// importJailName 从 bastille import 的输出中提取 jail 名；解析失败返回空串。
func importJailName(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		for i, f := range fields {
			if strings.EqualFold(f, "jail") && i+1 < len(fields) {
				if name := strings.Trim(fields[i+1], ".: "); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// bastilleSetupMode 初始化设置（docs/container-support.md §3.3）。
// 服务端统一附加 -y 避免交互阻塞；响应 {ok, detail?}。
//   - default:  bastille setup（自动 loopback+firewall+storage）
//   - firewall: bastille setup firewall [extIf]
//   - vnet:     bastille setup vnet [extIf tunIf addr]（部分版本交互式）
//   - bridge:   bastille setup bridge
//   - shared:   bastille setup shared [extIf]
//   - linux:    bastille setup linux（Linuxulator + debootstrap）
func bastilleSetupMode(mode, extIf, tunIf, addr string) (map[string]any, error) {
	args := []string{"setup", "-y"}
	switch mode {
	case "default":
	case "firewall":
		args = append(args, "firewall")
		if extIf != "" {
			args = append(args, extIf)
		}
	case "vnet":
		args = append(args, "vnet")
		if extIf != "" {
			args = append(args, extIf, tunIf, addr)
		}
	case "bridge":
		args = append(args, "bridge")
	case "shared":
		args = append(args, "shared")
		if extIf != "" {
			args = append(args, extIf)
		}
	case "linux":
		args = append(args, "linux")
	default:
		return nil, fmt.Errorf("mode 仅支持 default/firewall/vnet/bridge/shared/linux")
	}
	out, err := cliRun(cliLongTimeout, bastilleBin, args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "detail": strings.TrimSpace(out)}, nil
}

// bastilleRdrList 端口转发规则列表（bastille rdr [JAIL] list 输出解析）。
// jail 为空时合并全部 jail 的规则（每条带 Jail 标识）；解析失败的行静默跳过。
func bastilleRdrList(jail string) ([]rdrRule, error) {
	if jail != "" {
		return bastilleRdrListOne(jail)
	}
	return bastilleRdrRulesByJail()
}

// bastilleRdrListOne 读取单个 jail 的转发规则。
func bastilleRdrListOne(jail string) ([]rdrRule, error) {
	out, err := cliRun(cliTimeout, bastilleBin, "rdr", jail, "list")
	if err != nil {
		return nil, err
	}
	return parseRdrOutput(jail, out), nil
}

// bastilleRdrRulesByJail 返回全部 jail 的转发规则（按 jail 分组）。
func bastilleRdrRulesByJail() ([]rdrRule, error) {
	entries, err := os.ReadDir(filepath.Join(bastilleRoot, "jails"))
	if err != nil {
		return nil, fmt.Errorf("读取 jail 列表失败: %w", err)
	}
	var all []rdrRule
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out, err := cliRun(cliTimeout, bastilleBin, "rdr", e.Name(), "list")
		if err != nil {
			continue // 单个 jail 无规则时 list 可能报错，跳过
		}
		all = append(all, parseRdrOutput(e.Name(), out)...)
	}
	return all, nil
}

// parseRdrOutput 解析 bastille rdr list 输出行。
func parseRdrOutput(jail, out string) []rdrRule {
	rules := make([]rdrRule, 0, 8)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hostPort, jailPort := parseRdrLine(line)
		if hostPort <= 0 || jailPort <= 0 {
			continue
		}
		proto := ""
		if fields := strings.Fields(line); len(fields) > 0 {
			for i, f := range fields {
				if f == "proto" && i+1 < len(fields) {
					proto = fields[i+1]
					break
				}
			}
		}
		rules = append(rules, rdrRule{Jail: jail, Proto: proto, HostPort: hostPort, JailPort: jailPort})
	}
	return rules
}

// bastilleRdrAdd 添加端口转发（bastille rdr JAIL tcp|udp HOST_PORT JAIL_PORT）。
// 失败时附加 PF 初始化提示：PF 未配置是 rdr 失败的最常见原因。
func bastilleRdrAdd(jail, proto string, hostPort, jailPort int) error {
	_, err := cliRun(cliTimeout, bastilleBin, "rdr", jail, proto,
		strconv.Itoa(hostPort), strconv.Itoa(jailPort))
	if err != nil {
		return fmt.Errorf("%w（若 PF 防火墙未初始化，请先调用 POST /api/bastille/setup {\"mode\":\"firewall\"}）", err)
	}
	return nil
}

// bastilleRdrDelete 删除端口转发：CLI 无单条删除命令，
// 服务端读取 rdr list → clear → 重放其余规则。
func bastilleRdrDelete(jail, proto string, hostPort, jailPort int) error {
	rules, err := bastilleRdrList(jail)
	if err != nil {
		return err
	}
	if _, err := cliRun(cliTimeout, bastilleBin, "rdr", jail, "clear"); err != nil {
		return err
	}
	for _, r := range rules {
		if r.Proto == proto && r.HostPort == hostPort && r.JailPort == jailPort {
			continue
		}
		if err := bastilleRdrAdd(jail, r.Proto, r.HostPort, r.JailPort); err != nil {
			return err
		}
	}
	return nil
}

// bastilleTemplates 模板列表（客户端契约：[{namespace, name}]，namespace 为 project 目录名）。
func bastilleTemplates() ([]map[string]any, error) {
	root := filepath.Join(bastilleRoot, "templates")
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取模板目录失败: %w", err)
	}
	items := make([]map[string]any, 0, 8)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		tmpls, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			continue
		}
		for _, t := range tmpls {
			if t.IsDir() {
				items = append(items, map[string]any{
					"namespace": p.Name(),
					"name":      t.Name(),
				})
			}
		}
	}
	return items, nil
}

// bastilleApply 应用模板 → 后台任务 {jobId}。
// args 的 KEY=VALUE 作为环境变量传给 bastille 进程，供模板脚本引用。
func bastilleApply(jail, template string, args map[string]string) (string, error) {
	jobID := jobs.create()
	if jobID == "" {
		return "", errors.New("任务已满，请稍后重试")
	}
	job := jobs.get(jobID)
	if job == nil {
		return "", errors.New("任务创建失败")
	}
	go func() {
		cmd := commandWithEnv(args, bastilleBin, "template", jail, template)
		col := &logCollector{job: job}
		cmd.Stdout = col
		cmd.Stderr = col
		err := cmd.Run()
		job.mu.Lock()
		defer job.mu.Unlock()
		if err != nil {
			job.status = "failed"
			job.log = append(job.log, fmt.Sprintf("执行失败: %v", err))
		} else {
			job.status = "done"
		}
	}()
	return jobID, nil
}

// commandWithEnv 构造带额外环境变量的命令（模板 args 以 KEY=VALUE 注入）。
func commandWithEnv(env map[string]string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}
