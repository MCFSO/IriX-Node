// 兼容层凭据自动生成测试：
// 未填 key/password 时自动随机生成、持久化并回写配置文件；
// 重启后保持不变；用户自己填的值不被覆盖。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEnsureConfigFileFillsCompatSecrets 首次生成的配置文件里
// compat 的 key/password 必须已经有值（即使两层都是 enabled=false）。
func TestEnsureConfigFileFillsCompatSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	created, err := ensureConfigFile(path)
	if err != nil {
		t.Fatalf("生成示例配置失败: %v", err)
	}
	if !created {
		t.Fatal("首次生成应返回 created=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 生成的必须仍是合法 JSON（定点替换不能破坏结构）
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		t.Fatalf("生成的配置文件应可解析: %v\n%s", err, data)
	}
	if cfg.Compat == nil || cfg.Compat.MCSM == nil || cfg.Compat.MSLX == nil {
		t.Fatalf("生成的配置缺少 compat 块: %+v", cfg.Compat)
	}
	if len(cfg.Compat.MCSM.Key) < 16 {
		t.Fatalf("compat.mcsm.key 应已自动填入随机值，实际 %q", cfg.Compat.MCSM.Key)
	}
	if len(cfg.Compat.MSLX.Password) < 16 {
		t.Fatalf("compat.mslx.password 应已自动填入随机值，实际 %q", cfg.Compat.MSLX.Password)
	}
	// 默认仍是关闭状态
	if cfg.Compat.MCSM.Enabled != nil && *cfg.Compat.MCSM.Enabled {
		t.Fatal("mcsm 默认应关闭")
	}
	if cfg.Compat.MSLX.Enabled != nil && *cfg.Compat.MSLX.Enabled {
		t.Fatal("mslx 默认应关闭")
	}
	// 定点替换不能破坏文件其它部分
	text := string(data)
	if len(text) < len(exampleConfigJSON)/2 {
		t.Fatalf("生成的配置文件被意外截断（%d 字节）", len(text))
	}
	if !containsAll(text, `"_comment_compat"`, `"allowPanelIps"`, `"jwtSecret"`) {
		t.Fatalf("生成的配置文件缺少说明字段或结构: %s", text)
	}
}

// containsAll 判断 s 是否包含全部子串（测试辅助）。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestSetupCompatAutoGeneratesSecrets 启用兼容层但未填凭据时：
// 自动生成、可用、落盘并回写配置文件；重启后凭据不变。
func TestSetupCompatAutoGeneratesSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 造一份「用户没填 key/password」的配置
	raw := `{
  "port": 12346,
  "compat": {
    "mcsm": { "enabled": true, "prefix": "/mcsm", "key": "", "allowPanelIps": [] },
    "mslx": { "enabled": true, "prefix": "/mslx", "username": "admin", "password": "", "jwtSecret": "", "allowPanelIps": [] }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewDaemon(dataDir)
	t.Cleanup(func() { d.workers.stop() })
	d.setupCompat(&nodeOptions{
		ConfigPath:        cfgPath,
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0, // 端口 0 = 由系统分配（测试专用）
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0, // 端口 0 = 由系统分配（测试专用）
		// 故意不填 Key / Pass / Secret
	})
	if d.compatMC == nil || d.compatMSLX == nil {
		t.Fatal("凭据缺失时应自动生成并启用两层，而不是跳过")
	}
	key := d.compatMC.key
	pass := d.compatMSLX.pass
	if len(key) < 16 || len(pass) < 16 {
		t.Fatalf("自动生成的凭据过短: key=%q pass=%q", key, pass)
	}
	if len(d.compatMSLX.secret) < 16 {
		t.Fatalf("JWT 密钥过短: %q", string(d.compatMSLX.secret))
	}

	// 自动凭据文件：含三项，权限 0600（Windows 无 POSIX 权限位，跳过断言）
	secPath := filepath.Join(dataDir, compatSecretFile)
	st, err := os.Stat(secPath)
	if err != nil {
		t.Fatalf("应写入自动凭据文件: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Fatalf("自动凭据文件权限应为 0600，实际 %o", mode)
		}
	}
	var sec compatSecrets
	secData, _ := os.ReadFile(secPath)
	if err := json.Unmarshal(secData, &sec); err != nil {
		t.Fatalf("自动凭据文件解析失败: %v", err)
	}
	if sec.MCSMKey != key || sec.MSLXPassword != pass {
		t.Fatalf("自动凭据文件内容与内存不一致: %+v", sec)
	}

	// 配置文件已被回写（且权限未被改成 0644）
	cfg, _, err := loadConfigFile(cfgPath)
	if err != nil {
		t.Fatalf("回写后的配置应仍可解析: %v", err)
	}
	if cfg.Compat.MCSM.Key != key || cfg.Compat.MSLX.Password != pass {
		t.Fatalf("应把自动生成的凭据回写配置文件: key=%q pass=%q",
			cfg.Compat.MCSM.Key, cfg.Compat.MSLX.Password)
	}
	if st, err := os.Stat(cfgPath); err == nil && runtime.GOOS != "windows" {
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Fatalf("回写不应改变配置文件权限，实际 %o", mode)
		}
	}

	// 模拟重启：新的守护进程（配置里已有值）必须复用同一份凭据
	d2 := NewDaemon(dataDir)
	t.Cleanup(func() { d2.workers.stop() })
	d2.setupCompat(&nodeOptions{
		ConfigPath:        cfgPath,
		CompatMCSMEnabled: true,
		CompatMCSMPort:    0, // 端口 0 = 由系统分配（测试专用）
		CompatMCSMKey:     key,
		CompatMSLXEnabled: true,
		CompatMSLXPort:    0, // 端口 0 = 由系统分配（测试专用）
		CompatMSLXUser:    "admin",
		CompatMSLXPass:    pass,
	})
	if d2.compatMC == nil || d2.compatMC.key != key {
		t.Fatalf("重启后节点密钥应保持不变: %q → %q", key, d2.compatMC.key)
	}
	if d2.compatMSLX == nil || d2.compatMSLX.pass != pass {
		t.Fatalf("重启后登录密码应保持不变")
	}
	if string(d2.compatMSLX.secret) != string(d.compatMSLX.secret) {
		t.Fatal("重启后 JWT 密钥应保持不变（否则已签发 token 全部失效）")
	}
	// 凭据合法且互不相同
	if key == pass {
		t.Fatal("自动生成的密钥与密码不应相同")
	}
}

// TestSetupCompatDisabledStillHasSecrets 两层都关闭时也要准备好凭据并落盘，
// 用户后续只改 enabled=true 即可直接用。
func TestSetupCompatDisabledStillHasSecrets(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewDaemon(dataDir)
	t.Cleanup(func() { d.workers.stop() })
	d.setupCompat(&nodeOptions{}) // 两层都未启用，也没有配置文件
	if d.compatMC != nil || d.compatMSLX != nil {
		t.Fatal("未启用时不应挂载兼容层")
	}
	secPath := filepath.Join(dataDir, compatSecretFile)
	if _, err := os.Stat(secPath); err != nil {
		t.Fatalf("未启用时也应生成并保存凭据: %v", err)
	}
	var sec compatSecrets
	data, _ := os.ReadFile(secPath)
	if err := json.Unmarshal(data, &sec); err != nil {
		t.Fatal(err)
	}
	if len(sec.MCSMKey) < 16 || len(sec.MSLXPassword) < 16 || len(sec.MSLXJWTSecret) < 16 {
		t.Fatalf("自动凭据应三项齐全: %+v", sec)
	}
}

// TestPatchConfigFileKeepsUserValue 用户已填写的字段绝不被覆盖。
func TestPatchConfigFileKeepsUserValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"compat":{"mcsm":{"enabled":true,"key":"user-key"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if patchCompatConfigFile(path, "mcsm", "key", "generated-key") {
		t.Fatal("字段已有值时不应回写")
	}
	data, _ := os.ReadFile(path)
	if string(data) != raw {
		t.Fatalf("配置文件被意外修改: %s", data)
	}

	// 空值时才回写，且保持原有排版（只替换 "" 这一处）
	raw2 := `{
  "compat": {
    "mcsm": { "enabled": true, "key": "" }
  }
}`
	if err := os.WriteFile(path, []byte(raw2), 0o644); err != nil {
		t.Fatal(err)
	}
	if !patchCompatConfigFile(path, "mcsm", "key", "new-key-123") {
		t.Fatal("空值时应回写")
	}
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		t.Fatalf("回写后应仍是合法 JSON: %v", err)
	}
	if cfg.Compat.MCSM.Key != "new-key-123" {
		t.Fatalf("回写值不符: %q", cfg.Compat.MCSM.Key)
	}
}
