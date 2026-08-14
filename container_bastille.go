//go:build freebsd

// Bastille 容器能力实现（FreeBSD 节点）：包装系统 bastille CLI。
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
			"ports":     []string{}, // 端口转发列表见 rdr，此处不重复解析
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

// bastilleCreate 创建 jail。
// type: thin(-T) / clone(-C) / empty(-E) / linux(-L)；vnet(-V) / bridge(-B)。
func bastilleCreate(name, release, ip, jtype string, vnet, bridge bool, mac string) error {
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
	_, err := cliRun(cliLongTimeout, "bastille", args...)
	return err
}

// bastilleAction jail 操作（start/stop/restart/destroy）。
func bastilleAction(name, action string) error {
	_, err := cliRun(cliTimeout, "bastille", action, name)
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
