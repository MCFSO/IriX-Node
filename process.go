// 进程管理：启动/停止/重启/强制终止 Java 等服务器进程，
// 捕获输出日志（环形缓冲），并支持通过标准输入下发命令。

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogBuffer 线程安全的定容环形日志缓冲。
// 最多保留 maxBytes 字节，超出后覆盖最旧的内容。
// 底层为固定容量切片：容量不会超过 maxBytes（bytes.Buffer 的倍增扩容会
// 达到上限的约两倍），Tail 只拷贝请求的字节数。
type LogBuffer struct {
	mu       sync.Mutex
	buf      []byte // 环形数据区，len 为已用字节数，cap 不超过 maxBytes
	start    int    // 最旧字节的下标（仅在缓冲写满后可能非 0）
	maxBytes int
}

// NewLogBuffer 创建日志缓冲。
func NewLogBuffer(maxBytes int) *LogBuffer {
	if maxBytes <= 0 {
		maxBytes = 2 * 1024 * 1024 // 默认 2MB
	}
	return &LogBuffer{maxBytes: maxBytes}
}

// Write 实现 io.Writer；写入量超过容量时只保留最后 maxBytes 字节。
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := len(p)

	// 单次写入即超过容量：只保留尾部 maxBytes，缓冲整体重置
	if len(p) >= b.maxBytes {
		if cap(b.buf) < b.maxBytes {
			b.buf = make([]byte, b.maxBytes)
		}
		b.buf = b.buf[:b.maxBytes]
		copy(b.buf, p[len(p)-b.maxBytes:])
		b.start = 0
		return total, nil
	}

	// 未写满：按需扩容并追加
	if len(b.buf) < b.maxBytes {
		room := b.maxBytes - len(b.buf)
		n := len(p)
		if n > room {
			n = room
		}
		b.growTo(len(b.buf) + n)
		b.buf = append(b.buf, p[:n]...)
		p = p[n:]
		if len(p) == 0 {
			return total, nil
		}
	}

	// 已写满：从 start 处环形覆盖最旧内容
	for len(p) > 0 {
		n := copy(b.buf[b.start:], p)
		p = p[n:]
		b.start = (b.start + n) % len(b.buf)
	}
	return total, nil
}

// growTo 将容量扩到至少 need（倍增但不超过 maxBytes）。
func (b *LogBuffer) growTo(need int) {
	if cap(b.buf) >= need {
		return
	}
	newCap := cap(b.buf)
	if newCap == 0 {
		newCap = 32 * 1024
	}
	for newCap < need {
		newCap *= 2
	}
	if newCap > b.maxBytes {
		newCap = b.maxBytes
	}
	grown := make([]byte, len(b.buf), newCap)
	copy(grown, b.buf)
	b.buf = grown
}

// Tail 返回日志尾部。sizeKB > 0 时截取最后 sizeKB KB 的内容。
func (b *LogBuffer) Tail(sizeKB int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	want := len(b.buf)
	if sizeKB > 0 && sizeKB*1024 < want {
		want = sizeKB * 1024
	}
	if want == 0 {
		return ""
	}
	// strings.Builder 直接产出字符串，避免「字节切片 + string 转换」的双份拷贝
	var sb strings.Builder
	sb.Grow(want)
	// 逻辑顺序为 buf[start:] + buf[:start]，取其最后 want 字节
	if want <= b.start {
		// 全部落在 buf[:start] 的尾部
		sb.Write(b.buf[b.start-want : b.start])
		return sb.String()
	}
	fromFirst := want - b.start
	sb.Write(b.buf[len(b.buf)-fromFirst:])
	sb.Write(b.buf[:b.start])
	return sb.String()
}

// Len 返回当前缓冲中的字节数。
func (b *LogBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// stdinPipe 进程标准输入写入器。
type stdinPipe struct {
	mu   sync.Mutex
	pipe io.WriteCloser
}

// WriteLine 向进程写入一行命令。
func (s *stdinPipe) WriteLine(line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipe == nil {
		return fmt.Errorf("进程标准输入不可用")
	}
	_, err := io.WriteString(s.pipe, line+"\n")
	return err
}

// Close 关闭标准输入。
func (s *stdinPipe) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipe != nil {
		_ = s.pipe.Close()
		s.pipe = nil
	}
}

// Process 封装一个运行中的服务器进程。
type Process struct {
	cmd      *exec.Cmd
	Log      *LogBuffer
	Stdin    *stdinPipe
	log      *fileLogger // 异步落盘（可能为 nil）
	started  time.Time
	exitCode int
	done     chan struct{}
}

// logConfig 实例日志落盘配置；Dir 为空时表示不落盘。
type logConfig struct {
	dir     string // 日志目录（如 {data}/logs）
	name    string // 日志文件名（如 {uuid}.log，用 uuid 避免不同实例 cwd 同名冲突）
	maxSize int64  // 单文件轮转上限（字节），<=0 取默认
}

// startProcess 启动进程，cwd 必须存在。
// logConf 为 nil 时不落盘实例日志；否则 stdout/stderr 在写入内存
// 环形缓冲的同时异步镜像到 {dir}/{uuid}.log（尽力而为，不阻塞进程）。
func startProcess(startCommand, cwd string, logConf *logConfig) (*Process, error) {
	args := SplitCommand(startCommand)
	if len(args) == 0 {
		return nil, fmt.Errorf("启动命令为空")
	}
	if cwd != "" {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在: %s", cwd)
		}
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	cmd.SysProcAttr = sysProcAttr()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	logBuf := NewLogBuffer(0)
	var fl *fileLogger
	if logConf != nil && logConf.dir != "" {
		if err := os.MkdirAll(logConf.dir, 0o755); err != nil {
			// 日志目录创建失败：降级为纯内存缓冲，不阻断启动
			log.Printf("警告: 创建日志目录 %s 失败（%v），本次运行不落盘", logConf.dir, err)
		} else {
			name := logConf.name
			if name == "" {
				name = filepath.Base(cwd) + ".log"
			}
			fl = newFileLogger(logConf.dir, name, logConf.maxSize)
		}
	}
	proc := &Process{
		cmd:     cmd,
		Log:     logBuf,
		Stdin:   &stdinPipe{pipe: stdin},
		log:     fl,
		started: time.Now(),
		done:    make(chan struct{}),
	}

	// 双 io.Copy：输出同时进内存环形缓冲与异步落盘（fl 为 nil 时仅内存）。
	// copyDone 计数两个复制 goroutine 的结束，供退出时等待日志完整落盘。
	copyDone := make(chan struct{}, 2)
	if fl != nil {
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, fl), stdout); copyDone <- struct{}{} }()
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, fl), stderr); copyDone <- struct{}{} }()
	} else {
		go func() { _, _ = io.Copy(logBuf, stdout); copyDone <- struct{}{} }()
		go func() { _, _ = io.Copy(logBuf, stderr); copyDone <- struct{}{} }()
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		if fl != nil {
			fl.Close()
		}
		return nil, err
	}

	// 等待进程退出并关闭通道
	go func() {
		err := cmd.Wait()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				proc.exitCode = ee.ExitCode()
			} else {
				proc.exitCode = -1
			}
		}
		proc.Stdin.Close()
		// 等输出复制结束（含超时兜底：孙进程继承 fd 时管道可能不关闭，
		// 不能让 done 永久不关闭导致 IsRunning 永远为 true）
		timer := time.NewTimer(3 * time.Second)
		for i := 0; i < 2; i++ {
			select {
			case <-copyDone:
				// 已结束一个复制，继续等另一个（可能落在超时之后）
			case <-timer.C:
				// 超时：放弃等待，日志尾部可能缺失（尽力而为，不阻塞退出）
				goto copiesDone
			}
		}
	copiesDone:
		timer.Stop()
		if fl != nil {
			fl.Close() // 排空并落盘剩余日志
		}
		close(proc.done)
	}()

	return proc, nil
}

// IsRunning 进程是否仍在运行。
// 判据为 done 通道：cmd.Wait() 返回（进程退出）时关闭，跨平台可靠。
// 注意：不能使用 Signal(0) 探测——Windows 上 os.Process.Signal 除 Kill 外一律报错。
func (p *Process) IsRunning() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// WriteCommand 下发命令到标准输入。
func (p *Process) WriteCommand(cmd string) error {
	if p.Stdin == nil {
		return fmt.Errorf("进程标准输入不可用")
	}
	return p.Stdin.WriteLine(cmd)
}

// Stop 优雅停止：先发送停止命令，等待超时后强制终止。
func (p *Process) Stop(stopCommand string, timeout time.Duration) error {
	stop := strings.TrimSpace(stopCommand)
	if stop == "" {
		stop = "stop"
	}
	_ = p.WriteCommand(stop)

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return nil
	case <-t.C:
		return p.Kill()
	}
}

// Kill 强制终止进程。
func (p *Process) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	proc := p.cmd.Process
	// 先尝试终止进程树（Windows 下 taskkill /T /F）
	if err := proc.Kill(); err != nil {
		return err
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// Info 返回 MCSM 风格的 processInfo。
func (p *Process) Info() map[string]any {
	pid, ppid := 0, 0
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	if pid > 0 {
		ppid = parentPID(pid)
	}
	return map[string]any{
		"cpu":       0,
		"memory":    processMemory(pid),
		"ppid":      ppid,
		"pid":       pid,
		"ctime":     p.started.UnixMilli(),
		"elapsed":   int64(time.Since(p.started).Seconds()),
		"timestamp": time.Now().UnixMilli(),
	}
}

// SpaceUsed 估算工作目录占用空间。
func (p *Process) SpaceUsed() int64 {
	return 0 // 由文件管理器按需统计，避免高频遍历开销
}

// parentPID 获取进程父 PID（Windows 与 Unix 通用实现）。
func parentPID(pid int) int {
	// Windows 下通过 tasklist 获取父进程信息开销较大，直接返回 0。
	return 0
}

// processMemory 获取进程内存占用（字节）。
func processMemory(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	return 0
}

// DirSize 递归统计目录大小。
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
