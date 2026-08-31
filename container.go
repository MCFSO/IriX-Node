// 容器环境 API（NODE_API.md §6.1 契约）：
// - Linux 节点：Docker 全功能（/api/container/*、/api/image/*、/api/volume/*、/api/network/*）
// - FreeBSD 节点：Bastille 全功能（/api/bastille/*）
// - 其他平台：能力探测返回 available=false，操作端点报「当前平台不支持」。
//
// 实现方式：包装系统自带的 docker / bastille CLI（纯标准库，零依赖），
// 长任务（镜像构建 / bootstrap / 模板应用）以 jobId + 日志流模式暴露。
// 平台差异实现见 container_docker.go（linux）与 container_bastille.go（freebsd）。

package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errContainerUnsupported 当前平台不支持容器能力的哨兵错误。
var errContainerUnsupported = errors.New("当前平台不支持容器环境")

// normalizeRelease 兼容客户端把显示标签 "name:version" 当作 release 传入的情况
// （如 "15.0-RELEASE:15.0-RELEASE"），剥离冒号后缀取发行版名；纯名称原样返回。
func normalizeRelease(release string) string {
	if idx := strings.Index(release, ":"); idx > 0 {
		return release[:idx]
	}
	return release
}

// isAllDigits 判断字符串是否非空且全为数字。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// splitPorts docker ps 的 Ports 形如 "0.0.0.0:25565->25565/tcp, :::25565->25565/tcp"，
// 拆为客户端契约要求的字符串数组。
func splitPorts(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dockerTime 把 docker 输出的时间（"2026-08-14 12:34:56 +0000 UTC"）转为 ISO-8601；
// 解析失败原样返回（容错，不因时间格式变化中断列表）。
func dockerTime(s string) string {
	if t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", strings.TrimSpace(s)); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// bastilleVolume 挂载对（宿主机路径 → jail 内路径），Bastille create 的 volumes 条目。
type bastilleVolume struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

// rdrRule 单条端口转发规则。
type rdrRule struct {
	Jail     string
	Proto    string
	HostPort int
	JailPort int
}

// parseRdrLine 解析单条 pf rdr 规则行的 hostPort/jailPort。
// 规则形如 "rdr on em0 inet proto tcp from any to any port = 2001 -> 10.17.89.1 port 22"：
// "->" 之前的最后一个 "port [=] N" 为 hostPort，之后的为 jailPort。
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

// lastPortIn 取字符串中最后一个 "port = N" 或 "port N" 的 N；不存在返回 0。
func lastPortIn(s string) int {
	fields := strings.Fields(s)
	last := 0
	for i := 0; i < len(fields); i++ {
		if fields[i] == "port" {
			// 支持 "port = 2001"（官方 rdr list 输出带等号）与 "port 2001" 两种形式
			if i+2 < len(fields) && fields[i+1] == "=" {
				if n, err := strconv.Atoi(fields[i+2]); err == nil {
					last = n
				}
				i += 2
				continue
			}
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil {
					last = n
				}
			}
		}
	}
	return last
}

// jobLogMaxLines 长任务日志保留行数上限，防止构建日志无限膨胀。
const jobLogMaxLines = 500

// maxJobs 长任务并发上限，防止刷任务耗尽内存。
const maxJobs = 64

// longJob 长任务（镜像构建 / bootstrap / 模板应用）状态。
type longJob struct {
	mu     sync.Mutex
	status string   // building | done | failed
	log    []string // 输出行（保留最近 jobLogMaxLines 行）
	image  string   // 任务产出（如 "name:tag"）
}

// logCollector 把进程输出按行收集进 longJob.log（并发安全，超限丢最旧）。
// 自行维护跨 Write 的残段，保证行不被截断。
type logCollector struct {
	job   *longJob
	carry string
}

func (c *logCollector) Write(p []byte) (int, error) {
	c.job.mu.Lock()
	defer c.job.mu.Unlock()
	s := c.carry + string(p)
	c.carry = ""
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(s[:i], "\r")
		s = s[i+1:]
		if line != "" {
			c.job.log = append(c.job.log, line)
			if len(c.job.log) > jobLogMaxLines {
				c.job.log = c.job.log[len(c.job.log)-jobLogMaxLines:]
			}
		}
	}
	c.carry = s
	return len(p), nil
}

// jobStore 长任务注册表。
type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*longJob
}

// jobs 全局长任务注册表。
var jobs = &jobStore{jobs: map[string]*longJob{}}

// create 创建任务并返回 id；并发已满返回空字符串。
func (s *jobStore) create() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) >= maxJobs {
		return ""
	}
	id := newUUID()
	s.jobs[id] = &longJob{status: "building"}
	return id
}

// get 按 id 获取任务（不存在返回 nil）。
func (s *jobStore) get(id string) *longJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// runLongJob 后台执行命令，输出按行写入任务日志；结束置 done/failed。
// stdout 与 stderr 同时写入同一收集器（内部加锁），顺序不保证但互不丢失。
func runLongJob(jobID, name string, args ...string) {
	job := jobs.get(jobID)
	if job == nil {
		return
	}
	cmd := exec.Command(name, args...)
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
}

// cliTimeout 普通 CLI 命令超时。
const cliTimeout = 30 * time.Second

// cliLongTimeout 拉取类长命令超时（镜像拉取可达数分钟）。
const cliLongTimeout = 10 * time.Minute

// cliRun 执行命令行（带超时），返回 stdout+stderr 合并输出。
func cliRun(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s 执行失败: %v（%s）", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// registerContainerRoutes 注册容器环境路由（NODE_API.md §6.1）。
func (d *Daemon) registerContainerRoutes(mux *http.ServeMux) {
	// 能力探测
	perm("容器", "GET /api/container/info", "查看容器能力")
	mux.HandleFunc("GET /api/container/info", d.auth(d.handleContainerInfo))

	// Docker（Linux）
	perm("容器", "GET /api/container/ps", "列出容器")
	mux.HandleFunc("GET /api/container/ps", d.auth(d.handleContainerPS))
	perm("容器", "POST /api/container/create", "创建容器")
	mux.HandleFunc("POST /api/container/create", d.auth(d.handleContainerCreate))
	perm("容器", "POST /api/container/{id}/start", "启动容器")
	mux.HandleFunc("POST /api/container/{id}/start", d.auth(d.handleContainerStart))
	perm("容器", "POST /api/container/{id}/stop", "停止容器")
	mux.HandleFunc("POST /api/container/{id}/stop", d.auth(d.handleContainerStop))
	perm("容器", "POST /api/container/{id}/restart", "重启容器")
	mux.HandleFunc("POST /api/container/{id}/restart", d.auth(d.handleContainerRestart))
	perm("容器", "POST /api/container/{id}/kill", "强杀容器")
	mux.HandleFunc("POST /api/container/{id}/kill", d.auth(d.handleContainerKill))
	perm("容器", "DELETE /api/container/{id}", "删除容器")
	mux.HandleFunc("DELETE /api/container/{id}", d.auth(d.handleContainerRemove))
	perm("容器", "GET /api/container/{id}/logs", "查看容器日志")
	mux.HandleFunc("GET /api/container/{id}/logs", d.auth(d.handleContainerLogs))
	perm("容器", "POST /api/container/{id}/exec", "容器内执行命令")
	mux.HandleFunc("POST /api/container/{id}/exec", d.auth(d.handleContainerExec))
	perm("容器", "GET /api/container/{id}/stats", "查看容器统计")
	mux.HandleFunc("GET /api/container/{id}/stats", d.auth(d.handleContainerStats))
	perm("容器", "GET /api/image/list", "列出镜像")
	mux.HandleFunc("GET /api/image/list", d.auth(d.handleImageList))
	perm("容器", "POST /api/image/pull", "拉取镜像")
	mux.HandleFunc("POST /api/image/pull", d.auth(d.handleImagePull))
	perm("容器", "POST /api/image/build", "构建镜像")
	mux.HandleFunc("POST /api/image/build", d.auth(d.handleImageBuild))
	perm("容器", "GET /api/image/build-progress", "查看构建进度")
	mux.HandleFunc("GET /api/image/build-progress", d.auth(d.handleImageBuildProgress))
	perm("容器", "DELETE /api/image/{name}", "删除镜像")
	mux.HandleFunc("DELETE /api/image/{name}", d.auth(d.handleImageRemove))
	perm("容器", "GET /api/volume/list", "列出数据卷")
	mux.HandleFunc("GET /api/volume/list", d.auth(d.handleVolumeList))
	perm("容器", "DELETE /api/volume/{name}", "删除数据卷")
	mux.HandleFunc("DELETE /api/volume/{name}", d.auth(d.handleVolumeRemove))
	perm("容器", "GET /api/network/list", "列出网络")
	mux.HandleFunc("GET /api/network/list", d.auth(d.handleNetworkList))
	// 容器克隆 / 导出导入 / 资源限制
	perm("容器", "POST /api/container/{id}/clone", "克隆容器")
	mux.HandleFunc("POST /api/container/{id}/clone", d.auth(d.handleContainerClone))
	perm("容器", "POST /api/container/{id}/export", "导出容器")
	mux.HandleFunc("POST /api/container/{id}/export", d.auth(d.handleContainerExport))
	perm("容器", "POST /api/container/{id}/limits", "修改容器资源限制")
	mux.HandleFunc("POST /api/container/{id}/limits", d.auth(d.handleContainerLimits))
	perm("容器", "POST /api/image/import", "导入镜像")
	mux.HandleFunc("POST /api/image/import", d.auth(d.handleImageImport))

	// Bastille（FreeBSD）
	perm("Bastille 基础", "GET /api/bastille/releases", "查看 Bastille 版本")
	mux.HandleFunc("GET /api/bastille/releases", d.auth(d.handleBastilleReleases))
	perm("Bastille 基础", "POST /api/bastille/bootstrap", "引导 Bastille")
	mux.HandleFunc("POST /api/bastille/bootstrap", d.auth(d.handleBastilleBootstrap))
	perm("Bastille 基础", "POST /api/bastille/setup", "初始化 Bastille 配置")
	mux.HandleFunc("POST /api/bastille/setup", d.auth(d.handleBastilleSetup))
	perm("Bastille 基础", "GET /api/bastille/jails", "列出 jail")
	mux.HandleFunc("GET /api/bastille/jails", d.auth(d.handleBastilleJails))
	perm("Bastille 基础", "POST /api/bastille/jails/create", "创建 jail")
	mux.HandleFunc("POST /api/bastille/jails/create", d.auth(d.handleBastilleCreate))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/start", "启动 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/start", d.auth(d.handleBastilleStart))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/stop", "停止 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/stop", d.auth(d.handleBastilleStop))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/restart", "重启 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/restart", d.auth(d.handleBastilleRestart))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/destroy", "销毁 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/destroy", d.auth(d.handleBastilleDestroy))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/clone", "克隆 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/clone", d.auth(d.handleBastilleClone))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/export", "导出 jail")
	mux.HandleFunc("POST /api/bastille/jails/{name}/export", d.auth(d.handleBastilleExport))
	perm("Bastille 基础", "POST /api/bastille/jails/import", "导入 jail")
	mux.HandleFunc("POST /api/bastille/jails/import", d.auth(d.handleBastilleImport))
	perm("Bastille 基础", "GET /api/bastille/jails/{name}/console", "jail 控制台")
	mux.HandleFunc("GET /api/bastille/jails/{name}/console", d.auth(d.handleBastilleConsole))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/cmd", "jail 内执行命令")
	mux.HandleFunc("POST /api/bastille/jails/{name}/cmd", d.auth(d.handleBastilleCmd))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/pkg", "jail 内安装软件包")
	mux.HandleFunc("POST /api/bastille/jails/{name}/pkg", d.auth(d.handleBastillePkg))
	perm("Bastille 基础", "GET /api/bastille/jails/{name}/config", "查看 jail 配置")
	mux.HandleFunc("GET /api/bastille/jails/{name}/config", d.auth(d.handleBastilleConfig))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/config", "设置 jail 配置")
	mux.HandleFunc("POST /api/bastille/jails/{name}/config", d.auth(d.handleBastilleConfigSet))
	perm("Bastille 基础", "DELETE /api/bastille/jails/{name}/config", "删除 jail 配置")
	mux.HandleFunc("DELETE /api/bastille/jails/{name}/config", d.auth(d.handleBastilleConfigUnset))
	perm("Bastille 基础", "GET /api/bastille/jails/{name}/mounts", "查看 jail 挂载")
	mux.HandleFunc("GET /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMounts))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/mounts", "添加 jail 挂载")
	mux.HandleFunc("POST /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMountAdd))
	perm("Bastille 基础", "DELETE /api/bastille/jails/{name}/mounts", "移除 jail 挂载")
	mux.HandleFunc("DELETE /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMountRemove))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/limits", "修改 jail 资源限制")
	mux.HandleFunc("POST /api/bastille/jails/{name}/limits", d.auth(d.handleBastilleLimits))
	perm("Bastille 基础", "GET /api/bastille/templates", "查看 jail 模板")
	mux.HandleFunc("GET /api/bastille/templates", d.auth(d.handleBastilleTemplates))
	perm("Bastille 基础", "POST /api/bastille/templates/apply", "应用 jail 模板")
	mux.HandleFunc("POST /api/bastille/templates/apply", d.auth(d.handleBastilleApply))
	perm("Bastille 基础", "POST /api/bastille/rdr", "添加端口转发")
	mux.HandleFunc("POST /api/bastille/rdr", d.auth(d.handleBastilleRdrAdd))
	perm("Bastille 基础", "DELETE /api/bastille/rdr", "删除端口转发")
	mux.HandleFunc("DELETE /api/bastille/rdr", d.auth(d.handleBastilleRdrDelete))
	perm("Bastille 基础", "GET /api/bastille/rdr", "查看端口转发")
	mux.HandleFunc("GET /api/bastille/rdr", d.auth(d.handleBastilleRdrList))
	// 长任务进度（bootstrap / setup / 模板应用），与 /api/image/build-progress 同构
	perm("Bastille 基础", "GET /api/bastille/jobs/{jobId}", "查看 Bastille 任务进度")
	mux.HandleFunc("GET /api/bastille/jobs/{jobId}", d.auth(d.handleBastilleJobProgress))

	// 运行会话（docs/irix-node-container-api.md §4.11）：jail 内后台长任务
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/run", "启动 jail 后台任务")
	mux.HandleFunc("POST /api/bastille/jails/{name}/run", d.auth(d.handleBastilleRunStart))
	perm("Bastille 基础", "GET /api/bastille/jails/{name}/run/{session}", "查看后台任务状态")
	mux.HandleFunc("GET /api/bastille/jails/{name}/run/{session}", d.auth(d.handleBastilleRunStatus))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/run/{session}/stdin", "向后台任务发送输入")
	mux.HandleFunc("POST /api/bastille/jails/{name}/run/{session}/stdin", d.auth(d.handleBastilleRunStdin))
	perm("Bastille 基础", "POST /api/bastille/jails/{name}/run/{session}/stop", "停止后台任务")
	mux.HandleFunc("POST /api/bastille/jails/{name}/run/{session}/stop", d.auth(d.handleBastilleRunStop))
	perm("Bastille 基础", "DELETE /api/bastille/jails/{name}/run/{session}", "删除后台任务")
	mux.HandleFunc("DELETE /api/bastille/jails/{name}/run/{session}", d.auth(d.handleBastilleRunDelete))

	// jail 文件管理（NODE_API.md §6.1）：jail 内路径的列表/读写/删除/上传/下载
	perm("Bastille 文件", "GET /api/bastille/jails/{name}/files", "列出 jail 文件")
	mux.HandleFunc("GET /api/bastille/jails/{name}/files", d.auth(d.handleBastilleFilesList))
	perm("Bastille 文件", "GET /api/bastille/jails/{name}/files/content", "读取 jail 文件内容")
	mux.HandleFunc("GET /api/bastille/jails/{name}/files/content", d.auth(d.handleBastilleFilesContentRead))
	perm("Bastille 文件", "PUT /api/bastille/jails/{name}/files/content", "写入 jail 文件内容")
	mux.HandleFunc("PUT /api/bastille/jails/{name}/files/content", d.auth(d.handleBastilleFilesContentWrite))
	perm("Bastille 文件", "DELETE /api/bastille/jails/{name}/files", "删除 jail 文件")
	mux.HandleFunc("DELETE /api/bastille/jails/{name}/files", d.auth(d.handleBastilleFilesDelete))
	perm("Bastille 文件", "POST /api/bastille/jails/{name}/files/mkdir", "新建 jail 目录")
	mux.HandleFunc("POST /api/bastille/jails/{name}/files/mkdir", d.auth(d.handleBastilleFilesMkdir))
	perm("Bastille 文件", "POST /api/bastille/jails/{name}/files/touch", "新建 jail 文件")
	mux.HandleFunc("POST /api/bastille/jails/{name}/files/touch", d.auth(d.handleBastilleFilesTouch))
	perm("Bastille 文件", "POST /api/bastille/jails/{name}/files/upload", "上传 jail 文件")
	mux.HandleFunc("POST /api/bastille/jails/{name}/files/upload", d.auth(d.handleBastilleFilesUpload))
	perm("Bastille 文件", "GET /api/bastille/jails/{name}/files/download", "下载 jail 文件")
	mux.HandleFunc("GET /api/bastille/jails/{name}/files/download", d.auth(d.handleBastilleFilesDownload))

	// 节点级归档（docs/irix-node-container-api.md §4.8）：编排迁移用，任意宿主机路径
	perm("容器", "POST /api/container/archive", "创建节点归档")
	mux.HandleFunc("POST /api/container/archive", d.auth(d.handleArchiveCreate))
	perm("容器", "GET /api/container/archive", "下载节点归档")
	mux.HandleFunc("GET /api/container/archive", d.auth(d.handleArchiveDownload))
	perm("容器", "POST /api/container/archive/upload", "上传节点归档")
	mux.HandleFunc("POST /api/container/archive/upload", d.auth(d.handleArchiveUpload))
	perm("容器", "POST /api/container/archive/restore", "恢复节点归档")
	mux.HandleFunc("POST /api/container/archive/restore", d.auth(d.handleArchiveRestore))
}

// handleContainerInfo 能力探测。
// GET /api/container/info → {runtime: "docker"|"bastille", platform, version, available, error?}
func (d *Daemon) handleContainerInfo(w http.ResponseWriter, r *http.Request) {
	rt, platform, version, ok := containerRuntimeInfo()
	info := map[string]any{
		"runtime":   rt,
		"platform":  platform,
		"version":   version,
		"available": ok,
	}
	if !ok {
		info["error"] = "未检测到可用的容器运行时（请确认已安装并可用：Linux 需要 docker CLI，FreeBSD 需要 bastille）"
	}
	writeOK(w, info)
}

// containerUnavailable 平台不支持时的统一错误响应。
func containerUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "当前平台不支持该容器能力")
}

// containerErr 输出容器操作错误：平台不支持 → 501，其余 → 500。
func containerErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errContainerUnsupported) {
		containerUnavailable(w)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// handleContainerPS 容器列表。
// GET /api/container/ps?all=1
func (d *Daemon) handleContainerPS(w http.ResponseWriter, r *http.Request) {
	items, err := dockerPS(queryParam(r, "all") == "1")
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleContainerCreate 创建容器（不启动）。
// POST /api/container/create body: {name, image, command?, workdir?, ports, volumes, env, restartPolicy?, memoryLimitMb?, cpus?, diskLimitMb?}
func (d *Daemon) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string            `json:"name"`
		Image         string            `json:"image"`
		Command       string            `json:"command"`
		Workdir       string            `json:"workdir"`
		Ports         []string          `json:"ports"`
		Volumes       []string          `json:"volumes"`
		Env           map[string]string `json:"env"`
		RestartPolicy string            `json:"restartPolicy"`
		MemoryLimitMb int               `json:"memoryLimitMb"`
		Cpus          float64           `json:"cpus"`
		DiskLimitMb   int               `json:"diskLimitMb"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Image == "" {
		writeError(w, http.StatusBadRequest, "缺少 image 参数")
		return
	}
	info, err := dockerCreate(body.Name, body.Image, body.Command, body.Workdir, body.Ports, body.Volumes,
		body.Env, body.RestartPolicy, body.MemoryLimitMb, body.Cpus, body.DiskLimitMb)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, info)
}

// handleContainerStart 启动容器。
// POST /api/container/{id}/start
func (d *Daemon) handleContainerStart(w http.ResponseWriter, r *http.Request) {
	if err := dockerAction(r.PathValue("id"), "start"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleContainerStop 停止容器。
func (d *Daemon) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	if err := dockerAction(r.PathValue("id"), "stop"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleContainerRestart 重启容器。
func (d *Daemon) handleContainerRestart(w http.ResponseWriter, r *http.Request) {
	if err := dockerAction(r.PathValue("id"), "restart"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleContainerKill 强制终止容器。
func (d *Daemon) handleContainerKill(w http.ResponseWriter, r *http.Request) {
	if err := dockerAction(r.PathValue("id"), "kill"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleContainerRemove 删除容器。
// DELETE /api/container/{id}?force=1
func (d *Daemon) handleContainerRemove(w http.ResponseWriter, r *http.Request) {
	if err := dockerRemove(r.PathValue("id"), queryParam(r, "force") == "1"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleContainerLogs 容器日志尾部。
// GET /api/container/{id}/logs?tail=N
func (d *Daemon) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	tail := 100
	if v, err := strconv.Atoi(queryParam(r, "tail")); err == nil && v > 0 {
		tail = v
	}
	logs, err := dockerLogs(r.PathValue("id"), tail)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, logs)
}

// handleContainerExec 容器内执行命令。
// POST /api/container/{id}/exec body: {command}
func (d *Daemon) handleContainerExec(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Command == "" {
		writeError(w, http.StatusBadRequest, "缺少 command 参数")
		return
	}
	out, err := dockerExec(r.PathValue("id"), body.Command)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, out)
}

// handleContainerStats 容器资源统计。
// GET /api/container/{id}/stats → {cpuPercent, memoryBytes, memoryLimitBytes, netRxBytes, netTxBytes}
func (d *Daemon) handleContainerStats(w http.ResponseWriter, r *http.Request) {
	st, err := dockerStats(r.PathValue("id"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, st)
}

// handleImageList 镜像列表。
// GET /api/image/list
func (d *Daemon) handleImageList(w http.ResponseWriter, r *http.Request) {
	items, err := dockerImages()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleImagePull 拉取镜像（同步等待，最长 10 分钟）。
// POST /api/image/pull body: {name}
func (d *Daemon) handleImagePull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	if err := dockerPull(body.Name); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleImageBuild 构建镜像 → {jobId}。
// POST /api/image/build body: {dockerfile, name, tag}
func (d *Daemon) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dockerfile string `json:"dockerfile"`
		Name       string `json:"name"`
		Tag        string `json:"tag"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Name == "" || body.Tag == "" {
		writeError(w, http.StatusBadRequest, "缺少 name/tag 参数")
		return
	}
	jobID, err := dockerBuildStart(d, body.Dockerfile, body.Name, body.Tag)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, map[string]any{"jobId": jobID})
}

// handleImageBuildProgress 构建进度。
// GET /api/image/build-progress?jobId → {status: building|done|failed, log: [...], image}
func (d *Daemon) handleImageBuildProgress(w http.ResponseWriter, r *http.Request) {
	jobID := queryParam(r, "jobId")
	job := jobs.get(jobID)
	if job == nil {
		writeError(w, http.StatusBadRequest, "构建任务不存在或已过期")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	writeOK(w, map[string]any{"status": job.status, "log": job.log, "image": job.image})
}

// handleImageRemove 删除镜像。
// DELETE /api/image/{name}
func (d *Daemon) handleImageRemove(w http.ResponseWriter, r *http.Request) {
	if err := dockerImageRemove(r.PathValue("name")); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleVolumeList 卷列表。
// GET /api/volume/list
func (d *Daemon) handleVolumeList(w http.ResponseWriter, r *http.Request) {
	items, err := dockerVolumeList()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleVolumeRemove 删除卷。
// DELETE /api/volume/{name}
func (d *Daemon) handleVolumeRemove(w http.ResponseWriter, r *http.Request) {
	if err := dockerVolumeRemove(r.PathValue("name")); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleNetworkList 网络列表。
// GET /api/network/list
func (d *Daemon) handleNetworkList(w http.ResponseWriter, r *http.Request) {
	items, err := dockerNetworkList()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleBastilleReleases 已 bootstrap 的发行版列表。
// GET /api/bastille/releases
func (d *Daemon) handleBastilleReleases(w http.ResponseWriter, r *http.Request) {
	items, err := bastilleReleases()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleBastilleBootstrap bootstrap 发行版 → {jobId}。
// POST /api/bastille/bootstrap body: {release}
func (d *Daemon) handleBastilleBootstrap(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Release string `json:"release"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Release == "" {
		writeError(w, http.StatusBadRequest, "缺少 release 参数")
		return
	}
	jobID, err := bastilleBootstrap(body.Release)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, map[string]any{"jobId": jobID})
}

// handleBastilleJails jail 列表。
// GET /api/bastille/jails
func (d *Daemon) handleBastilleJails(w http.ResponseWriter, r *http.Request) {
	items, err := bastilleJails()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleBastilleCreate 创建 jail（含类型/VNET/桥接/IP 与创建后配置）。
// POST /api/bastille/jails/create
// body（新契约 NODE_API.md §6.1）: {name, release, ip?, type: thin|thick|clone|empty|linux,
//
//	vnet?: bool|"none"|"vnet"|"bridge", bridge?, mac?}
//
// body（旧契约兼容，container-api.md §4.2）: 另支持 interface、volumes、
//
//	workdir、memoryLimitMb、cpus、diskLimitMb（创建后应用）。
//
// type 映射：thin=默认(无标志) / thick(-T) / clone(-C) / empty(-E, 仅 NAME) / linux(-L)；
// vnet 为 bool 时 true→vnet、false→none；vnet/bridge 需网卡且 IP 须含子网掩码；
// mac（bool 或 MAC 地址字符串）→ bastille create -M（静态 MAC，仅 VNET）；
// linux 与任何 VNET 模式互斥。响应: {name, warnings}。
func (d *Daemon) handleBastilleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string   `json:"name"`
		Release       string   `json:"release"`
		IP            string   `json:"ip"`
		Type          string   `json:"type"`
		Vnet          any      `json:"vnet"`      // 新契约：bool 或 "none"|"vnet"|"bridge"
		Interface     string   `json:"interface"` // 旧契约：VNET/bridge 网卡
		Bridge        string   `json:"bridge"`    // 新契约：bridge 模式网卡名
		Mac           any      `json:"mac"`       // 新契约：bool 或 MAC 地址字符串（静态 MAC）
		Volumes       []string `json:"volumes"`   // "宿主机路径:jail内路径"
		Workdir       string   `json:"workdir"`
		MemoryLimitMb int      `json:"memoryLimitMb"`
		Cpus          int      `json:"cpus"`
		DiskLimitMb   int      `json:"diskLimitMb"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	// bastille 拒绝纯数字 jail 名（客户端校验允许纯数字），提前以 400 拦截并给中文提示
	if isAllDigits(body.Name) {
		writeError(w, http.StatusBadRequest, "jail 名不能只包含数字（bastille 限制），请至少包含一个字母（如 mc"+body.Name+"）")
		return
	}
	if body.Type == "" {
		body.Type = "thin"
	}
	// vnet 归一化：bool（新契约）或字符串（旧契约）
	vnetMode := "none"
	switch v := body.Vnet.(type) {
	case bool:
		if v {
			vnetMode = "vnet"
		}
	case string:
		vnetMode = strings.TrimSpace(v)
	}
	iface := body.Interface
	if body.Bridge != "" {
		iface = body.Bridge
		if vnetMode == "" || vnetMode == "none" {
			vnetMode = "bridge"
		}
	}
	// mac 归一化：bool → 仅加 -M 标志；字符串 → 加 -M 并写入 jail.conf
	macFlag, macAddr := false, ""
	switch m := body.Mac.(type) {
	case bool:
		macFlag = m
	case string:
		if m = strings.TrimSpace(m); m != "" {
			macFlag, macAddr = true, m
		}
	}
	// volumes：字符串 "宿主机路径:jail内路径" → 结构化挂载对
	volumes := make([]bastilleVolume, 0, len(body.Volumes))
	for _, v := range body.Volumes {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		parts := strings.SplitN(v, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, http.StatusBadRequest, "volumes 条目格式错误（应为 \"宿主机路径:jail内路径\"）: "+v)
			return
		}
		volumes = append(volumes, bastilleVolume{Source: parts[0], Dest: parts[1]})
	}
	info, err := bastilleCreate(body.Name, body.Release, body.IP, body.Type, vnetMode,
		iface, macFlag, macAddr, volumes, body.Workdir, body.MemoryLimitMb, body.Cpus, body.DiskLimitMb)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, info)
}

// handleBastilleStart 启动 jail。
func (d *Daemon) handleBastilleStart(w http.ResponseWriter, r *http.Request) {
	if err := bastilleAction(r.PathValue("name"), "start", false); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleStop 停止 jail。
func (d *Daemon) handleBastilleStop(w http.ResponseWriter, r *http.Request) {
	if err := bastilleAction(r.PathValue("name"), "stop", false); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRestart 重启 jail。
func (d *Daemon) handleBastilleRestart(w http.ResponseWriter, r *http.Request) {
	if err := bastilleAction(r.PathValue("name"), "restart", false); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleDestroy 销毁 jail。
// POST /api/bastille/jails/{name}/destroy?force=1 → force=1 附加 -a（可摧毁运行中的 jail）
func (d *Daemon) handleBastilleDestroy(w http.ResponseWriter, r *http.Request) {
	if err := bastilleAction(r.PathValue("name"), "destroy", queryParam(r, "force") == "1"); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleConsole jail 日志尾部。
// GET /api/bastille/jails/{name}/console?tail=N
func (d *Daemon) handleBastilleConsole(w http.ResponseWriter, r *http.Request) {
	tail := 100
	if v, err := strconv.Atoi(queryParam(r, "tail")); err == nil && v > 0 {
		tail = v
	}
	logs, err := bastilleLogs(r.PathValue("name"), tail)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, logs)
}

// handleBastilleCmd jail 内执行命令。
// POST /api/bastille/jails/{name}/cmd body: {command}
func (d *Daemon) handleBastilleCmd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Command == "" {
		writeError(w, http.StatusBadRequest, "缺少 command 参数")
		return
	}
	out, err := bastilleCmd(r.PathValue("name"), body.Command)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, out)
}

// handleBastillePkg jail 内软件包管理（docs/irix-node-container-api.md §4.9）。
// POST /api/bastille/jails/{name}/pkg body: {action, packages}
// action: install/delete/update/upgrade/autoremove（其他 pkg 子命令亦可透传）；
// 服务端统一附加 -y 避免交互阻塞；响应 data 为命令输出文本。
func (d *Daemon) handleBastillePkg(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action   string   `json:"action"`
		Packages []string `json:"packages"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Action == "" {
		writeError(w, http.StatusBadRequest, "缺少 action 参数")
		return
	}
	if (body.Action == "install" || body.Action == "delete") && len(body.Packages) == 0 {
		writeError(w, http.StatusBadRequest, "action 为 "+body.Action+" 时必须提供 packages 列表")
		return
	}
	out, err := bastillePkg(r.PathValue("name"), body.Action, body.Packages)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, out)
}

// handleBastilleConfig jail.conf 配置（扁平对象，docs/irix-node-container-api.md §4.12）。
// GET /api/bastille/jails/{name}/config → data: {key: value, ...}
func (d *Daemon) handleBastilleConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := bastilleConfigGet(r.PathValue("name"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, cfg)
}

// handleBastilleConfigSet 设置 jail 配置项。
// POST /api/bastille/jails/{name}/config body: {key, value}
// 服务端执行 bastille config <name> <key> <value>。
func (d *Daemon) handleBastilleConfigSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "缺少 key 参数")
		return
	}
	if err := bastilleConfigSet(r.PathValue("name"), body.Key, body.Value); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleConfigUnset 删除 jail 配置项（从 jail.conf 移除）。
// DELETE /api/bastille/jails/{name}/config?key=<key>
// 不存在的 key 返回 200（幂等）。
func (d *Daemon) handleBastilleConfigUnset(w http.ResponseWriter, r *http.Request) {
	key := queryParam(r, "key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "缺少 key 参数")
		return
	}
	if err := bastilleConfigUnset(r.PathValue("name"), key); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleMounts 挂载列表（docs/irix-node-container-api.md §4.10）。
// GET /api/bastille/jails/{name}/mounts
// data 为数组，条目: {src?, dst, fstype, options?, permanent}
// （合并 fstab 条目与当前 mount 输出；permanent 表示条目来自 fstab）。
func (d *Daemon) handleBastilleMounts(w http.ResponseWriter, r *http.Request) {
	items, err := bastilleMountList(r.PathValue("name"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleBastilleTemplates 模板列表（project/template 格式）。
// GET /api/bastille/templates
func (d *Daemon) handleBastilleTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := bastilleTemplates()
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, items)
}

// handleBastilleApply 应用模板 → {jobId}。
// POST /api/bastille/templates/apply body: {jail, template, args: {KEY=VALUE}}
func (d *Daemon) handleBastilleApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Jail     string            `json:"jail"`
		Template string            `json:"template"`
		Args     map[string]string `json:"args"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Jail == "" || body.Template == "" {
		writeError(w, http.StatusBadRequest, "缺少 jail/template 参数")
		return
	}
	jobID, err := bastilleApply(body.Jail, body.Template, body.Args)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, map[string]any{"jobId": jobID})
}

// handleBastilleRdrAdd 添加端口转发。
// POST /api/bastille/rdr body: {jail, proto, hostPort, jailPort}
// 语法：bastille rdr JAIL tcp|udp HOST_PORT JAIL_PORT
func (d *Daemon) handleBastilleRdrAdd(w http.ResponseWriter, r *http.Request) {
	jail, proto, hostPort, jailPort, ok := parseRdrBody(w, r)
	if !ok {
		return
	}
	if err := bastilleRdrAdd(jail, proto, hostPort, jailPort); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRdrDelete 删除端口转发。
// DELETE /api/bastille/rdr body: 同上
// CLI 无单条删除：服务端读取 rdr list → clear → 重放其余规则。
func (d *Daemon) handleBastilleRdrDelete(w http.ResponseWriter, r *http.Request) {
	jail, proto, hostPort, jailPort, ok := parseRdrBody(w, r)
	if !ok {
		return
	}
	if err := bastilleRdrDelete(jail, proto, hostPort, jailPort); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// parseRdrBody 解析端口转发请求体。
func parseRdrBody(w http.ResponseWriter, r *http.Request) (jail, proto string, hostPort, jailPort int, ok bool) {
	var body struct {
		Jail     string `json:"jail"`
		Proto    string `json:"proto"`
		HostPort int    `json:"hostPort"`
		JailPort int    `json:"jailPort"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return "", "", 0, 0, false
	}
	if body.Jail == "" || body.Proto == "" || body.HostPort <= 0 || body.JailPort <= 0 {
		writeError(w, http.StatusBadRequest, "缺少 jail/proto/hostPort/jailPort 参数")
		return "", "", 0, 0, false
	}
	return body.Jail, body.Proto, body.HostPort, body.JailPort, true
}

// handleBastilleJobProgress 长任务进度（bootstrap / 模板应用）。
// GET /api/bastille/jobs/{jobId} → {status: building|done|failed, log: [...]}
func (d *Daemon) handleBastilleJobProgress(w http.ResponseWriter, r *http.Request) {
	job := jobs.get(r.PathValue("jobId"))
	if job == nil {
		writeError(w, http.StatusBadRequest, "任务不存在或已过期")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	writeOK(w, map[string]any{"status": job.status, "log": job.log})
}

// parseDockerSize 解析 docker 输出中的容量字符串（如 "1.2MiB"、"187MB"、"3.4kB"），
// 无法解析时返回 0（解析容错：字段缺失/格式变化不中断列表）。
func parseDockerSize(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	lower := strings.ToLower(s)
	mult := uint64(1)
	for _, suffix := range []struct {
		suf string
		m   uint64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1e9}, {"tb", 1e12},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suffix.suf) {
			lower = strings.TrimSuffix(lower, suffix.suf)
			mult = suffix.m
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil {
		return 0
	}
	return uint64(v * float64(mult))
}

// handleContainerClone 克隆容器（Docker：commit 文件系统为临时镜像再创建）。
// POST /api/container/{id}/clone body: {name} → {id, name, image}
func (d *Daemon) handleContainerClone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	info, err := dockerClone(r.PathValue("id"), body.Name)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, info)
}

// handleContainerExport 导出容器文件系统为归档，返回下载票据。
// POST /api/container/{id}/export → {password, addr, fileName}
func (d *Daemon) handleContainerExport(w http.ResponseWriter, r *http.Request) {
	info, err := dockerExport(d, r.PathValue("id"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, info)
}

// handleImageImport 从同步区归档导入镜像。
// POST /api/image/import body: {fileName, name}
func (d *Daemon) handleImageImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileName string `json:"fileName"`
		Name     string `json:"name"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.FileName == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "缺少 fileName/name 参数")
		return
	}
	if err := dockerImageImport(d, body.FileName, body.Name); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleSetup 容器软件初始化设置（docs/container-support.md §3.3）。
// POST /api/bastille/setup body: {mode: default|firewall|vnet|bridge|shared|linux, extIf?, tunIf?, addr?}
// 服务端统一附加 -y 避免交互阻塞。响应: {ok, detail?}。
func (d *Daemon) handleBastilleSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode  string `json:"mode"`
		ExtIf string `json:"extIf"`
		TunIf string `json:"tunIf"`
		Addr  string `json:"addr"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Mode == "" {
		writeError(w, http.StatusBadRequest, "缺少 mode 参数（default/firewall/vnet/bridge/shared/linux）")
		return
	}
	result, err := bastilleSetupMode(body.Mode, body.ExtIf, body.TunIf, body.Addr)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, result)
}

// handleBastilleClone 克隆 jail（bastille clone NAME NEW_NAME [NEW_IP]）。
// POST /api/bastille/jails/{name}/clone body: {newName, ip?}
func (d *Daemon) handleBastilleClone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NewName string `json:"newName"`
		IP      string `json:"ip"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.NewName == "" {
		writeError(w, http.StatusBadRequest, "缺少 newName 参数")
		return
	}
	if err := bastilleClone(r.PathValue("name"), body.NewName, body.IP); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleExport 导出 jail 为归档到同步区。
// POST /api/bastille/jails/{name}/export → {path: 归档路径（可作 import 的 file）}
func (d *Daemon) handleBastilleExport(w http.ResponseWriter, r *http.Request) {
	info, err := bastilleExport(d, r.PathValue("name"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, info)
}

// handleBastilleImport 从归档导入 jail（bastille import [-f] FILE [RELEASE]）。
// POST /api/bastille/jails/import body: {file, release?, force?} → {name}
func (d *Daemon) handleBastilleImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File    string `json:"file"`
		Release string `json:"release"`
		Force   bool   `json:"force"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.File == "" {
		writeError(w, http.StatusBadRequest, "缺少 file 参数")
		return
	}
	name, err := bastilleImport(d, body.File, body.Release, body.Force)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, map[string]any{"name": name})
}

// handleBastilleMountAdd 添加挂载（docs/irix-node-container-api.md §4.10）。
// POST /api/bastille/jails/{name}/mounts body: {src?, dst, fstype, options?}
//   - nullfs（默认）→ bastille mount <name> <src> <dst>（写 fstab 并即时挂载）
//   - procfs/devfs → 追加 fstab 条目并立即挂载（thin jail 下为宿主 <jailroot>/<dst>）
func (d *Daemon) handleBastilleMountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Src     string `json:"src"`
		Dest    string `json:"dest"` // 旧契约字段名，兼容
		Dst     string `json:"dst"`
		Fstype  string `json:"fstype"`
		Options string `json:"options"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	dst := body.Dst
	if dst == "" {
		dst = body.Dest
	}
	if dst == "" {
		writeError(w, http.StatusBadRequest, "缺少 dst 参数（jail 内目标路径）")
		return
	}
	if err := bastilleMountAdd(r.PathValue("name"), body.Src, dst, body.Fstype, body.Options); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleMountRemove 卸载并移除 fstab 条目。
// DELETE /api/bastille/jails/{name}/mounts?dst=<jail内路径>
// （兼容旧契约 body: {dest}）；fstab 中找不到条目时仅卸载，不报错。
func (d *Daemon) handleBastilleMountRemove(w http.ResponseWriter, r *http.Request) {
	dst := queryParam(r, "dst")
	if dst == "" {
		var body struct {
			Dest string `json:"dest"`
			Dst  string `json:"dst"`
		}
		if err := parseJSONBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
			return
		}
		dst = body.Dst
		if dst == "" {
			dst = body.Dest
		}
	}
	if dst == "" {
		writeError(w, http.StatusBadRequest, "缺少 dst 参数（jail 内目标路径）")
		return
	}
	if err := bastilleMountRemove(r.PathValue("name"), dst); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleLimits 设置 jail 硬件资源限制（docs/container-support.md §3.3）。
// POST /api/bastille/jails/{name}/limits body: {memoryMb?, cpus?, diskMb?}
// memoryMb → rctl memoryuse；cpus → rctl cpuset（分配 0..cpus-1 号核）；diskMb → ZFS 配额
func (d *Daemon) handleBastilleLimits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MemoryMb int `json:"memoryMb"`
		Cpus     int `json:"cpus"`
		DiskMb   int `json:"diskMb"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if err := bastilleApplyLimits(r.PathValue("name"), body.MemoryMb, body.Cpus, body.DiskMb); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRdrList 端口转发规则列表（可按 jail 过滤；不传 jail 时返回全部）。
// GET /api/bastille/rdr?jail= → [{jail, proto, hostPort, jailPort}]
func (d *Daemon) handleBastilleRdrList(w http.ResponseWriter, r *http.Request) {
	rules, err := bastilleRdrList(queryParam(r, "jail"))
	if err != nil {
		containerErr(w, err)
		return
	}
	items := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		items = append(items, map[string]any{
			"jail":     r.Jail,
			"proto":    r.Proto,
			"hostPort": r.HostPort,
			"jailPort": r.JailPort,
		})
	}
	writeOK(w, items)
}

// handleContainerLimits 动态调整容器资源限制（docker update，运行中即时生效）。
// POST /api/container/{id}/limits body: {memoryMb?, cpus?}
func (d *Daemon) handleContainerLimits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MemoryMb int     `json:"memoryMb"`
		Cpus     float64 `json:"cpus"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if err := dockerLimits(r.PathValue("id"), body.MemoryMb, body.Cpus); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// --- 运行会话（docs/irix-node-container-api.md §4.11） ---

// handleBastilleRunStart 在 jail 内后台启动运行会话。
// POST /api/bastille/jails/{name}/run body: {command, cwd?, watch?}
// command 以 shell 语义执行（sh -c 包装）；watch=true 时进程退出自动停止 jail。
// 响应 data: {sessionId}。
func (d *Daemon) handleBastilleRunStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Watch   bool   `json:"watch"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Command == "" {
		writeError(w, http.StatusBadRequest, "缺少 command 参数")
		return
	}
	sessionID, err := bastilleRunStart(r.PathValue("name"), body.Command, body.Cwd, body.Watch)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, map[string]any{"sessionId": sessionID})
}

// handleBastilleRunStatus 查询会话状态与增量日志。
// GET /api/bastille/jails/{name}/run/{session}?tail=N&since=<字节偏移>
// 响应 data: {running, exitCode?, offset, log}；since 缺省时返回最后 tail 行（默认 200）。
func (d *Daemon) handleBastilleRunStatus(w http.ResponseWriter, r *http.Request) {
	tail := 200
	if v, err := strconv.Atoi(queryParam(r, "tail")); err == nil && v > 0 {
		tail = v
	}
	since := int64(0)
	if v, err := strconv.ParseInt(queryParam(r, "since"), 10, 64); err == nil && v > 0 {
		since = v
	}
	data, err := bastilleRunStatus(r.PathValue("name"), r.PathValue("session"), tail, since)
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, data)
}

// handleBastilleRunStdin 向会话进程 stdin 写入输入（控制台命令）。
// POST /api/bastille/jails/{name}/run/{session}/stdin body: {input}
func (d *Daemon) handleBastilleRunStdin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input string `json:"input"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if err := bastilleRunStdin(r.PathValue("name"), r.PathValue("session"), body.Input); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRunStop 终止会话进程（SIGTERM → 10s 超时 SIGKILL）。
// POST /api/bastille/jails/{name}/run/{session}/stop
func (d *Daemon) handleBastilleRunStop(w http.ResponseWriter, r *http.Request) {
	if err := bastilleRunStop(r.PathValue("name"), r.PathValue("session")); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRunDelete 清理会话（终止进程 + 删除日志缓冲）。
// DELETE /api/bastille/jails/{name}/run/{session}
func (d *Daemon) handleBastilleRunDelete(w http.ResponseWriter, r *http.Request) {
	if err := bastilleRunDelete(r.PathValue("name"), r.PathValue("session")); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// --- 节点级归档（docs/irix-node-container-api.md §4.8） ---

// archiveDir 节点级归档目录（{data}/archives）。
func (d *Daemon) archiveDir() string {
	return filepath.Join(d.DataDir, "archives")
}

// errArchiveTraversal 归档条目路径越界（zip-slip / 绝对路径）哨兵错误，
// 命中时返回 400（归档内容非法）而非 500。
var errArchiveTraversal = errors.New("归档条目越界")

// safeArchiveName 校验并规范化归档文件名：只允许纯文件名（拒绝路径分隔符与
// ../ 等穿越尝试），返回规范化后的文件名。
func safeArchiveName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.Contains(name, "/") {
		return "", errors.New("归档文件名不能包含路径分隔符")
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", errors.New("归档文件名无效")
	}
	return name, nil
}

// handleArchiveCreate 压缩节点任意路径为 zip 归档（§4.8）。
// POST /api/container/archive body: {path, archive?} → {path}
// archive 缺省自动命名为 "<basename>_<时间戳>.zip"。
func (d *Daemon) handleArchiveCreate(w http.ResponseWriter, r *http.Request) {
	// 节点级归档可压缩任意宿主机路径，属高危操作：仅管理员可用，
	// 防止非管理员账户借此读取节点敏感文件（CodeQL 审计 #30）。
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Path    string `json:"path"`
		Archive string `json:"archive"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if _, err := os.Stat(body.Path); err != nil {
		writeError(w, http.StatusBadRequest, "路径不存在或不可访问: "+err.Error())
		return
	}
	name := body.Archive
	if name == "" {
		base := filepath.Base(body.Path)
		if base == "." || base == string(filepath.Separator) || base == "" {
			base = "archive"
		}
		name = fmt.Sprintf("%s_%s.zip", base, time.Now().Format("20060102_150405"))
	}
	name, err := safeArchiveName(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(d.archiveDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建归档目录失败: "+err.Error())
		return
	}
	dst := filepath.Join(d.archiveDir(), name)
	if err := zipPathTo(body.Path, dst); err != nil {
		writeError(w, http.StatusInternalServerError, "压缩失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"path": dst})
}

// handleArchiveDownload 原始字节下载归档（不走 JSON 信封）。
// GET /api/container/archive?file=<归档名>
func (d *Daemon) handleArchiveDownload(w http.ResponseWriter, r *http.Request) {
	name, err := safeArchiveName(queryParam(r, "file"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := filepath.Join(d.archiveDir(), name)
	if !pathWithin(d.archiveDir(), p) {
		writeError(w, http.StatusBadRequest, "归档路径越界")
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusNotFound, "归档不存在: "+name)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, p)
}

// handleArchiveUpload 原始字节上传归档（multipart，字段名 file）。
// POST /api/container/archive/upload → {path}
func (d *Daemon) handleArchiveUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误（multipart）: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少 file 字段: "+err.Error())
		return
	}
	defer file.Close()
	name, err := safeArchiveName(header.Filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(d.archiveDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "创建归档目录失败: "+err.Error())
		return
	}
	dst := filepath.Join(d.archiveDir(), name)
	out, err := os.Create(dst)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建归档文件失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		_ = os.Remove(dst)
		writeError(w, http.StatusInternalServerError, "写入归档失败: "+err.Error())
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		writeError(w, http.StatusInternalServerError, "写入归档失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"path": dst})
}

// handleArchiveRestore 解压归档到目标路径（覆盖式恢复，防 zip-slip）。
// POST /api/container/archive/restore body: {file, destPath}
func (d *Daemon) handleArchiveRestore(w http.ResponseWriter, r *http.Request) {
	// 节点级归档可解压到任意宿主机路径，属高危操作：仅管理员可用，
	// 防止非管理员账户借此向节点任意位置写文件（CodeQL 审计 #31）。
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		File     string `json:"file"`
		DestPath string `json:"destPath"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.File == "" || body.DestPath == "" {
		writeError(w, http.StatusBadRequest, "缺少 file/destPath 参数")
		return
	}
	name, err := safeArchiveName(body.File)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	archivePath := filepath.Join(d.archiveDir(), name)
	if !pathWithin(d.archiveDir(), archivePath) {
		writeError(w, http.StatusBadRequest, "归档路径越界")
		return
	}
	if !filepath.IsAbs(body.DestPath) {
		writeError(w, http.StatusBadRequest, "destPath 必须为绝对路径")
		return
	}
	if err := unzipTo(archivePath, body.DestPath); err != nil {
		if errors.Is(err, errArchiveTraversal) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}
	writeOK(w, true)
}

// zipPathTo 将 path（文件或目录）压缩为 zip 归档 dst（先写临时文件再原子改名）。
// 目录归档时条目为相对路径（不含被压缩目录本身），恢复时解压到目标目录内。
func zipPathTo(src, dst string) error {
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	zw := zip.NewWriter(f)
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		// 单文件：条目名为文件名
		if err := addZipFile(zw, src, filepath.Base(src)); err != nil {
			return err
		}
	} else {
		root := filepath.Clean(src)
		err := filepath.WalkDir(src, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil // 不含被压缩目录本身
			}
			if de.Type()&os.ModeSymlink != 0 {
				return nil // 跳过符号链接（与发行版大小统计一致）
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if de.IsDir() {
				return addZipDir(zw, rel)
			}
			return addZipFile(zw, path, rel)
		})
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// addZipDir 向 zip 写入空目录条目（以 / 结尾）。
func addZipDir(zw *zip.Writer, name string) error {
	h := &zip.FileHeader{Name: name + "/", Method: zip.Store}
	h.SetMode(0o755 | os.ModeDir)
	_, err := zw.CreateHeader(h)
	return err
}

// addZipFile 向 zip 写入单个文件条目（defalte 压缩）。
func addZipFile(zw *zip.Writer, path, name string) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	if fi, err := os.Stat(path); err == nil {
		h.SetMode(fi.Mode().Perm())
		h.Modified = fi.ModTime()
	}
	fw, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(fw, src)
	return err
}

// unzipTo 解压 zip 归档到 destPath（覆盖式恢复）。
// 每个条目的目标路径都校验在 destPath 内（防 zip-slip 路径穿越）。
func unzipTo(archivePath, destPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	dest := filepath.Clean(destPath)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		// 条目名以 / 或 \ 开头视为绝对路径（跨平台一致，FreeBSD 上本来就是绝对路径）
		if strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, `\`) {
			return fmt.Errorf("%w: %s", errArchiveTraversal, f.Name)
		}
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." || name == "" {
			continue
		}
		// 防穿越：绝对路径或含 .. 直接拒绝
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s", errArchiveTraversal, f.Name)
		}
		out := filepath.Join(dest, name)
		if !pathWithin(dest, out) {
			return fmt.Errorf("%w: %s", errArchiveTraversal, f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(out)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(w, rc)
		rc.Close()
		closeErr := w.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
