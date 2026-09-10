//go:build solaris || illumos || mips || mipsle || mips64 || mips64le || ppc64 || aix || dragonfly || plan9 || js || wasip1 || (netbsd && (386 || arm || arm64)) || (openbsd && (386 || arm || ppc64 || riscv64))

// 账户管理的 SQLite 装配桩（SQLite 驱动不可用的平台）。
// Go 的纯 Go SQLite 驱动 modernc.org/sqlite 未覆盖以下平台，无法编译；
// 这里 openSqlite 直接返回错误，强制改用 postgres / mysql
// （-accounts-driver postgres 并配置 -accounts-dsn）——
//   - solaris / illumos（全系）
//   - mips / mipsle / mips64 / mips64le（全系）
//   - ppc64（大端 PowerPC；小端 ppc64le 不受影响）
//   - aix（IBM AIX）
//   - dragonfly（DragonFly BSD）
//   - plan9（Plan 9 / Plan 9front）
//   - js / wasip1（WebAssembly）
//   - netbsd 除 amd64 外（386 / arm / arm64）
//   - openbsd 除 amd64 / arm64 外（386 / arm / ppc64 / riscv64）

package main

import "fmt"

// openSqlite 在 SQLite 驱动不可用的平台下返回错误，提示改用 postgres/mysql。
func openSqlite(dataDir, dsn string) (string, error) {
	return "", fmt.Errorf("当前平台不支持 SQLite 账户存储；请使用 -accounts-driver postgres 并配置 -accounts-dsn 连接串")
}
