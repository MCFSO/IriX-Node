// 权限目录一致性测试（docs/accounts-design.md §权限模型）：
// 目录必须与实际注册的路由一一对应。
//   - 正向：目录中每个端点都能在 ServeMux 匹配到相同模式（r.Pattern 一致）；
//   - 反向：扫描路由注册源文件，除直连票据通道与公开登录入口外，
//     每个 mux.HandleFunc 注册的模式都必须出现在权限目录中。

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// routeSourceFiles 参与路由注册的源文件（反向扫描清单）。
var routeSourceFiles = []string{
	"instance.go", "cluster.go", "container.go", "vault.go", "accounts_handlers.go",
}

// permExemptRoutes 免权限目录的路由：直连票据通道与公开登录入口。
var permExemptRoutes = map[string]bool{
	"GET /download/":       true,
	"POST /upload/":        true,
	"POST /api/auth/login": true,
}

// TestPermCatalogMatchesMux 正向校验：目录中每个键都与注册路由模式一致。
func TestPermCatalogMatchesMux(t *testing.T) {
	d, _ := newTestDaemon(t)
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	for _, g := range permCatalog() {
		for _, e := range g.Entries {
			method, pat, ok := strings.Cut(e.Key, " ")
			if !ok {
				t.Fatalf("目录键格式错误: %q", e.Key)
			}
			path := pat
			for _, seg := range []string{"{id}", "{name}", "{session}", "{jobId}"} {
				path = strings.ReplaceAll(path, seg, "x")
			}
			req := httptest.NewRequest(method, path, nil)
			_, p := mux.Handler(req)
			if p != e.Key {
				t.Errorf("目录键 %q 在 mux 中匹配为 %q（注册模式不一致或未注册）", e.Key, p)
			}
		}
	}
}

// TestPermCatalogCoversAllRoutes 反向校验：扫描路由注册源文件，
// 所有 mux.HandleFunc 注册（除豁免路由外）都必须出现在权限目录中。
func TestPermCatalogCoversAllRoutes(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	handleRe := regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`)
	var missing []string
	for _, name := range routeSourceFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		for _, m := range handleRe.FindAllSubmatch(data, -1) {
			pat := string(m[1])
			if permExemptRoutes[pat] {
				continue
			}
			if !permKeyExists(pat) {
				missing = append(missing, name+" → "+pat)
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("以下已注册路由缺少权限目录条目（需在注册处相邻调用 perm(组名, 模式, 描述)）:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
