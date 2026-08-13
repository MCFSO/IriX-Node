// 异步日志与实例日志落盘测试。

package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoCommand 返回一个长驻且有持续输出的进程命令（按平台）。
func echoCommand() string {
	if runtime.GOOS == "windows" {
		return "ping 127.0.0.1 -t"
	}
	return "sh -c 'while true; do echo heartbeat; sleep 0.2; done'"
}

// TestFileLoggerWritesToDisk 落盘内容与写入顺序一致。
func TestFileLoggerWritesToDisk(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "test.log", 1<<20)
	fl.Write([]byte("第一行\n"))
	fl.Write([]byte("第二行\n"))
	fl.Write([]byte("第三行\n"))
	fl.Close()
	data, err := os.ReadFile(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if got := string(data); got != "第一行\n第二行\n第三行\n" {
		t.Fatalf("日志内容不一致: %q", got)
	}
	t.Logf("[验证] 日志已落盘且顺序正确（%d 字节）", len(data))
}

// TestFileLoggerRotation 超过上限时轮转为 .1，新日志写入新文件。
func TestFileLoggerRotation(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "rot.log", 64) // 64 字节即触发轮转
	for i := 0; i < 10; i++ {
		fl.Write([]byte(strings.Repeat("x", 32)))
	}
	fl.Close()
	oldInfo, err1 := os.Stat(filepath.Join(dir, "rot.log.1"))
	curInfo, err2 := os.Stat(filepath.Join(dir, "rot.log"))
	if err1 != nil || err2 != nil {
		t.Fatalf("轮转文件缺失: .1=%v 当前=%v", err1, err2)
	}
	if curInfo.Size() > 64 || curInfo.Size() == 0 {
		t.Fatalf("当前文件大小异常: %d（应在 1~64 之间）", curInfo.Size())
	}
	if oldInfo.Size() < 32 || oldInfo.Size() > 64 {
		t.Fatalf("轮转历史文件大小异常: %d", oldInfo.Size())
	}
	t.Logf("[验证] 超过上限后轮转为 .1（旧=%d 字节，当前=%d 字节）", oldInfo.Size(), curInfo.Size())
}

// TestFileLoggerDropNoBlock 落盘追不上时丢弃数据，但 Write 绝不阻塞。
func TestFileLoggerDropNoBlock(t *testing.T) {
	dir := t.TempDir()
	fl := newFileLogger(dir, "drop.log", 1<<20)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 10000; j++ {
				fl.Write([]byte("xxxxx"))
			}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Write 被阻塞（磁盘追不上时不得阻塞调用方）")
	}
	fl.Close()
	if n := fl.drop.Load(); n <= 0 {
		t.Fatalf("预期发生丢弃计数，实际 %d", n)
	}
	t.Logf("[验证] 队列满时 Write 立即返回，丢弃 %d 字节", fl.drop.Load())
}

// TestStartProcessLogsToDisk 端到端：实例启动后 stdout 异步落盘到 {data}/logs/{uuid}.log。
func TestStartProcessLogsToDisk(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.LogDir = filepath.Join(dir, "logs")
	inst := NewInstance("log-uuid", InstanceConfig{
		Nickname:     "日志落盘",
		StartCommand: echoCommand(),
		Cwd:          dir,
	})
	d.Instances = append(d.Instances, inst)
	if err := d.startInstance(inst); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	logPath := filepath.Join(dir, "logs", "log-uuid.log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// 文件存在即代表输出流已建立（内容在 bufio 缓冲，flush 后落盘）
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 停止进程；done 通道保证退出前日志已 flush 落盘
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()
	if proc == nil || !proc.IsRunning() {
		t.Fatalf("实例未进入运行态")
	}
	_ = proc.Kill()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.IsRunning() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if proc.IsRunning() {
		t.Fatalf("进程未在超时内退出")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("日志文件为空（长驻输出进程应有日志）")
	}
	t.Logf("[验证] 实例日志端到端落盘成功（%d 字节）", len(data))
}

// TestAsyncLoggerDropNoBlock 全局异步日志器缓冲满时丢弃且不阻塞调用方。
func TestAsyncLoggerDropNoBlock(t *testing.T) {
	oldOut := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(oldOut)

	a := newAsyncLogger(64)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			a.Printf("日志 %d", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Printf 被阻塞（日志风暴不得阻塞调用方）")
	}
	a.Close()
	if n := a.drop.Load(); n <= 0 {
		t.Fatalf("预期缓冲满触发丢弃，实际 %d", n)
	}
	t.Logf("[验证] 缓冲满时 Printf 立即返回，丢弃 %d 条", a.drop.Load())
}
