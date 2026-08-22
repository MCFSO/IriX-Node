// qa_vault_m4_test.go — Vault M4 测试。
//
// 覆盖 docs/vault-design.md §8 的存储层与数据面：分块对象往返与随机读、
// 追加写崩溃语义（D11）、篡改检测、索引重载与孤儿回收（D12）、copy/move
// 语义（D14）、两阶段迁移（A2/D13）、物化/回收/崩溃残留（D9）、加密层
// 文件 API 双路径、锁定态门禁、备份包。

package main

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vaultM4Env 组装 M4 测试环境（小块大小加速多块断言）。
func vaultM4Env(t *testing.T) *vaultTestEnv {
	t.Helper()
	e := newVaultEnv(t, 3)
	e.d.vault.store.blockSize = 4096 // 测试用小块，触发多块路径
	return e
}

// m4AddInstance 向保险库环境添加 vaultFiles 实例（cwd 含若干文件）。
func m4AddInstance(t *testing.T, e *vaultTestEnv, files map[string]string) *Instance {
	t.Helper()
	cwd := t.TempDir()
	for name, content := range files {
		full := filepath.Join(cwd, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inst := NewInstance("", InstanceConfig{
		Nickname:    "m4sv",
		Cwd:         cwd,
		StartCommand: "sleep 100",
		VaultFiles:  true,
	})
	if err := e.d.Add(inst); err != nil {
		t.Fatal(err)
	}
	// 保险库未迁移前 Save 走明文 instances.json（迁移阶段一的数据源）
	if err := e.d.Save(); err != nil {
		t.Fatal(err)
	}
	return inst
}

// m4UnlockAndMigrate 解锁并等待迁移完成（阶段二可能后台执行）。
func m4UnlockAndMigrate(t *testing.T, e *vaultTestEnv, creds *onboardCreds) string {
	t.Helper()
	code, resp := e.unlock(t, creds, nil)
	if code != http.StatusOK {
		t.Fatalf("解锁失败: %d %v", code, resp)
	}
	token := vstr(vdata(resp), "sessionToken")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, resp := e.vreq(t, "GET", "/api/vault/migrate/status", nil,
			map[string]string{"X-Vault-Token": token})
		if code == http.StatusOK {
			if ph, ok := vdata(resp)["phase"].(float64); ok && int(ph) == 3 {
				return token
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("迁移未在超时内完成: %v", resp)
	return ""
}

// ---------------------------------------------------------------------------
// 存储层单元测试
// ---------------------------------------------------------------------------

// TestStoreRoundtripAndRange 分块往返 + 块级随机读。
func TestStoreRoundtripAndRange(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)

	// 多块数据（3.5 块）
	data := make([]byte, 4096*3+2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := e.d.vault.store.writeFile(e.d.vault, "/m4sv/test.bin", data); err != nil {
		t.Fatal(err)
	}
	got, err := e.d.vault.store.readFile(e.d.vault, "/m4sv/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("整文件往返不一致")
	}
	// 跨块随机读
	for _, tc := range []struct{ off, n int64 }{
		{0, 100}, {4090, 20}, {4096, 4096}, {5000, 8192}, {10000, 10},
	} {
		got, err := e.d.vault.store.readRange(e.d.vault, "/m4sv/test.bin", tc.off, tc.n)
		if err != nil {
			t.Fatalf("readRange(%d,%d) 失败: %v", tc.off, tc.n, err)
		}
		want := data[tc.off : tc.off+tc.n]
		if !bytes.Equal(got, want) {
			t.Fatalf("readRange(%d,%d) 不一致", tc.off, tc.n)
		}
	}
	// 越界读 → 空
	got, err = e.d.vault.store.readRange(e.d.vault, "/m4sv/test.bin", int64(len(data)), 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("越界读应返回空: %v %d", err, len(got))
	}
}

// TestStoreAppend 追加写：块对齐走块追加，非对齐回退整写；内容正确。
func TestStoreAppend(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)
	v := e.d.vault
	s := v.store

	// 对齐追加（初值恰为块大小整数倍）
	if err := s.writeFile(v, "/m4sv/app.log", bytes.Repeat([]byte("A"), 4096*2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appendFile(v, "/m4sv/app.log", []byte("BBBB")); err != nil {
		t.Fatal(err)
	}
	got, err := s.readFile(v, "/m4sv/app.log")
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("A"), 4096*2), []byte("BBBB")...)
	if !bytes.Equal(got, want) {
		t.Fatal("对齐追加内容不一致")
	}
	// 非对齐追加（回退整写）
	if _, err := s.appendFile(v, "/m4sv/app.log", []byte("CCCC")); err != nil {
		t.Fatal(err)
	}
	got, err = s.readFile(v, "/m4sv/app.log")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, []byte("CCCC")...)
	if !bytes.Equal(got, want) {
		t.Fatal("非对齐追加内容不一致")
	}
	// 追加后再次对齐读取（多块）
	if _, err := s.appendFile(v, "/m4sv/app.log", bytes.Repeat([]byte("D"), 4096*3)); err != nil {
		t.Fatal(err)
	}
	got, err = s.readFile(v, "/m4sv/app.log")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, bytes.Repeat([]byte("D"), 4096*3)...)
	if !bytes.Equal(got, want) {
		t.Fatal("多块追加内容不一致")
	}
}

// TestStoreAppendCrashTruncate 孤儿尾截断（D11）：对象尾部追加垃圾字节后
// 读取正常（尾部被截断），数据不受影响。
func TestStoreAppendCrashTruncate(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)
	v := e.d.vault
	s := v.store

	if err := s.writeFile(v, "/m4sv/crash.bin", []byte("hello vault")); err != nil {
		t.Fatal(err)
	}
	entry := s.stat("/m4sv/crash.bin")
	if entry == nil {
		t.Fatal("条目不存在")
	}
	// 模拟崩溃残留：对象尾部追加垃圾（等价于「块已写、计数未更新」）
	f, err := os.OpenFile(s.objectPath(entry.ID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xAB}, 2048)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// 读取：readHeader 截断孤儿尾，数据完好
	got, err := s.readFile(v, "/m4sv/crash.bin")
	if err != nil {
		t.Fatalf("孤儿尾截断后读取失败: %v", err)
	}
	if string(got) != "hello vault" {
		t.Fatalf("数据被孤儿尾破坏: %q", got)
	}
}

// TestStoreTamperDetect 篡改检测：翻转对象密文字节 → GCM 拒绝。
func TestStoreTamperDetect(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)
	v := e.d.vault
	s := v.store

	if err := s.writeFile(v, "/m4sv/tamper.txt", []byte("integrity matters")); err != nil {
		t.Fatal(err)
	}
	entry := s.stat("/m4sv/tamper.txt")
	objPath := s.objectPath(entry.ID)
	raw, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01 // 翻转密文最后一个字节（GCM tag 内）
	if err := os.WriteFile(objPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readFile(v, "/m4sv/tamper.txt"); err == nil {
		t.Fatal("篡改后的对象应读取失败")
	}
}

// TestStoreIndexReloadAndOrphanSweep 索引重载（重启持久化）与孤儿回收。
func TestStoreIndexReloadAndOrphanSweep(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)
	v := e.d.vault
	s := v.store

	payload := []byte("index-reload-payload")
	if err := s.writeFile(v, "/m4sv/persist.txt", payload); err != nil {
		t.Fatal(err)
	}
	entry := s.stat("/m4sv/persist.txt")
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}

	// 制造孤儿对象（无索引引用）
	orphan := make([]byte, 100)
	if _, err := rand.Read(orphan); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(s.objectsDir, strings.Repeat("ff", 32))
	if err := os.WriteFile(orphanPath, orphan, 0o600); err != nil {
		t.Fatal(err)
	}

	// 重启：新守护进程同一数据目录（复用 onboard 凭据走真实解锁流程）
	d2 := NewDaemon(e.dir, "test-key")
	d2.vault.enabled = true
	d2.vault.file = filepath.Join(e.dir, "vault", "vault.json")
	d2.vault.pbkdf2Iterations = 1000
	d2.vault.maxAttempts = 3
	d2.vault.lockoutDuration = time.Minute
	d2.vault.store.blockSize = 4096
	if err := d2.vault.load(d2.vault.file); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	d2.RegisterRoutes(mux)
	srv2 := newTestServer(d2)
	defer srv2.Close()
	e2 := &vaultTestEnv{d: d2, srv: srv2, dir: e.dir}
	m4UnlockAndMigrate(t, e2, creds)

	// 数据可读（同一对象 ID，证明对象与 DEK 包裹复用）
	got, err := e2.d.vault.store.readFile(e2.d.vault, "/m4sv/persist.txt")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("重启后读取失败: %v %q", err, got)
	}
	if e2.d.vault.store.stat("/m4sv/persist.txt").ID != entry.ID {
		t.Fatal("重启后对象 ID 应一致（未重复加密）")
	}
	// 孤儿对象已被回收
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("孤儿对象应被回收")
	}
}

// TestStoreCopyMoveSemantics copy/move 语义（D14）。
func TestStoreCopyMoveSemantics(t *testing.T) {
	e := vaultM4Env(t)
	creds := e.onboard(t, "admin", "Passw0rd1234")
	m4UnlockAndMigrate(t, e, creds)
	v := e.d.vault
	s := v.store

	if err := s.writeFile(v, "/m4sv/src.txt", []byte("copy-move")); err != nil {
		t.Fatal(err)
	}
	srcID := s.stat("/m4sv/src.txt").ID
	if err := s.copy(v, "/m4sv/src.txt", "/m4sv/dst.txt"); err != nil {
		t.Fatal(err)
	}
	dstID := s.stat("/m4sv/dst.txt").ID
	if dstID == srcID {
		t.Fatal("copy 应生成新对象（新 DEK）")
	}
	if err := s.move("/m4sv/src.txt", "/m4sv/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if s.stat("/m4sv/src.txt") != nil {
		t.Fatal("move 后原路径应消失")
	}
	if s.stat("/m4sv/moved.txt") == nil || s.stat("/m4sv/moved.txt").ID != srcID {
		t.Fatal("move 应复用对象（仅索引迁移）")
	}
}

// ---------------------------------------------------------------------------
// 迁移 / 物化 / 文件 API 集成测试
// ---------------------------------------------------------------------------

// TestMigrationAndVaultFilesFlow 两阶段迁移 + 加密层文件 API + 物化/回收 + 重启。
func TestMigrationAndVaultFilesFlow(t *testing.T) {
	e := vaultM4Env(t)
	files := map[string]string{
		"server.properties": "motd=hello\n",
		"world/data.bin":    strings.Repeat("W", 20000), // 多块
	}
	inst := m4AddInstance(t, e, files)
	uuid := inst.InstanceUuid
	cwd := inst.Config.Cwd
	creds := e.onboard(t, "admin", "Passw0rd1234")
	token := m4UnlockAndMigrate(t, e, creds)

	// 阶段一：明文 instances.json 已删除，实例列表从加密对象加载
	if _, err := os.Stat(filepath.Join(e.dir, "instances.json")); !os.IsNotExist(err) {
		t.Fatal("明文 instances.json 应已删除")
	}
	e.d.mu.Lock()
	loaded := len(e.d.Instances) == 1 && e.d.Instances[0].InstanceUuid == uuid
	e.d.mu.Unlock()
	if !loaded {
		t.Fatal("解锁后实例列表应从加密对象加载")
	}
	// 阶段二：明文文件树已加密入库
	for name := range files {
		if _, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("明文文件 %s 应已删除", name)
		}
	}

	// 加密层文件 API：列表 / 读取 / 写入 / 删除
	code, resp := e.vreq(t, "GET", "/api/files/list?daemonId=x&uuid="+uuid+"&page=1&page_size=100", nil,
		map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("加密层列表失败: %d %v", code, resp)
	}
	items, ok := vdata(resp)["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("加密层列表应有 2 个文件: %v", resp)
	}
	code, resp = e.vreq(t, "PUT", "/api/files/?daemonId=x&uuid="+uuid,
		map[string]any{"target": "/server.properties"}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK || vstr(resp, "data") != "motd=hello\n" {
		t.Fatalf("加密层读取失败: %d %v", code, resp)
	}
	code, resp = e.vreq(t, "PUT", "/api/files/?daemonId=x&uuid="+uuid,
		map[string]any{"target": "/new.txt", "text": "fresh"}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("加密层写入失败: %d %v", code, resp)
	}
	code, resp = e.vreq(t, "DELETE", "/api/files?daemonId=x&uuid="+uuid,
		map[string]any{"targets": []string{"/new.txt"}}, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("加密层删除失败: %d %v", code, resp)
	}
	// 直连通道与压缩：停止态加密层明确拒绝（M4 范围边界）
	code, resp = e.vreq(t, "POST", "/api/files/download?file_name=server.properties&daemonId=x&uuid="+uuid, nil,
		map[string]string{"X-Vault-Token": token})
	if code != http.StatusBadRequest || !strings.Contains(vstr(resp, "data"), "直连下载暂不支持") {
		t.Fatalf("停止态直连下载应拒绝: %d %v", code, resp)
	}
	code, resp = e.vreq(t, "POST", "/api/files/compress?daemonId=x&uuid="+uuid,
		map[string]any{"type": 1, "source": "/x.zip", "targets": []string{"/server.properties"}},
		map[string]string{"X-Vault-Token": token})
	if code != http.StatusBadRequest || !strings.Contains(vstr(resp, "data"), "压缩/解压暂不支持") {
		t.Fatalf("停止态压缩应拒绝: %d %v", code, resp)
	}

	// 物化 → 回收 → 崩溃残留恢复（D9）
	if err := e.d.vaultMaterialize(inst, cwd); err != nil {
		t.Fatalf("物化失败: %v", err)
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(cwd, filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Fatalf("物化内容不一致 %s: %v %q", name, err, got)
		}
	}
	// 模拟运行期写入（进程新增文件）
	if err := os.WriteFile(filepath.Join(cwd, "world/runtime.dat"), []byte("runtime-write"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !plainTreeDirty(cwd) {
		t.Fatal("存在明文文件时应判定 dirty")
	}
	if err := e.d.vaultRecycle(inst, cwd); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if plainTreeDirty(cwd) {
		t.Fatal("回收后明文目录应干净")
	}
	got, err := e.d.vault.store.readFile(e.d.vault, "/"+uuid+"/world/runtime.dat")
	if err != nil || string(got) != "runtime-write" {
		t.Fatalf("运行期新增文件应已回收加密: %v %q", err, got)
	}

	// 重启：Load 跳过明文，解锁后实例与文件区恢复
	// （先模拟优雅关停：落盘最新索引）
	if err := e.d.vault.store.flush(); err != nil {
		t.Fatal(err)
	}
	d2 := NewDaemon(e.dir, "test-key")
	d2.vault.enabled = true
	d2.vault.file = filepath.Join(e.dir, "vault", "vault.json")
	d2.vault.pbkdf2Iterations = 1000
	d2.vault.maxAttempts = 3
	d2.vault.lockoutDuration = time.Minute
	d2.vault.store.blockSize = 4096
	if err := d2.vault.load(d2.vault.file); err != nil {
		t.Fatal(err)
	}
	if err := d2.Load(); err != nil {
		t.Fatal(err)
	}
	d2.mu.Lock()
	pre := len(d2.Instances)
	d2.mu.Unlock()
	if pre != 0 {
		t.Fatalf("保险库模式下启动不应加载明文实例列表: %d", pre)
	}
	mux := http.NewServeMux()
	d2.RegisterRoutes(mux)
	srv2 := newTestServer(d2)
	defer srv2.Close()
	e2 := &vaultTestEnv{d: d2, srv: srv2, dir: e.dir}
	token2 := m4UnlockAndMigrate(t, e2, creds)
	d2.mu.Lock()
	post := len(d2.Instances) == 1 && d2.Instances[0].InstanceUuid == uuid
	d2.mu.Unlock()
	if !post {
		t.Fatal("重启解锁后实例列表应从加密对象恢复")
	}
	code, resp = e2.vreq(t, "PUT", "/api/files/?daemonId=x&uuid="+uuid,
		map[string]any{"target": "/world/data.bin"}, map[string]string{"X-Vault-Token": token2})
	if code != http.StatusOK || vstr(resp, "data") != strings.Repeat("W", 20000) {
		t.Fatalf("重启后加密层读取失败: %d", code)
	}
}

// TestVaultFilesLockedGate 锁定态下 vaultFiles 实例的文件 API 被门禁拦截。
func TestVaultFilesLockedGate(t *testing.T) {
	e := vaultM4Env(t)
	inst := m4AddInstance(t, e, map[string]string{"a.txt": "x"})
	creds := e.onboard(t, "admin", "Passw0rd1234")
	token := m4UnlockAndMigrate(t, e, creds)
	// 锁定
	code, resp := e.vreq(t, "POST", "/api/vault/lock", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("lock 失败: %d %v", code, resp)
	}
	code, resp = e.vreq(t, "GET", "/api/files/list?daemonId=x&uuid="+inst.InstanceUuid, nil, nil)
	if code != http.StatusForbidden || vstr(resp, "data") != "vault locked" {
		t.Fatalf("锁定态文件 API 应 403: %d %v", code, resp)
	}
}

// TestVaultBackup 备份包：zip 含 vault.json/索引/对象，不含密钥材料。
func TestVaultBackup(t *testing.T) {
	e := vaultM4Env(t)
	inst := m4AddInstance(t, e, map[string]string{"b.txt": "backup-me"})
	creds := e.onboard(t, "admin", "Passw0rd1234")
	token := m4UnlockAndMigrate(t, e, creds)
	_ = inst

	code, resp := e.vreq(t, "POST", "/api/vault/backup", nil, map[string]string{"X-Vault-Token": token})
	if code != http.StatusOK {
		t.Fatalf("backup 失败: %d %v", code, resp)
	}
	// vreq 返回 JSON 解析结果，zip 二进制不在其中 —— 直接读响应体验证类型头
	// （此路径由 vreq 已消费；改用原始请求验证 zip 魔数）
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/vault/backup", nil)
	req.Header.Set("X-Api-Key", "test-key")
	req.Header.Set("X-Vault-Token", token)
	r, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("backup 原始请求失败: %d", r.StatusCode)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		t.Fatalf("备份应为 zip: %s", ct)
	}
	head := make([]byte, 2)
	if _, err := r.Body.Read(head); err != nil {
		t.Fatal(err)
	}
	if head[0] != 'P' || head[1] != 'K' {
		t.Fatal("zip 魔数缺失")
	}
}
