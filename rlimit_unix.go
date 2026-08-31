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
//
// 注意跨平台类型差异：Linux 的 syscall.Rlimit.Cur/Max 为 uint64，而
// FreeBSD/Darwin/NetBSD 等为 int64。因此内部一律直接操作结构体字段
// （rl.Cur = rl.Max 为同类型赋值，任何平台都能编译），仅在返回边界处
// 统一转换为 uint64（int64→uint64 折回转换对非负值安全且合法）。
func raiseFDLimit() (soft uint64, err error) {
	var rl syscall.Rlimit
	if e := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); e != nil {
		return 0, fmt.Errorf("读取 RLIMIT_NOFILE 失败: %w", e)
	}
	if rl.Cur >= rl.Max {
		// 已达硬上限，无需也无权再提升。
		return uint64(rl.Cur), nil
	}
	// 软上限对齐硬上限（Cur ≤ Max 无需特权，恒可设置）。
	// 直接赋值结构体字段，避免 uint64 字面量在 int64 字段平台上的类型不匹配。
	rl.Cur = rl.Max
	if e := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); e != nil {
		return uint64(rl.Cur), fmt.Errorf("提升 RLIMIT_NOFILE 失败（当前 %d，硬上限 %d）: %w",
			uint64(rl.Cur), uint64(rl.Max), e)
	}
	// 确认生效（部分平台 Getrlimit 返回的是旧值缓存，重新读一次）。
	if e := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); e == nil {
		return uint64(rl.Cur), nil
	}
	return uint64(rl.Cur), nil
}
