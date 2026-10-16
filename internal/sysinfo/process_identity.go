// 进程身份核对（平台无关的公共定义）。
//
// 各平台实现见 process_identity_{linux,freebsd,windows,other}.go：用进程创建时间
// 作为「进程身份」，与 PID 一起核对，避免 PID 被复用后误杀其他进程。

package sysinfo

import "errors"

// ErrProcessIdentityUnsupported 表示当前平台不支持持久化进程身份核对。
// 此时无法安全地校验 PID 是否被复用，调用方应保守处理（通常跳过终止）。
var ErrProcessIdentityUnsupported = errors.New("当前平台暂不支持持久化进程身份")
