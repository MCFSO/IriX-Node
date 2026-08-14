//go:build freebsd

// Bastille 容器能力实现（FreeBSD 节点）：包装系统 bastille CLI（docs/container-support.md §3.3 契约）。
// Bastille 的数据目录固定为 /usr/local/bastille（releases/jails/templates）。
// 命令输出解析均容错：字段缺失 / 格式变化返回默认值，不中断列表。

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// bastilleRoot Bastille 数据目录。
const bastilleRoot = "/usr/local/bastille"

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
	if out, err := cliRun(cliTimeout, "bastille", "version"); err == nil {
		return strings.TrimSpace(out), true
	}
	return "", true
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
			"version":   strings.TrimSuffix(name, "-RELEASE"),
			"sizeBytes": 0, // 目录大小无意义，由客户端按需估算
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
	go runLongJob(jobID, "bastille", "bootstrap", release)
	return jobID, nil
}

// bastilleJails jail 列表。
// 名称与 release 从 /usr/local/bastille/jails 目录读取（thin jail 的 root
// 为指向 releases 的符号链接，可据此推断发行版）；运行状态由 jls 判定。
func bastilleJails() ([]map[string]any, error) {
	entries, err := os.ReadDir(filepath.Join(bastilleRoot, "jails"))
	if err != nil {
		return nil, fmt.Errorf("读取 jail 列表失败: %w", err)
	}
	running := bastilleRunningSet()
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		status := "stopped"
		if running[name] {
			status = "running"
		}
		items = append(items, map[string]any{
			"name":      name,
			"release":   bastilleJailRelease(name),
			"status":    status,
			"state":     status,
			"ports":     []string{}, // 端口转发列表见 GET /api/bastille/rdr
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
	out, err := cliRun(cliTimeout, "jls")
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

// bastilleCreate 创建 jail（含类型/VNET/桥接/IP 与创建后配置）。
// type: thin(-T) / clone(-C) / empty(-E) / linux(-L)；vnet(-V) / bridge(-B)。
// 创建成功后依次应用：volumes（bastille mount 写 fstab）、workdir（exec.start
// 前置 cd）、limits（rctl / ZFS 配额）；配置步骤失败记入 warnings 不影响创建结果。
func bastilleCreate(name, release, ip, jtype string, vnet, bridge bool, mac string,
	volumes []bastilleVolume, workdir string, memoryLimitMb, cpus, diskLimitMb int) (map[string]any, error) {
	args := []string{"create"}
	switch jtype {
	case "thin":
		args = append(args, "-T")
	case "clone":
		args = append(args, "-C")
	case "empty":
		args = append(args, "-E")
	case "linux":
		args = append(args, "-L")
	}
	if vnet || bridge {
		args = append(args, "-V")
	}
	if bridge {
		args = append(args, "-B")
	}
	if mac != "" {
		args = append(args, "-M", mac)
	}
	if ip == "" {
		ip = "0.0.0.0"
	}
	args = append(args, name, release, ip)
	if _, err := cliRun(cliLongTimeout, "bastille", args...); err != nil {
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
	if memoryLimitMb > 0 {
		if err := bastilleApplyLimits(name, memoryLimitMb, cpus, diskLimitMb); err != nil {
			warnings = append(warnings, fmt.Sprintf("设置资源限制失败: %v", err))
		}
	} else if cpus > 0 || diskLimitMb > 0 {
		if err := bastilleApplyLimits(name, memoryLimitMb, cpus, diskLimitMb); err != nil {
			warnings = append(warnings, fmt.Sprintf("设置资源限制失败: %v", err))
		}
	}
	return map[string]any{"name": name, "warnings": warnings}, nil
}

// bastilleAction jail 操作（start/stop/restart/destroy）。
// force 为 true 时 destroy 附加 -a（bastille 摧毁运行中的 jail 必须带 -a）。
func bastilleAction(name, action string, force bool) error {
	args := []string{action}
	if action == "destroy" && force {
		args = append(args, "-a")
	}
	args = append(args, name)
	_, err := cliRun(cliTimeout, "bastille", args...)
	return err
}

// bastilleLogs jail 控制台日志尾部（bastille logs 输出全部，Go 侧截取尾部 N 行）。
func bastilleLogs(name string, tail int) (string, error) {
	out, err := cliRun(cliTimeout, "bastille", "logs", name)
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
	return cliRun(cliTimeout, "bastille", "cmd", name, "sh", "-c", command)
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

// bastilleMount 挂载宿主机目录到 jail（bastille mount 写入 fstab，运行中则即时挂载）。
func bastilleMount(name, source, dest string) error {
	_, err := cliRun(cliTimeout, "bastille", "mount", name, source, dest)
	return err
}

// bastilleUmount 解除 jail 挂载（按目标路径）。
func bastilleUmount(name, dest string) error {
	_, err := cliRun(cliTimeout, "bastille", "umount", name, dest)
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

// bastilleApplyLimits 应用资源限制：
// - memoryLimitMb → rctl memoryuse（FreeBSD 系统命令，规则直传）
// - cpus → rctl cpuset（分配 0..cpus-1 号核）
// - diskLimitMb → ZFS 配额（jail 数据集按挂载点路径定位）
func bastilleApplyLimits(name string, memoryLimitMb, cpus, diskLimitMb int) error {
	if memoryLimitMb > 0 {
		rule := fmt.Sprintf("memoryuse=%dM", memoryLimitMb)
		if _, err := cliRun(cliTimeout, "rctl", "-a", "jail:"+name, rule); err != nil {
			return err
		}
	}
	if cpus > 0 {
		set := "0"
		if cpus > 1 {
			set = fmt.Sprintf("0-%d", cpus-1)
		}
		rule := "cpuset=" + set
		if _, err := cliRun(cliTimeout, "rctl", "-a", "jail:"+name, rule); err != nil {
			return err
		}
	}
	if diskLimitMb > 0 {
		jailDir := filepath.Join(bastilleRoot, "jails", name)
		// zfs set 接受挂载点路径；非 ZFS 环境将报错（容错：仅记录失败）
		if _, err := cliRun(cliTimeout, "zfs", "set", fmt.Sprintf("quota=%dM", diskLimitMb), jailDir); err != nil {
			return err
		}
	}
	return nil
}

// bastilleClone 克隆 jail 为新的名字与可选新 IP（bastille clone NAME NEW_NAME [NEW_IP]）。
func bastilleClone(name, newName, newIP string) error {
	args := []string{"clone", name, newName}
	if newIP != "" {
		args = append(args, newIP)
	}
	_, err := cliRun(cliLongTimeout, "bastille", args...)
	return err
}

// bastilleExport 导出 jail 为归档到同步区 .exports/。
// 返回 {path}：归档相对同步区根的路径，可直接用作 import 的 file 参数。
func bastilleExport(d *Daemon, name string) (map[string]any, error) {
	expDir := filepath.Join(d.clusterRoot(), ".exports")
	if err := os.MkdirAll(expDir, 0o755); err != nil {
		return nil, err
	}
	relName := ".exports/" + name + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".tar.gz"
	filePath := filepath.Join(d.clusterRoot(), filepath.FromSlash(relName))
	f, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("bastille", "export", name)
	cmd.Stdout = f
	cmd.Stderr = f // 警告信息一并落盘，失败时便于排查
	if err := cmd.Run(); err != nil {
		f.Close()
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("bastille export 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return map[string]any{"path": "/" + relName}, nil
}

// bastilleImport 从同步区归档导入 jail（bastille import FILE [NEW_NAME]）。
// replace 为 true 时先销毁同名 jail（-a，可摧毁运行中的）。
func bastilleImport(d *Daemon, file, newName string, replace bool) error {
	filePath, err := d.clusterPath(file)
	if err != nil {
		return err
	}
	if replace && newName != "" {
		// 已存在才销毁；不存在时 destroy 会报错，忽略之
		if _, statErr := os.Stat(filepath.Join(bastilleRoot, "jails", newName)); statErr == nil {
			_, _ = cliRun(cliTimeout, "bastille", "destroy", "-a", newName)
		}
	}
	args := []string{"import", filePath}
	if newName != "" {
		args = append(args, newName)
	}
	_, err = cliRun(cliLongTimeout, "bastille", args...)
	return err
}

// bastilleSetupMode 初始化设置（docs/container-support.md §3.3）：
//   - pf:    bastille setup pf <extIf>（防火墙规则）
//   - vnet:  bastille setup vnet <extIf> <tunIf> <addr>（VNET 网关）
//   - linux: bastille setup linux（Linuxulator 初始化）
//   - check: bastille setup --check（环境检查）
func bastilleSetupMode(mode, extIf, tunIf, addr string) (map[string]any, error) {
	var args []string
	switch mode {
	case "pf":
		args = []string{"setup", "pf", extIf}
	case "vnet":
		args = []string{"setup", "vnet", extIf, tunIf, addr}
	case "linux":
		args = []string{"setup", "linux"}
	case "check":
		args = []string{"setup", "--check"}
	default:
		return nil, fmt.Errorf("mode 仅支持 pf/vnet/linux/check")
	}
	out, err := cliRun(cliLongTimeout, "bastille", args...)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"ok": true, "detail": strings.TrimSpace(out)}
	if mode == "check" {
		result["checked"] = strings.TrimSpace(out)
	}
	return result, nil
}

// bastilleRdrList 端口转发规则列表（bastille rdr <jail> list 输出解析）。
// 返回 [{proto, hostPort, jailPort}]；解析失败的行静默跳过。
func bastilleRdrList(jail string) ([]map[string]any, error) {
	args := []string{"rdr"}
	if jail != "" {
		args = append(args, jail)
	}
	args = append(args, "list")
	out, err := cliRun(cliTimeout, "bastille", args...)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 8)
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
		items = append(items, map[string]any{
			"proto":    proto,
			"hostPort": hostPort,
			"jailPort": jailPort,
		})
	}
	return items, nil
}

// parseRdrLine 解析单条 pf rdr 规则行的 hostPort/jailPort。
// 规则形如 "rdr pass proto tcp from any to any port 2222 -> 10.0.0.2 port 22"：
// "->" 之前的最后一个 "port N" 为 hostPort，之后的为 jailPort。
func parseRdrLine(line string) (hostPort, jailPort int) {
	idx := strings.Index(line, "->")
	hostPart, jailPart := line, ""
	if idx >= 0 {
		hostPart, jailPart = line[:idx], line[idx+2:]
	}
	hostPort = lastPortIn(hostPart)
	jailPort = lastPortIn(jailPart)
	return hostPort, jailPort
}

// lastPortIn 取字符串中最后一个 "port N" 的 N；不存在返回 0。
func lastPortIn(s string) int {
	fields := strings.Fields(s)
	last := 0
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "port" {
			if n, err := strconv.Atoi(fields[i+1]); err == nil {
				last = n
			}
		}
	}
	return last
}

// bastilleRdr 端口转发：add=true 添加，false 删除。
// 语法：bastille rdr <jail> add|delete <proto> <hostPort> <jailPort>
func bastilleRdr(jail, proto string, hostPort, jailPort int, add bool) error {
	action := "add"
	if !add {
		action = "delete"
	}
	_, err := cliRun(cliTimeout, "bastille", "rdr", jail, action, proto,
		strconv.Itoa(hostPort), strconv.Itoa(jailPort))
	return err
}

// bastilleTemplates 模板列表（project/template 格式）。
func bastilleTemplates() ([]string, error) {
	root := filepath.Join(bastilleRoot, "templates")
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取模板目录失败: %w", err)
	}
	items := make([]string, 0, 8)
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
				items = append(items, p.Name()+"/"+t.Name())
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
		cmd := commandWithEnv(args, "bastille", "template", jail, template)
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
