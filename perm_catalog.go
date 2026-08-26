// 端点权限目录（docs/accounts-design.md §权限模型）：
// 注册路由时用 perm() 织入（组名 + 路由模式 + 中文描述），
// 供账户管理 UI 渲染「每一步开关」与逐端点鉴权使用。
//
// 键 = 请求的 r.Pattern（Go 1.22+ ServeMux 匹配后的路由模式，
// 如 "GET /api/instance"、"POST /api/frp/tunnels/{id}/start"），
// 与 RegisterRoutes 注册时的模式字符串严格一致。

package main

import "sync"

// permEntry 单个端点条目。
type permEntry struct {
	Key  string `json:"key"`  // 路由模式（方法 + 空格 + 路径，与 r.Pattern 一致）
	Desc string `json:"desc"` // 中文描述（开关 UI 展示）
}

// permGroup 端点分组（前端按模块渲染开关）。
type permGroup struct {
	Name    string      `json:"name"`
	Entries []permEntry `json:"entries"`
}

// 权限目录（perm() 注册时累积；RegisterRoutes 可被测试多次调用，
// 因此按 key 去重、按注册顺序保留分组）。
var (
	permMu     sync.Mutex
	permGroups []permGroup
	permIndex  = map[string]string{} // key → 组名（去重）
)

// perm 注册一个端点到权限目录（同一 key 重复注册自动忽略）。
func perm(group, key, desc string) {
	permMu.Lock()
	defer permMu.Unlock()
	if _, dup := permIndex[key]; dup {
		return
	}
	permIndex[key] = group
	for i := range permGroups {
		if permGroups[i].Name == group {
			permGroups[i].Entries = append(permGroups[i].Entries, permEntry{key, desc})
			return
		}
	}
	permGroups = append(permGroups, permGroup{group, []permEntry{{key, desc}}})
}

// permCatalog 返回权限目录快照（深拷贝，调用方可安全序列化）。
func permCatalog() []permGroup {
	permMu.Lock()
	defer permMu.Unlock()
	out := make([]permGroup, 0, len(permGroups))
	for _, g := range permGroups {
		ng := permGroup{Name: g.Name, Entries: make([]permEntry, len(g.Entries))}
		copy(ng.Entries, g.Entries)
		out = append(out, ng)
	}
	return out
}

// permKeyExists 判断路由模式是否在权限目录中。
func permKeyExists(key string) bool {
	permMu.Lock()
	defer permMu.Unlock()
	_, ok := permIndex[key]
	return ok
}

// permAllowed 判断账户是否放行指定端点（admin 在调用方旁路）。
// 账户系统未启用时恒 true（此时只有 apikey 通道可达，向后兼容）。
func (d *Daemon) permAllowed(username, pattern string) bool {
	if pattern == "" || d.accounts == nil {
		return d.accounts == nil
	}
	v, ok := d.accounts.loadPermissions(username)[pattern]
	return ok && v
}
