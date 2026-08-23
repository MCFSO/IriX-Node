//go:build freebsd

// Bastille 容器能力实现（FreeBSD 节点）：包装系统 bastille CLI（docs/container-support.md §3.3 契约）。
// Bastille 的数据目录固定为 /usr/local/bastille（releases/jails/templates）。
// 命令输出解析均容错：字段缺失 / 格式变化返回默认值，不中断列表。

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// bastilleJailRelease 推断 jail 的发行版：
// 优先解析 jail.conf 的 osrelease 行（bastille 1.4+ 均有）；
// 回退旧版 thin jail 的 root 符号链接（指向 releases）。
func bastilleJailRelease(name string) string {
	if data, err := os.ReadFile(filepath.Join(bastilleRoot, "jails", name, "jail.conf")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "osrelease") {
				continue
			}
			if idx := strings.IndexByte(line, '"'); idx >= 0 {
				rest := line[idx+1:]
				if end := strings.IndexByte(rest, '"'); end > 0 {
					return rest[:end]
				}
			}
		}
	}
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
// macFlag/macAddr：静态 MAC（bastille create -M，仅 VNET）；macAddr 非空时创建后
// 改写 jail.conf 的 vnet.interface 为 { name "..."; mac "..."; }。
// 校验：linux 与任何 VNET 模式互斥；VNET 时 IP 必须含子网掩码、interface 必须非空。
// 创建成功后依次应用：volumes（bastille mount 写 fstab, nullfs）、workdir（exec.start
// 前置 cd）、limits（bastille limits / ZFS 配额）；配置步骤失败记入 warnings 不影响创建结果。
func bastilleCreate(name, release, ip, jtype, vnetMode, iface string, macFlag bool, macAddr string,
	volumes []bastilleVolume, workdir string, memoryLimitMb, cpus, diskLimitMb int) (map[string]any, error) {
	if jtype == "" {
		jtype = "thin"
	}
	// 客户端可能把显示标签 "name:version" 当作 release 传入，剥离冒号后缀
	release = normalizeRelease(release)
	// bastille 拒绝纯数字 jail 名（客户端校验允许纯数字，服务端拦截并给中文提示）
	if isAllDigits(name) {
		return nil, fmt.Errorf("jail 名不能只包含数字（bastille 限制），请至少包含一个字母（如 mc%s）", name)
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
	if macFlag && vnetMode == "none" {
		return nil, fmt.Errorf("mac 仅适用于 VNET 模式（bastille create -M/--static-mac）")
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
	if macFlag {
		args = append(args, "-M")
	}
	// 创建前诊断：给出可读的中文原因，而不是透出 bastille 的原始 stderr
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("创建 Jail 需要 root 权限（bastille 要求），请以 root 身份运行 irix-node")
	}
	if jtype != "empty" {
		if st, err := os.Stat(filepath.Join(bastilleRoot, "releases", release)); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("发行版 %s 尚未 bootstrap 完成（未找到其目录），请先调用 POST /api/bastille/bootstrap 并等待任务完成", release)
		}
		if st, err := os.Stat(filepath.Join(bastilleRoot, "jails", name)); err == nil && st.IsDir() {
			return nil, fmt.Errorf("jail %s 已存在，请换一个名称或先销毁", name)
		}
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
	if macAddr != "" {
		if err := bastilleSetMac(name, macAddr); err != nil {
			warnings = append(warnings, fmt.Sprintf("设置静态 MAC 失败: %v", err))
		}
	}
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

// bastilleSetMac 把 jail.conf 的 vnet.interface 改写为带静态 MAC 的形式：
//
//	vnet.interface = { name "<iface>"; mac "<mac>"; };
func bastilleSetMac(name, mac string) error {
	confPath := filepath.Join(bastilleRoot, "jails", name, "jail.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	content := string(data)
	idx := strings.Index(content, "vnet.interface")
	if idx < 0 {
		return fmt.Errorf("jail.conf 中未找到 vnet.interface 条目")
	}
	// 提取行内第一个双引号中的接口名
	rest := content[idx:]
	q := strings.IndexByte(rest, '"')
	if q < 0 {
		return fmt.Errorf("jail.conf 的 vnet.interface 行格式无法解析（无引号接口名）")
	}
	end := strings.IndexByte(rest[q+1:], '"')
	if end < 0 {
		return fmt.Errorf("jail.conf 的 vnet.interface 行格式无法解析（引号未闭合）")
	}
	iface := rest[q+1 : q+1+end]
	semi := strings.IndexByte(rest, ';')
	if semi < 0 {
		return fmt.Errorf("jail.conf 的 vnet.interface 行格式无法解析（无行尾分号）")
	}
	replacement := fmt.Sprintf(`vnet.interface = { name "%s"; mac "%s"; };`, iface, mac)
	content = content[:idx] + replacement + content[idx+semi+1:]
	return os.WriteFile(confPath, []byte(content), 0o644)
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

// bastillePkg jail 内软件包管理（docs/irix-node-container-api.md §4.9）。
// 语法：bastille pkg <name> <action> [-y] [pkgs...]；统一附加 -y 避免交互阻塞。
// 返回命令输出文本（pkg 安装耗时较长，客户端超时已放大到 10 分钟）。
func bastillePkg(name, action string, packages []string) (string, error) {
	args := []string{"pkg", name, action, "-y"}
	args = append(args, packages...)
	return cliRun(cliLongTimeout, bastilleBin, args...)
}

// configKeyPattern 配置键格式（仅允许字母数字与 ._-，防 CLI 参数注入）。
var configKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// bastilleConfigGet 解析 jail.conf 为扁平键值对象（docs/irix-node-container-api.md §4.12）。
// 仅解析 "key = value;" 形式的参数行；注释 / 块结构 / 无法解析的行跳过。
func bastilleConfigGet(name string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(bastilleRoot, "jails", name, "jail.conf"))
	if err != nil {
		return nil, fmt.Errorf("读取 jail.conf 失败: %w", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "}") {
			continue
		}
		idx := strings.IndexByte(t, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(t[:idx])
		if key == "" {
			continue
		}
		val := strings.TrimSpace(t[idx+1:])
		val = strings.TrimSuffix(val, ";")
		val = strings.TrimSpace(val)
		// 剥离成对的外层引号（单引号或双引号）
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, nil
}

// bastilleConfigSet 设置配置项：bastille config <name> <key> <value>。
func bastilleConfigSet(name, key, value string) error {
	if !configKeyPattern.MatchString(key) {
		return fmt.Errorf("配置键格式无效（仅允许字母/数字/._-）: %s", key)
	}
	_, err := cliRun(cliTimeout, bastilleBin, "config", name, key, value)
	return err
}

// bastilleConfigUnset 从 jail.conf 移除配置项（直接改写文件，幂等）。
// 形如 "key = ...;" 的参数行整行删除；不存在的 key 返回 nil。
func bastilleConfigUnset(name, key string) error {
	confPath := filepath.Join(bastilleRoot, "jails", name, "jail.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("读取 jail.conf 失败: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	var kept []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, key) && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(t, key)), "=") {
			continue // 命中 "key = ..." 整行删除
		}
		kept = append(kept, line)
	}
	return os.WriteFile(confPath, []byte(strings.Join(kept, "\n")), 0o644)
}

// bastilleMount 挂载宿主机目录到 jail（nullfs；写入 fstab，运行中则即时挂载）。
// 语法：bastille mount JAIL HOST JAILPATH nullfs rw 0 0
// 实测：bastille 1.4.4 校验完整 fstab 格式（OPTIONS 必须含 rw/ro，DUMP/PASS 必填），
// 只传 nullfs 会报 "FSTAB format not recognized"。
func bastilleMount(name, source, dest string) error {
	_, err := cliRun(cliTimeout, bastilleBin, "mount", name, source, dest, "nullfs", "rw", "0", "0")
	return err
}

// bastilleUmount 解除 jail 挂载（按目标路径）。
func bastilleUmount(name, dest string) error {
	_, err := cliRun(cliTimeout, bastilleBin, "umount", name, dest)
	return err
}

// --- 挂载管理（docs/irix-node-container-api.md §4.10） ---

// mountBin mount 命令绝对路径（FreeBSD 服务方式启动时 PATH 常缺 sbin）。
const mountBin = "/sbin/mount"

// bastilleFstabPath 返回 jail 的 fstab 路径。
func bastilleFstabPath(name string) string {
	return filepath.Join(bastilleRoot, "jails", name, "fstab")
}

// bastilleJailRoot 返回 jail 根目录的宿主路径（thin jail 为指向 releases 的符号链接）。
func bastilleJailRoot(name string) string {
	return filepath.Join(bastilleRoot, "jails", name, "root")
}

// bastilleJailHostPath 把 jail 内路径解析为宿主机绝对路径（NODE_API.md §6.1 文件管理）。
// jail 名必须对应 jails/ 下的真实目录；thin jail 的 root 符号链接解析与
// .. / 符号链接越界校验由 resolveJailHostPath 处理。
func bastilleJailHostPath(name, jailPath string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: jail 名无效: %s", errJailPath, name)
	}
	jailDir := filepath.Join(bastilleRoot, "jails", name)
	if st, err := os.Stat(jailDir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: jail %s 不存在", errJailPath, name)
	}
	return resolveJailHostPath(filepath.Join(jailDir, "root"), jailPath)
}

// fstabEntry 单条 fstab 挂载（device mountpoint fstype options dump pass）。
type fstabEntry struct {
	Device  string
	Mount   string
	Fstype  string
	Options string
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

// bastilleMountList 挂载列表（fstab 条目 + 当前实际挂载，permanent 区分来源）。
// 条目: {src?, dst, fstype, options?, permanent}；procfs/devfs 的 src 为 null。
func bastilleMountList(name string) ([]map[string]any, error) {
	entries, err := readFstab(name)
	if err != nil {
		return nil, fmt.Errorf("读取 fstab 失败: %w", err)
	}
	items := make([]map[string]any, 0, len(entries)+2)
	seen := map[string]bool{} // "fstype|dst" 去重
	for _, e := range entries {
		key := e.Fstype + "|" + e.Mount
		seen[key] = true
		item := map[string]any{
			"dst":       e.Mount,
			"fstype":    e.Fstype,
			"options":   e.Options,
			"permanent": true,
		}
		if e.Device != "proc" && e.Device != "devfs" {
			item["src"] = e.Device
		}
		items = append(items, item)
	}
	// 合并当前实际挂载（未写入 fstab 的即时挂载，permanent=false）
	jailRoot := bastilleJailRoot(name)
	out, err := cliRun(cliTimeout, mountBin)
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			dev, mnt, fstype, opts, ok := parseMountLine(line)
			if !ok || !strings.HasPrefix(mnt, jailRoot) {
				continue
			}
			dst := strings.TrimPrefix(mnt, jailRoot)
			if dst == "" || !strings.HasPrefix(dst, "/") {
				continue
			}
			key := fstype + "|" + dst
			if seen[key] {
				continue
			}
			seen[key] = true
			item := map[string]any{
				"dst":       dst,
				"fstype":    fstype,
				"options":   opts,
				"permanent": false,
			}
			if dev != "proc" && dev != "devfs" {
				item["src"] = dev
			}
			items = append(items, item)
		}
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

// bastilleMountAdd 添加挂载（docs/irix-node-container-api.md §4.10）：
//   - nullfs（默认）：bastille mount <name> <src> <dst>（写 fstab + 即时挂载）
//   - procfs/devfs：追加 fstab 条目并立即挂载到宿主 <jailroot>/<dst>；
//     挂载失败时回滚 fstab 条目。
func bastilleMountAdd(name, src, dst, fstype, options string) error {
	if fstype == "" {
		fstype = "nullfs"
	}
	if options == "" {
		options = "rw"
	}
	if !strings.HasPrefix(dst, "/") {
		return fmt.Errorf("dst 必须为 jail 内绝对路径（以 / 开头）: %s", dst)
	}
	switch fstype {
	case "nullfs":
		if src == "" {
			return fmt.Errorf("nullfs 挂载必须提供 src 参数（宿主机源路径）")
		}
		_, err := cliRun(cliTimeout, bastilleBin, "mount", name, src, dst, "nullfs", options, "0", "0")
		return err
	case "procfs", "devfs":
		// 防重复：fstab 中已有同 fstype+dst 条目则报错
		data, err := os.ReadFile(bastilleFstabPath(name))
		if err != nil {
			return fmt.Errorf("读取 fstab 失败: %w", err)
		}
		lines := strings.Split(string(data), "\n")
		for _, ln := range lines {
			fields := strings.Fields(strings.TrimSpace(ln))
			if len(fields) >= 3 && fields[2] == fstype && fields[1] == dst {
				return fmt.Errorf("挂载点已存在（fstab 已有 %s %s），请先删除", fstype, dst)
			}
		}
		// 追加 fstab 条目（device 用 fstype 名，与 FreeBSD 惯例一致）
		line := fmt.Sprintf("%s %s %s %s 0 0", fstype, dst, fstype, options)
		lines = append(lines, line)
		if err := writeFstab(name, lines); err != nil {
			return fmt.Errorf("写入 fstab 失败: %w", err)
		}
		// 立即挂载：宿主路径为 jailroot + dst（thin jail 为符号链接，可穿透）
		hostPath := filepath.Join(bastilleJailRoot(name), strings.TrimPrefix(dst, "/"))
		if err := os.MkdirAll(hostPath, 0o755); err != nil {
			_ = bastilleFstabRemove(name, dst, fstype)
			return fmt.Errorf("创建挂载点目录失败: %w", err)
		}
		if _, err := cliRun(cliTimeout, mountBin, "-t", fstype, fstype, hostPath); err != nil {
			_ = bastilleFstabRemove(name, dst, fstype)
			return err
		}
		return nil
	default:
		return fmt.Errorf("fstype 仅支持 nullfs/procfs/devfs（收到 %s）", fstype)
	}
}

// mustReadFstab 读取 fstab 原文（bastilleMountAdd 内部使用，失败返回空串）。
func mustReadFstab(name string) []byte {
	data, err := os.ReadFile(bastilleFstabPath(name))
	if err != nil {
		return nil
	}
	return data
}

// bastilleFstabRemove 从 fstab 移除匹配 mount 的条目（可选限定 fstype）；返回是否移除。
func bastilleFstabRemove(name, mount, fstype string) bool {
	lines := strings.Split(string(mustReadFstab(name)), "\n")
	var kept []string
	removed := false
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 3 && fields[1] == mount && (fstype == "" || fields[2] == fstype) {
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

// bastilleMountRemove 卸载并移除 fstab 条目（docs/irix-node-container-api.md §4.10）。
// fstab 中找不到条目时仅卸载，不报错。
func bastilleMountRemove(name, dst string) error {
	removed := bastilleFstabRemove(name, dst, "")
	err := bastilleUmount(name, dst)
	if err != nil && !removed {
		return err
	}
	return nil
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

// bastilleClone 克隆 jail 为新的名字与可选新 IP。
// 语法：bastille clone TARGET NEW_NAME [IP]；附加 -a（auto 模式）：
// 源 jail 运行中时自动停止/恢复，否则会报 "Jail is running"。
func bastilleClone(name, newName, newIP string) error {
	args := []string{"clone", "-a", name, newName}
	if newIP != "" {
		args = append(args, newIP)
	}
	_, err := cliRun(cliLongTimeout, bastilleBin, args...)
	return err
}

// bastilleExport 导出 jail 为 txz 归档到 bastille/backups/。
// 语法：bastille export --txz TARGET PATH；实测 PATH 为「输出目录」（须存在），
// 归档文件名由 bastille 生成（<name>_<时间戳>.txz + .sha256）；
// 运行中的 jail 需 -a（auto 模式，自动停/启）。
// 返回 {path}：归档文件绝对路径，可直接作 import 的 file。
func bastilleExport(d *Daemon, name string) (map[string]any, error) {
	backupDir := filepath.Join(bastilleRoot, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := cliRun(cliLongTimeout, bastilleBin, "export", "-a", "--txz", name, backupDir); err != nil {
		return nil, err
	}
	// 归档文件名由 bastille 生成：取备份目录内最新 .txz
	latest, latestMod := "", time.Time{}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".txz") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latest = e.Name()
		}
	}
	if latest == "" {
		return nil, fmt.Errorf("导出失败：备份目录中未找到归档文件")
	}
	return map[string]any{"path": filepath.Join(backupDir, latest)}, nil
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

// bastilleJailNames 返回全部 jail 名集合。
func bastilleJailNames() map[string]bool {
	set := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(bastilleRoot, "jails"))
	if err != nil {
		return set
	}
	for _, e := range entries {
		if e.IsDir() {
			set[e.Name()] = true
		}
	}
	return set
}

// bastilleImport 从归档导入 jail（bastille import [-f] FILE [RELEASE]）。
// RELEASE 为「导入到指定发行版」；force(-f) 跳过校验和。
// 返回导入后的 jail 名：对比导入前后 jails 目录差异（bastille import
// 的输出无稳定格式，目录差异最可靠），兜底解析输出。
func bastilleImport(d *Daemon, file, release string, force bool) (string, error) {
	filePath, err := d.resolveImportFile(file)
	if err != nil {
		return "", err
	}
	before := bastilleJailNames()
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
	for name := range bastilleJailNames() {
		if !before[name] {
			return name, nil
		}
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

// --- 运行会话（docs/irix-node-container-api.md §4.11） ---
//
// 在 jail 内后台运行长任务（MC 服务端等）：进程经 `bastille cmd <name> sh -c ...`
// 启动（jexec 语义），stdout/stderr 写入内存环形缓冲（字节偏移游标，支持 since
// 增量读取）并镜像落盘 <bastilleRoot>/run/<name>/<session>.log（重启后客户端
// 仍可按会话 id 读到历史日志）。进程退出后会话保留 sessionRetain 时长供客户端
// 读取最终状态，之后随下次操作惰性清理。watch=true 时进程退出自动停止 jail。

const (
	// sessionLogMaxBytes 会话内存环形缓冲上限。
	sessionLogMaxBytes = 1 << 20
	// sessionDiskMaxBytes 会话磁盘日志上限（超出后保留尾部 sessionDiskTrimBytes）。
	sessionDiskMaxBytes = 20 << 20
	// sessionDiskTrimBytes 磁盘日志超限时保留的尾部字节数。
	sessionDiskTrimBytes = 5 << 20
	// sessionRetain 会话结束后保留时长（供客户端读取最终状态）。
	sessionRetain = 30 * time.Minute
	// maxBastilleSessions 同时运行中的会话上限。
	maxBastilleSessions = 32
	// sessionStopTimeout 停止会话时 SIGTERM 后的等待时长，超时 SIGKILL。
	sessionStopTimeout = 10 * time.Second
)

// sessionLog 会话日志环形缓冲（字节偏移语义：offset 单调递增，since 增量读取）。
type sessionLog struct {
	mu      sync.Mutex
	buf     []byte // 线性缓冲，写满后从头部丢弃
	max     int
	dropped int64 // 已丢弃字节数（offset 基数）
}

func newSessionLog() *sessionLog {
	return &sessionLog{max: sessionLogMaxBytes}
}

// write 追加内容，超出容量时丢弃最旧字节。
func (l *sessionLog) write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(p) >= l.max {
		l.buf = append(l.buf[:0], p[len(p)-l.max:]...)
		l.dropped += int64(len(p) - l.max)
		return
	}
	if len(l.buf)+len(p) > l.max {
		drop := len(l.buf) + len(p) - l.max
		l.buf = l.buf[drop:]
		l.dropped += int64(drop)
	}
	l.buf = append(l.buf, p...)
}

// snapshot 返回自 since 偏移之后的新增内容与当前末尾偏移。
// since <= 0 时返回最后 tail 行（tail <= 0 表示全量）；since 早于缓冲起点
// （数据已被丢弃）时返回缓冲内全部内容（客户端可能看到少量重复，可接受）。
func (l *sessionLog) snapshot(since int64, tail int) (string, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := l.dropped + int64(len(l.buf))
	if since <= 0 {
		s := string(l.buf)
		if tail > 0 {
			s = lastNLines(s, tail)
		}
		return s, total
	}
	if since < l.dropped {
		return string(l.buf), total
	}
	start := int(since - l.dropped)
	if start > len(l.buf) {
		start = len(l.buf)
	}
	return string(l.buf[start:]), total
}

// lastNLines 取字符串最后 N 行（按 \n 拆分，保留行内容不含换行符）。
func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// sessionSink 会话输出汇：内存环形缓冲 + 磁盘日志镜像（同锁保证字节顺序一致）。
type sessionSink struct {
	mu   sync.Mutex
	ring *sessionLog
	file *os.File
	size int64
}

// Write 实现 io.Writer：写入环形缓冲并镜像落盘（磁盘超限时截断保留尾部）。
func (s *sessionSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring.write(p)
	if s.file != nil {
		if s.size+int64(len(p)) > sessionDiskMaxBytes {
			s.trimLocked()
		}
		n, err := s.file.Write(p)
		s.size += int64(n)
		return n, err
	}
	return len(p), nil
}

// trimLocked 磁盘日志超限：读取尾部 sessionDiskTrimBytes 并重建文件。
func (s *sessionSink) trimLocked() {
	if s.file == nil {
		return
	}
	fi, err := s.file.Stat()
	if err != nil || fi.Size() <= sessionDiskTrimBytes {
		return
	}
	tail := make([]byte, sessionDiskTrimBytes)
	if _, err := s.file.Seek(-sessionDiskTrimBytes, io.SeekEnd); err != nil {
		return
	}
	if _, err := io.ReadFull(s.file, tail); err != nil {
		return
	}
	if err := s.file.Truncate(0); err != nil {
		return
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return
	}
	_, _ = s.file.Write(tail)
	s.size = int64(len(tail))
}

// Close 关闭磁盘日志句柄（删除会话时调用）。
func (s *sessionSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// bastilleSession 单个运行会话。
type bastilleSession struct {
	mu       sync.Mutex
	id       string
	jail     string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	sink     *sessionSink
	watch    bool
	running  bool
	exitCode int
	done     chan struct{}
	endedAt  time.Time
	logPath  string
}

// isRunning 会话进程是否仍在运行。
func (s *bastilleSession) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// sessionStore 会话注册表（全局唯一 id：s-<递增计数>）。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*bastilleSession
	counter  int
}

// bastilleSessions 全局长任务会话注册表。
var bastilleSessions = &sessionStore{sessions: map[string]*bastilleSession{}}

// create 分配会话 id 并清理过期会话；运行中会话数达上限返回空串。
func (s *sessionStore) create() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	running := 0
	for _, sess := range s.sessions {
		if sess.isRunning() {
			running++
		}
	}
	if running >= maxBastilleSessions {
		return ""
	}
	s.counter++
	return fmt.Sprintf("s-%d", s.counter)
}

// register 登记会话。
func (s *sessionStore) register(sess *bastilleSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.id] = sess
}

// get 按 id 获取会话（不存在返回 nil）。
func (s *sessionStore) get(id string) *bastilleSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// remove 移除会话并关闭其磁盘日志句柄。
func (s *sessionStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		delete(s.sessions, id)
		_ = sess.sink.Close()
	}
}

// sweepLocked 清理已结束且超过保留时长的会话（调用方须持有 mu）。
func (s *sessionStore) sweepLocked() {
	cutoff := time.Now().Add(-sessionRetain)
	for id, sess := range s.sessions {
		sess.mu.Lock()
		expired := !sess.running && !sess.endedAt.IsZero() && sess.endedAt.Before(cutoff)
		sess.mu.Unlock()
		if expired {
			delete(s.sessions, id)
			_ = sess.sink.Close()
		}
	}
}

// bastilleRunStart 在 jail 内后台启动运行会话。
// command 以 shell 语义执行（sh -c 包装）；cwd 非空时前置 cd；
// watch=true 时进程退出自动执行 bastille stop <name>。
// 返回会话 id；失败返回错误（含中文原因）。
func bastilleRunStart(name, command, cwd string, watch bool) (string, error) {
	if command == "" {
		return "", errors.New("缺少 command 参数")
	}
	if !bastilleJailNames()[name] {
		return "", fmt.Errorf("jail %s 不存在", name)
	}
	if !bastilleRunningSet()[name] {
		return "", fmt.Errorf("jail %s 未运行，无法启动会话（请先调用 start）", name)
	}
	id := bastilleSessions.create()
	if id == "" {
		return "", fmt.Errorf("运行中会话已达上限（%d），请先清理已结束的会话", maxBastilleSessions)
	}
	fullCmd := command
	if cwd != "" {
		fullCmd = fmt.Sprintf("cd %s && %s", cwd, command)
	}
	cmd := exec.Command(bastilleBin, "cmd", name, "sh", "-c", fullCmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	// 磁盘日志：<bastilleRoot>/run/<jail>/<session>.log（重启恢复用）
	logDir := filepath.Join(bastilleRoot, "run", name)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("创建会话日志目录失败: %w", err)
	}
	logPath := filepath.Join(logDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("创建会话日志失败: %w", err)
	}
	sink := &sessionSink{ring: newSessionLog(), file: logFile}
	sess := &bastilleSession{
		id: id, jail: name, cmd: cmd, stdin: stdin, sink: sink, watch: watch,
		running: true, exitCode: -1, done: make(chan struct{}), logPath: logPath,
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("启动会话进程失败: %w", err)
	}
	bastilleSessions.register(sess)
	// 输出复制：stdout/stderr 都写入同一 sink（内部加锁，互不丢失）
	copyStream := func(rd io.Reader) {
		_, _ = io.Copy(sink, rd)
	}
	go copyStream(stdout)
	go copyStream(stderr)
	// 结束收尾：记录退出码；watch 看门狗停止 jail
	go func() {
		waitErr := cmd.Wait()
		sess.mu.Lock()
		sess.running = false
		if waitErr != nil && cmd.ProcessState != nil {
			sess.exitCode = cmd.ProcessState.ExitCode()
		} else {
			sess.exitCode = 0
		}
		sess.endedAt = time.Now()
		sess.mu.Unlock()
		close(sess.done)
		if watch {
			if err := bastilleAction(name, "stop", false); err != nil {
				_, _ = sink.Write([]byte(fmt.Sprintf("\n[irix-node] 看门狗：进程退出后停止 jail 失败: %v\n", err)))
			} else {
				_, _ = sink.Write([]byte("\n[irix-node] 看门狗：进程已退出，jail 已停止\n"))
			}
		}
	}()
	return id, nil
}

// bastilleRunStatus 查询会话状态与增量日志。
// 返回 {running, exitCode?, offset, log}；会话不在内存（节点重启）时回退读取
// 磁盘日志（running=false，exitCode 省略）。
func bastilleRunStatus(name, session string, tail int, since int64) (map[string]any, error) {
	sess := bastilleSessions.get(session)
	if sess == nil {
		// 重启恢复：磁盘日志兜底
		logPath := filepath.Join(bastilleRoot, "run", name, session+".log")
		data, err := os.ReadFile(logPath)
		if err != nil {
			return nil, fmt.Errorf("会话 %s 不存在（id 无效或已清理）", session)
		}
		log := string(data)
		if tail > 0 {
			log = lastNLines(log, tail)
		}
		return map[string]any{"running": false, "offset": len(data), "log": log}, nil
	}
	if sess.jail != name {
		return nil, fmt.Errorf("会话 %s 不属于 jail %s", session, name)
	}
	sess.mu.Lock()
	running := sess.running
	exitCode := sess.exitCode
	sess.mu.Unlock()
	log, offset := sess.sink.ring.snapshot(since, tail)
	data := map[string]any{"running": running, "offset": offset, "log": log}
	if !running {
		data["exitCode"] = exitCode
	}
	return data, nil
}

// bastilleRunStdin 向会话进程 stdin 写入输入（客户端自带换行，原样透传）。
func bastilleRunStdin(name, session, input string) error {
	sess := bastilleSessions.get(session)
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在（id 无效或已清理）", session)
	}
	if sess.jail != name {
		return fmt.Errorf("会话 %s 不属于 jail %s", session, name)
	}
	if !sess.isRunning() {
		return fmt.Errorf("会话 %s 已结束，无法发送命令", session)
	}
	if _, err := sess.stdin.Write([]byte(input)); err != nil {
		return fmt.Errorf("写入 stdin 失败: %w", err)
	}
	return nil
}

// bastilleRunStop 终止会话进程（SIGTERM → sessionStopTimeout 超时 SIGKILL）。
// 会话已结束时幂等返回 nil。
func bastilleRunStop(name, session string) error {
	sess := bastilleSessions.get(session)
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在（id 无效或已清理）", session)
	}
	if sess.jail != name {
		return fmt.Errorf("会话 %s 不属于 jail %s", session, name)
	}
	if !sess.isRunning() {
		return nil
	}
	if err := sess.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("发送 SIGTERM 失败: %w", err)
	}
	select {
	case <-sess.done:
		return nil
	case <-time.After(sessionStopTimeout):
		if err := sess.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("SIGTERM 超时后强杀失败: %w", err)
		}
	}
	return nil
}

// bastilleRunDelete 清理会话：终止进程（运行中则 SIGKILL）+ 删除磁盘日志。
func bastilleRunDelete(name, session string) error {
	sess := bastilleSessions.get(session)
	if sess != nil {
		if sess.jail != name {
			return fmt.Errorf("会话 %s 不属于 jail %s", session, name)
		}
		if sess.isRunning() {
			_ = sess.cmd.Process.Kill()
		}
		bastilleSessions.remove(session)
	}
	logPath := filepath.Join(bastilleRoot, "run", name, session+".log")
	_ = os.Remove(logPath)
	return nil
}
