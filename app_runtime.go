package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// nodeRuntime 负责节点进程的运行时编排。
//
// 启动参数解析、依赖初始化和路由注册仍由 main 完成；完成组装后，
// nodeRuntime 接管 HTTP、周期任务与受管子进程的生命周期。这样关停顺序只
// 在一个地方定义，测试也可以在不发送操作系统信号的情况下验证关停语义。
type nodeRuntime struct {
	daemon   *Daemon
	server   *http.Server
	listener net.Listener
	loadTune bool

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

// newNodeRuntime 创建节点运行时。
// server 或 listener 可以为空，便于只测试后台资源的关停流程。
func newNodeRuntime(d *Daemon, server *http.Server, listener net.Listener) *nodeRuntime {
	return &nodeRuntime{
		daemon:       d,
		server:       server,
		listener:     listener,
		shutdownDone: make(chan struct{}),
	}
}

// run 等待外部取消或 HTTP 服务退出，并在两条路径上执行同样的清理。
// 监听器由组合根在 TLS 和连接层日志包装完成后传入。
func (r *nodeRuntime) run(ctx context.Context) error {
	if r.server == nil {
		return errors.Join(errors.New("HTTP 服务未初始化"), r.shutdown(context.Background()))
	}
	if r.listener == nil {
		return errors.Join(errors.New("HTTP 监听器未初始化"), r.shutdown(context.Background()))
	}
	if ctx.Err() != nil {
		return r.shutdown(context.Background())
	}
	if r.daemon != nil {
		r.daemon.StartAutoSave()
		r.daemon.workers.start(r.daemon.tasks.cleanupLoop)
		r.daemon.workers.start(tickets.cleanupLoop)
		if r.daemon.vault != nil && r.daemon.vault.enabled {
			r.daemon.workers.start(r.daemon.vault.janitor)
			r.daemon.workers.start(r.daemon.vault.store.flushLoop)
		}
		if r.loadTune {
			r.daemon.workers.start(tuner.loop)
		}
	}
	done := make(chan error, 1)
	go func() { done <- r.server.Serve(r.listener) }()
	if r.daemon != nil {
		r.daemon.autoStartInstances()
	}
	var serveErr error
	served := false
	select {
	case serveErr = <-done:
		served = true
	case <-ctx.Done():
	}
	// Serve 会先于在途请求返回；清理完成后 run 才返回，调用方不必
	// 另外维护 stopped 通道。HTTP 超时只限制网络排空，不丢弃资源清理。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	shutdownErr := r.shutdown(shutdownCtx)
	cancel()
	if !served {
		serveErr = <-done
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}

// shutdown 按依赖逆序执行优雅关停，并且可安全重复调用。
//
// 关停顺序：
//  1. 停止接收新 HTTP 请求并等待在途请求；
//  2. 停止配置异步落盘循环，刷入最后一次实例配置；
//  3. 停止实例与 FRP 子进程，避免留下孤儿进程；
//  4. 关闭账户数据库/Redis；
//  5. 刷入保险库索引。
//
// 所有清理错误合并返回，保证单个子系统故障不会阻止其它资源释放。
// ctx 只限制 HTTP 网络排空；被取消的处理器仍须结束后才能关闭数据库。
func (r *nodeRuntime) shutdown(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		defer close(r.shutdownDone)
		var errs []error

		if r.daemon != nil {
			r.daemon.httpRequests.seal()
			r.daemon.processStarts.seal()
			r.daemon.closeConsoles()
		}
		if r.server != nil {
			if err := r.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs = append(errs, fmt.Errorf("HTTP 关停失败: %w", err))
				// 超时后关闭活动连接，取消请求上下文并解除网络读写阻塞。
				if closeErr := r.server.Close(); closeErr != nil {
					errs = append(errs, fmt.Errorf("HTTP 连接关闭失败: %w", closeErr))
				}
			}
		}
		// Serve 可能在注册监听器前就返回错误；显式关闭一次可以避免这种
		// 启动失败路径遗留监听器。正常 Serve 路径下重复 Close 会返回
		// net.ErrClosed，按幂等语义忽略。
		if r.listener != nil {
			if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, fmt.Errorf("HTTP 监听器关闭失败: %w", err))
			}
		}

		if r.daemon == nil {
			r.shutdownErr = errors.Join(errs...)
			return
		}

		// Shutdown 不等待劫持连接，Close 也不保证处理器已返回；显式等待
		// 请求和已登记启动完成后，才能关闭数据库或执行最后一次写盘。
		r.daemon.httpRequests.wait()
		r.daemon.processStarts.wait()
		r.daemon.StopAutoSave()
		r.daemon.workers.stop()
		if err := r.daemon.FlushDirty(); err != nil {
			errs = append(errs, fmt.Errorf("实例配置关停前落盘失败: %w", err))
		}

		// stopAll 内部对每个实例使用独立的停止超时，并在超时后强杀。
		if err := r.daemon.stopAll(30 * time.Second); err != nil {
			errs = append(errs, err)
		}
		if err := r.daemon.frpStopAll(); err != nil {
			errs = append(errs, err)
		}
		r.daemon.processWatchers.Wait()
		if err := r.daemon.closeAccounts(); err != nil {
			errs = append(errs, err)
		}

		// 保险库只在已解锁时存在需要刷新的内存索引。
		if r.daemon.vault != nil && r.daemon.vault.enabled && r.daemon.vault.unlockedSafe() {
			if err := r.daemon.vault.store.flush(); err != nil {
				errs = append(errs, fmt.Errorf("保险库索引关停前落盘失败: %w", err))
			} else {
				r.daemon.auditLogf("保险库索引已落盘（关停）")
			}
		}

		r.shutdownErr = errors.Join(errs...)
	})
	<-r.shutdownDone
	return r.shutdownErr
}
