// 容器 / Bastille 参数校验回归测试（红队审计 高-4）。
//
// 覆盖：volumes 宿主源白名单（绝对路径 / 系统目录 / 数据目录 / 符号链接穿透）、
// env 的 LD_* 拒绝、workdir 绝对路径与换行引号拒绝、mac 地址格式、
// image 位置参数防注入、镜像构建的管理员门槛与 BuildKit 越权指令拒绝。

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 纯校验函数
// ---------------------------------------------------------------------------

func TestValidateContainerEnvRejectsLD(t *testing.T) {
	// LD_* 一律拒绝（大小写不敏感）
	for _, k := range []string{"LD_PRELOAD", "ld_library_path", "LD_AUDIT"} {
		if err := validateContainerEnv(map[string]string{k: "/tmp/x.so"}); err == nil {
			t.Fatalf("%s 应被拒绝", k)
		}
	}
	// 正常变量放行
	if err := validateContainerEnv(map[string]string{"JAVA_HOME": "/usr/local/jdk", "PATH": "/usr/bin"}); err != nil {
		t.Fatalf("正常环境变量应放行: %v", err)
	}
	// 空名 / 含 '=' 的名拒绝
	if err := validateContainerEnv(map[string]string{"  ": "x"}); err == nil {
		t.Fatal("空变量名应被拒绝")
	}
	if err := validateContainerEnv(map[string]string{"A=B": "x"}); err == nil {
		t.Fatal("含 = 的变量名应被拒绝")
	}
}

func TestValidateContainerWorkdir(t *testing.T) {
	// 空值合法（不设置工作目录）
	if err := validateContainerWorkdir(""); err != nil {
		t.Fatalf("空 workdir 应放行: %v", err)
	}
	// 绝对路径放行
	if err := validateContainerWorkdir("/data/server"); err != nil {
		t.Fatalf("绝对路径应放行: %v", err)
	}
	// 相对路径拒绝
	if err := validateContainerWorkdir("data/server"); err == nil {
		t.Fatal("相对 workdir 应被拒绝")
	}
	// 单引号 / 换行 / 回车 / NUL 拒绝（会注入 jail.conf 的 exec.start）
	for _, bad := range []string{"/data/it's", "/data/a\nb", "/data/a\rb", "/data/a\x00b"} {
		if err := validateContainerWorkdir(bad); err == nil {
			t.Fatalf("workdir %q 应被拒绝", bad)
		}
	}
	// .. 段拒绝
	if err := validateContainerWorkdir("/data/../../etc"); err == nil {
		t.Fatal("含 .. 的 workdir 应被拒绝")
	}
}

func TestValidateMACAddress(t *testing.T) {
	for _, ok := range []string{"02:00:00:00:00:01", "AA:bb:CC:dd:EE:ff"} {
		if err := validateMACAddress(ok); err != nil {
			t.Fatalf("合法 MAC %q 应放行: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"02:00:00:00:00",        // 段数不足
		"02:00:00:00:00:0100",   // 段过长
		"02-00-00-00-00-01",     // 分隔符错误
		"02:00:00:00:00:0z",     // 非十六进制
		`02:00:00:00:00:01"; }`, // 试图闭合 jail.conf 行的注入形态
		"",                      // 空
	} {
		if err := validateMACAddress(bad); err == nil {
			t.Fatalf("非法 MAC %q 应被拒绝", bad)
		}
	}
}

func TestValidateCLIPositional(t *testing.T) {
	if err := validateCLIPositional("image", "alpine:3.20"); err != nil {
		t.Fatalf("正常镜像名应放行: %v", err)
	}
	for _, bad := range []string{"--privileged", "-v", "-"} {
		if err := validateCLIPositional("image", bad); err == nil {
			t.Fatalf("位置参数 %q 应被拒绝", bad)
		}
	}
	if err := validateCLIPositional("image", "a\nb"); err == nil {
		t.Fatal("含换行的位置参数应被拒绝")
	}
}

func TestValidateDockerfileSafety(t *testing.T) {
	blocked := []string{
		"FROM alpine\nRUN --mount=type=bind,source=/,target=/host ls /\n",
		"FROM alpine\nRUN --mount type=bind,target=/x ls /\n", // 空格形式
		"FROM alpine\nRUN --mount=type=cache,target=/root/.cache go build\n",
		"FROM alpine\nRUN --mount=type=secret,id=tok cat /run/secrets/tok\n",
		"FROM alpine\nRUN --security=insecure id\n",
		"FROM alpine\nRUN --network=host curl -s http://127.0.0.1:12346/ && true\n",
	}
	for _, df := range blocked {
		if err := validateDockerfileSafety(df); err == nil {
			t.Fatalf("危险 Dockerfile 应被拒绝: %q", df)
		}
	}
	allowed := []string{
		"FROM alpine\nRUN apk add --no-cache openjdk17\n",
		"FROM eclipse-temurin:21-jre\nCOPY server.jar /app/\n",
		"FROM alpine\nRUN --mount=type=tmpfs,target=/tmp ls /tmp\n", // tmpfs 不越界
	}
	for _, df := range allowed {
		if err := validateDockerfileSafety(df); err != nil {
			t.Fatalf("正常 Dockerfile 应放行: %v（%q）", err, df)
		}
	}
}

// ---------------------------------------------------------------------------
// 宿主机挂载源路径
// ---------------------------------------------------------------------------

func TestHostMountSourceValidation(t *testing.T) {
	d, dir := newTestDaemon(t)

	// 相对路径 / 空源 / 控制字符：拒绝
	for _, bad := range []string{"data/mc", "  ", "data\nmc", "./data"} {
		if err := d.validateHostMountSource(bad); err == nil {
			t.Fatalf("宿主源 %q 应被拒绝", bad)
		}
	}
	// 文件系统根：拒绝
	if err := d.validateHostMountSource("/"); err == nil {
		t.Fatal("根目录应被拒绝")
	}
	// 节点数据目录（账户库 / 保险库 / instances.json 所在）：拒绝
	if err := d.checkHostPathAllowed(dir); err == nil {
		t.Fatal("节点数据目录本身应被拒绝")
	}
	if err := d.checkHostPathAllowed(filepath.Join(dir, "vault")); err == nil {
		t.Fatal("节点数据目录内的路径应被拒绝")
	}
	// 数据目录之外的工作目录：放行（不误伤「把实例目录挂进容器」）
	if err := d.checkHostPathAllowed(worldDir(t)); err != nil {
		t.Fatalf("数据目录之外的工作目录应放行: %v", err)
	}
}

// TestHostMountSymlinkEscapeRejected 符号链接指向系统目录时必须拒绝。
func TestHostMountSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 /etc 且创建符号链接需要特权，跳过符号链接穿透用例")
	}
	d, _ := newTestDaemon(t)
	world := worldDir(t)

	escape := filepath.Join(world, "escape")
	if err := os.Symlink("/etc", escape); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if err := d.validateHostMountSource(escape); err == nil {
		t.Fatal("指向 /etc 的符号链接应被拒绝")
	}

	// 合法链接（解析后仍在允许范围内）应放行
	sub := filepath.Join(world, "data")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	okLink := filepath.Join(world, "ok")
	if err := os.Symlink(sub, okLink); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	if err := d.validateHostMountSource(okLink); err != nil {
		t.Fatalf("合法符号链接应放行: %v", err)
	}
}

func TestValidateContainerVolume(t *testing.T) {
	d, _ := newTestDaemon(t)
	world := worldDir(t)

	// 命名卷：放行（不是绑定挂载）
	if err := d.validateContainerVolume("mcdata:/data"); err != nil {
		t.Fatalf("命名卷应放行: %v", err)
	}
	// 模式后缀：ro/rw 放行，其余拒绝
	if err := d.validateContainerVolume("mcdata:/data:ro"); err != nil {
		t.Fatalf("ro 模式应放行: %v", err)
	}
	if err := d.validateContainerVolume("mcdata:/data:rw"); err != nil {
		t.Fatalf("rw 模式应放行: %v", err)
	}
	if err := d.validateContainerVolume("mcdata:/data:exec"); err == nil {
		t.Fatal("非法模式应被拒绝")
	}
	// 相对宿主源（形如 ./data，含 / 故不是命名卷）：拒绝
	if err := d.validateContainerVolume("./data:/data"); err == nil {
		t.Fatal("相对宿主源应被拒绝")
	}
	// 根目录：拒绝（典型 `-v /:/host` 提权）
	if err := d.validateContainerVolume("/:/host"); err == nil {
		t.Fatal("根目录宿主源应被拒绝")
	}
	// 容器内目标不是绝对路径 / 含 .. ：拒绝
	if err := d.validateContainerVolume("mcdata:data"); err == nil {
		t.Fatal("相对容器内目标应被拒绝")
	}
	if err := d.validateContainerVolume("mcdata:/.."); err == nil {
		t.Fatal("含 .. 的容器内目标应被拒绝")
	}
	// 段数错误：拒绝
	if err := d.validateContainerVolume("a:b:c:d"); err == nil {
		t.Fatal("段数错误应被拒绝")
	}
	// 合法绑定挂载（数据目录之外的 POSIX 绝对路径）：放行。
	// Windows 下宿主路径含盘符冒号（C:\…），容器宿主路径本就按 POSIX 语义
	// 解释（该平台无容器能力），故跳过。
	if runtime.GOOS != "windows" {
		if err := d.validateContainerVolume(world + ":/data"); err != nil {
			t.Fatalf("合法绑定挂载应放行: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP 端点
// ---------------------------------------------------------------------------

// TestContainerCreateParamValidation 创建容器端点的参数白名单（进入 docker CLI 前）。
func TestContainerCreateParamValidation(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]any
		want string // 期望错误消息包含的子串
	}{
		{"LD_ 环境变量", map[string]any{"image": "alpine", "env": map[string]string{"LD_PRELOAD": "/tmp/x.so"}}, "LD_"},
		{"相对 workdir", map[string]any{"image": "alpine", "workdir": "data"}, "workdir"},
		{"workdir 含单引号", map[string]any{"image": "alpine", "workdir": "/data/it's"}, "workdir"},
		{"相对卷源", map[string]any{"image": "alpine", "volumes": []string{"./data:/data"}}, "绝对路径"},
		{"根目录卷源", map[string]any{"image": "alpine", "volumes": []string{"/:/host"}}, "根目录"},
		{"卷目标含 ..", map[string]any{"image": "alpine", "volumes": []string{"/tmp:/.."}}, ".."},
		{"image 以 - 开头", map[string]any{"image": "--privileged"}, "命令行选项"},
		{"name 含非法字符", map[string]any{"image": "alpine", "name": "../x"}, "name"},
	}
	for _, tc := range cases {
		code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/create"), tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d %v", tc.name, code, env)
		}
		msg, _ := env["data"].(string)
		if !strings.Contains(msg, tc.want) {
			t.Fatalf("%s 错误消息应含 %q: %s", tc.name, tc.want, msg)
		}
	}
}

// TestImageBuildAdminGateAndBuildKitBlock 镜像构建端点：管理员门槛 + 越权指令拒绝。
func TestImageBuildAdminGateAndBuildKitBlock(t *testing.T) {
	d := newTestAccountsDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 管理员：BuildKit bind mount 拒绝 400
	code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/image/build"),
		map[string]any{
			"dockerfile": "FROM alpine\nRUN --mount=type=bind,source=/,target=/host ls /host\n",
			"name":       "irix-test", "tag": "1",
		})
	if code != http.StatusBadRequest {
		t.Fatalf("BuildKit bind mount 应 400: %d %v", code, env)
	}
	if msg, _ := env["data"].(string); !strings.Contains(msg, "--mount") {
		t.Fatalf("错误消息应指明 --mount: %s", msg)
	}
	// 管理员：--network=host 拒绝 400
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/image/build"),
		map[string]any{"dockerfile": "FROM alpine\nRUN --network=host true\n", "name": "irix-test", "tag": "1"})
	if code != http.StatusBadRequest {
		t.Fatalf("--network=host 应 400: %d %v", code, env)
	}

	// 非管理员：即使被授予该端点开关，仍拒绝（requireAdmin）
	if err := d.accounts.createAccount("bob", "BobPass12345", false); err != nil {
		t.Fatalf("创建非管理员账户失败: %v", err)
	}
	bobToken, _ := loginAs(t, srv, "bob", "BobPass12345")
	if err := d.accounts.setPermissions("bob", map[string]bool{"POST /api/image/build": true}); err != nil {
		t.Fatalf("授予端点开关失败: %v", err)
	}
	code, body := doReqToken(t, http.MethodPost, srv.URL+"/api/image/build", bobToken,
		map[string]any{"dockerfile": "FROM alpine\n", "name": "irix-test", "tag": "1"})
	if code != http.StatusForbidden || !strings.Contains(string(body), "管理员") {
		t.Fatalf("非管理员构建镜像应 403 且提示管理员权限: %d %s", code, body)
	}
}

// TestBastilleCreateParamValidation jail 创建端点参数白名单（桩平台下 400 早于 501）。
func TestBastilleCreateParamValidation(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"非法 MAC", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "10.0.0.2", "mac": `02:00:00:00:00:01"; }`}, "mac"},
		{"相对 workdir", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "10.0.0.2", "workdir": "data"}, "workdir"},
		{"workdir 含换行", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "10.0.0.2", "workdir": "/data\nexec.stop='x';"}, "workdir"},
		{"release 路径穿越", map[string]any{"name": "mc-1", "release": "../../etc"}, "release"},
		{"jail 名含 ..", map[string]any{"name": "mc..1", "release": "14.2-RELEASE", "ip": "10.0.0.2"}, "jail 名"},
		{"ip 以 - 开头", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "--evil"}, "ip"},
		{"卷源为根目录", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "10.0.0.2", "volumes": []string{"/:/data"}}, "根目录"},
		{"卷目标相对", map[string]any{"name": "mc-1", "release": "14.2-RELEASE", "ip": "10.0.0.2", "volumes": []string{"/tmp:data"}}, "绝对路径"},
	}
	for _, tc := range cases {
		code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/create"), tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d %v", tc.name, code, env)
		}
		msg, _ := env["data"].(string)
		if !strings.Contains(msg, tc.want) {
			t.Fatalf("%s 错误消息应含 %q: %s", tc.name, tc.want, msg)
		}
	}
}

// TestBastilleMountSourceValidation 挂载端点宿主源白名单（桩平台下 400 早于 501）。
func TestBastilleMountSourceValidation(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(t, d)
	defer srv.Close()

	// 根目录作为宿主源：400
	code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"),
		map[string]any{"src": "/", "dst": "/data", "fstype": "nullfs", "options": "rw"})
	if code != http.StatusBadRequest {
		t.Fatalf("根目录宿主源应 400: %d %v", code, env)
	}
	// 相对宿主源：400
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"),
		map[string]any{"src": "data/mc", "dst": "/data", "fstype": "nullfs", "options": "rw"})
	if code != http.StatusBadRequest {
		t.Fatalf("相对宿主源应 400: %d %v", code, env)
	}
	// 目标含 .. ：400
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"),
		map[string]any{"src": "/tmp", "dst": "/..", "fstype": "nullfs", "options": "rw"})
	if code != http.StatusBadRequest {
		t.Fatalf("目标含 .. 应 400: %d %v", code, env)
	}
}
