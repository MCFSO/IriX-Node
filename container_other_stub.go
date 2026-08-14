//go:build !linux && !freebsd

// 容器能力桩：非 Linux/FreeBSD 平台无 Docker/Bastille 运行时，探测返回不可用。

package main

import "runtime"

// containerRuntimeInfo 能力探测：不支持任何容器运行时。
func containerRuntimeInfo() (rt, platform, version string, ok bool) {
	return "", runtime.GOOS, "", false
}
