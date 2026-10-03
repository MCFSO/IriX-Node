package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 运行时关停测试：验证配置刷盘和重复关停不会重复执行资源释放流程。
func TestNodeRuntimeShutdownFlushesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = 0
	inst := NewInstance("", InstanceConfig{Nickname: "shutdown", Cwd: dir})
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}

	rt := newNodeRuntime(d, &http.Server{}, nil)
	if err := rt.shutdown(context.Background()); err != nil {
		t.Fatalf("首次关停失败: %v", err)
	}
	if d.dirty.Load() {
		t.Fatal("关停后实例配置仍处于脏状态")
	}
	if _, err := os.Stat(d.instanceFile()); err != nil {
		t.Fatalf("关停后 instances.json 未落盘: %v", err)
	}
	restarted := NewDaemon(dir, "test-key")
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if restarted.Find(inst.InstanceUuid) == nil {
		t.Fatal("关停后重新加载丢失实例")
	}
	if err := d.FlushDirty(); err != nil {
		t.Fatalf("关停后再次刷盘不应失败: %v", err)
	}

	// sync.Once 保证第二次调用不会再次关闭数据库、停止进程或覆盖持久化文件。
	if err := rt.shutdown(context.Background()); err != nil {
		t.Fatalf("重复关停失败: %v", err)
	}
}

// lifecycleWait 用信号和超时限定测试等待，避免关停回归导致整套测试挂起。
func lifecycleWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("生命周期操作未在测试时限内完成")
	}
}

// lifecycleRuntime 使用真实监听器，允许通过 context 取消测试节点。
func lifecycleRuntime(t *testing.T, d *Daemon, server *http.Server) (*nodeRuntime, string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := newNodeRuntime(d, server, listener)
	done := make(chan error, 1)
	go func() {
		done <- rt.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		lifecycleWait(t, rt.shutdownDone)
	})
	return rt, "http://" + listener.Addr().String(), cancel, done
}

func lifecycleResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("节点运行时未退出")
		return nil
	}
}

// 已进入处理器的请求可以完成其状态修改；数据库必须保持可用直到它返回。
func TestNodeRuntimeDrainsRequestsBeforeClosingStores(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	d.SaveDebounce = time.Hour
	if err := d.initAccounts(accountsConfig{}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	handlerErr := make(chan error, 1)
	inst := NewInstance("", InstanceConfig{Nickname: "最后一个请求", Cwd: dir})
	server := &http.Server{Handler: d.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if err := d.accounts.db.Ping(); err != nil {
			handlerErr <- err
			return
		}
		handlerErr <- d.Add(inst)
		writeOK(w, "请求完成")
	}))}
	rt, baseURL, cancel, done := lifecycleRuntime(t, d, server)
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(baseURL)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		requestDone <- err
	}()
	lifecycleWait(t, entered)
	cancel()
	// 处理器仍持有在途登记，运行时不能提前完成关停。
	select {
	case <-rt.shutdownDone:
		t.Fatal("在途请求结束前关闭了存储")
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	if err := lifecycleResult(t, requestDone); err != nil {
		t.Fatalf("在途请求失败: %v", err)
	}
	if err := lifecycleResult(t, handlerErr); err != nil {
		t.Fatalf("请求结束前账户数据库已不可用: %v", err)
	}
	if err := lifecycleResult(t, done); err != nil {
		t.Fatal(err)
	}
	if err := d.accounts.db.Ping(); err == nil {
		t.Fatal("关停后账户数据库仍然打开")
	}
	restarted := NewDaemon(dir, "test-key")
	if err := restarted.Load(); err != nil || restarted.Find(inst.InstanceUuid) == nil {
		t.Fatalf("排空请求后产生的最后配置未保存: %v", err)
	}
}

type lifecycleFailedListener struct {
	err    error
	closes atomic.Int32
}

func (l *lifecycleFailedListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *lifecycleFailedListener) Addr() net.Addr            { return &net.TCPAddr{} }
func (l *lifecycleFailedListener) Close() error {
	if l.closes.Add(1) > 1 {
		return net.ErrClosed
	}
	return nil
}

// Accept 永久失败也必须清理已初始化资源，不能停在等待信号的路径上。
func TestNodeRuntimeCleansUpAfterServeFailure(t *testing.T) {
	d := NewDaemon(t.TempDir(), "test-key")
	if err := d.initAccounts(accountsConfig{}); err != nil {
		t.Fatal(err)
	}
	acceptErr := errors.New("模拟监听器失败")
	listener := &lifecycleFailedListener{err: acceptErr}
	rt := newNodeRuntime(d, d.newHTTPServer(""), listener)
	if err := rt.run(context.Background()); !errors.Is(err, acceptErr) {
		t.Fatalf("监听错误未保留: %v", err)
	}
	if d.accounts.db.Ping() == nil {
		t.Fatal("服务异常退出后数据库没有关闭")
	}
	if d.workers.start(func(context.Context) {}) {
		t.Fatal("服务异常退出后仍可登记周期任务")
	}
	closes := listener.closes.Load()
	if err := rt.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if listener.closes.Load() != closes {
		t.Fatal("重复关停再次执行了监听器清理")
	}
}

// 配置写入失败必须向上报告，同时继续关闭其余资源，并保留待重试标记。
func TestNodeRuntimeReportsPersistenceFailureAndContinuesCleanup(t *testing.T) {
	dir := t.TempDir()
	d := NewDaemon(dir, "test-key")
	if err := d.initAccounts(accountsConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Add(NewInstance("", InstanceConfig{Cwd: dir})); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d.instanceFile()+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	rt := newNodeRuntime(d, nil, nil)
	err := rt.shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "落盘失败") {
		t.Fatalf("关停吞掉了落盘错误: %v", err)
	}
	if !d.dirty.Load() {
		t.Fatal("写入失败后丢失了待重试标记")
	}
	if d.accounts.db.Ping() == nil {
		t.Fatal("落盘失败阻止了数据库关闭")
	}
	if got := rt.shutdown(context.Background()); got != err {
		t.Fatal("重复关停没有返回同一清理结果")
	}
}

// 即使实例未运行，劫持的 WebSocket 也要关闭，否则在途请求永远无法排空。
func TestNodeRuntimeClosesIdleConsoleAndRejectsNewWork(t *testing.T) {
	d := NewDaemon(t.TempDir(), "test-key")
	inst := NewInstance("", InstanceConfig{Cwd: d.DataDir})
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	server := d.newHTTPServer("")
	_, baseURL, cancel, done := lifecycleRuntime(t, d, server)
	client := dialTestWS(t, baseURL+"/api/instance/console/ws?uuid="+inst.InstanceUuid+"&apikey=test-key")
	defer client.conn.Close()
	cancel()
	if err := lifecycleResult(t, done); err != nil {
		t.Fatal(err)
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.br.ReadByte(); err == nil {
		t.Fatal("节点关停后控制台仍然可读")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("控制台没有随关停断开")
	}
	if err := d.startInstance(inst); err == nil || !strings.Contains(err.Error(), "关停") {
		t.Fatalf("节点关停后仍能启动实例: %v", err)
	}
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/overview?apikey=test-key", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("关停响应没有保留状态和 CORS: %d %v", w.Code, w.Header())
	}
}

// 网络排空超时时强制关闭连接，处理器看到取消后退出，再执行配置刷盘。
func TestNodeRuntimeForcesHTTPClosureAfterDrainTimeout(t *testing.T) {
	d := NewDaemon(t.TempDir(), "test-key")
	entered, canceled := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: d.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))}
	rt, baseURL, _, done := lifecycleRuntime(t, d, server)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(baseURL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	lifecycleWait(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("HTTP 排空超时没有返回上下文错误: %v", err)
	}
	lifecycleWait(t, canceled)
	if err := lifecycleResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("运行时丢失了关停错误: %v", err)
	}
}

func TestBackgroundWorkersWaitsForCanceledTasks(t *testing.T) {
	b := newBackgroundWorkers()
	canceled, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	b.start(func(ctx context.Context) {
		<-ctx.Done()
		close(canceled)
		<-release
	})
	stopped := make(chan struct{})
	go func() {
		b.stop()
		close(stopped)
	}()
	lifecycleWait(t, canceled)
	if b.start(func(context.Context) {}) {
		t.Fatal("开始取消后仍能启动周期任务")
	}
	select {
	case <-stopped:
		t.Fatal("任务未退出时后台管理器提前返回")
	default:
	}
	unblock()
	lifecycleWait(t, stopped)
	b.stop()
}

// 并发触发多个关停入口只会释放一次监听器，并共享同一个完成信号。
func TestNodeRuntimeConcurrentShutdown(t *testing.T) {
	d := NewDaemon(t.TempDir(), "test-key")
	listener := &lifecycleFailedListener{}
	rt := newNodeRuntime(d, nil, listener)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- rt.shutdown(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if listener.closes.Load() != 1 {
		t.Fatalf("监听器被重复清理 %d 次", listener.closes.Load())
	}
}
