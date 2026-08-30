//go:build solaris || illumos || mips || mipsle || mips64 || mips64le || (netbsd && (386 || arm || arm64)) || (openbsd && (386 || arm || ppc64 || riscv64))

// 账户管理的 SQLite 装配桩（SQLite 驱动不可用的平台）。
// Go 的纯 Go SQLite 驱动 modernc.org/sqlite 未覆盖以下平台，无法编译；
// 这里 openSqlite 直接返回错误，强制改用 postgres / mysql
// （-accounts-driver postgres 并配置 -accounts-dsn）——
//   - solaris / illumos（全系）
//   - mips / mipsle / mips64 / mips64le（全系）
//   - netbsd 除 amd64 外（386 / arm / arm64）
//   - openbsd 除 amd64 / arm64 外（386 / arm / ppc64 / riscv64）

package main

import "fmt"

// openSqlite 在 solaris/illumos 下不可用：返回错误提示改用 postgres/mysql。
func openSqlite(dataDir, dsn string) (string, error) {
	return "", fmt.Errorf("当前平台（solaris/illumos）不支持 SQLite 账户存储；请使用 -accounts-driver postgres 并配置 -accounts-dsn 连接串")
}
