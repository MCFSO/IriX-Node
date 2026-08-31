//go:build openbsd

// OpenBSD 权限自限制：启动早期调用 pledge(2) + unveil(2)，把运行态收敛到
// 「数据目录读写 + 监听/拨号网络 + 派生实例进程」所需最小能力。
// 参考 OpenBSD pledge(2) 与 unveil(2) 手册；golang.org/x/sys/unix 提供 Go 绑定。

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// restrictPrivileges 在 OpenBSD 上主动收紧进程权限：
//  1. unveil 放行数据目录（递归）与系统二进制目录（实例启动命令解析/执行）；
//  2. 随后以 unveil(NULL,NULL) 锁定，此后任何未放行路径访问触发 SIGABRT；
//  3. pledge 收敛 syscalls 到必要集（inet=网络监听/拨号、rpath/wpath/cpath=
//     数据目录读写创建、flock=日志轮转、proc/exec=派生实例进程、dns=集群拉取、
//     unix=本地套接字、tty=可选控制台；getpw=账户名解析）。
//
// dataDir 为绝对路径；调用点保证在 net.Listen 之前。
func restrictPrivileges(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("restrictPrivileges: 数据目录为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("数据目录绝对化失败: %w", err)
	}

	// 1. 放行数据目录（递归）：实例 cwd、日志、备份、保险库、归档、jdk、frp 等均在其中
	if err := unix.Unveil(abs, "rwc"); err != nil {
		return fmt.Errorf("unveil 数据目录失败: %w", err)
	}
	// 2. 放行系统二进制目录：实例启动命令 / docker / bastille 等可执行文件解析与 exec
	for _, bin := range []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin"} {
		if _, err := os.Stat(bin); err == nil {
			if err := unix.Unveil(bin, "rx"); err != nil {
				return fmt.Errorf("unveil %s 失败: %w", bin, err)
			}
		}
	}
	// 3. 放行 /etc/resolv.conf（DNS 解析需要读）与设备目录（/dev/null 等）
	if err := unix.Unveil("/etc/resolv.conf", "r"); err != nil {
		// 非致命：部分环境可能无该文件
		_ = err
	}

	// 4. 锁定 unveil：此后未放行路径一律拒绝
	if err := unix.UnveilBlock(); err != nil {
		return fmt.Errorf("unveil 锁定失败: %w", err)
	}

	// 5. pledge 收敛 syscall 集
	//    inet:   HTTP 监听 / 集群拉取 / 核心下载拨号
	//    dns:    集群拉取 / 核心下载的主机名解析
	//    rpath:  读取实例文件、日志、归档
	//    wpath:  写入/追加日志、实例文件
	//    cpath:  创建实例目录、临时文件、归档
	//    flock:  日志轮转文件锁
	//    unix:   本地 Unix 域套接字（部分 IPC）
	//    proc:   父进程控制（waitpid 管理实例子进程）
	//    exec:   派生实例进程（启动命令、docker/bastille 调用）
	//    getpw:  账户名/UID 解析（可选）
	//    tty:    实例控制台可选绑定（少数场景）
	//    id:     setuid/setgid（如启用降权）
	promises := "inet dns rpath wpath cpath flock unix proc exec getpw tty id"
	if err := unix.PledgePromises(promises); err != nil {
		return fmt.Errorf("pledge 失败: %w", err)
	}
	// execpromises 限制子进程能力：派生出的实例进程仍需 rpath/wpath/cpath/inet/exec 等，
	// 此处放行与父进程同级（实例本身通常需更宽权限运行）。
	if err := unix.PledgeExecpromises(promises); err != nil {
		return fmt.Errorf("pledge execpromises 失败: %w", err)
	}

	alog.Printf("OpenBSD 权限自限制已启用（pledge=%q，unveil 已锁定到 %s）", promises, abs)
	return nil
}
