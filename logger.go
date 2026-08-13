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
// 后台单 goroutine 消费队列，缓冲写入 {path}，超过 maxSize 轮转为 {path}.1。
type fileLogger struct {
	ch   chan []byte // 待落盘队列（有界）
	stop chan struct{}
	done chan struct{}

	path    string // 当前日志文件路径
	maxSize int64  // 单文件轮转上限（字节）

	// 以下字段仅由消费 goroutine 访问
	file   *os.File
	buf    *bufio.Writer
	size   int64
	lastOp *time.Time // 上次打开/写入失败的冷却时间

	closeOnce sync.Once
	drop      atomic.Int64 // 丢弃字节数
}

// newFileLogger 创建落盘器并启动消费 goroutine。
// dir 为日志目录，name 为文件名（如 {uuid}.log）；maxSize<=0 时取默认 64MB。
func newFileLogger(dir, name string, maxSize int64) *fileLogger {
	if maxSize <= 0 {
		maxSize = 64 << 20
	}
	f := &fileLogger{
		ch:      make(chan []byte, 512),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		path:    filepath.Join(dir, name),
		maxSize: maxSize,
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

// run 消费主循环：正常消费逐段写盘；收到 stop 后排空剩余队列再 flush 关闭。
func (f *fileLogger) run() {
	defer close(f.done)
	for {
		select {
		case p := <-f.ch:
			f.writeAll(p)
		case <-f.stop:
			// 排空剩余队列后落盘退出（磁盘慢时逐段处理，内存恒定）
			for {
				select {
				case p := <-f.ch:
					f.writeAll(p)
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
	if f.size+int64(len(p)) > f.maxSize && f.size > 0 {
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
	f.lastOp = nil
	return nil
}

// rotate 轮转：当前文件落盘后改为 {path}.1（旧的 .1 直接删除），新建当前文件。
func (f *fileLogger) rotate() error {
	if f.buf != nil {
		_ = f.buf.Flush()
	}
	_ = f.file.Close()
	f.file, f.buf = nil, nil
	_ = os.Remove(f.path + ".1")
	if err := os.Rename(f.path, f.path+".1"); err != nil && !os.IsNotExist(err) {
		// rename 失败（如被占用）：直接丢弃旧文件重新开始
		_ = os.Remove(f.path)
	}
	return f.open()
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
