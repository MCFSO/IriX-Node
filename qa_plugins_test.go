// 插件/Mod 元数据测试：plugin.yml / paper-plugin.yml / fabric.mod.json /
// mods.toml 解析、图标 base64、configDir 匹配、API 端到端。

package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// buildJar 构造包含指定条目的 jar 文件。
func buildJar(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目失败: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("写入 zip 条目失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写入 jar 失败: %v", err)
	}
}

// TestParsePluginYML plugin.yml 解析（名称/描述/版本/图标/配置目录）。
func TestParsePluginYML(t *testing.T) {
	d, dir := newTestDaemon(t)
	iconPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	buildJar(t, filepath.Join(dir, "plugins", "EssentialsX-2.20.1.jar"), map[string][]byte{
		"plugin.yml": []byte("name: EssentialsX\nmain: com.earth2me.essentials.Essentials\n" +
			"version: '2.20.1'\ndescription: Essential server commands\nicon: icon.png\n"),
		"icon.png": iconPNG,
	})
	// 配置目录
	if err := os.MkdirAll(filepath.Join(dir, "plugins", "Essentials"), 0o755); err != nil {
		t.Fatalf("创建配置目录失败: %v", err)
	}

	item := d.parsePluginJar(dir, filepath.Join(dir, "plugins"), "EssentialsX-2.20.1.jar", "plugin")
	if item == nil {
		t.Fatalf("解析失败")
	}
	if item.Name != "EssentialsX" || item.Version != "2.20.1" ||
		item.Description != "Essential server commands" {
		t.Fatalf("元数据错误: %+v", item)
	}
	if item.IconBase64 != base64.StdEncoding.EncodeToString(iconPNG) {
		t.Fatalf("图标 base64 错误: %s", item.IconBase64)
	}
	if item.ConfigDir != "/plugins/Essentials" {
		t.Fatalf("配置目录错误: %s", item.ConfigDir)
	}
	t.Logf("[验证] plugin.yml 解析正确: %+v", item)
}

// TestParsePaperPluginYML paper-plugin.yml 解析。
func TestParsePaperPluginYML(t *testing.T) {
	d, dir := newTestDaemon(t)
	buildJar(t, filepath.Join(dir, "plugins", "sample-plugin.jar"), map[string][]byte{
		"paper-plugin.yml": []byte("name: SamplePlugin\nversion: 1.0.0\ndescription: A sample\n"),
	})
	item := d.parsePluginJar(dir, filepath.Join(dir, "plugins"), "sample-plugin.jar", "plugin")
	if item == nil || item.Name != "SamplePlugin" || item.Version != "1.0.0" {
		t.Fatalf("paper-plugin.yml 解析错误: %+v", item)
	}
	t.Logf("[验证] paper-plugin.yml 解析正确")
}

// TestParseFabricMod fabric.mod.json 解析（mods 递归 + 版本子目录）。
func TestParseFabricMod(t *testing.T) {
	d, dir := newTestDaemon(t)
	iconPNG := []byte{0x89, 'P', 'N', 'G'}
	fabricJSON, _ := json.Marshal(map[string]any{
		"id": "sodium", "name": "Sodium", "version": "0.5.8",
		"description": "Rendering engine", "icon": "assets/sodium/icon.png",
	})
	buildJar(t, filepath.Join(dir, "mods", "1.20.1", "sodium-0.5.8.jar"), map[string][]byte{
		"fabric.mod.json":        fabricJSON,
		"assets/sodium/icon.png": iconPNG,
	})
	item := d.parsePluginJar(dir, filepath.Join(dir, "mods", "1.20.1"), "sodium-0.5.8.jar", "mod")
	if item == nil || item.Name != "Sodium" || item.Version != "0.5.8" ||
		item.Description != "Rendering engine" {
		t.Fatalf("fabric.mod.json 解析错误: %+v", item)
	}
	if item.IconBase64 != base64.StdEncoding.EncodeToString(iconPNG) {
		t.Fatalf("图标解析错误")
	}
	t.Logf("[验证] fabric.mod.json 解析正确（含图标）")
}

// TestParseModsTOML mods.toml 解析。
func TestParseModsTOML(t *testing.T) {
	d, dir := newTestDaemon(t)
	buildJar(t, filepath.Join(dir, "mods", "libs", "example-1.0.jar"), map[string][]byte{
		"META-INF/mods.toml": []byte("modLoader=\"javafml\"\nloaderVersion=\"[44,)\"\n\n" +
			"[[mods]]\nmodId=\"examplemod\"\ndisplayName=\"Example Mod\"\n" +
			"version=\"1.0.0\"\ndescription='''An example mod'''\nlogoFile=\"logo.png\"\n"),
		"logo.png": []byte{1, 2, 3},
	})
	item := d.parsePluginJar(dir, filepath.Join(dir, "mods", "libs"), "example-1.0.jar", "mod")
	if item == nil || item.Name != "Example Mod" || item.Version != "1.0.0" ||
		item.Description != "An example mod" {
		t.Fatalf("mods.toml 解析错误: %+v", item)
	}
	if item.IconBase64 == "" {
		t.Fatalf("logoFile 未解析为图标")
	}
	t.Logf("[验证] mods.toml 解析正确（含 logoFile）")
}

// TestParseNoMetadata 无元数据的 jar 返回 nil。
func TestParseNoMetadata(t *testing.T) {
	d, dir := newTestDaemon(t)
	buildJar(t, filepath.Join(dir, "plugins", "mystery.jar"), map[string][]byte{
		"random.txt": []byte("hello"),
	})
	item := d.parsePluginJar(dir, filepath.Join(dir, "plugins"), "mystery.jar", "plugin")
	if item != nil {
		t.Fatalf("无元数据 jar 应返回 nil: %+v", item)
	}
	t.Logf("[验证] 无元数据 jar 被忽略")
}

// TestPluginsAPI GET /api/instance/plugins 端到端。
func TestPluginsAPI(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	// plugin + mod 各一个
	buildJar(t, filepath.Join(dir, "plugins", "P.jar"), map[string][]byte{
		"plugin.yml": []byte("name: P\nversion: 1.0\ndescription: plugin\n"),
	})
	buildJar(t, filepath.Join(dir, "mods", "v2", "M.jar"), map[string][]byte{
		"fabric.mod.json": []byte(`{"id":"m","name":"M","version":"2.0","description":"mod"}`),
	})
	// 非 jar 文件忽略
	_ = os.WriteFile(filepath.Join(dir, "plugins", "readme.txt"), []byte("x"), 0o644)

	srv := newTestServer(d)
	defer srv.Close()

	resp, err := testClient.Get(srv.URL + "/api/instance/plugins?uuid=" + inst.InstanceUuid + "&apikey=test-key")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Status int `json:"status"`
		Data   struct {
			Items []pluginItem `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if len(out.Data.Items) != 2 {
		t.Fatalf("应有 2 个条目: %+v", out.Data.Items)
	}
	if out.Data.Items[0].Type != "plugin" || out.Data.Items[1].Type != "mod" {
		t.Fatalf("条目类型/排序错误: %+v", out.Data.Items)
	}
	if out.Data.Items[0].Path != "/plugins/P.jar" || out.Data.Items[1].Path != "/mods/v2/M.jar" {
		t.Fatalf("路径错误: %+v", out.Data.Items)
	}
	t.Logf("[验证] plugins API 端到端（%d 项）", len(out.Data.Items))
}

// TestParseSimpleYAML 极简 YAML 解析边界。
func TestParseSimpleYAML(t *testing.T) {
	m := parseSimpleYAML([]byte(`
# 注释
name: "Quoted Name"
version: '1.2.3'
main: com.example.Main
description: |
  multi
  line
authors: [a, b]
api-version: '1.13'
  indented: ignored
`))
	if m["name"] != "Quoted Name" || m["version"] != "1.2.3" {
		t.Fatalf("标量解析错误: %v", m)
	}
	if _, ok := m["description"]; ok {
		t.Fatalf("块标量应忽略: %v", m)
	}
	if _, ok := m["authors"]; ok {
		t.Fatalf("列表应忽略: %v", m)
	}
	if _, ok := m["indented"]; ok {
		t.Fatalf("缩进键应忽略: %v", m)
	}
	t.Logf("[验证] 极简 YAML 解析边界正确")
}
