//go:build solaris || illumos

// 账户管理的 SQLite 装配桩（solaris/illumos 平台）。
// Go 的纯 Go SQLite 驱动 modernc.org/sqlite 未覆盖 solaris/illumos，无法编译；
// 这两个平台不支持 SQLite 账户存储，openSqlite 直接返回错误，强制改用
// postgres / mysql（-accounts-driver postgres 并配置 -accounts-dsn）。

package main

import "fmt"

// openSqlite 在 solaris/illumos 下不可用：返回错误提示改用 postgres/mysql。
func openSqlite(dataDir, dsn string) (string, error) {
	return "", fmt.Errorf("当前平台（solaris/illumos）不支持 SQLite 账户存储；请使用 -accounts-driver postgres 并配置 -accounts-dsn 连接串")
}
