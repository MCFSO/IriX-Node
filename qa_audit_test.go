// 审计日志测试：验证每次 API 请求的完整细节（时间/IP/方法/路径与查询/状态码/
// 耗时/请求体）被记录到审计日志文件，apikey 明文打码、超长请求体截断、
// 关闭开关生效。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAuditServer 启动带审计中间件的测试服务器（与 main 相同的中间件链）。
func newAuditServer(d *Daemon) *httptest.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(d.auditMiddleware(limitAPIBody(mux)))
}

// bodyHasStatus 解析 MCSM 风格响应体，判断 status 字段是否等于 want。
// 错误响应 HTTP 状态码与 body.status 一致（writeError 透传）。
func bodyHasStatus(body []byte, want int) bool {
	var resp struct {
		Status int `json:"status"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	return resp.Status == want
}

// TestAuditLogRecordsEveryRequest 端到端验证：请求细节全部落盘，apikey 不打明文。
func TestAuditLogRecordsEveryRequest(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.AuditLog = newFileLogger(filepath.Join(dir, "logs"), "audit.log", 1<<20)
	// 提前注册清理：断言失败（Fatalf）时也要排空审计落盘，避免文件句柄泄漏
	t.Cleanup(func() { d.AuditLog.Close() })
	srv := newAuditServer(d)
	defer srv.Close()

	// 创建实例（POST，带请求体）
	cwd := filepath.ToSlash(dir)
	body := `{"nickname":"审计测试","startCommand":"ping 127.0.0.1 -t","cwd":"` + cwd + `"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/instance?apikey=test-key", strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("创建实例请求失败: %v", err)
	}
	defer resp.Body.Close()
	var created struct {
		Data struct {
			InstanceUuid string `json:"instanceUuid"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	if created.Data.InstanceUuid == "" {
		t.Fatalf("未创建成功: %s", resp.Status)
	}

	// 查询实例详情（GET，查询参数）
	if _, body := doReq(t, srv.URL+"/api/instance?uuid="+created.Data.InstanceUuid+"&apikey=test-key"); !bodyHasStatus(body, 200) {
		t.Fatalf("查询详情失败: %s", body)
	}
	// 下发命令（GET，command 查询参数；实例未运行 → body.status 400，但审计同样记录命令）
	if _, body := doReq(t, srv.URL+"/api/protected_instance/command?uuid="+created.Data.InstanceUuid+
		"&command=stop&apikey=test-key"); bodyHasStatus(body, 200) {
		t.Fatalf("未运行实例下发命令应失败: %s", body)
	}

	// 必须先排空落盘队列再读文件（Close 幂等，t.Cleanup 兜底 Fatalf 路径）
	d.AuditLog.Close()
	data, err := os.ReadFile(filepath.Join(dir, "logs", "audit.log"))
	if err != nil {
		t.Fatalf("读取审计日志失败: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"POST /api/instance", // 方法 + 路径
		"审计测试",               // 请求体内容
		"GET /api/instance",
		"command=stop", // 用户下发的命令
		"apikey=***",   // apikey 打码
		"200",          // 响应状态码
	} {
		if !strings.Contains(got, want) {
			t.Errorf("审计日志缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "test-key") {
		t.Errorf("apikey 明文泄漏进审计日志:\n%s", got)
	}
	// 每条审计行单行（防伪造/防错乱）
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Count(line, "|") < 5 {
			t.Errorf("审计行字段不完整: %q", line)
		}
	}
}

// TestAuditBodyCaptureTruncates 请求体捕获：只保留前 auditBodyMax 字节并标记截断。
func TestAuditBodyCaptureTruncates(t *testing.T) {
	big := strings.Repeat("x", auditBodyMax+100)
	ab := &auditBody{ReadCloser: io.NopCloser(strings.NewReader(big))}
	got, err := io.ReadAll(ab)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != big {
		t.Fatalf("捕获包装改变了读取内容")
	}
	if len(ab.kept) != auditBodyMax || !ab.truncated {
		t.Errorf("超长请求体应保留 %d 字节并标记截断，实际 %d 字节 truncated=%v",
			auditBodyMax, len(ab.kept), ab.truncated)
	}
	// 未超限：完整保留且不标记
	small := "hello"
	ab2 := &auditBody{ReadCloser: io.NopCloser(strings.NewReader(small))}
	_, _ = io.ReadAll(ab2)
	if string(ab2.kept) != small || ab2.truncated {
		t.Errorf("短请求体应完整保留，实际 %q truncated=%v", ab2.kept, ab2.truncated)
	}
}

// TestAuditDisabledNoFile 关闭审计落盘（AuditLog 为 nil）时不产生文件、不崩溃。
func TestAuditDisabledNoFile(t *testing.T) {
	d, dir := newTestDaemon(t)
	srv := newAuditServer(d) // 未设置 AuditLog
	defer srv.Close()
	if code, _ := doReq(t, srv.URL+"/api/overview?apikey=test-key"); code != 200 {
		t.Fatalf("状态码: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "audit.log")); !os.IsNotExist(err) {
		t.Errorf("审计日志未启用时不应落盘")
	}
}

// TestRedactQuery apikey 打码：任意大小写键名均被打码，其余参数保持原样。
func TestRedactQuery(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"apikey=secret":                       "?apikey=***",
		"uuid=abc&apikey=secret&command=stop": "?uuid=abc&apikey=***&command=stop",
		"APiKey=SECRET":                       "?apikey=***",
		"a=1&b=2":                             "?a=1&b=2",
	}
	for in, want := range cases {
		if got := redactQuery(in); got != want {
			t.Errorf("redactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRedactPath 直连通道路径中的票据密码必须打码（审计报告 #3）：
// 票据是 10 分钟有效的免密凭据，日志可读者拿到明文即可在有效期内免密下载。
func TestRedactPath(t *testing.T) {
	cases := map[string]string{
		"/api/overview":                    "/api/overview",
		"/download/":                       "/download/",
		"/download/abc123/private.txt":     "/download/***/private.txt",
		"/download/abc123/sub/private.txt": "/download/***/sub/private.txt",
		"/download/abc123":                 "/download/***",
		"/upload/abc123":                   "/upload/***",
		"/upload/":                         "/upload/",
		"/api/files/download":              "/api/files/download",
	}
	for in, want := range cases {
		if got := redactPath(in); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSanitizeLog 控制字符转义：防止恶意请求体换行伪造审计行。
func TestSanitizeLog(t *testing.T) {
	if got := sanitizeLog("a\nb\tc\rd"); got != `a\nb\tc\rd` {
		t.Errorf("sanitizeLog 结果 = %q", got)
	}
	if got := sanitizeLog("正常日志"); got != "正常日志" {
		t.Errorf("无控制字符时不应改写: %q", got)
	}
}

// TestAuditLogReadDisabled 未启用审计落盘（AuditLog 为 nil）时，
// GET /api/audit/log 返回 200 与空字符串（docs/backend-requirements.md P0）。
func TestAuditLogReadDisabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	srv := newTestServer(d)
	defer srv.Close()
	code, body := doReq(t, srv.URL+"/api/audit/log?apikey=test-key")
	if code != http.StatusOK || !bodyHasStatus(body, http.StatusOK) {
		t.Fatalf("期望 200，实际 %d: %s", code, body)
	}
	if !strings.Contains(string(body), `"data":""`) {
		t.Errorf("未启用审计落盘时 data 应为空字符串: %s", body)
	}
}

// auditLogData 解析审计日志响应体的 data 字符串字段。
func auditLogData(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应失败: %v（%s）", err, body)
	}
	return resp.Data
}

// TestAuditLogReadEmpty 审计日志文件尚不存在时返回空字符串（200），不报错。
func TestAuditLogReadEmpty(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.AuditLog = newFileLogger(filepath.Join(dir, "logs"), "audit.log", 1<<20)
	t.Cleanup(func() { d.AuditLog.Close() })
	srv := newTestServer(d)
	defer srv.Close()
	code, body := doReq(t, srv.URL+"/api/audit/log?apikey=test-key")
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", code, body)
	}
	if got := auditLogData(t, body); got != "" {
		t.Errorf("无日志文件时 data 应为空字符串，实际 %q", got)
	}
}

// TestAuditLogReadTailSince 端到端验证审计日志读取接口（docs/backend-requirements.md P0）：
// tail 截取最后 N 行、since 增量过滤、tail 上限钳制。
func TestAuditLogReadTailSince(t *testing.T) {
	d, dir := newTestDaemon(t)
	d.AuditLog = newFileLogger(filepath.Join(dir, "logs"), "audit.log", 1<<20)
	t.Cleanup(func() { d.AuditLog.Close() })
	srv := newTestServer(d)
	defer srv.Close()

	// 写入 5 条审计行并排空落盘队列（Close 幂等，Cleanup 兜底 Fatalf 路径）
	for i := 1; i <= 5; i++ {
		d.auditLogf("审计读取测试行 %d", i)
	}
	d.AuditLog.Close()

	// tail 缺省（500）→ 包含全部 5 行
	_, body := doReq(t, srv.URL+"/api/audit/log?apikey=test-key")
	got := auditLogData(t, body)
	for i := 1; i <= 5; i++ {
		if !strings.Contains(got, fmt.Sprintf("审计读取测试行 %d", i)) {
			t.Errorf("缺省 tail 缺少行 %d:\n%s", i, got)
		}
	}
	// tail=2 → 恰好最后 2 行（按序：行 4、行 5）
	_, body = doReq(t, srv.URL+"/api/audit/log?tail=2&apikey=test-key")
	got = auditLogData(t, body)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], "审计读取测试行 4") ||
		!strings.Contains(lines[1], "审计读取测试行 5") {
		t.Errorf("tail=2 应恰好返回最后 2 行，实际 %d 行:\n%s", len(lines), got)
	}
	// since=0（1970）→ 全部内容（文件 mtime 恒在其后）
	_, body = doReq(t, srv.URL+"/api/audit/log?since=0&apikey=test-key")
	got = auditLogData(t, body)
	if !strings.Contains(got, "审计读取测试行 1") || !strings.Contains(got, "审计读取测试行 5") {
		t.Errorf("since=0 应返回全部内容:\n%s", got)
	}
	// since=未来时间 → 空（mtime 近似过滤）
	_, body = doReq(t, srv.URL+"/api/audit/log?since="+fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())+"&apikey=test-key")
	if got := auditLogData(t, body); got != "" {
		t.Errorf("since=未来时间应返回空，实际 %q", got)
	}
	// tail 上限钳制：超大 tail 应钳到 20000 而非报错
	code, body := doReq(t, srv.URL+"/api/audit/log?tail=999999&apikey=test-key")
	if code != http.StatusOK || !bodyHasStatus(body, http.StatusOK) {
		t.Errorf("超大 tail 应被钳制而非报错: %d %s", code, body)
	}
}

// TestAuditLogArchive 审计日志轮转归档（等保二级「审计记录保护与定期备份」）：
// 每次轮转把将被覆盖的审计段复制到归档目录，防止轮转覆盖丢失历史。
func TestAuditLogArchive(t *testing.T) {
	dir := t.TempDir()
	archive := t.TempDir()
	f := newFileLogger(dir, "audit.log", 1024)
	f.archiveDir = archive
	line := bytes.Repeat([]byte("A"), 200) // 200B/段
	for i := 0; i < 20; i++ {
		_, _ = f.Write(line)
	}
	f.Close()
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("归档文件应 ≥2 个，实际 %d（轮转未触发或归档未生效）", len(entries))
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(archive, e.Name()))
		if err != nil {
			t.Fatalf("读取归档失败 %s: %v", e.Name(), err)
		}
		if len(data) < 200 {
			t.Fatalf("归档内容异常（应含完整审计段）: %s 仅 %d 字节", e.Name(), len(data))
		}
	}
}
