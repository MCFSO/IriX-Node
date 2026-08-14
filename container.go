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
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errContainerUnsupported 当前平台不支持容器能力的哨兵错误。
var errContainerUnsupported = errors.New("当前平台不支持容器环境")

// bastilleVolume 挂载对（宿主机路径 → jail 内路径），Bastille create 的 volumes 条目。
type bastilleVolume struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
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
	mux.HandleFunc("GET /api/container/info", d.auth(d.handleContainerInfo))

	// Docker（Linux）
	mux.HandleFunc("GET /api/container/ps", d.auth(d.handleContainerPS))
	mux.HandleFunc("POST /api/container/create", d.auth(d.handleContainerCreate))
	mux.HandleFunc("POST /api/container/{id}/start", d.auth(d.handleContainerStart))
	mux.HandleFunc("POST /api/container/{id}/stop", d.auth(d.handleContainerStop))
	mux.HandleFunc("POST /api/container/{id}/restart", d.auth(d.handleContainerRestart))
	mux.HandleFunc("POST /api/container/{id}/kill", d.auth(d.handleContainerKill))
	mux.HandleFunc("DELETE /api/container/{id}", d.auth(d.handleContainerRemove))
	mux.HandleFunc("GET /api/container/{id}/logs", d.auth(d.handleContainerLogs))
	mux.HandleFunc("POST /api/container/{id}/exec", d.auth(d.handleContainerExec))
	mux.HandleFunc("GET /api/container/{id}/stats", d.auth(d.handleContainerStats))
	mux.HandleFunc("GET /api/image/list", d.auth(d.handleImageList))
	mux.HandleFunc("POST /api/image/pull", d.auth(d.handleImagePull))
	mux.HandleFunc("POST /api/image/build", d.auth(d.handleImageBuild))
	mux.HandleFunc("GET /api/image/build-progress", d.auth(d.handleImageBuildProgress))
	mux.HandleFunc("DELETE /api/image/{name}", d.auth(d.handleImageRemove))
	mux.HandleFunc("GET /api/volume/list", d.auth(d.handleVolumeList))
	mux.HandleFunc("DELETE /api/volume/{name}", d.auth(d.handleVolumeRemove))
	mux.HandleFunc("GET /api/network/list", d.auth(d.handleNetworkList))
	// 容器克隆 / 导出导入 / 资源限制
	mux.HandleFunc("POST /api/container/{id}/clone", d.auth(d.handleContainerClone))
	mux.HandleFunc("POST /api/container/{id}/export", d.auth(d.handleContainerExport))
	mux.HandleFunc("POST /api/container/{id}/limits", d.auth(d.handleContainerLimits))
	mux.HandleFunc("POST /api/image/import", d.auth(d.handleImageImport))

	// Bastille（FreeBSD）
	mux.HandleFunc("GET /api/bastille/releases", d.auth(d.handleBastilleReleases))
	mux.HandleFunc("POST /api/bastille/bootstrap", d.auth(d.handleBastilleBootstrap))
	mux.HandleFunc("POST /api/bastille/setup", d.auth(d.handleBastilleSetup))
	mux.HandleFunc("GET /api/bastille/jails", d.auth(d.handleBastilleJails))
	mux.HandleFunc("POST /api/bastille/jails/create", d.auth(d.handleBastilleCreate))
	mux.HandleFunc("POST /api/bastille/jails/{name}/start", d.auth(d.handleBastilleStart))
	mux.HandleFunc("POST /api/bastille/jails/{name}/stop", d.auth(d.handleBastilleStop))
	mux.HandleFunc("POST /api/bastille/jails/{name}/restart", d.auth(d.handleBastilleRestart))
	mux.HandleFunc("POST /api/bastille/jails/{name}/destroy", d.auth(d.handleBastilleDestroy))
	mux.HandleFunc("POST /api/bastille/jails/{name}/clone", d.auth(d.handleBastilleClone))
	mux.HandleFunc("POST /api/bastille/jails/{name}/export", d.auth(d.handleBastilleExport))
	mux.HandleFunc("POST /api/bastille/jails/import", d.auth(d.handleBastilleImport))
	mux.HandleFunc("GET /api/bastille/jails/{name}/console", d.auth(d.handleBastilleConsole))
	mux.HandleFunc("POST /api/bastille/jails/{name}/cmd", d.auth(d.handleBastilleCmd))
	mux.HandleFunc("GET /api/bastille/jails/{name}/config", d.auth(d.handleBastilleConfig))
	mux.HandleFunc("GET /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMounts))
	mux.HandleFunc("POST /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMountAdd))
	mux.HandleFunc("DELETE /api/bastille/jails/{name}/mounts", d.auth(d.handleBastilleMountRemove))
	mux.HandleFunc("POST /api/bastille/jails/{name}/limits", d.auth(d.handleBastilleLimits))
	mux.HandleFunc("GET /api/bastille/templates", d.auth(d.handleBastilleTemplates))
	mux.HandleFunc("POST /api/bastille/templates/apply", d.auth(d.handleBastilleApply))
	mux.HandleFunc("POST /api/bastille/rdr", d.auth(d.handleBastilleRdrAdd))
	mux.HandleFunc("DELETE /api/bastille/rdr", d.auth(d.handleBastilleRdrDelete))
	mux.HandleFunc("GET /api/bastille/rdr", d.auth(d.handleBastilleRdrList))
	// 长任务进度（bootstrap / setup / 模板应用），与 /api/image/build-progress 同构
	mux.HandleFunc("GET /api/bastille/jobs/{jobId}", d.auth(d.handleBastilleJobProgress))
}

// handleContainerInfo 能力探测。
// GET /api/container/info → {runtime: "docker"|"bastille", platform, version, available}
func (d *Daemon) handleContainerInfo(w http.ResponseWriter, r *http.Request) {
	runtime, platform, version, ok := containerRuntimeInfo()
	writeOK(w, map[string]any{
		"runtime":   runtime,
		"platform":  platform,
		"version":   version,
		"available": ok,
	})
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
// body: {name, release, ip?, type: thin|thick|clone|empty|linux, vnet?, bridge?, mac?,
//
//	volumes?: [{source, dest}], workdir?, memoryLimitMb?, cpus?, diskLimitMb?}
//
// 响应: {name, warnings: [配置步骤失败告警]}（创建成功但后置配置失败不视为失败）
func (d *Daemon) handleBastilleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string           `json:"name"`
		Release       string           `json:"release"`
		IP            string           `json:"ip"`
		Type          string           `json:"type"`
		Vnet          bool             `json:"vnet"`
		Bridge        bool             `json:"bridge"`
		Mac           string           `json:"mac"`
		Volumes       []bastilleVolume `json:"volumes"`
		Workdir       string           `json:"workdir"`
		MemoryLimitMb int              `json:"memoryLimitMb"`
		Cpus          int              `json:"cpus"`
		DiskLimitMb   int              `json:"diskLimitMb"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Name == "" || body.Release == "" {
		writeError(w, http.StatusBadRequest, "缺少 name/release 参数")
		return
	}
	info, err := bastilleCreate(body.Name, body.Release, body.IP, body.Type, body.Vnet, body.Bridge,
		body.Mac, body.Volumes, body.Workdir, body.MemoryLimitMb, body.Cpus, body.DiskLimitMb)
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

// handleBastilleConfig jail.conf 内容。
// GET /api/bastille/jails/{name}/config
func (d *Daemon) handleBastilleConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := bastilleConfig(r.PathValue("name"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, cfg)
}

// handleBastilleMounts 挂载列表。
// GET /api/bastille/jails/{name}/mounts
func (d *Daemon) handleBastilleMounts(w http.ResponseWriter, r *http.Request) {
	mounts, err := bastilleMounts(r.PathValue("name"))
	if err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, mounts)
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
func (d *Daemon) handleBastilleRdrAdd(w http.ResponseWriter, r *http.Request) {
	jail, proto, hostPort, jailPort, ok := parseRdrBody(w, r)
	if !ok {
		return
	}
	if err := bastilleRdr(jail, proto, hostPort, jailPort, true); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleRdrDelete 删除端口转发。
// DELETE /api/bastille/rdr body: 同上
func (d *Daemon) handleBastilleRdrDelete(w http.ResponseWriter, r *http.Request) {
	jail, proto, hostPort, jailPort, ok := parseRdrBody(w, r)
	if !ok {
		return
	}
	if err := bastilleRdr(jail, proto, hostPort, jailPort, false); err != nil {
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
// POST /api/bastille/setup body: {mode: pf|vnet|linux|check, extIf?, tunIf?, addr?}
//
//	pf:    bastille setup pf <extIf>（防火墙规则）
//	vnet:  bastille setup vnet <extIf> <tunIf> <addr>（VNET 网关）
//	linux: bastille setup linux（Linuxulator 初始化）
//	check: bastille setup --check（环境检查）
//
// 响应: {ok, detail?, checked?}（check 模式含 checked）
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
		writeError(w, http.StatusBadRequest, "缺少 mode 参数（pf/vnet/linux/check）")
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

// handleBastilleImport 从同步区归档导入 jail（bastille import FILE [NEW_NAME]）。
// POST /api/bastille/jails/import body: {file, newName?, replace?}
func (d *Daemon) handleBastilleImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File    string `json:"file"`
		NewName string `json:"newName"`
		Replace bool   `json:"replace"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.File == "" {
		writeError(w, http.StatusBadRequest, "缺少 file 参数")
		return
	}
	if err := bastilleImport(d, body.File, body.NewName, body.Replace); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleMountAdd 挂载宿主机目录到 jail（bastille mount NAME SOURCE DEST）。
// POST /api/bastille/jails/{name}/mounts body: {source, dest}
func (d *Daemon) handleBastilleMountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
		Dest   string `json:"dest"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Source == "" || body.Dest == "" {
		writeError(w, http.StatusBadRequest, "缺少 source/dest 参数")
		return
	}
	if err := bastilleMount(r.PathValue("name"), body.Source, body.Dest); err != nil {
		containerErr(w, err)
		return
	}
	writeOK(w, true)
}

// handleBastilleMountRemove 解除 jail 挂载（bastille umount NAME DEST）。
// DELETE /api/bastille/jails/{name}/mounts body: {dest}
func (d *Daemon) handleBastilleMountRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dest string `json:"dest"`
	}
	if err := parseJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if body.Dest == "" {
		writeError(w, http.StatusBadRequest, "缺少 dest 参数")
		return
	}
	if err := bastilleUmount(r.PathValue("name"), body.Dest); err != nil {
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

// handleBastilleRdrList 端口转发规则列表（可按 jail 过滤）。
// GET /api/bastille/rdr?jail= → [{proto, hostPort, jailPort}]
func (d *Daemon) handleBastilleRdrList(w http.ResponseWriter, r *http.Request) {
	items, err := bastilleRdrList(queryParam(r, "jail"))
	if err != nil {
		containerErr(w, err)
		return
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
