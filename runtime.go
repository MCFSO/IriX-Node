// Java 运行时检测与 JDK 安装（docs/irix-node-local-parity.md §4.2）。
//
// §4.2.1 检测：扫描节点自管 JDK 目录（{data}/jdk）、JAVA_HOME、PATH、
// 常见安装目录（Linux /usr/lib/jvm、Windows Program Files 等），逐个执行
// `java -version` 解析版本/厂商，available=false 表示路径存在但不可执行。
// §4.2.2 安装（任务化，节点直连 Adoptium 下载）见 installJDK。

package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// javaRuntime 单个 Java 运行时信息。
type javaRuntime struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Vendor    string `json:"vendor"`
	Major     int    `json:"major"`
	Available bool   `json:"available"`
}

// handleRuntimeJava 检测节点上的全部 Java 运行时。
// GET /api/runtime/java
// 响应 data: {default: javaRuntime|null, all: [javaRuntime...]}
// default 为可用版本号最高的运行时（无可用时为 null，客户端显示「未检测到」）。
func (d *Daemon) handleRuntimeJava(w http.ResponseWriter, r *http.Request) {
	all := d.detectJava()
	var def any
	if len(all) > 0 && all[0].Available {
		def = all[0]
	}
	writeOK(w, map[string]any{
		"default": def,
		"all":     all,
	})
}

// javaProbeTimeout 单个 java -version 探测超时。
const javaProbeTimeout = 5 * time.Second

// javaProbeConcurrency 并行探测上限（探测需拉起 JVM，逐个执行过慢）。
const javaProbeConcurrency = 8

// javaCandidates 收集候选 java 可执行文件路径（去重、仅保留存在的文件）。
// 优先级顺序：节点自管 JDK → JAVA_HOME → PATH → 常见安装目录。
func (d *Daemon) javaCandidates() []string {
	exe := "java"
	if runtime.GOOS == "windows" {
		exe = "java.exe"
	}
	bin := filepath.Join("bin", exe)

	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		p = filepath.Clean(p)
		if p == "" || seen[p] {
			return
		}
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	addGlob := func(pat string) {
		if ms, err := filepath.Glob(pat); err == nil {
			for _, m := range ms {
				add(m)
			}
		}
	}

	// 节点自管 JDK（F5 安装到 {data}/jdk/jdk-<major>/bin/java）
	if d.DataDir != "" {
		addGlob(filepath.Join(d.DataDir, "jdk", "*", bin))
	}
	// JAVA_HOME
	if jh := strings.TrimSpace(os.Getenv("JAVA_HOME")); jh != "" {
		add(filepath.Join(jh, bin))
	}
	// PATH
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			add(filepath.Join(dir, exe))
		}
	}
	// 常见安装目录
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if root == "" {
				continue
			}
			addGlob(filepath.Join(root, "Java", "*", bin))
			addGlob(filepath.Join(root, "Eclipse Adoptium", "*", bin))
			addGlob(filepath.Join(root, "Microsoft", "jdk-*", bin))
			addGlob(filepath.Join(root, "Zulu", "*", bin))
			addGlob(filepath.Join(root, "Amazon Corretto", "*", bin))
			addGlob(filepath.Join(root, "Temurin", "*", bin))
		}
	default:
		addGlob("/usr/lib/jvm/*/bin/java")
		addGlob("/usr/lib/jvm/*/bin/java.exe")
		addGlob("/usr/local/openjdk*/bin/java")
		addGlob("/opt/java/*/bin/java")
		addGlob("/Library/Java/JavaVirtualMachines/*/Contents/Home/bin/java")
	}
	return paths
}

// javaVersionRe 匹配 `version "21.0.4"` 形式。
var javaVersionRe = regexp.MustCompile(`version "([^"]+)"`)

// probeJava 探测单个 java 可执行文件：执行 -version 解析版本与厂商。
func probeJava(ctx context.Context, path string) javaRuntime {
	rt := javaRuntime{Path: path}
	cmd := exec.CommandContext(ctx, path, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		rt.Available = false
		return rt
	}
	text := string(out)
	rt.Available = true
	rt.Vendor = javaVendor(text)
	if m := javaVersionRe.FindStringSubmatch(text); m != nil {
		rt.Version = m[1]
		rt.Major = javaMajorOf(m[1])
	}
	return rt
}

// detectJava 检测全部候选并排序（可用优先，其次大版本号降序）。
func (d *Daemon) detectJava() []javaRuntime {
	paths := d.javaCandidates()
	all := make([]javaRuntime, 0, len(paths))
	ctx, cancel := context.WithTimeout(context.Background(), javaProbeTimeout+2*time.Second)
	defer cancel()

	sem := make(chan struct{}, javaProbeConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rt := probeJava(ctx, p)
			mu.Lock()
			all = append(all, rt)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool {
		if all[i].Available != all[j].Available {
			return all[i].Available
		}
		if all[i].Major != all[j].Major {
			return all[i].Major > all[j].Major
		}
		return all[i].Path < all[j].Path
	})
	return all
}

// javaMajorOf 解析版本号的大版本：21.0.4 → 21；1.8.0_392 → 8（旧式 1.x 前缀）。
func javaMajorOf(v string) int {
	groups := make([]string, 0, 2)
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	}) {
		if part == "" {
			continue
		}
		if _, err := strconv.Atoi(part); err == nil {
			groups = append(groups, part)
		}
		if len(groups) >= 2 {
			break
		}
	}
	if len(groups) == 0 {
		return 0
	}
	major, _ := strconv.Atoi(groups[0])
	if major == 1 && len(groups) >= 2 {
		if m2, err := strconv.Atoi(groups[1]); err == nil {
			major = m2 // 1.8 → 8
		}
	}
	return major
}

// javaVendors 厂商标识 → 展示名（按特征串匹配 -version 输出）。
var javaVendors = []struct{ marker, name string }{
	{"temurin", "Eclipse Adoptium (Temurin)"},
	{"eclipse adoptium", "Eclipse Adoptium"},
	{"adoptopenjdk", "AdoptOpenJDK"},
	{"zulu", "Azul Zulu"},
	{"microsoft", "Microsoft"},
	{"corretto", "Amazon Corretto"},
	{"graalvm", "GraalVM"},
	{"liberica", "BellSoft Liberica"},
	{"dragonwell", "Alibaba Dragonwell"},
	{"sap machine", "SAP SapMachine"},
	{"oracle", "Oracle"},
	{"openjdk", "OpenJDK"},
}

// javaVendor 从 -version 输出中识别厂商。
func javaVendor(text string) string {
	lower := strings.ToLower(text)
	for _, v := range javaVendors {
		if strings.Contains(lower, v.marker) {
			return v.name
		}
	}
	return "未知"
}
