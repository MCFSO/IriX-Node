// 守护进程核心：实例数据模型、持久化与实例注册表。

package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// InstanceStatus 实例运行状态（与 MCSM 一致）。
// -1 = 忙碌, 0 = 已关闭, 1 = 停止中, 2 = 启动中, 3 = 运行中
const (
	StatusBusy     = -1
	StatusStopped  = 0
	StatusStopping = 1
	StatusStarting = 2
	StatusRunning  = 3
)

// EventTask 事件任务配置。
type EventTask struct {
	AutoStart   bool `json:"autoStart"`
	AutoRestart bool `json:"autoRestart"`
	Ignore      bool `json:"ignore"`
}

// PingConfig 服务器状态探测配置。
type PingConfig struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Type int    `json:"type"`
}

// InstanceConfig 实例配置（与 MCSM InstanceConfig 字段对齐）。
type InstanceConfig struct {
	Nickname          string     `json:"nickname"`
	StartCommand      string     `json:"startCommand"`
	StopCommand       string     `json:"stopCommand"`
	Cwd               string     `json:"cwd"`
	IE                string     `json:"ie"`
	OE                string     `json:"oe"`
	CreateDatetime    int64      `json:"createDatetime"`
	LastDatetime      int64      `json:"lastDatetime"`
	Type              string     `json:"type"`
	Tag               []string   `json:"tag"`
	EndTime           int64      `json:"endTime"`
	FileCode          string     `json:"fileCode"`
	ProcessType       string     `json:"processType"`
	UpdateCommand     string     `json:"updateCommand"`
	ActionCommandList []string   `json:"actionCommandList"`
	Crlf              int        `json:"crlf"`
	EventTask         EventTask  `json:"eventTask"`
	PingConfig        PingConfig `json:"pingConfig"`
}

// FillDefaults 补齐空字段的默认值。
func (c *InstanceConfig) FillDefaults() {
	if c.IE == "" {
		c.IE = "utf-8"
	}
	if c.OE == "" {
		c.OE = "utf-8"
	}
	if c.FileCode == "" {
		c.FileCode = "utf-8"
	}
	if c.Type == "" {
		c.Type = "universal"
	}
	if c.ProcessType == "" {
		c.ProcessType = "universal"
	}
	if c.StopCommand == "" {
		c.StopCommand = "stop"
	}
	if c.Crlf == 0 {
		c.Crlf = 2
	}
	if c.Tag == nil {
		c.Tag = []string{}
	}
	if c.ActionCommandList == nil {
		c.ActionCommandList = []string{}
	}
	if c.PingConfig.Port == 0 {
		c.PingConfig.Port = 25565
	}
	if c.EndTime == 0 {
		c.EndTime = time.Now().AddDate(0, 0, 365).UnixMilli()
	}
}

// PersistedInstance 持久化到磁盘的实例记录。
type PersistedInstance struct {
	InstanceUuid string         `json:"instanceUuid"`
	Config       InstanceConfig `json:"config"`
	Started      int            `json:"started"`
}

// Instance 运行时的实例对象（含进程管理状态）。
type Instance struct {
	mu sync.Mutex

	Config       InstanceConfig
	InstanceUuid string
	Started      int

	Status int        // 当前状态
	Proc   *Process   // 进程包装（可能为 nil）
	Stdin  *stdinPipe // 进程标准输入
	Busy   bool       // 忙碌标记（操作进行中）

	// 自动重启防抖（受 mu 保护）：10 秒窗口内最多自动重启 3 次，防止崩溃循环。
	arWindowStart time.Time
	arAttempts    int
}

// NewInstance 由配置创建实例对象。
func NewInstance(uuid string, cfg InstanceConfig) *Instance {
	cfg.FillDefaults()
	if uuid == "" {
		uuid = newUUID()
	}
	return &Instance{
		Config:       cfg,
		InstanceUuid: uuid,
		Status:       StatusStopped,
	}
}

// SetStatus 设置状态并更新时间戳。
func (i *Instance) SetStatus(s int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Status = s
	i.Config.LastDatetime = time.Now().UnixMilli()
}

// Detail 返回 MCSM 风格的 InstanceDetail JSON 结构。
func (i *Instance) Detail() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()

	procInfo := map[string]any{
		"cpu": 0, "memory": 0, "ppid": 0, "pid": 0, "ctime": 0, "elapsed": 0, "timestamp": 0,
	}
	space := int64(0)
	if i.Proc != nil {
		procInfo = i.Proc.Info()
		space = i.Proc.SpaceUsed()
	}
	return map[string]any{
		"config":       i.Config,
		"instanceUuid": i.InstanceUuid,
		"started":      i.Started,
		"status":       i.Status,
		"space":        space,
		"info": map[string]any{
			"currentPlayers": -1,
			"maxPlayers":     -1,
			"playersChart":   []any{},
			"version":        "",
			"fileLock":       0,
		},
		"processInfo": procInfo,
	}
}

// Daemon 守护进程根对象。
type Daemon struct {
	mu     sync.Mutex
	saveMu sync.Mutex // 持久化写盘互斥：保证并发 Save 不互相覆盖
	// 注意：saveMu 必须始终先于 mu 获取，避免锁顺序反转。

	DataDir     string
	APIKey      string
	PairingHash string
	Port        int
	BindHost    string // 实际监听主机（空 = 127.0.0.1），决定下载/上传直连地址
	UUID        string
	Instances   []*Instance
	StartedAt   time.Time

	LogDir      string      // 实例日志落盘目录（空 = 不落盘）
	LogMaxBytes int64       // 单实例日志文件轮转上限（字节）
	AuditLog    *fileLogger // 审计日志落盘器（nil = 未启用 -audit-log=false）

	jobs  *jobStore  // 容器长任务注册表（container.go）
	tasks *taskStore // 通用异步任务表（JDK 安装 / 核心下载 / 备份恢复等）

	trashMu sync.Mutex // 回收站元数据读写互斥（{data}/trash/<uuid>.json）

	// 集群协调状态（P2，见 docs/cluster-node-api.md），受 clusterMu 保护
	clusterMu        sync.Mutex
	clusterMonitor   string                  // 监控节点 id（空 = 尚无监控者）
	clusterRole      string                  // 自身角色：monitor | worker
	clusterPeers     []map[string]any        // 已登记的对等节点列表
	clusterEvents    []map[string]any        // 最近事件（保留 100 条）
	clusterHeartbeat map[string]any          // 最近一次心跳快照
	transfers        map[string]*transferJob // 节点间数据拉取任务

	// transferAllowLoopback 仅测试用：放行集群拉取访问环回地址。
	// 生产必须保持 false —— 集群拉取由节点间直传使用，目标应为对等节点
	// 的公网/LAN 地址；放行环回会让认证后的 /api/cluster/transfer 变成
	// 全功能 SSRF（自我回打 / 探测本机服务），审计报告 #5。
	transferAllowLoopback bool

	// transferAllowCIDR -transfer-allow-cidr 原始配置（逗号分隔 CIDR，如
	// "192.168.0.0/16,10.0.0.0/8"）：显式放行 RFC1918 内网地址的集群拉取
	// （集群 LAN 直传所需）。默认空 = 拒绝全部内网地址。
	// 解析结果存 transferAllowNets（启动时解析一次，之后只读）。
	transferAllowCIDR string
	transferAllowNets []*net.IPNet
}

// NewDaemon 创建守护进程实例。
func NewDaemon(dataDir, apiKey string) *Daemon {
	return &Daemon{
		DataDir:       dataDir,
		APIKey:        apiKey,
		Port:          12346,
		UUID:          newUUID(),
		Instances:     []*Instance{},
		StartedAt:     time.Now(),
		LogMaxBytes:   64 << 20, // 默认 64MB
		clusterRole:   "worker",
		clusterPeers:  []map[string]any{},
		clusterEvents: []map[string]any{},
		transfers:     map[string]*transferJob{},
		tasks:         newTaskStore(),
	}
}

// logConfig 返回实例日志落盘配置；LogDir 为空时返回 nil（不落盘）。
// 与本地 Rust logger 对齐：轮转保留 5 份（.1 … .5），每 7 天或超上限轮转。
func (d *Daemon) logConfig() *logConfig {
	if d.LogDir == "" {
		return nil
	}
	return &logConfig{
		dir:      d.LogDir,
		maxSize:  d.LogMaxBytes,
		keep:     5,
		interval: 7 * 24 * time.Hour,
	}
}

// instanceFile 实例配置持久化文件路径。
func (d *Daemon) instanceFile() string {
	return filepath.Join(d.DataDir, "instances.json")
}

// Load 从磁盘加载实例配置。
// 若 instances.json 损坏（崩溃中断写盘等），将损坏文件备份为
// instances.json.corrupt-<时间戳> 后按空列表继续启动，保证守护进程可用。
func (d *Daemon) Load() error {
	data, err := os.ReadFile(d.instanceFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []PersistedInstance
	if err := json.Unmarshal(data, &list); err != nil {
		backup := d.instanceFile() + ".corrupt-" + fmt.Sprintf("%d", time.Now().UnixMilli())
		if rerr := os.Rename(d.instanceFile(), backup); rerr != nil {
			return fmt.Errorf("解析 instances.json 失败: %w（备份损坏文件也失败: %v）", err, rerr)
		}
		alog.Printf("警告: instances.json 损坏（%v），已备份到 %s，按空列表继续启动", err, backup)
		return nil
	}
	d.mu.Lock()
	changed := false
	for _, p := range list {
		// 旧数据迁移：此前相对路径 cwd 被原样入库，同一实例的 cwd 会随
		// 节点启动目录漂移（systemd 下为 /）；加载时统一转绝对路径。
		// 空 cwd 保持不动——启动时会被明确拒绝并提示，而不是静默漂移。
		if cwd := strings.TrimSpace(p.Config.Cwd); cwd != "" {
			if abs, err := filepath.Abs(cwd); err == nil && abs != cwd {
				p.Config.Cwd = abs
				changed = true
			}
		}
		inst := NewInstance(p.InstanceUuid, p.Config)
		inst.Started = p.Started
		d.Instances = append(d.Instances, inst)
	}
	d.mu.Unlock()
	if changed {
		// Save 需先 saveMu 后 mu，因此必须先释放 mu 再调用
		if err := d.Save(); err != nil {
			alog.Printf("警告: cwd 规范化后写回 instances.json 失败: %v", err)
		}
	}
	return nil
}

// Save 将实例配置持久化到磁盘。
// 原子写：先写临时文件、fsync 落盘，再 rename，避免崩溃产生半截 JSON；
// saveMu 串行化保证并发 Save 不会互相覆盖。
func (d *Daemon) Save() error {
	d.saveMu.Lock()
	defer d.saveMu.Unlock()

	d.mu.Lock()
	list := make([]PersistedInstance, 0, len(d.Instances))
	for _, inst := range d.Instances {
		inst.mu.Lock()
		list = append(list, PersistedInstance{
			InstanceUuid: inst.InstanceUuid,
			Config:       inst.Config,
			Started:      inst.Started,
		})
		inst.mu.Unlock()
	}
	d.mu.Unlock()

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.instanceFile() + ".tmp"
	// 不用 os.WriteFile：必须 fsync 后再 rename，
	// 否则崩溃/掉电时可能只有 rename 生效而数据仍在页缓存中丢失。
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, d.instanceFile())
}

// Find 按 uuid 查找实例。
func (d *Daemon) Find(uuid string) *Instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, inst := range d.Instances {
		if inst.InstanceUuid == uuid {
			return inst
		}
	}
	return nil
}

// Add 添加实例并持久化。
func (d *Daemon) Add(inst *Instance) error {
	d.mu.Lock()
	d.Instances = append(d.Instances, inst)
	d.mu.Unlock()
	return d.Save()
}

// UpdateInstance 更新实例配置并持久化。
// 传入已解析的实例指针，避免「查找—使用」之间实例被并发删除。
func (d *Daemon) UpdateInstance(inst *Instance, cfg InstanceConfig) error {
	if inst == nil {
		return fmt.Errorf("实例不存在")
	}
	cfg.FillDefaults()
	inst.mu.Lock()
	// CreateDatetime 必须在锁内读取：与并发 Update/Save 竞争
	cfg.CreateDatetime = inst.Config.CreateDatetime
	cfg.LastDatetime = time.Now().UnixMilli()
	inst.Config = cfg
	inst.mu.Unlock()
	return d.Save()
}

// Remove 删除实例；deleteFiles 为 true 时同时删除工作目录。
func (d *Daemon) Remove(uuid string, deleteFiles bool) error {
	inst := d.Find(uuid)
	if inst == nil {
		return fmt.Errorf("实例不存在: %s", uuid)
	}
	// Proc/Status/Cwd 均须在锁内读取（AGENTS.md 约定）
	inst.mu.Lock()
	proc, status, cwd := inst.Proc, inst.Status, inst.Config.Cwd
	inst.mu.Unlock()
	if proc != nil && status != StatusStopped {
		if err := proc.Kill(); err != nil {
			return fmt.Errorf("终止进程失败: %w", err)
		}
	}
	d.mu.Lock()
	for i, x := range d.Instances {
		if x.InstanceUuid == uuid {
			d.Instances = append(d.Instances[:i], d.Instances[i+1:]...)
			break
		}
	}
	d.mu.Unlock()

	if deleteFiles && cwd != "" {
		_ = os.RemoveAll(cwd)
	}
	return d.Save()
}

// List 返回全部实例详情（按创建时间排序）。
func (d *Daemon) List() []map[string]any {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()

	// 排序键在锁内取出：排序回调里直接读 Config 与并发 Update 竞争
	type pair struct {
		inst *Instance
		ct   int64
	}
	pairs := make([]pair, 0, len(insts))
	for _, inst := range insts {
		inst.mu.Lock()
		pairs = append(pairs, pair{inst, inst.Config.CreateDatetime})
		inst.mu.Unlock()
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].ct < pairs[b].ct })

	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.inst.Detail())
	}
	return out
}

// CountRunning 统计运行中的实例数。
func (d *Daemon) CountRunning() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, inst := range d.Instances {
		inst.mu.Lock()
		if inst.Status == StatusRunning || inst.Status == StatusStarting {
			n++
		}
		inst.mu.Unlock()
	}
	return n
}

// CwdOf 返回实例的工作目录（文件管理器根目录）。
func (d *Daemon) CwdOf(uuid string) (string, error) {
	inst := d.Find(uuid)
	if inst == nil {
		return "", fmt.Errorf("实例不存在: %s", uuid)
	}
	inst.mu.Lock()
	cwd := inst.Config.Cwd
	inst.mu.Unlock()
	if cwd == "" {
		return "", fmt.Errorf("实例工作目录为空")
	}
	return cwd, nil
}

// newUUID 生成随机 UUID v4（crypto/rand，跨平台安全随机；
// 极低概率失败时回退到时间 + 线性同余，仅作兜底）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (i * 8))
		}
		seed := uint64(time.Now().UnixNano()) ^ uint64(os.Getpid())<<32
		for i := 8; i < 16; i++ {
			seed = seed*6364136223846793005 + 1442695040888963407
			b[i] = byte(seed >> 56)
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ToLower(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
