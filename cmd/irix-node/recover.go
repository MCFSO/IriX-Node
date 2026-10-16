// 后台 goroutine 的 panic 兜底。
//
// Go 运行时中，未被恢复的 panic 会立即终止**整个进程**，且无法被其他
// goroutine 捕获——对守护进程而言意味着全部受管实例随之中断。因此凡是
// 「生命周期不绑定到单个 HTTP 请求」的 goroutine（周期任务、长任务、
// 进程看门狗）都必须经 recoverLog / safeGo 兜底，把 panic 降级为一条日志。
//
// 请求处理器内的 panic 由 net/http 自身 recover（并断开该连接），无需在此处理。

package main

import "runtime/debug"

// recoverLog 兜底当前 goroutine 的 panic，配合 defer 使用：
//
//	defer recoverLog("实例看门狗")
//
// 必须放在后台 goroutine 函数体的第一行。
func recoverLog(name string) {
	if rec := recover(); rec != nil {
		alog.Printf("后台任务 %s 发生 panic（已恢复，进程继续运行）: %v\n%s", name, rec, debug.Stack())
	}
}

// safeGo 启动带 panic 兜底的后台 goroutine。
func safeGo(name string, fn func()) {
	go func() {
		defer recoverLog(name)
		fn()
	}()
}
