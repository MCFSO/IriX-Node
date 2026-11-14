// MSLX 插件接口兼容层。
//
// MSLX 的插件是 .NET DLL，由它的 daemon 进程内加载执行——本节点不复刻这套运行时，
// 因此这里的原则是：
//
//   - **只读列表类接口返回「空列表」**，而不是 501。面板在打开「服务端列表」时会先
//     拉 `/api/plugins/list` 来注册插件页面与菜单；这个请求一旦失败，整页会显示
//     「目标节点插件加载失败」并挡住所有实例操作（线上就是这么被拦住的）。
//     返回空列表在语义上也是准确的：本节点确实没有安装任何 MSLX 插件。
//   - **写操作（安装/卸载/上传/动作）明确报不支持**，返回中文错误而不是静默成功。
package main

import (
	"net/http"
	"strings"
)

// handlePlugins 处理 /api/plugins/*。
func (ms *mslxCompat) handlePlugins(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 || segs[0] == "" {
		// 与 MSLX 一致：缺省即列表
		mslxOK(w, []any{}, "插件列表获取成功")
		return
	}
	switch segs[0] {
	case "list":
		// 面板据此注册插件页面/菜单；空列表 = 本节点没有 MSLX 插件
		mslxOK(w, []any{}, "插件列表获取成功")
		return
	case "task":
		// /api/plugins/task/install/{id}：没有插件任务可查
		mslxFail(w, http.StatusNotFound, "本节点没有插件安装任务")
		return
	case "action", "install", "upload", "uninstall", "enable", "disable":
		// 兼容层不提供 .NET 插件宿主（见 docs/compat-daemon.md「范围与边界」）。
		// 列表接口返回空即可让面板正常工作，写操作明确说明原因。
		mslxFail(w, http.StatusNotImplemented,
			"irix-node 兼容层不提供 MSLX 插件运行时（插件是 .NET DLL，需要在其 daemon 进程内加载）；"+
				"实例管理、文件、控制台等功能不受影响。请求："+strings.Join(segs, "/"))
		return
	default:
		mslxFail(w, http.StatusNotImplemented, "irix-node 未实现该 MSLX 插件接口："+strings.Join(segs, "/"))
	}
}
