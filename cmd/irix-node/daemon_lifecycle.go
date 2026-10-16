package main

import (
	"context"
	"sync"
	"time"
)

// operationGate 把「禁止新操作」和「等待已登记操作」分开。
// 登记与封闭共享同一把锁，避免 WaitGroup 在 Wait 开始后从零增加。
type operationGate struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (g *operationGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *operationGate) leave() { g.wg.Done() }

func (g *operationGate) seal() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *operationGate) wait() { g.wg.Wait() }

// backgroundWorkers 统一管理可取消的周期任务。构造时不启动 goroutine，
// 运行时或首次请求登记任务；停止时先禁止登记，再取消并等待全部任务退出。
type backgroundWorkers struct {
	gate   operationGate
	ctx    context.Context
	cancel context.CancelFunc
}

func newBackgroundWorkers() *backgroundWorkers {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundWorkers{ctx: ctx, cancel: cancel}
}

func (b *backgroundWorkers) start(run func(context.Context)) bool {
	if !b.gate.enter() {
		return false
	}
	go func() {
		defer b.gate.leave()
		run(b.ctx)
	}()
	return true
}

func (b *backgroundWorkers) stop() {
	b.gate.seal()
	b.cancel()
	b.gate.wait()
}

// waitNextTick 在取消后立即返回，避免关停等待长达数分钟的采样/清理周期。
func waitNextTick(ctx context.Context, tick <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return ctx.Err() == nil
	}
}

// autoStartInstances 在运行时就绪后启动配置了自动启动的实例。
// 关停时 processStarts 禁止后续启动，并等待已经进入启动路径的操作完成。
func (d *Daemon) autoStartInstances() {
	if d.vault != nil && d.vault.enabled {
		alog.Printf("保险库已启用：跳过实例自动启动（解锁后才能启动实例）")
		return
	}
	d.mu.Lock()
	instances := append([]*Instance(nil), d.Instances...)
	d.mu.Unlock()
	for _, inst := range instances {
		inst.mu.Lock()
		autoStart := inst.Config.EventTask.AutoStart
		inst.mu.Unlock()
		if autoStart {
			go func(inst *Instance) {
				if err := d.startInstance(inst); err != nil {
					alog.Printf("自动启动实例 %s 失败: %v", inst.InstanceUuid, err)
				}
			}(inst)
		}
	}
}
