// 审计日志测试：验证每次 API 请求的完整细节（时间/IP/方法/路径与查询/状态码/
// 耗时/请求体）被记录到审计日志文件，apikey 明文打码、超长请求体截断、
// 关闭开关生效。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newAuditServer 启动带审计中间件的测试服务器（与 main 相同的中间件链）。
func newAuditServer(d *Daemon) *httptest.Server {
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	return httptest.NewServer(d.auditMiddleware(limitAPIBody(mux)))
}

// bodyHasStatus 解析 MCSM 风格响应体，判断 status 字段是否等于 want。
// API 的 HTTP 状态码恒为 200，真实结果在 body.status 中。
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

// TestSanitizeLog 控制字符转义：防止恶意请求体换行伪造审计行。
func TestSanitizeLog(t *testing.T) {
	if got := sanitizeLog("a\nb\tc\rd"); got != `a\nb\tc\rd` {
		t.Errorf("sanitizeLog 结果 = %q", got)
	}
	if got := sanitizeLog("正常日志"); got != "正常日志" {
		t.Errorf("无控制字符时不应改写: %q", got)
	}
}
