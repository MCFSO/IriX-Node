//go:build js

// WebAssembly（js/wasm）平台：wasm 运行时（浏览器 / Node）不投递 Unix 信号，
// 信号注册为空操作；优雅关停依赖 wasm 宿主正常退出时的清理路径。

package main

import "os"

// setupSignalHandler 在 js/wasm 下为空操作（无信号机制可注册）。
func setupSignalHandler(signals chan os.Signal) {}
