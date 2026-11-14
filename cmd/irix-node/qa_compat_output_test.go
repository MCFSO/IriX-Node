// 控制台输出合并（mcPumpOutput）回归测试。
//
// 背景：兼容层曾把每个输出行单独推一帧——实测一台狂刷日志的实例 3 秒内
// 产生近 5 万帧，面板端表现为「忽而卡死、忽而一次性涌出」（用户反馈很不流畅）。
// 现在按 30ms 窗口合并，并保留「低输出时最坏 30ms 延迟」与「积压超 16KB 立即发」。

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMcPumpOutputBatches 大批积压必须合并成少量帧，且内容/顺序完整。
func TestMcPumpOutputBatches(t *testing.T) {
	ch := make(chan string, 4096)
	const lines = 2000
	for i := 0; i < lines; i++ {
		ch <- fmt.Sprintf("line-%d", i)
	}
	close(ch)

	var frames []string
	start := time.Now()
	mcPumpOutput(ch, func(s string) { frames = append(frames, s) })
	elapsed := time.Since(start)

	// 2000 行 × ~9 字节 ≈ 18KB：最多两帧（16KB 上限 + 剩余）
	if len(frames) > 3 {
		t.Fatalf("应合并为极少量帧，实际 %d 帧", len(frames))
	}
	got := strings.Join(frames, "")
	want := ""
	for i := 0; i < lines; i++ {
		want += fmt.Sprintf("line-%d\n", i)
	}
	if got != want {
		t.Fatalf("合并后内容/顺序不一致（len got=%d want=%d）", len(got), len(want))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("合并推送不应卡住：耗时 %v", elapsed)
	}
}

// TestMcPumpOutputFlushesSingleLine 低输出（单行）必须在窗口内送达，不能被攒着。
func TestMcPumpOutputFlushesSingleLine(t *testing.T) {
	ch := make(chan string, 1)
	got := make(chan string, 1)
	go mcPumpOutput(ch, func(s string) {
		select {
		case got <- s:
		default:
		}
	})
	start := time.Now()
	ch <- "hello"
	select {
	case s := <-got:
		if s != "hello\n" {
			t.Fatalf("单行内容异常: %q", s)
		}
		if d := time.Since(start); d > mcOutputFlushInterval*4 {
			t.Fatalf("单行输出延迟过大: %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("单行输出没有被推送（窗口冲刷失效）")
	}
	close(ch)
}

// TestMcPumpOutputFlushesOnClose 通道关闭时必须把剩余内容冲刷出去。
func TestMcPumpOutputFlushesOnClose(t *testing.T) {
	ch := make(chan string, 4)
	ch <- "tail-a"
	ch <- "tail-b"
	close(ch)

	var got strings.Builder
	mcPumpOutput(ch, func(s string) { got.WriteString(s) })
	if got.String() != "tail-a\ntail-b\n" {
		t.Fatalf("关闭时应冲刷剩余内容，实际 %q", got.String())
	}
}
