// 配置文件（config.json）加载与启动选项合并的单元测试。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// baseOptions 返回与 main 中 flag 默认值一致的选项基线。
func baseOptions() *nodeOptions {
	return &nodeOptions{
		Port:           12346,
		BindHost:       "",
		DataDir:        "",
		APIKey:         "",
		InstanceLog:    true,
		InstanceLogMax: 64,
		AuditLog:       true,
		AuditLogMax:    64,
		LoadTune:       true,
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在：空配置、loaded=false、无错误（无配置文件启动）
	cfg, loaded, err := loadConfigFile(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatalf("配置文件不存在不应报错: %v", err)
	}
	if loaded || cfg == nil {
		t.Fatalf("期望 loaded=false 且配置非 nil，得到 loaded=%v cfg=%v", loaded, cfg)
	}

	// 路径为空：视为未加载
	if _, loaded, err := loadConfigFile(""); err != nil || loaded {
		t.Fatalf("空路径应直接跳过: loaded=%v err=%v", loaded, err)
	}

	// 非法 JSON：报中文错误
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfigFile(bad); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}

	// 正常解析：字段取值正确，未写字段指针为 nil（区分未设置与显式 false/0）
	ok := filepath.Join(dir, "ok.json")
	content := `{
		"bind": "0.0.0.0",
		"port": 23333,
		"data": "/srv/irix",
		"apiKey": "secret",
		"instanceLog": false,
		"instanceLogMax": 128,
		"auditLog": false,
		"loadTune": false,
		"transferAllowCidr": "192.168.0.0/16,10.0.0.0/8"
	}`
	if err := os.WriteFile(ok, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, loaded, err = loadConfigFile(ok)
	if err != nil || !loaded {
		t.Fatalf("加载合法配置失败: %v", err)
	}
	if cfg.Bind != "0.0.0.0" || cfg.Port != 23333 || cfg.Data != "/srv/irix" || cfg.APIKey != "secret" {
		t.Fatalf("字符串/整数字段解析错误: %+v", cfg)
	}
	if cfg.TransferAllowCIDR != "192.168.0.0/16,10.0.0.0/8" {
		t.Fatalf("transferAllowCidr 解析错误: %q", cfg.TransferAllowCIDR)
	}
	if cfg.InstanceLog == nil || *cfg.InstanceLog {
		t.Fatal("instanceLog 应解析为显式 false")
	}
	if cfg.AuditLog == nil || *cfg.AuditLog {
		t.Fatal("auditLog 应解析为显式 false")
	}
	if cfg.LoadTune == nil || *cfg.LoadTune {
		t.Fatal("loadTune 应解析为显式 false")
	}
	if cfg.InstanceLogMax == nil || *cfg.InstanceLogMax != 128 {
		t.Fatal("instanceLogMax 应解析为 128")
	}
	if cfg.AuditLogMax != nil {
		t.Fatal("未写的 auditLogMax 应为 nil（回退默认值）")
	}

	// 空对象：全部回退默认
	cfg, _, err = loadConfigFile(writeTemp(t, dir, "empty.json", "{}"))
	if err != nil || cfg == nil {
		t.Fatalf("空对象加载失败: %v", err)
	}
	if cfg.Port != 0 || cfg.Bind != "" || cfg.InstanceLog != nil {
		t.Fatalf("空对象应全部为未设置: %+v", cfg)
	}
}

// TestEnsureConfigFile 首次启动自动生成示例配置：生成、可加载、不覆盖已有、失败路径。
func TestEnsureConfigFile(t *testing.T) {
	dir := t.TempDir()

	// 内嵌的示例配置本身必须是合法 JSON（_comment 等未知字段在加载时被忽略）
	if !json.Valid(exampleConfigJSON) {
		t.Fatal("内嵌的示例配置不是合法 JSON")
	}

	// 不存在的路径（父目录也不存在）：自动生成，且生成后可被正常加载解析
	p := filepath.Join(dir, "cfg", "config.json")
	created, err := ensureConfigFile(p)
	if err != nil {
		t.Fatalf("自动生成失败: %v", err)
	}
	if !created {
		t.Fatal("文件不存在时应返回 created=true")
	}
	cfg, loaded, err := loadConfigFile(p)
	if err != nil || !loaded {
		t.Fatalf("生成的配置文件应可正常加载: %v", err)
	}
	if cfg.Port != 12346 || cfg.Bind != "127.0.0.1" || cfg.InstanceLog == nil || !*cfg.InstanceLog {
		t.Fatalf("生成的示例配置解析异常: %+v", cfg)
	}

	// 已存在：绝不覆盖，返回 created=false
	if err := os.WriteFile(p, []byte(`{"port": 9999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err = ensureConfigFile(p)
	if err != nil || created {
		t.Fatalf("已存在的配置不应被覆盖: created=%v err=%v", created, err)
	}
	cfg, _, _ = loadConfigFile(p)
	if cfg.Port != 9999 {
		t.Fatalf("已有配置被改动: %+v", cfg)
	}

	// 空路径：直接跳过，无副作用
	if created, err := ensureConfigFile(""); err != nil || created {
		t.Fatalf("空路径应跳过: created=%v err=%v", created, err)
	}

	// 路径不可写（父路径是普通文件）：返回错误，不 panic
	blocked := filepath.Join(dir, "afile", "config.json")
	if err := os.WriteFile(filepath.Join(dir, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfigFile(blocked); err == nil {
		t.Fatal("父路径为普通文件时应返回错误")
	}
}

// writeTemp 写临时文件并返回路径。
func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyConfig(t *testing.T) {
	no := false
	mb := 128

	// 配置文件覆盖 flag 默认值
	o := baseOptions()
	o.applyConfig(&Config{
		Port:           23333,
		Bind:           "192.168.1.5",
		Data:           "/srv/irix",
		APIKey:         "secret",
		InstanceLog:    &no,
		InstanceLogMax: &mb,
	}, map[string]bool{})
	if o.Port != 23333 || o.BindHost != "192.168.1.5" || o.DataDir != "/srv/irix" || o.APIKey != "secret" {
		t.Fatalf("配置文件未覆盖默认值: %+v", o)
	}
	if o.InstanceLog || o.InstanceLogMax != 128 {
		t.Fatalf("布尔/整数覆盖错误: %+v", o)
	}

	// 命令行显式参数优先于配置文件
	o = baseOptions()
	o.Port = 12346 // flag 值（未被覆盖的默认）
	o.applyConfig(&Config{Port: 23333, Bind: "0.0.0.0"}, map[string]bool{"port": true})
	if o.Port != 12346 {
		t.Fatalf("命令行显式参数应优先: %+v", o)
	}
	if o.BindHost != "0.0.0.0" {
		t.Fatalf("未显式设置的参数应被配置覆盖: %+v", o)
	}

	// 未写字段保持默认值
	o = baseOptions()
	o.applyConfig(&Config{}, map[string]bool{})
	if !o.InstanceLog || o.Port != 12346 || o.InstanceLogMax != 64 || o.LoadTune != true {
		t.Fatalf("空配置不应改动默认值: %+v", o)
	}

	// 显式 false 优先于默认 true（配置文件）
	o = baseOptions()
	o.applyConfig(&Config{AuditLog: &no}, map[string]bool{})
	if o.AuditLog {
		t.Fatalf("显式 false 未生效: %+v", o)
	}

	// 命令行显式参数优先于配置文件的显式 false
	o = baseOptions()
	o.applyConfig(&Config{InstanceLog: &no}, map[string]bool{"instance-log": true})
	if !o.InstanceLog {
		t.Fatalf("命令行显式参数应优先于配置文件: %+v", o)
	}

	// nil 配置不改动任何值
	o = baseOptions()
	o.applyConfig(nil, map[string]bool{})
	if *o != *baseOptions() {
		t.Fatalf("nil 配置不应改动选项: %+v", o)
	}
}
