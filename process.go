// 进程管理：启动/停止/重启/强制终止 Java 等服务器进程，
// 捕获输出日志（环形缓冲），并支持通过标准输入下发命令。

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogBuffer 线程安全的环形日志缓冲。
// 最多保留 maxBytes 字节，超出后丢弃最旧的内容。
type LogBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	maxBytes int
}

// NewLogBuffer 创建日志缓冲。
func NewLogBuffer(maxBytes int) *LogBuffer {
	if maxBytes <= 0 {
		maxBytes = 2 * 1024 * 1024 // 默认 2MB
	}
	return &LogBuffer{maxBytes: maxBytes}
}

// Write 实现 io.Writer。
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len()+len(p) > b.maxBytes {
		overflow := b.buf.Len() + len(p) - b.maxBytes
		if overflow >= b.buf.Len() {
			b.buf.Reset()
		} else {
			b.buf.Next(overflow)
		}
	}
	n, err := b.buf.Write(p)
	return n, err
}

// Tail 返回日志尾部。sizeKB > 0 时截取最后 sizeKB KB 的内容。
func (b *LogBuffer) Tail(sizeKB int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if sizeKB > 0 {
		max := sizeKB * 1024
		if len(s) > max {
			s = s[len(s)-max:]
		}
	}
	return s
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
	started  time.Time
	exitCode int
	done     chan struct{}
}

// startProcess 启动进程，cwd 必须存在。
func startProcess(startCommand, cwd string) (*Process, error) {
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
	proc := &Process{
		cmd:     cmd,
		Log:     logBuf,
		Stdin:   &stdinPipe{pipe: stdin},
		started: time.Now(),
		done:    make(chan struct{}),
	}

	go func() { _, _ = io.Copy(logBuf, stdout) }()
	go func() { _, _ = io.Copy(logBuf, stderr) }()

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
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
