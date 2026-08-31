//go:build !openbsd

// restrictPrivileges 在非 OpenBSD 平台为空操作；此处验证其可正常调用且不会崩溃。
// （OpenBSD 的 pledge 调用会改变进程 syscall 能力，不适合在测试二进制中随意执行。）

package main

import "testing"

func TestRestrictPrivilegesNoop(t *testing.T) {
	// 非 OpenBSD 平台为空操作：对任意路径返回 nil，且不对进程施加限制。
	if err := restrictPrivileges(""); err != nil {
		t.Fatalf("空数据目录应为空操作并返回 nil: %v", err)
	}
	if err := restrictPrivileges("/tmp/irix-node-test-data"); err != nil {
		t.Fatalf("非空数据目录应为空操作并返回 nil: %v", err)
	}
}
