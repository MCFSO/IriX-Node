// 守护进程核心：实例数据模型、持久化与实例注册表。

package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
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
	UUID        string
	Instances   []*Instance
	StartedAt   time.Time
}

// NewDaemon 创建守护进程实例。
func NewDaemon(dataDir, apiKey string) *Daemon {
	return &Daemon{
		DataDir:   dataDir,
		APIKey:    apiKey,
		Port:      12346,
		UUID:      newUUID(),
		Instances: []*Instance{},
		StartedAt: time.Now(),
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
		log.Printf("警告: instances.json 损坏（%v），已备份到 %s，按空列表继续启动", err, backup)
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range list {
		inst := NewInstance(p.InstanceUuid, p.Config)
		inst.Started = p.Started
		d.Instances = append(d.Instances, inst)
	}
	return nil
}

// Save 将实例配置持久化到磁盘。
// 原子写：先写临时文件再 rename，避免崩溃产生半截 JSON；
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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

// Update 更新实例配置并持久化。
func (d *Daemon) Update(uuid string, cfg InstanceConfig) (*Instance, error) {
	inst := d.Find(uuid)
	if inst == nil {
		return nil, fmt.Errorf("实例不存在: %s", uuid)
	}
	cfg.FillDefaults()
	cfg.CreateDatetime = inst.Config.CreateDatetime
	cfg.LastDatetime = time.Now().UnixMilli()
	inst.mu.Lock()
	inst.Config = cfg
	inst.mu.Unlock()
	if err := d.Save(); err != nil {
		return nil, err
	}
	return inst, nil
}

// Remove 删除实例；deleteFiles 为 true 时同时删除工作目录。
func (d *Daemon) Remove(uuid string, deleteFiles bool) error {
	inst := d.Find(uuid)
	if inst == nil {
		return fmt.Errorf("实例不存在: %s", uuid)
	}
	if inst.Proc != nil && inst.Status != StatusStopped {
		if err := inst.Proc.Kill(); err != nil {
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

	if deleteFiles && inst.Config.Cwd != "" {
		_ = os.RemoveAll(inst.Config.Cwd)
	}
	return d.Save()
}

// List 返回全部实例详情（按创建时间排序）。
func (d *Daemon) List() []map[string]any {
	d.mu.Lock()
	insts := make([]*Instance, len(d.Instances))
	copy(insts, d.Instances)
	d.mu.Unlock()

	sort.Slice(insts, func(a, b int) bool {
		return insts[a].Config.CreateDatetime < insts[b].Config.CreateDatetime
	})
	out := make([]map[string]any, 0, len(insts))
	for _, inst := range insts {
		out = append(out, inst.Detail())
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
	if inst.Config.Cwd == "" {
		return "", fmt.Errorf("实例工作目录为空")
	}
	return inst.Config.Cwd, nil
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
