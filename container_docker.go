//go:build linux

// Docker 容器能力实现（Linux 节点）：包装系统 docker CLI。
// 所有命令输出解析均容错：字段缺失 / 格式变化返回默认值，不中断列表。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// containerRuntimeInfo 能力探测：Linux 节点 → Docker。
func containerRuntimeInfo() (runtime, platform, version string, ok bool) {
	v, ok := dockerAvailable()
	return "docker", "linux", v, ok
}

// dockerAvailable 探测 docker CLI 可用性与服务端版本。
func dockerAvailable() (version string, ok bool) {
	out, err := cliRun(cliTimeout, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return "", false
	}
	return v, true
}

// dockerJSONLines 解析 docker --format '{{json .}}' 的逐行 JSON 输出。
// 非 JSON 行（警告、错误）静默跳过。
func dockerJSONLines(out string) []map[string]any {
	lines := make([]map[string]any, 0, 8)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			lines = append(lines, m)
		}
	}
	return lines
}

// jstr 从 JSON 行取字符串字段（缺失或非字符串时返回空串）。
func jstr(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// dockerPS 容器列表。
func dockerPS(all bool) ([]map[string]any, error) {
	args := []string{"ps"}
	if all {
		args = append(args, "-a")
	}
	args = append(args, "--format", "{{json .}}")
	out, err := cliRun(cliTimeout, "docker", args...)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 8)
	for _, m := range dockerJSONLines(out) {
		items = append(items, map[string]any{
			"id":            jstr(m, "ID"),
			"name":          jstr(m, "Names"),
			"image":         jstr(m, "Image"),
			"status":        jstr(m, "Status"),
			"state":         jstr(m, "State"),
			"ports":         jstr(m, "Ports"),
			"createdAt":     jstr(m, "CreatedAt"),
			"restartPolicy": "", // docker ps 不提供该字段，需 inspect 才可获取
		})
	}
	return items, nil
}

// dockerCreate 创建容器（不启动），返回 {id, name}。
// workDir 经 -w 设置容器内工作目录；diskLimitGb 经 --storage-opt size= 限制
// 容器可写层大小（需 overlay2 且启用 quota，否则该参数无效）。
func dockerCreate(name, image, command, workDir string, ports, volumes []string, env map[string]string, restartPolicy string, memoryLimitMb int, cpus float64, diskLimitGb int) (map[string]any, error) {
	args := []string{"create"}
	if name != "" {
		args = append(args, "--name", name)
	}
	for _, p := range ports {
		if strings.TrimSpace(p) != "" {
			args = append(args, "-p", p)
		}
	}
	for _, v := range volumes {
		if strings.TrimSpace(v) != "" {
			args = append(args, "-v", v)
		}
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	if restartPolicy != "" {
		args = append(args, "--restart", restartPolicy)
	}
	if memoryLimitMb > 0 {
		args = append(args, "-m", fmt.Sprintf("%dm", memoryLimitMb))
	}
	if cpus > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}
	if diskLimitGb > 0 {
		args = append(args, "--storage-opt", fmt.Sprintf("size=%dG", diskLimitGb))
	}
	if workDir != "" {
		args = append(args, "-w", workDir)
	}
	args = append(args, image)
	if command != "" {
		args = append(args, SplitCommand(command)...)
	}
	out, err := cliRun(cliTimeout, "docker", args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":   strings.TrimSpace(out),
		"name": name,
	}, nil
}

// dockerAction 容器操作（start/stop/restart/kill）。
func dockerAction(id, action string) error {
	_, err := cliRun(cliTimeout, "docker", action, id)
	return err
}

// dockerRemove 删除容器；force 时加 -f。
func dockerRemove(id string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, id)
	_, err := cliRun(cliTimeout, "docker", args...)
	return err
}

// dockerLogs 容器日志尾部。
func dockerLogs(id string, tail int) (string, error) {
	return cliRun(cliTimeout, "docker", "logs", "--tail", strconv.Itoa(tail), id)
}

// dockerExec 容器内执行命令（经 sh -c 支持管道/重定向等 shell 语法）。
func dockerExec(id, command string) (string, error) {
	return cliRun(cliTimeout, "docker", "exec", id, "sh", "-c", command)
}

// dockerStats 容器资源统计。
// CPUPerc 形如 "0.05%"：按百分比数值原样返回（0.05 = 0.05%）。
func dockerStats(id string) (map[string]any, error) {
	out, err := cliRun(cliTimeout, "docker", "stats", "--no-stream", "--format", "{{json .}}", id)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	for _, line := range dockerJSONLines(out) {
		m = line // 指定单容器时仅一行
	}
	cpu := 0.0
	if v, err := strconv.ParseFloat(strings.TrimSuffix(jstr(m, "CPUPerc"), "%"), 64); err == nil {
		cpu = v
	}
	// MemUsage 形如 "1.2MiB / 7.7GiB"
	memUsed, memLimit := uint64(0), uint64(0)
	if parts := strings.Split(jstr(m, "MemUsage"), "/"); len(parts) == 2 {
		memUsed = parseDockerSize(parts[0])
		memLimit = parseDockerSize(parts[1])
	}
	// NetIO 形如 "1.2kB / 3.4kB"
	netRx, netTx := uint64(0), uint64(0)
	if parts := strings.Split(jstr(m, "NetIO"), "/"); len(parts) == 2 {
		netRx = parseDockerSize(parts[0])
		netTx = parseDockerSize(parts[1])
	}
	return map[string]any{
		"cpuPercent":       cpu,
		"memoryBytes":      memUsed,
		"memoryLimitBytes": memLimit,
		"netRxBytes":       netRx,
		"netTxBytes":       netTx,
	}, nil
}

// dockerImages 镜像列表（同一 ID 的多 tag 合并）。
func dockerImages() ([]map[string]any, error) {
	out, err := cliRun(cliTimeout, "docker", "images", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	type img struct {
		id, size, createdAt string
		tags                []string
	}
	order := make([]string, 0, 8)
	byID := map[string]*img{}
	for _, m := range dockerJSONLines(out) {
		id := jstr(m, "ID")
		repo, tag := jstr(m, "Repository"), jstr(m, "Tag")
		if id == "" || repo == "" || tag == "" || repo == "<none>" || tag == "<none>" {
			continue
		}
		entry := byID[id]
		if entry == nil {
			entry = &img{id: id, size: jstr(m, "Size"), createdAt: jstr(m, "CreatedAt")}
			byID[id] = entry
			order = append(order, id)
		}
		entry.tags = append(entry.tags, repo+":"+tag)
	}
	items := make([]map[string]any, 0, len(order))
	for _, id := range order {
		e := byID[id]
		items = append(items, map[string]any{
			"id":        e.id,
			"tags":      e.tags,
			"sizeBytes": parseDockerSize(e.size),
			"createdAt": e.createdAt,
		})
	}
	return items, nil
}

// dockerPull 拉取镜像（同步等待，最长 cliLongTimeout）。
func dockerPull(name string) error {
	_, err := cliRun(cliLongTimeout, "docker", "pull", name)
	return err
}

// dockerImageRemove 删除镜像。
func dockerImageRemove(name string) error {
	_, err := cliRun(cliTimeout, "docker", "rmi", name)
	return err
}

// dockerBuildStart 启动镜像构建长任务：dockerfile 内容写入 {data}/container-build/<jobId>/，
// 以该目录为构建上下文，产出镜像 name:tag。
func dockerBuildStart(d *Daemon, dockerfile, name, tag string) (string, error) {
	jobID := jobs.create()
	if jobID == "" {
		return "", errors.New("构建任务已满，请稍后重试")
	}
	dir := filepath.Join(d.DataDir, "container-build", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	tagArg := name + ":" + tag
	job := jobs.get(jobID)
	job.mu.Lock()
	job.image = tagArg
	job.mu.Unlock()
	go runLongJob(jobID, "docker", "build", "-t", tagArg, "-f", "Dockerfile", dir)
	return jobID, nil
}

// dockerVolumeList 卷列表。
func dockerVolumeList() ([]map[string]any, error) {
	out, err := cliRun(cliTimeout, "docker", "volume", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 8)
	for _, m := range dockerJSONLines(out) {
		items = append(items, map[string]any{
			"name":   jstr(m, "Name"),
			"driver": jstr(m, "Driver"),
		})
	}
	return items, nil
}

// dockerVolumeRemove 删除卷。
func dockerVolumeRemove(name string) error {
	_, err := cliRun(cliTimeout, "docker", "volume", "rm", name)
	return err
}

// dockerNetworkList 网络列表。
func dockerNetworkList() ([]map[string]any, error) {
	out, err := cliRun(cliTimeout, "docker", "network", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 8)
	for _, m := range dockerJSONLines(out) {
		items = append(items, map[string]any{
			"name":   jstr(m, "Name"),
			"driver": jstr(m, "Driver"),
			"subnet": "", // docker network ls 不提供子网信息
		})
	}
	return items, nil
}

// dockerClone 克隆容器：commit 当前文件系统为临时镜像，再以新名字创建容器。
// Docker 无原生 clone 命令，commit+create 是标准等效做法。
func dockerClone(id, name string) (map[string]any, error) {
	img := "irix-clone-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if _, err := cliRun(cliLongTimeout, "docker", "commit", id, img); err != nil {
		return nil, fmt.Errorf("提交容器文件系统失败: %w", err)
	}
	args := []string{"create"}
	if name != "" {
		args = append(args, "--name", name)
	}
	args = append(args, img)
	out, err := cliRun(cliTimeout, "docker", args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":    strings.TrimSpace(out),
		"name":  name,
		"image": img,
	}, nil
}

// dockerExport 导出容器文件系统为 tar 到同步区，返回下载票据。
// 响应: {password, addr, fileName}（fileName 相对同步区根，如 ".exports/xxx.tar"）
func dockerExport(d *Daemon, id string) (map[string]any, error) {
	expDir := filepath.Join(d.clusterRoot(), ".exports")
	if err := os.MkdirAll(expDir, 0o755); err != nil {
		return nil, err
	}
	fileName := id + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".tar"
	filePath := filepath.Join(expDir, fileName)
	f, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliLongTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "export", id)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		f.Close()
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("docker export 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	password := tickets.Create("cluster", d.clusterRoot(), "")
	if password == "" {
		return nil, errors.New("下载票据已满，请稍后重试")
	}
	return map[string]any{
		"password": password,
		"addr":     d.publicAddr(),
		"fileName": ".exports/" + fileName,
	}, nil
}

// dockerImageImport 从同步区归档导入镜像（docker import - <name>，tar 经 stdin 传入）。
func dockerImageImport(d *Daemon, fileName, name string) error {
	filePath, err := d.clusterPath(fileName)
	if err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), cliLongTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "import", "-", name)
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker import 失败: %v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}
