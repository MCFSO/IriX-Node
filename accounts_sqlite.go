//go:build !solaris && !illumos && !mips && !mipsle && !mips64 && !mips64le

// 账户管理的 SQLite 驱动装配（非 solaris/illumos/mips 系平台）。
// 用 build tag 把 modernc.org/sqlite 隔离在本文件：Solaris/illumos 下 Go 的
// 纯 Go SQLite 驱动（modernc.org/sqlite）未覆盖该平台，无法编译；MIPS 全系
// （mips/mipsle/mips64/mips64le）同样未被驱动覆盖。这些平台走
// accounts_nosqlite.go，强制改用 postgres/mysql。

package main

import (
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // SQLite 驱动（纯 Go，CGO_ENABLED=0 可交叉编译）
)

// sqliteDSN 构造 SQLite 连接串（WAL + busy_timeout，多连接并发读写友好）。
// Windows 盘符路径需转成 file:///C:/… 形式（file:C:/… 会被当作 URI authority）。
func sqliteDSN(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := &url.URL{Scheme: "file", Path: p}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = q.Encode()
	return u.String()
}

// openSqlite 返回 SQLite 连接串（dataDir 用于拼接默认数据库路径）。
// 仅非 solaris/illumos 平台编译本实现。
func openSqlite(dataDir, dsn string) (string, error) {
	if dsn == "" {
		dsn = filepath.Join(dataDir, "accounts.db")
	}
	return sqliteDSN(dsn), nil
}
