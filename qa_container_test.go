// 容器环境 API 测试（NODE_API.md §6.1 / docs/irix-node-container-api.md）。
// 归档端点（§4.8）平台无关，可全流程测试；Bastille 端点在非 FreeBSD 平台
// 走桩路径（统一 501），测试验证路由注册与参数校验（不 404、中文错误消息）。

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// containerTestURL 拼接带 apikey 的完整 URL。
func containerTestURL(srv *httptest.Server, path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return srv.URL + path + sep + "apikey=test-key"
}

// doJSON 发起带 JSON 请求体的请求，返回 HTTP 状态码与解析后的信封。
func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("响应不是 JSON 信封: %v（body=%s）", err, string(data))
	}
	return resp.StatusCode, envelope
}

// doRaw 发起任意方法请求，返回状态码与原始响应字节。
func doRaw(t *testing.T, method, url string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// writeTestZip 生成包含指定条目的 zip 文件（测试 zip-slip 用）。
func writeTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 zip 失败: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目失败: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("写入 zip 条目失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
}

// multipartUpload 构造 multipart 表单（字段名 file，内容为字节）。
func multipartUpload(t *testing.T, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("创建 multipart 字段失败: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("写入 multipart 失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// ---------------------------------------------------------------------------
// 节点级归档（docs/irix-node-container-api.md §4.8）全流程
// ---------------------------------------------------------------------------

func TestArchiveFullFlow(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 源目录：嵌套结构 + 文件内容
	srcDir := t.TempDir()
	files := map[string]string{
		"server.properties":       "motd=hello\n",
		"world/region/r.0.0.mca":  "binary-region-data",
		"mods/sodium-0.5.8.jar":   "jar-bytes",
		"plugins/EssentialsX.jar": "plugin-bytes",
	}
	for rel, content := range files {
		p := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写入文件失败: %v", err)
		}
	}

	// 1. 压缩（缺省自动命名）
	code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive"),
		map[string]any{"path": srcDir})
	if code != 200 {
		t.Fatalf("POST archive 失败: %d %v", code, env)
	}
	data := env["data"].(map[string]any)
	archivePath, _ := data["path"].(string)
	if archivePath == "" {
		t.Fatalf("POST archive 未返回 path: %v", env)
	}
	archiveName := filepath.Base(archivePath)
	if !strings.HasSuffix(archiveName, ".zip") {
		t.Fatalf("自动命名的归档应以 .zip 结尾: %s", archiveName)
	}

	// 2. 原始字节下载
	code, raw := doRaw(t, http.MethodGet, containerTestURL(srv, "/api/container/archive?file="+archiveName), nil, "")
	if code != 200 {
		t.Fatalf("GET archive 失败: %d", code)
	}
	// 下载内容应为一个合法 zip（可打开且条目齐全）
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("下载的归档不是合法 zip: %v", err)
	}
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	for rel := range files {
		if !got[filepath.ToSlash(rel)] {
			t.Fatalf("归档缺少条目 %s（现有: %v）", rel, got)
		}
	}

	// 3. 上传（multipart 原始字节）
	uploadName := "uploaded-archive.zip"
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", uploadName)
	if err != nil {
		t.Fatalf("创建 multipart 字段失败: %v", err)
	}
	if _, err := fw.Write([]byte("uploaded-raw-bytes")); err != nil {
		t.Fatalf("写入 multipart 失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	code, rawResp := doRaw(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/upload"),
		&buf, mw.FormDataContentType())
	if code != 200 {
		t.Fatalf("POST archive/upload 失败: %d %s", code, string(rawResp))
	}
	var upEnv map[string]any
	if err := json.Unmarshal(rawResp, &upEnv); err != nil {
		t.Fatalf("上传响应不是 JSON: %v", err)
	}
	uploaded := upEnv["data"].(map[string]any)
	if !strings.HasSuffix(uploaded["path"].(string), uploadName) {
		t.Fatalf("上传归档路径异常: %v", uploaded)
	}
	code, raw = doRaw(t, http.MethodGet, containerTestURL(srv, "/api/container/archive?file="+uploadName), nil, "")
	if code != 200 || string(raw) != "uploaded-raw-bytes" {
		t.Fatalf("上传后再下载字节不一致: %d %q", code, string(raw))
	}

	// 4. 恢复（覆盖式解压到 destPath）
	destDir := t.TempDir()
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": archiveName, "destPath": destDir})
	if code != 200 {
		t.Fatalf("POST archive/restore 失败: %d %v", code, env)
	}
	for rel, content := range files {
		p := filepath.Join(destDir, filepath.FromSlash(rel))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("恢复后缺少文件 %s: %v", rel, err)
		}
		if string(b) != content {
			t.Fatalf("恢复后文件内容不一致 %s: %q != %q", rel, string(b), content)
		}
	}

	// 5. 单文件归档
	singleFile := filepath.Join(t.TempDir(), "level.dat")
	if err := os.WriteFile(singleFile, []byte("level-data"), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive"),
		map[string]any{"path": singleFile, "archive": "level-backup.zip"})
	if code != 200 {
		t.Fatalf("单文件压缩失败: %d %v", code, env)
	}
	dest2 := t.TempDir()
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": "level-backup.zip", "destPath": dest2})
	if code != 200 {
		t.Fatalf("单文件恢复失败: %d %v", code, env)
	}
	b, err := os.ReadFile(filepath.Join(dest2, "level.dat"))
	if err != nil || string(b) != "level-data" {
		t.Fatalf("单文件恢复内容不一致: %v %q", err, string(b))
	}

	// 6. 指定 archive 名 + 不存在路径
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive"),
		map[string]any{"path": filepath.Join(t.TempDir(), "not-exists")})
	if code != 400 {
		t.Fatalf("不存在的路径应 400: %d", code)
	}
}

// ---------------------------------------------------------------------------
// 归档安全：文件名穿越 / zip-slip / 参数校验
// ---------------------------------------------------------------------------

func TestArchiveSecurity(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 下载文件名穿越
	for _, evil := range []string{"../etc/passwd", "..\\..\\x", "..", ".", "a/b.zip", ""} {
		code, _ := doRaw(t, http.MethodGet, containerTestURL(srv, "/api/container/archive?file="+evil), nil, "")
		if code == 200 {
			t.Fatalf("穿越文件名 %q 不应可下载", evil)
		}
	}
	// 不存在的归档
	code, _ := doRaw(t, http.MethodGet, containerTestURL(srv, "/api/container/archive?file=missing.zip"), nil, "")
	if code != 404 {
		t.Fatalf("不存在的归档应 404: %d", code)
	}
	// restore 文件名校验
	code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": "../evil.zip", "destPath": t.TempDir()})
	if code != 400 {
		t.Fatalf("restore 穿越文件名应 400: %d %v", code, env)
	}
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": "evil.zip", "destPath": "relative/path"})
	if code != 400 {
		t.Fatalf("restore 相对 destPath 应 400: %d %v", code, env)
	}

	// zip-slip：归档条目 ../ 与绝对路径必须拒绝
	slipDir := t.TempDir()
	evilZip := filepath.Join(slipDir, "evil.zip")
	writeTestZip(t, evilZip, map[string]string{"../escaped.txt": "evil", "world/ok.txt": "ok"})
	if err := os.MkdirAll(d.archiveDir(), 0o755); err != nil {
		t.Fatalf("创建归档目录失败: %v", err)
	}
	raw, err := os.ReadFile(evilZip)
	if err != nil {
		t.Fatalf("读取 zip 失败: %v", err)
	}
	body, ctype := multipartUpload(t, "evil.zip", raw)
	code, rawResp := doRaw(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/upload"),
		body, ctype)
	if code != 200 {
		t.Fatalf("上传 zip 失败: %d %s", code, string(rawResp))
	}
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": "evil.zip", "destPath": t.TempDir()})
	if code != 400 {
		t.Fatalf("zip-slip 条目应拒绝恢复: %d %v", code, env)
	}
	if _, err := os.Stat(filepath.Join(slipDir, "escaped.txt")); err == nil {
		t.Fatalf("zip-slip 文件被写出到归档目录外")
	}

	// 绝对路径条目
	absZip := filepath.Join(t.TempDir(), "abs.zip")
	writeTestZip(t, absZip, map[string]string{"/tmp/evil.txt": "evil"})
	raw, _ = os.ReadFile(absZip)
	body, ctype = multipartUpload(t, "abs.zip", raw)
	code, _ = doRaw(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/upload"),
		body, ctype)
	if code != 200 {
		t.Fatalf("上传 abs.zip 失败: %d", code)
	}
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/restore"),
		map[string]any{"file": "abs.zip", "destPath": t.TempDir()})
	if code != 400 {
		t.Fatalf("绝对路径条目应拒绝恢复: %d %v", code, env)
	}

	// 上传缺 file 字段
	code, _ = doRaw(t, http.MethodPost, containerTestURL(srv, "/api/container/archive/upload"), nil, "")
	if code != 400 {
		t.Fatalf("缺 file 字段应 400: %d", code)
	}
}

// ---------------------------------------------------------------------------
// Bastille 桩路由冒烟（Windows 走桩，验证路由注册 + 参数校验）
// ---------------------------------------------------------------------------

func TestBastilleRoutesSmoke(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		t.Skip("FreeBSD 上 Bastille 为真实实现（非桩），此冒烟测试仅覆盖桩路径")
	}
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 新契约 create body（vnet 布尔 / bridge / mac）应能解析通过参数校验，
	// 平台不支持时由桩返回 501（而非 400 或 404）。
	code, env := doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/create"),
		map[string]any{
			"name": "mc-test", "release": "14.2-RELEASE", "ip": "10.0.0.2/24",
			"type": "thin", "vnet": true, "bridge": "bridge0", "mac": "02:00:00:00:00:01",
		})
	if code != 501 {
		t.Fatalf("create（新契约）应 501: %d %v", code, env)
	}
	// 纯数字 jail 名：参数校验 400（早于桩）
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/create"),
		map[string]any{"name": "12345"})
	if code != 400 || !strings.Contains(env["data"].(string), "数字") {
		t.Fatalf("纯数字 jail 名应 400 并含中文提示: %d %v", code, env)
	}

	// 运行会话：桩 501（路由已注册）
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/bastille/jails/mc-1/run", map[string]any{"command": "java -jar server.jar", "cwd": "/data", "watch": true}},
		{http.MethodGet, "/api/bastille/jails/mc-1/run/s-1?tail=100&since=0", nil},
		{http.MethodPost, "/api/bastille/jails/mc-1/run/s-1/stdin", map[string]any{"input": "stop\n"}},
		{http.MethodPost, "/api/bastille/jails/mc-1/run/s-1/stop", nil},
		{http.MethodDelete, "/api/bastille/jails/mc-1/run/s-1", nil},
	} {
		code, env := doJSON(t, tc.method, containerTestURL(srv, tc.path), tc.body)
		if code != 501 {
			t.Fatalf("%s %s 应 501: %d %v", tc.method, tc.path, code, env)
		}
	}
	// run 缺 command：400
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/run"), map[string]any{})
	if code != 400 {
		t.Fatalf("run 缺 command 应 400: %d %v", code, env)
	}

	// pkg：桩 501；缺 action / install 缺 packages → 400
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/pkg"),
		map[string]any{"action": "install", "packages": []string{"openjdk17-jre"}})
	if code != 501 {
		t.Fatalf("pkg 应 501: %d %v", code, env)
	}
	code, _ = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/pkg"), map[string]any{})
	if code != 400 {
		t.Fatalf("pkg 缺 action 应 400: %d", code)
	}
	code, _ = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/pkg"),
		map[string]any{"action": "install"})
	if code != 400 {
		t.Fatalf("pkg install 缺 packages 应 400: %d", code)
	}

	// config：GET/POST/DELETE 桩 501；缺 key → 400
	code, _ = doJSON(t, http.MethodGet, containerTestURL(srv, "/api/bastille/jails/mc-1/config"), nil)
	if code != 501 {
		t.Fatalf("config GET 应 501: %d", code)
	}
	code, env = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/config"),
		map[string]any{"key": "hostname", "value": "mc-1"})
	if code != 501 {
		t.Fatalf("config POST 应 501: %d %v", code, env)
	}
	code, _ = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/config"), map[string]any{})
	if code != 400 {
		t.Fatalf("config POST 缺 key 应 400: %d", code)
	}
	code, _ = doJSON(t, http.MethodDelete, containerTestURL(srv, "/api/bastille/jails/mc-1/config?key=hostname"), nil)
	if code != 501 {
		t.Fatalf("config DELETE 应 501: %d", code)
	}
	code, _ = doJSON(t, http.MethodDelete, containerTestURL(srv, "/api/bastille/jails/mc-1/config"), nil)
	if code != 400 {
		t.Fatalf("config DELETE 缺 key 应 400: %d", code)
	}

	// mounts：GET/POST/DELETE 桩 501；缺 dst → 400
	code, _ = doJSON(t, http.MethodGet, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"), nil)
	if code != 501 {
		t.Fatalf("mounts GET 应 501: %d", code)
	}
	code, _ = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"),
		map[string]any{"src": "/data/mc", "dst": "/data", "fstype": "nullfs", "options": "rw"})
	if code != 501 {
		t.Fatalf("mounts POST 应 501: %d", code)
	}
	code, _ = doJSON(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"),
		map[string]any{"src": "/data/mc"})
	if code != 400 {
		t.Fatalf("mounts POST 缺 dst 应 400: %d", code)
	}
	code, _ = doJSON(t, http.MethodDelete, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts?dst=/data"), nil)
	if code != 501 {
		t.Fatalf("mounts DELETE 应 501: %d", code)
	}
	code, _ = doJSON(t, http.MethodDelete, containerTestURL(srv, "/api/bastille/jails/mc-1/mounts"), nil)
	if code != 400 {
		t.Fatalf("mounts DELETE 缺 dst 应 400: %d", code)
	}

	// jail 文件管理：桩 501（路由已注册）；缺 path / 删根目录 → 400
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/bastille/jails/mc-1/files?path=/data", nil},
		{http.MethodGet, "/api/bastille/jails/mc-1/files/content?path=/data/x.txt", nil},
		{http.MethodPut, "/api/bastille/jails/mc-1/files/content", map[string]any{"path": "/data/x.txt", "content": "hello"}},
		{http.MethodDelete, "/api/bastille/jails/mc-1/files?path=/data/x.txt", nil},
		{http.MethodPost, "/api/bastille/jails/mc-1/files/mkdir", map[string]any{"path": "/data/new"}},
		{http.MethodPost, "/api/bastille/jails/mc-1/files/touch", map[string]any{"path": "/data/new.txt"}},
	} {
		code, env := doJSON(t, tc.method, containerTestURL(srv, tc.path), tc.body)
		if code != 501 {
			t.Fatalf("%s %s 应 501: %d %v", tc.method, tc.path, code, env)
		}
	}
	// upload：multipart 载荷，路径解析在表单解析前 → 桩 501
	upBody, upCtype := multipartUpload(t, "a.txt", []byte("hi"))
	code, _ = doRaw(t, http.MethodPost, containerTestURL(srv, "/api/bastille/jails/mc-1/files/upload?path=/data"), upBody, upCtype)
	if code != 501 {
		t.Fatalf("files/upload 应 501: %d", code)
	}
	// 缺 path → 400
	code, _ = doJSON(t, http.MethodGet, containerTestURL(srv, "/api/bastille/jails/mc-1/files/content"), nil)
	if code != 400 {
		t.Fatalf("content GET 缺 path 应 400: %d", code)
	}
	code, _ = doRaw(t, http.MethodGet, containerTestURL(srv, "/api/bastille/jails/mc-1/files/download"), nil, "")
	if code != 400 {
		t.Fatalf("download 缺 path 应 400: %d", code)
	}
	code, _ = doJSON(t, http.MethodDelete, containerTestURL(srv, "/api/bastille/jails/mc-1/files"), nil)
	if code != 400 {
		t.Fatalf("files DELETE 缺 path（删根）应 400: %d", code)
	}

	// 未注册路由应 404（确认上面 501 是桩路径而非路由缺失）
	code, _ = doRaw(t, http.MethodGet, containerTestURL(srv, "/api/bastille/jails/mc-1/not-a-route"), nil, "")
	if code != 404 {
		t.Fatalf("未注册路由应 404: %d", code)
	}
}

// ---------------------------------------------------------------------------
// jail 文件管理全流程（bastilleResolve 替身为临时目录，任意平台可测）
// ---------------------------------------------------------------------------

func TestBastilleJailFiles(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()

	// 伪造 jail 根：生产实现把 jail 内路径解析到 jails/<name>/root，
	// 这里用临时目录承接同一套 resolveJailHostPath 逻辑与处理器。
	root := filepath.Join(t.TempDir(), "jail-root")
	if err := os.MkdirAll(filepath.Join(root, "data", "sub"), 0o755); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "server.jar"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("创建测试文件失败: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("创建外部文件失败: %v", err)
	}
	orig := bastilleResolve
	bastilleResolve = func(name, jailPath string) (string, error) {
		if name != "mc-1" {
			return "", fmt.Errorf("%w: jail %s 不存在", errJailPath, name)
		}
		return resolveJailHostPath(root, jailPath)
	}
	defer func() { bastilleResolve = orig }()
	url := func(p string) string { return containerTestURL(srv, p) }

	// 列表：根目录（缺 path 默认）
	code, env := doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files"), nil)
	if code != 200 {
		t.Fatalf("根目录列表应 200: %d %v", code, env)
	}
	data := env["data"].(map[string]any)
	if int(data["total"].(float64)) != 1 {
		t.Fatalf("根目录 total 应 1: %v", data)
	}
	rootItems := data["items"].([]any)
	if rootItems[0].(map[string]any)["name"] != "data" ||
		rootItems[0].(map[string]any)["isDir"] != true ||
		rootItems[0].(map[string]any)["path"] != "/data" {
		t.Fatalf("根目录条目契约不符: %v", rootItems[0])
	}

	// 列表：/data（绝对与相对路径等价），目录在前，条目字段契约
	for _, p := range []string{"/api/bastille/jails/mc-1/files?path=/data",
		"/api/bastille/jails/mc-1/files?path=data"} {
		code, env = doJSON(t, http.MethodGet, url(p), nil)
		if code != 200 {
			t.Fatalf("列表 %s 应 200: %d %v", p, code, env)
		}
		data = env["data"].(map[string]any)
		if int(data["total"].(float64)) != 2 {
			t.Fatalf("列表 %s total 应 2: %v", p, data)
		}
		items := data["items"].([]any)
		first := items[0].(map[string]any)
		if first["name"] != "sub" || first["isDir"] != true || first["path"] != "/data/sub" {
			t.Fatalf("列表 %s 目录应在前: %v", p, first)
		}
		second := items[1].(map[string]any)
		if second["name"] != "server.jar" || second["isDir"] != false ||
			second["path"] != "/data/server.jar" || int(second["size"].(float64)) != 11 {
			t.Fatalf("列表 %s 文件条目契约不符: %v", p, second)
		}
		if second["mtime"].(string) == "" {
			t.Fatalf("列表 %s mtime 不应为空", p)
		}
	}

	// 分页：page_size=1 第二页为 server.jar
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files?path=/data&page=2&page_size=1"), nil)
	if code != 200 {
		t.Fatalf("分页列表应 200: %d %v", code, env)
	}
	data = env["data"].(map[string]any)
	pageItems := data["items"].([]any)
	if int(data["total"].(float64)) != 2 || len(pageItems) != 1 ||
		pageItems[0].(map[string]any)["name"] != "server.jar" {
		t.Fatalf("分页结果不符: %v", data)
	}

	// 文本读取：命中 / 目录拒绝
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/server.jar"), nil)
	if code != 200 || env["data"] != "hello world" {
		t.Fatalf("读取内容应 200 hello world: %d %v", code, env)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data"), nil)
	if code != 400 || !strings.Contains(env["data"].(string), "目录") {
		t.Fatalf("目录按文本读取应 400: %d %v", code, env)
	}

	// 文本写入（含空内容覆盖）
	code, env = doJSON(t, http.MethodPut, url("/api/bastille/jails/mc-1/files/content"),
		map[string]any{"path": "/data/sub/new.txt", "content": "abc"})
	if code != 200 {
		t.Fatalf("写入内容应 200: %d %v", code, env)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/sub/new.txt"), nil)
	if code != 200 || env["data"] != "abc" {
		t.Fatalf("回读内容应 abc: %d %v", code, env)
	}
	code, _ = doJSON(t, http.MethodPut, url("/api/bastille/jails/mc-1/files/content"),
		map[string]any{"path": "/data/sub/new.txt", "content": ""})
	if code != 200 {
		t.Fatalf("空内容覆盖应 200: %d", code)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/sub/new.txt"), nil)
	if code != 200 || env["data"] != "" {
		t.Fatalf("空内容回读应空串: %d %v", code, env)
	}

	// 新建目录 / 空文件
	code, _ = doJSON(t, http.MethodPost, url("/api/bastille/jails/mc-1/files/mkdir"),
		map[string]any{"path": "/data/created/deep"})
	if code != 200 {
		t.Fatalf("mkdir 应 200: %d", code)
	}
	code, _ = doJSON(t, http.MethodPost, url("/api/bastille/jails/mc-1/files/touch"),
		map[string]any{"path": "/data/created/empty.txt"})
	if code != 200 {
		t.Fatalf("touch 应 200: %d", code)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files?path=/data"), nil)
	if code != 200 || int(env["data"].(map[string]any)["total"].(float64)) != 3 {
		t.Fatalf("mkdir 后 /data total 应 3: %d %v", code, env)
	}

	// 上传（multipart）→ 响应保存路径
	upRawBody, upRawCtype := multipartUpload(t, "up.txt", []byte("uploaded"))
	rawCode, rawData := doRaw(t, http.MethodPost, url("/api/bastille/jails/mc-1/files/upload?path=/data/sub"), upRawBody, upRawCtype)
	if rawCode != 200 {
		t.Fatalf("上传应 200: %d %s", rawCode, string(rawData))
	}
	var upEnv map[string]any
	if err := json.Unmarshal(rawData, &upEnv); err != nil {
		t.Fatalf("上传响应不是 JSON: %v", err)
	}
	if upEnv["data"].(map[string]any)["path"] != "/data/sub/up.txt" {
		t.Fatalf("上传响应 path 不符: %v", upEnv)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/sub/up.txt"), nil)
	if code != 200 || env["data"] != "uploaded" {
		t.Fatalf("上传后回读失败: %d %v", code, env)
	}

	// 下载（二进制流）；目录下载拒绝
	rawCode, rawData = doRaw(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/download?path=/data/sub/up.txt"), nil, "")
	if rawCode != 200 || string(rawData) != "uploaded" {
		t.Fatalf("下载应 200 uploaded: %d %q", rawCode, string(rawData))
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/download?path=/data"), nil)
	if code != 400 {
		t.Fatalf("目录下载应 400: %d %v", code, env)
	}

	// 删除（递归、幂等）
	code, _ = doJSON(t, http.MethodDelete, url("/api/bastille/jails/mc-1/files?path=/data/created"), nil)
	if code != 200 {
		t.Fatalf("删除应 200: %d", code)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files?path=/data"), nil)
	if code != 200 || int(env["data"].(map[string]any)["total"].(float64)) != 2 {
		t.Fatalf("删除后 /data total 应 2: %d %v", code, env)
	}
	code, _ = doJSON(t, http.MethodDelete, url("/api/bastille/jails/mc-1/files?path=/data/created"), nil)
	if code != 200 {
		t.Fatalf("重复删除应幂等 200: %d", code)
	}

	// 路径越界：.. 逃逸 → 400
	for _, p := range []string{
		"/api/bastille/jails/mc-1/files?path=/../..",
		"/api/bastille/jails/mc-1/files?path=../../..",
		"/api/bastille/jails/mc-1/files/content?path=/../../outside.txt",
		"/api/bastille/jails/mc-1/files/download?path=/../../outside.txt",
	} {
		code, env = doJSON(t, http.MethodGet, url(p), nil)
		if code != 400 || !strings.Contains(env["data"].(string), "越界") {
			t.Fatalf("越界请求应 400: %s → %d %v", p, code, env)
		}
	}
	// 不存在的 jail → 400
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-x/files"), nil)
	if code != 400 || !strings.Contains(env["data"].(string), "不存在") {
		t.Fatalf("jail 不存在应 400: %d %v", code, env)
	}

	// 超大文件文本读取 → 400（提示走下载接口）
	big := make([]byte, maxTextReadBytes+1)
	if err := os.WriteFile(filepath.Join(root, "data", "big.bin"), big, 0o644); err != nil {
		t.Fatalf("创建大文件失败: %v", err)
	}
	code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/big.bin"), nil)
	if code != 400 || !strings.Contains(env["data"].(string), "文件过大") {
		t.Fatalf("超大文件读取应 400: %d %v", code, env)
	}

	// 符号链接逃逸 → 400（Windows 需开发者模式，创建失败则跳过该项）
	if err := os.Symlink(outside, filepath.Join(root, "data", "leak")); err == nil {
		code, env = doJSON(t, http.MethodGet, url("/api/bastille/jails/mc-1/files/content?path=/data/leak"), nil)
		if code != 400 || !strings.Contains(env["data"].(string), "符号链接") {
			t.Fatalf("符号链接逃逸读取应 400: %d %v", code, env)
		}
		code, _ = doJSON(t, http.MethodPut, url("/api/bastille/jails/mc-1/files/content"),
			map[string]any{"path": "/data/leak", "content": "x"})
		if code != 400 {
			t.Fatalf("符号链接逃逸写入应 400: %d", code)
		}
	}
}
