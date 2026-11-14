// global.go — 进程级唯一时间源。
//
// 业务代码一律通过 timesys.Now() 获取墙上时间，从而让「整个应用围绕同一套
// 时间系统」成为可验证的事实：把全局时钟换成测试假时钟，所有依赖时间的行为
// （会话过期、封禁、TLS 证书有效期、审计时间戳……）都会随之改变。
package timesys

import (
	"sync/atomic"
	"time"
)

// current 进程当前时间源（默认是未锁定的系统时钟）。
var current atomic.Pointer[Clock]

func init() {
	current.Store(NewClock(DefaultLimits()))
}

// Current 返回进程当前时间源（永不为 nil）。
func Current() *Clock {
	if c := current.Load(); c != nil {
		return c
	}
	return NewClock(DefaultLimits())
}

// SetCurrent 替换进程时间源，返回恢复原时间源的函数（测试用，可 defer）。
func SetCurrent(c *Clock) func() {
	if c == nil {
		c = NewClock(DefaultLimits())
	}
	prev := current.Swap(c)
	return func() { current.Store(prev) }
}

// Now 返回当前时间（受纪律约束；未初始化时等同系统时间）。
func Now() time.Time { return Current().Now() }

// Since 返回自 t 以来经过的时间。
func Since(t time.Time) time.Duration { return Now().Sub(t) }

// Until 返回到 t 还有多久。
func Until(t time.Time) time.Duration { return t.Sub(Now()) }

// NowMillis 返回当前 Unix 毫秒时间戳。
func NowMillis() int64 { return Now().UnixMilli() }
