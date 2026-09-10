//go:build !js

// 平台信号装配。js/wasm 无 Unix 信号机制，见 signals_js.go。

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// setupSignalHandler 注册平台信号处理：
//   - 忽略 SIGHUP：SSH/终端前台启动的节点在会话断开时不应被杀
//     （否则端口静默关闭，客户端表现为「网络错误」且服务器无任何日志）。
//     systemd/rc.d 等服务管理器不会发送 SIGHUP，忽略它不影响优雅关停。
//   - 优雅关停信号：os.Interrupt 与 SIGTERM 触发优雅关停流程。
func setupSignalHandler(signals chan os.Signal) {
	signal.Ignore(syscall.SIGHUP)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
}
