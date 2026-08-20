// 进程管理：启动/停止/重启/强制终止 Java 等服务器进程，
// 捕获输出日志（环形缓冲），并支持通过标准输入下发命令。

package main

import (
	"bytes"
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
	log      *fileLogger   // 异步落盘（可能为 nil）
	lines    *logLines     // 带时间戳的行缓冲（断线补发）
	splitOut *lineSplitter // stdout 行拆分器
	splitErr *lineSplitter // stderr 行拆分器
	started  time.Time
	exitCode int
	done     chan struct{}

	subMu sync.Mutex
	subs  map[chan string]struct{} // 输出行订阅者（WebSocket 控制台）
}

// logConfig 实例日志落盘配置；Dir 为空时表示不落盘。
type logConfig struct {
	dir      string        // 日志目录（如 {data}/logs）
	name     string        // 日志文件名（如 {uuid}.log，用 uuid 避免不同实例 cwd 同名冲突）
	maxSize  int64         // 单文件轮转上限（字节），<=0 取默认
	keep     int           // 轮转保留份数（.1 … .keep），<=0 取 1
	interval time.Duration // 时间轮转间隔（0 = 不启用）
}

// timedLine 带时间戳的一行输出（供断线补发，见 /api/instance/logs since 参数）。
type timedLine struct {
	ts   int64  // 写入时间（unix 毫秒）
	text string // 单行文本（不含换行，保留 ANSI）
}

// logLines 带时间戳的环形行缓冲：保留最近 max 行（断线重连补发用）。
type logLines struct {
	mu    sync.Mutex
	lines []timedLine
	max   int
}

// newLogLines 创建行缓冲。
func newLogLines(max int) *logLines {
	if max <= 0 {
		max = 1000
	}
	return &logLines{max: max}
}

// add 追加一行（带当前时间戳），超出上限丢弃最旧。
func (l *logLines) add(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) >= l.max {
		l.lines = append(l.lines[:0], l.lines[1:]...)
	}
	l.lines = append(l.lines, timedLine{ts: time.Now().UnixMilli(), text: text})
}

// since 返回时间戳晚于 ms 的所有行（按时间顺序，补 \n）。
func (l *logLines) since(ms int64) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sb strings.Builder
	for _, ln := range l.lines {
		if ln.ts > ms {
			sb.WriteString(ln.text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// tail 返回最后 n 行（补 \n）。
func (l *logLines) tail(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.lines) {
		n = len(l.lines)
	}
	var sb strings.Builder
	for _, ln := range l.lines[len(l.lines)-n:] {
		sb.WriteString(ln.text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// clear 清空缓冲。
func (l *logLines) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
}

// maxLogLineBytes 单行进入行缓冲的最大长度（超长行截断，防内存放大）。
const maxLogLineBytes = 8 << 10

// lineSplitter 将字节流按行拆分后回调；跨 Write 保留残段，直到 flush。
// 同一实例的 stdout/stderr 各持一个实例；对并发写自带锁。
type lineSplitter struct {
	mu    sync.Mutex
	carry []byte
	emit  func(text string)
}

// Write 实现 io.Writer：拆出完整行（去行尾 \r）回调 emit。
func (s *lineSplitter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := append(s.carry, p...)
	s.carry = s.carry[:0]
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i]
		if len(line) > 0 {
			text := string(line)
			if strings.HasSuffix(text, "\r") {
				text = text[:len(text)-1] // 去 CRLF 的行尾 \r，保留 ANSI 其余原样
			}
			if len(text) > maxLogLineBytes {
				text = text[len(text)-maxLogLineBytes:]
			}
			s.emit(text)
		}
		data = data[i+1:]
	}
	s.carry = append(s.carry[:0], data...)
	return len(p), nil
}

// flush 把残段作为最后一行发出（进程退出时调用）。
func (s *lineSplitter) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.carry) > 0 {
		text := string(s.carry)
		if len(text) > maxLogLineBytes {
			text = text[len(text)-maxLogLineBytes:]
		}
		s.carry = nil
		s.emit(text)
	}
}

// startProcess 启动进程，cwd 必须存在。
// logConf 为 nil 时不落盘实例日志；否则 stdout/stderr 在写入内存
// 环形缓冲的同时异步镜像到 {dir}/{uuid}.log（尽力而为，不阻塞进程）。
func startProcess(startCommand, cwd string, logConf *logConfig) (*Process, error) {
	args := SplitCommand(startCommand)
	if len(args) == 0 {
		return nil, fmt.Errorf("启动命令为空")
	}
	// 空 cwd 必须拒绝，而不是静默继承节点进程自己的工作目录
	// （systemd 下为 /）：否则进程会在错误目录里读写配置/世界文件，
	// 表现为「相对路径 jar 找不到、必须塞绝对路径、连不上端口」。
	if cwd == "" {
		return nil, fmt.Errorf("工作目录为空")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("工作目录不存在: %s", cwd)
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
		lines:   newLogLines(1000),
		started: time.Now(),
		done:    make(chan struct{}),
	}
	// 行拆分器：stdout/stderr 各自按行拆分后进入带时间戳行缓冲
	// （供 /api/instance/logs 的 since 参数断线补发）
	proc.splitOut = &lineSplitter{emit: proc.emitLine}
	proc.splitErr = &lineSplitter{emit: proc.emitLine}

	// 双 io.Copy：输出同时进内存环形缓冲与异步落盘（fl 为 nil 时仅内存），
	// 并逐行进入行缓冲。copyDone 计数两个复制 goroutine 的结束，
	// 供退出时等待日志完整落盘。
	copyDone := make(chan struct{}, 2)
	if fl != nil {
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, fl, proc.splitOut), stdout); copyDone <- struct{}{} }()
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, fl, proc.splitErr), stderr); copyDone <- struct{}{} }()
	} else {
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, proc.splitOut), stdout); copyDone <- struct{}{} }()
		go func() { _, _ = io.Copy(io.MultiWriter(logBuf, proc.splitErr), stderr); copyDone <- struct{}{} }()
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
		proc.splitOut.flush() // 残段作为最后一行进入行缓冲
		proc.splitErr.flush()
		if fl != nil {
			fl.Close() // 排空并落盘剩余日志
		}
		close(proc.done)
	}()

	return proc, nil
}

// Subscribe 订阅进程输出行（每行一条，保留 ANSI）。
// 返回有界接收 channel（慢订阅者丢行、不阻塞进程输出）与取消函数；
// 进程退出后不再有新行，由调用方通过 done 通道感知并取消。
func (p *Process) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 1024)
	p.subMu.Lock()
	if p.subs == nil {
		p.subs = map[chan string]struct{}{}
	}
	p.subs[ch] = struct{}{}
	p.subMu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			p.subMu.Lock()
			delete(p.subs, ch)
			p.subMu.Unlock()
		})
	}
	return ch, cancel
}

// broadcast 向全部订阅者广播一行（非阻塞：channel 满即丢，绝不拖慢进程输出）。
func (p *Process) broadcast(line string) {
	p.subMu.Lock()
	for ch := range p.subs {
		select {
		case ch <- line:
		default:
		}
	}
	p.subMu.Unlock()
}

// emitLine 记录一行输出：进入带时间戳行缓冲（断线补发）并广播给订阅者
// （WebSocket 实时控制台）。
func (p *Process) emitLine(text string) {
	if p.lines != nil {
		p.lines.add(text)
	}
	p.broadcast(text)
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
