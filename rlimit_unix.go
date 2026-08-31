//go:build !windows && !openbsd

// Unix（Linux/FreeBSD/NetBSD/Darwin 等）平台：启动时把进程可打开文件数
// （RLIMIT_NOFILE）软上限提升到硬上限。
//
// 动机：高并发（百万级连接）压测下，若节点自身文件描述符软上限过低
// （系统常见默认 1024），每个 TCP 连接占用一个 fd，很快耗尽——
// Accept 返回 EMFILE，客户端表现为大量 connection refused，成功率断崖式下跌。
// 在监听端口之前把软上限拉到硬上限，是接受百万并发连接的必要前提
// （硬上限本身可由部署侧 ulimit -Hn / systemd LimitNOFILE 调高）。
//
// 提升失败不致命：仅告警并沿用当前限制，交由部署侧处理。

package main

import (
	"fmt"
	"syscall"
)

// raiseFDLimit 把 RLIMIT_NOFILE 软上限提升到硬上限，返回提升后的软上限。
// 无可提升空间或失败时返回当前软上限（err 描述原因）。
func raiseFDLimit() (soft uint64, err error) {
	var rl syscall.Rlimit
	if e := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); e != nil {
		return 0, fmt.Errorf("读取 RLIMIT_NOFILE 失败: %w", e)
	}
	cur, max := rl.Cur, rl.Max
	if cur >= max {
		// 已达硬上限，无需也无权再提升。
		return cur, nil
	}
	// 软上限直接对齐硬上限；先尝试一个较大值，避免 1M 连接直接吃掉全部余量时仍不足。
	target := max
	if e := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: target, Max: max}); e != nil {
		// 个别内核/容器对 Setrlimit 有额外约束，回退到硬上限的 90% 再试一次。
		fallback := max - (max / 10)
		if fallback < cur {
			return cur, fmt.Errorf("提升 RLIMIT_NOFILE 失败（当前 %d，硬上限 %d）: %w", cur, max, e)
		}
		if e2 := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: fallback, Max: max}); e2 != nil {
			return cur, fmt.Errorf("提升 RLIMIT_NOFILE 失败（当前 %d，硬上限 %d）: %w", cur, max, e)
		}
		target = fallback
	}
	// 确认生效（部分平台 Getrlimit 返回的是旧值缓存，重新读一次）。
	if e := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); e == nil {
		cur = rl.Cur
	}
	return cur, nil
}
