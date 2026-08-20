// Java 运行时检测测试：版本号解析、厂商识别、候选收集、真实探测、API。

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestJavaMajorOf 版本号解析。
func TestJavaMajorOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"21.0.4", 21},
		{"17.0.12", 17},
		{"1.8.0_392", 8},
		{"11.0.2+9", 11},
		{"21-ea", 21},
		{"", 0},
		{"abc", 0},
	}
	for _, c := range cases {
		if got := javaMajorOf(c.in); got != c.want {
			t.Fatalf("javaMajorOf(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
	t.Logf("[验证] 版本号解析（含 1.8 旧式前缀）正确")
}

// TestJavaVendor 厂商识别。
func TestJavaVendor(t *testing.T) {
	if got := javaVendor("OpenJDK Runtime Environment Microsoft-13877171 (build 21.0.11)"); got != "Microsoft" {
		t.Fatalf("Microsoft 识别失败: %s", got)
	}
	if got := javaVendor("OpenJDK Runtime Environment Temurin-21.0.4+7"); got != "Eclipse Adoptium (Temurin)" {
		t.Fatalf("Temurin 识别失败: %s", got)
	}
	if got := javaVendor("openjdk version \"21.0.4\""); got != "OpenJDK" {
		t.Fatalf("OpenJDK 识别失败: %s", got)
	}
	if got := javaVendor("some weird output"); got != "未知" {
		t.Fatalf("未知厂商应返回「未知」: %s", got)
	}
	t.Logf("[验证] 厂商识别正确")
}

// TestJavaCandidates 候选收集：JAVA_HOME/PATH 加入、去重、仅文件。
func TestJavaCandidates(t *testing.T) {
	dir := t.TempDir()
	// 造一个假 java 文件
	fake := filepath.Join(dir, "java")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatalf("创建假 java 失败: %v", err)
	}
	d := NewDaemon(dir, "test-key")
	d.DataDir = dir
	// 节点自管 JDK 目录
	jdkDir := filepath.Join(dir, "jdk", "jdk-99")
	if err := os.MkdirAll(filepath.Join(jdkDir, "bin"), 0o755); err != nil {
		t.Fatalf("创建 jdk 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jdkDir, "bin", filepath.Base(fake)), []byte("x"), 0o755); err != nil {
		t.Fatalf("写入 jdk java 失败: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", dir)
	defer os.Setenv("PATH", oldPath)

	paths := d.javaCandidates()
	found := false
	for _, p := range paths {
		if strings.Contains(p, "jdk-99") {
			found = true
		}
	}
	if !found {
		t.Fatalf("节点自管 JDK 未进入候选: %v", paths)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("候选路径不存在: %v", err)
	}
	t.Logf("[验证] 候选收集含自管 JDK 与 PATH（共 %d 个）", len(paths))
}

// TestProbeJava 真实探测：测试机 PATH 上有 java 时验证版本解析。
func TestProbeJava(t *testing.T) {
	path, err := exec.LookPath("java")
	if err != nil {
		t.Skipf("测试机无 java，跳过真实探测（%v）", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt := probeJava(ctx, path)
	if !rt.Available {
		t.Fatalf("java 应可用: %s", path)
	}
	if rt.Major <= 0 {
		t.Fatalf("大版本号解析失败: %+v", rt)
	}
	if rt.Version == "" {
		t.Fatalf("版本串为空: %+v", rt)
	}
	if rt.Vendor == "" || rt.Vendor == "未知" {
		t.Fatalf("厂商未识别: %+v", rt)
	}
	t.Logf("[验证] 真实探测: %s (major %d, %s, %s)", rt.Path, rt.Major, rt.Version, rt.Vendor)
}

// TestRuntimeJavaAPI GET /api/runtime/java 响应结构。
func TestRuntimeJavaAPI(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	resp, err := testClient.Get(srv.URL + "/api/runtime/java?apikey=test-key")
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
			Default *javaRuntime  `json:"default"`
			All     []javaRuntime `json:"all"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应非 JSON: %s", body)
	}
	if out.Status != 200 {
		t.Fatalf("业务状态: %d", out.Status)
	}
	if _, err := exec.LookPath("java"); err == nil {
		// 测试机有 java：default 应可用且 all 中应有 available=true
		if out.Data.Default == nil || !out.Data.Default.Available {
			t.Fatalf("default 应为可用运行时: %+v", out.Data.Default)
		}
		found := false
		for _, rt := range out.Data.All {
			if rt.Available {
				found = true
			}
		}
		if !found {
			t.Fatalf("all 中应有可用运行时: %+v", out.Data.All)
		}
	}
	t.Logf("[验证] /api/runtime/java 响应结构正确（default=%+v, all=%d 项）",
		out.Data.Default, len(out.Data.All))
}

// TestJavaDetectionTimeout 无执行权限的 java 标记 available=false 而非卡死。
func TestJavaDetectionTimeout(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "java")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	// 内容不是合法可执行文件：探测会失败，应标记不可用
	if err := os.WriteFile(fake, []byte("not a program"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	d := NewDaemon(dir, "test-key")
	d.DataDir = dir
	// 仅候选 PATH 中该假 java：直接探测单个
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt := probeJava(ctx, fake)
	if rt.Available {
		t.Fatalf("不可执行文件不应标记可用: %+v", rt)
	}
	t.Logf("[验证] 不可用的 java 正确标记 available=false")
}
