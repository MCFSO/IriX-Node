// 异步日志：全局日志异步化 + 实例日志异步落盘。
//
// 两个层面：
//  1. 全局异步日志器 alog —— 所有 log.Printf 调用改为 alog.Printf，
//     写 stderr 由后台单 goroutine 完成，请求路径不再被日志 I/O 阻塞；
//     缓冲满时丢弃并计数（日志风暴不拖垮业务）。
//  2. 实例日志异步落盘 fileLogger —— 实例 stdout/stderr 在写入内存
//     环形缓冲（LogBuffer，供 outputlog API 读取）的同时镜像到
//     {data}/logs/{uuid}.log，按大小轮转。落盘是「尽力而为」：
//     Write 永不阻塞、永不返回错误，磁盘追不上时丢弃并计数，
//     保证磁盘慢绝不会反过来阻塞游戏服务器进程的 stdout 管道。

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// asyncLogger 全局异步日志器：Printf 入队，后台单 goroutine 顺序写 stderr。
// 缓冲有界且满时丢弃，保证高并发下日志写入绝不阻塞请求路径。
type asyncLogger struct {
	ch   chan string
	done chan struct{}
	drop atomic.Int64 // 缓冲满丢弃的条数
}

// newAsyncLogger 创建异步日志器。
func newAsyncLogger(size int) *asyncLogger {
	a := &asyncLogger{
		ch:   make(chan string, size),
		done: make(chan struct{}),
	}
	go a.run()
	return a
}

// Printf 异步记录日志；缓冲满时丢弃本条（仅计数，不阻塞调用方）。
func (a *asyncLogger) Printf(format string, args ...any) {
	select {
	case a.ch <- fmt.Sprintf(format, args...):
	default:
		a.drop.Add(1)
	}
}

// run 后台写日志主循环。
func (a *asyncLogger) run() {
	for s := range a.ch {
		log.Print(s)
	}
	close(a.done)
}

// Close 停止接收新日志，等待缓冲内日志全部写出后返回。
// 调用后不得再 Printf。
func (a *asyncLogger) Close() {
	close(a.ch)
	<-a.done
	if n := a.drop.Load(); n > 0 {
		log.Printf("警告: 异步日志缓冲已满，丢弃 %d 条日志", n)
	}
}

// alog 全局异步日志器（main 初始化后使用）。
var alog = newAsyncLogger(1024)

// fileLogger 实例日志异步落盘器。
//
// Write 实现 io.Writer：立即拷贝数据并投递到有界队列，满则丢弃（计数），
// 永远立即返回成功——进程 stdout 管道背压（io.Copy 阻塞）会阻塞游戏
// 服务器进程本身，落盘必须做到「追不上就丢」，绝不让磁盘拖住进程。
// 后台单 goroutine 消费队列，缓冲写入 {path}，超过 maxSize 或超过
// interval 时间轮转为 {path}.1 … {path}.{keep}（最多保留 keep 份）。
type fileLogger struct {
	ch   chan []byte // 待落盘队列（有界）
	stop chan struct{}
	done chan struct{}
	clr  chan chan error // 清空指令（响应通道；消费 goroutine 处理）

	path     string        // 当前日志文件路径
	maxSize  int64         // 单文件轮转上限（字节）
	keep     int           // 轮转保留份数（.1 … .keep），默认 1
	interval time.Duration // 时间轮转间隔（0 = 不启用）
	// archiveDir 非空时，每次轮转把即将被覆盖的 .1 归档到该目录
	// （<基名>-<时间戳>.log）。审计日志启用此机制（等保二级「审计记录
	// 保护与定期备份」，docs/vault-design.md §11）；实例日志不启用。
	archiveDir string

	// 以下字段仅由消费 goroutine 访问
	file   *os.File
	buf    *bufio.Writer
	size   int64
	opened time.Time  // 当前文件打开时间（时间轮转依据）
	lastOp *time.Time // 上次打开/写入失败的冷却时间

	closeOnce sync.Once
	drop      atomic.Int64 // 丢弃字节数
}

// newFileLogger 创建落盘器并启动消费 goroutine。
// dir 为日志目录，name 为文件名（如 {uuid}.log）；maxSize<=0 时取默认 64MB。
// 轮转保留 1 份（兼容既有调用；实例日志请用 newFileLoggerN）。
func newFileLogger(dir, name string, maxSize int64) *fileLogger {
	return newFileLoggerN(dir, name, maxSize, 1, 0)
}

// newFileLoggerN 创建落盘器：keep 为轮转保留份数（.1 … .keep，≤0 取 1），
// interval 为时间轮转间隔（0 表示仅按大小轮转）。
func newFileLoggerN(dir, name string, maxSize int64, keep int, interval time.Duration) *fileLogger {
	if maxSize <= 0 {
		maxSize = 64 << 20
	}
	if keep <= 0 {
		keep = 1
	}
	f := &fileLogger{
		ch:       make(chan []byte, 512),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		clr:      make(chan chan error),
		path:     filepath.Join(dir, name),
		maxSize:  maxSize,
		keep:     keep,
		interval: interval,
	}
	go f.run()
	return f
}

// Write 将数据投递到落盘队列（拷贝副本，调用方可复用缓冲区）。
// 永不阻塞、永不返回错误；队列满时丢弃并计数。
func (f *fileLogger) Write(p []byte) (int, error) {
	q := make([]byte, len(p))
	copy(q, p)
	select {
	case f.ch <- q:
	default:
		f.drop.Add(int64(len(p)))
	}
	return len(p), nil
}

// Close 停止投递，排空队列并落盘后返回。
// 与 Write 无竞态：不关闭 ch，仅通过 stop 通知消费 goroutine 退出。
func (f *fileLogger) Close() {
	f.closeOnce.Do(func() {
		close(f.stop)
		<-f.done
		if n := f.drop.Load(); n > 0 {
			log.Printf("警告: 实例日志 %s 落盘追不上输出，丢弃 %d 字节", f.path, n)
		}
	})
}

// run 消费主循环：正常消费逐段写盘；收到 stop 后排空剩余队列再 flush 关闭；
// 收到 clear 指令时清空当前文件与全部轮转文件。
func (f *fileLogger) run() {
	defer close(f.done)
	for {
		select {
		case p := <-f.ch:
			f.writeAll(p)
		case resp := <-f.clr:
			f.clearWithDrain(resp)
		case <-f.stop:
			// 排空剩余队列后落盘退出（磁盘慢时逐段处理，内存恒定）
			for {
				select {
				case p := <-f.ch:
					f.writeAll(p)
				case resp := <-f.clr:
					f.doClear(resp)
				default:
					f.flushClose()
					return
				}
			}
		}
	}
}

// writeAll 将一段数据写入文件（含轮转、失败降级与冷却重试）。
func (f *fileLogger) writeAll(p []byte) {
	if f.file == nil {
		if f.lastOp != nil && time.Since(*f.lastOp) < 5*time.Second {
			// 冷却期内丢弃，避免磁盘故障时反复系统调用
			f.drop.Add(int64(len(p)))
			return
		}
		if err := f.open(); err != nil {
			now := time.Now()
			f.lastOp = &now
			f.drop.Add(int64(len(p)))
			return
		}
	}
	// 大小轮转（与时间轮转互斥，一次写段至多轮转一次）
	if f.size+int64(len(p)) > f.maxSize && f.size > 0 {
		if err := f.rotate(); err != nil {
			now := time.Now()
			f.lastOp = &now
			f.drop.Add(int64(len(p)))
			return
		}
	}
	// 时间轮转：文件打开超过 interval（如 7 天）且已有内容时强制轮转，
	// 与本地 Rust logger 的「每 7 天轮转」对齐
	if f.interval > 0 && f.size > 0 && time.Since(f.opened) > f.interval {
		if err := f.rotate(); err != nil {
			now := time.Now()
			f.lastOp = &now
			f.drop.Add(int64(len(p)))
			return
		}
	}
	if _, err := f.buf.Write(p); err != nil {
		// 写失败（磁盘满/权限变更等）：关闭文件，冷却后重试
		_ = f.file.Close()
		f.file, f.buf = nil, nil
		now := time.Now()
		f.lastOp = &now
		f.drop.Add(int64(len(p)))
		return
	}
	f.size += int64(len(p))
}

// open 打开日志文件（追加模式）。
func (f *fileLogger) open() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	f.file = file
	f.buf = bufio.NewWriterSize(file, 16<<10)
	f.size = 0
	f.opened = time.Now()
	f.lastOp = nil
	return nil
}

// rotate 轮转：当前文件落盘后沿 .1 → .2 → … → .keep 依次后移
// （最旧的 .keep 直接删除），当前文件变为新的 .1，再新建当前文件。
// 轮转前先把即将被覆盖的 .1 归档（archiveDir 非空时，见 archiveRotated）。
func (f *fileLogger) rotate() error {
	if f.buf != nil {
		_ = f.buf.Flush()
	}
	_ = f.file.Close()
	f.file, f.buf = nil, nil
	if f.archiveDir != "" {
		f.archiveRotated()
	}
	if f.keep > 1 {
		_ = os.Remove(f.path + fmt.Sprintf(".%d", f.keep))
		for i := f.keep - 1; i >= 1; i-- {
			old := f.path + fmt.Sprintf(".%d", i)
			if _, err := os.Stat(old); err == nil {
				if err := os.Rename(old, f.path+fmt.Sprintf(".%d", i+1)); err != nil {
					_ = os.Remove(old)
				}
			}
		}
	}
	_ = os.Remove(f.path + ".1")
	if err := os.Rename(f.path, f.path+".1"); err != nil && !os.IsNotExist(err) {
		// rename 失败（如被占用）：直接丢弃旧文件重新开始
		_ = os.Remove(f.path)
	}
	return f.open()
}

// archiveRotated 把即将被 .1 覆盖的旧轮转文件复制到归档目录
// （{archiveDir}/{基名}-{时间戳}.log）。best-effort：失败仅告警，不阻断轮转。
// 消费 goroutine 内调用（rotate 路径）。
func (f *fileLogger) archiveRotated() {
	src := f.path + ".1"
	if _, err := os.Stat(src); err != nil {
		return // 尚无轮转文件可归档
	}
	dir := f.archiveDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		alog.Printf("警告: 日志归档目录创建失败 %s: %v", dir, err)
		return
	}
	base := strings.TrimSuffix(filepath.Base(f.path), filepath.Ext(f.path))
	// 毫秒精度：测试与高频轮转场景下同一秒内多次轮转不互相覆盖
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.log", base, time.Now().Format("20060102-150405.000")))
	if err := copyFile(src, dst); err != nil {
		alog.Printf("警告: 日志归档 %s 失败: %v", dst, err)
	}
}

// Clear 清空日志：删除当前文件与全部轮转文件（由消费 goroutine 执行，
// 避免与写文件句柄冲突；下次写入时自动重建空文件）。
// 已关闭的日志器返回错误。
func (f *fileLogger) Clear() error {
	resp := make(chan error, 1)
	select {
	case f.clr <- resp:
		select {
		case err := <-resp:
			return err
		case <-time.After(5 * time.Second):
			return fmt.Errorf("清空日志超时")
		}
	case <-f.stop:
		return fmt.Errorf("日志器已关闭")
	}
}

// clearWithDrain 执行清空：先排空当前队列（Clear 调用前已投递的数据一并
// 清除——select 无顺序保证，不排空则可能 Clear 先执行、旧数据后落盘），
// 再删除当前与轮转文件（消费 goroutine 内调用）。
func (f *fileLogger) clearWithDrain(resp chan<- error) {
	for {
		select {
		case p := <-f.ch:
			f.writeAll(p)
		default:
			f.doClear(resp)
			return
		}
	}
}

// doClear 执行清空（消费 goroutine 内调用）。
func (f *fileLogger) doClear(resp chan<- error) {
	if f.buf != nil {
		_ = f.buf.Flush()
	}
	if f.file != nil {
		_ = f.file.Close()
		f.file, f.buf = nil, nil
	}
	f.size = 0
	err := os.Remove(f.path)
	if os.IsNotExist(err) {
		err = nil // 文件本就不存在视为清空成功
	}
	for i := 1; i <= f.keep; i++ {
		_ = os.Remove(f.path + fmt.Sprintf(".%d", i))
	}
	resp <- err
}

// flushClose 排空写缓冲并关闭文件（消费 goroutine 退出前调用）。
func (f *fileLogger) flushClose() {
	if f.buf != nil {
		_ = f.buf.Flush()
	}
	if f.file != nil {
		_ = f.file.Close()
	}
}
