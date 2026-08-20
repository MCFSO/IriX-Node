// 实例级回收站测试：删除进回收站、列表、恢复（冲突改名）、清空、
// 越界/回收站内删除拒绝、元数据持久化。

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trashAPI 辅助：调用回收站接口并解析 {status, data}。
func trashAPI(t *testing.T, method, srvURL, apiPath, body string) (int, map[string]any) {
	t.Helper()
	var resp *http.Response
	var err error
	if method == http.MethodGet {
		resp, err = testClient.Get(srvURL + apiPath)
	} else {
		resp, err = testClient.Post(srvURL+apiPath, "application/json", strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return resp.StatusCode, out
}

// trashBody 构造回收站请求体。
func trashBody(uuid string, extra map[string]any) string {
	m := map[string]any{"uuid": uuid, "daemonId": "local"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// TestTrashLifecycle 删除→列表→恢复全流程。
func TestTrashLifecycle(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	worldFile := filepath.Join(dir, "world", "region", "r.0.0.mca")
	if err := os.MkdirAll(filepath.Dir(worldFile), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	_ = os.WriteFile(worldFile, []byte("world-data"), 0o644)
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/files/trash"
	auth := "?apikey=test-key"

	// 删除进回收站
	code, out := trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/world/region/r.0.0.mca"}}))
	if code != 200 {
		t.Fatalf("删除失败: %d %v", code, out)
	}
	if _, err := os.Stat(worldFile); err == nil {
		t.Fatalf("原文件应已移走")
	}
	// 回收站目录出现条目
	trashEntries, _ := os.ReadDir(filepath.Join(dir, trashDirName))
	if len(trashEntries) != 1 {
		t.Fatalf("回收站应有 1 项: %v", trashEntries)
	}

	// 列表
	code, out = trashAPI(t, http.MethodGet, "", base+"/list?uuid="+inst.InstanceUuid+"&apikey=test-key", "")
	if code != 200 {
		t.Fatalf("列表失败: %d %v", code, out)
	}
	items, _ := out["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("列表应有 1 项: %v", items)
	}
	item := items[0].(map[string]any)
	id, _ := item["id"].(string)
	if item["originalPath"] != "/world/region/r.0.0.mca" {
		t.Fatalf("原始路径错误: %v", item)
	}

	// 恢复
	code, out = trashAPI(t, http.MethodPost, "", base+"/restore"+auth,
		trashBody(inst.InstanceUuid, map[string]any{"ids": []string{id}}))
	if code != 200 {
		t.Fatalf("恢复失败: %d %v", code, out)
	}
	data, err := os.ReadFile(worldFile)
	if err != nil || string(data) != "world-data" {
		t.Fatalf("恢复内容错误: %v %q", err, data)
	}
	// 元数据清空
	code, out = trashAPI(t, http.MethodGet, "", base+"/list?uuid="+inst.InstanceUuid+"&apikey=test-key", "")
	items, _ = out["data"].(map[string]any)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("恢复后列表应为空: %v", items)
	}
	t.Logf("[验证] 删除→列表→恢复全流程正确")
}

// TestTrashRestoreConflict 恢复时目标冲突自动改名。
func TestTrashRestoreConflict(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	worldFile := filepath.Join(dir, "level.dat")
	_ = os.WriteFile(worldFile, []byte("v1"), 0o644)
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/files/trash"
	auth := "?apikey=test-key"

	code, _ := trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/level.dat"}}))
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}
	// 恢复前在原始位置新建同名文件
	_ = os.WriteFile(worldFile, []byte("v2"), 0o644)

	code, out := trashAPI(t, http.MethodGet, "", base+"/list?uuid="+inst.InstanceUuid+"&apikey=test-key", "")
	items, _ := out["data"].(map[string]any)["items"].([]any)
	id := items[0].(map[string]any)["id"].(string)

	code, out = trashAPI(t, http.MethodPost, "", base+"/restore"+auth,
		trashBody(inst.InstanceUuid, map[string]any{"ids": []string{id}}))
	if code != 200 {
		t.Fatalf("恢复失败: %d %v", code, out)
	}
	restored := out["data"].(map[string]any)[id].(string)
	if restored != "/level (1).dat" {
		t.Fatalf("冲突应自动改名: %s", restored)
	}
	if _, err := os.Stat(filepath.Join(dir, "level (1).dat")); err != nil {
		t.Fatalf("改名文件缺失: %v", err)
	}
	t.Logf("[验证] 恢复冲突自动改名: %s", restored)
}

// TestTrashEmpty 清空（全部 + 指定 id）。
func TestTrashEmpty(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/files/trash"
	auth := "?apikey=test-key"

	code, _ := trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/a.txt", "/b.txt"}}))
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}
	// 清空全部
	code, _ = trashAPI(t, http.MethodPost, "", base+"/empty"+auth,
		trashBody(inst.InstanceUuid, nil))
	if code != 200 {
		t.Fatalf("清空失败: %d", code)
	}
	trashEntries, _ := os.ReadDir(filepath.Join(dir, trashDirName))
	if len(trashEntries) != 0 {
		t.Fatalf("清空后回收站应为空: %v", trashEntries)
	}
	code, out := trashAPI(t, http.MethodGet, "", base+"/list?uuid="+inst.InstanceUuid+"&apikey=test-key", "")
	items, _ := out["data"].(map[string]any)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("清空后元数据应为空: %v", items)
	}
	t.Logf("[验证] 回收站清空（文件 + 元数据）正确")
}

// TestTrashRejections 越界路径与回收站内删除被拒绝。
func TestTrashRejections(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	srv := newTestServer(d)
	defer srv.Close()
	base := srv.URL + "/api/files/trash"
	auth := "?apikey=test-key"

	// 越界
	code, _ := trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"../evil.txt"}}))
	if code != 400 {
		t.Fatalf("越界应 400: %d", code)
	}
	// 回收站内内容
	trashDir := filepath.Join(dir, trashDirName)
	_ = os.MkdirAll(trashDir, 0o755)
	_ = os.WriteFile(filepath.Join(trashDir, "x.txt"), []byte("x"), 0o644)
	code, _ = trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/.irix-trash/x.txt"}}))
	if code != 400 {
		t.Fatalf("回收站内删除应 400: %d", code)
	}
	// 不存在的目标
	code, _ = trashAPI(t, http.MethodPost, "", base+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/no-such.txt"}}))
	if code != 400 {
		t.Fatalf("不存在目标应 400: %d", code)
	}
	t.Logf("[验证] 越界/回收站内/不存在目标均被拒绝")
}

// TestTrashPersistence 元数据持久化：重新加载守护进程后列表仍可读。
func TestTrashPersistence(t *testing.T) {
	d, dir := newTestDaemon(t)
	inst := sampleInst(1, dir)
	if err := d.Add(inst); err != nil {
		t.Fatalf("添加实例失败: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	srv := newTestServer(d)
	defer srv.Close()
	auth := "?apikey=test-key"

	code, _ := trashAPI(t, http.MethodPost, srv.URL, "/api/files/trash"+auth,
		trashBody(inst.InstanceUuid, map[string]any{"targets": []string{"/keep.txt"}}))
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}

	// 用同一数据目录重建守护进程（模拟重启）
	d2 := NewDaemon(dir, "test-key")
	if err := d2.Load(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	items := d2.loadTrash(inst.InstanceUuid)
	if len(items) != 1 || items[0].Name != "keep.txt" {
		t.Fatalf("重启后元数据丢失: %v", items)
	}
	t.Logf("[验证] 回收站元数据重启后仍可读")
}
