// MCSManager 兼容层：文件管理器「打开/保存」链路回归测试。
//
// 面板文件编辑器的真实调用链（frontend FileEditor.vue → panel PUT /api/files →
// daemon 事件 file/edit）：
//   - 打开：PUT /api/files {target}（**不带 text**）→ 期望回包是文件内容字符串；
//   - 保存：PUT /api/files {target, text} → 期望回包 true。
//
// 线上问题：早先的实现不区分「没有 text」，把打开动作当成「写入空串」，
// 结果一打开配置文件就被截断成 0 字节，编辑器也拿不到内容（表现为「没法编辑」、
// 原文件内容丢失）。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompatMCSMFileEditReadWrite 文件编辑必须是「无 text = 读、有 text = 写」，
// 且读取绝不能改动磁盘内容。
func TestCompatMCSMFileEditReadWrite(t *testing.T) {
	d, _ := newTestDaemon(t)
	c := newEIOClient(t, setupTestMCSM(t, d)+"/socket.io/")
	c.connect()
	if status, _ := c.requestRaw("auth", "e0", mcsmTestKey); status != 200 {
		t.Fatal("认证失败")
	}

	cwd := filepath.Join(worldDir(t), "srv-edit")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := sampleInst(1, cwd)
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, "server.properties")
	const original = "motd=hello\nmax-players=20\n"
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1) 不带 text：读内容，必须是字符串回包
	status, data := c.requestRaw("file/edit", "e1", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "server.properties",
	})
	if status != 200 {
		t.Fatalf("读取文件失败: %d %s", status, data)
	}
	var content string
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatalf("读取应返回内容字符串，实际 %s（面板编辑器据此填充）", data)
	}
	if content != original {
		t.Fatalf("读取内容不符: %q != %q", content, original)
	}
	// 关键回归点：读取不能改磁盘
	if got, err := os.ReadFile(file); err != nil {
		t.Fatalf("读取后文件消失: %v", err)
	} else if string(got) != original {
		t.Fatalf("读取动作改动了文件内容（被截断？）: %q", got)
	}

	// 2) 有 text：写入并返回 true
	status, data = c.requestRaw("file/edit", "e2", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "server.properties", "text": "motd=changed\n",
	})
	if status != 200 || strings.TrimSpace(string(data)) != "true" {
		t.Fatalf("写入文件应返回 true: %d %s", status, data)
	}
	if got, _ := os.ReadFile(file); string(got) != "motd=changed\n" {
		t.Fatalf("写入未落盘: %q", got)
	}

	// 3) text 为空串也是「写入」（面板清空文件就是这么发的）
	status, data = c.requestRaw("file/edit", "e3", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "server.properties", "text": "",
	})
	if status != 200 || strings.TrimSpace(string(data)) != "true" {
		t.Fatalf("空串写入应返回 true: %d %s", status, data)
	}
	if got, _ := os.ReadFile(file); len(got) != 0 {
		t.Fatalf("空串写入应清空文件，实际 %q", got)
	}

	// 4) 超过编辑器上限（5 MiB）：明确报错，不返回半截内容
	big := filepath.Join(cwd, "big.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", mcFileMaxEditSize+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	status, data = c.requestRaw("file/edit", "e4", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "big.log",
	})
	if status == 200 {
		t.Fatalf("超限文件读取应失败: %s", data)
	}
	if !strings.Contains(string(data), "过大") {
		t.Fatalf("超限错误应说明文件过大: %s", data)
	}

	// 5) 目录不能当文本打开
	status, data = c.requestRaw("file/edit", "e5", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "/",
	})
	if status == 200 {
		t.Fatalf("目录不应作为文本读取成功: %s", data)
	}

	// 6) 越界路径仍然拒绝（不能借 file/edit 读写实例目录外）
	secret := filepath.Join(worldDir(t), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, data = c.requestRaw("file/edit", "e6", map[string]any{
		"instanceUuid": inst.InstanceUuid, "target": "../outside-secret.txt",
	})
	if status == 200 {
		t.Fatalf("越界读取应被拒绝: %s", data)
	}
	if got, _ := os.ReadFile(secret); string(got) != "top-secret" {
		t.Fatalf("越界写入破坏了外部文件: %q", got)
	}
}
