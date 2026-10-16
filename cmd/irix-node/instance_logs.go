// 实例日志持久化查询 API（docs/irix-node-local-parity.md §4.1.2）。
//
// 日志文件由 fileLogger 异步落盘（{data}/logs/{uuid}.log + 轮转 .1 … .5，
// 保留 ANSI 供回放）；本文件提供历史日志读取（tail）、断线补发（since）
// 与清空接口。进程运行期间 since 查询走带时间戳的行缓冲（精确），
// 进程退出后回退到文件 mtime 过滤。

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logTailDefault 未指定 tail 参数时的默认行数（防未限定的全量读取）。
const logTailDefault = 1000

// logFilePaths 返回实例日志的全部文件路径（从旧到新：.5 … .1、主文件）。
// LogDir 为空（-instance-log=false）时返回空切片。
func (d *Daemon) logFilePaths(uuid string) []string {
	if d.LogDir == "" {
		return nil
	}
	base := filepath.Join(d.LogDir, uuid+".log")
	paths := make([]string, 0, 6)
	for i := 5; i >= 1; i-- { // 轮转份数固定对齐 logConfig().keep=5
		paths = append(paths, base+fmt.Sprintf(".%d", i))
	}
	paths = append(paths, base)
	return paths
}

// splitLogLines 按 \n 拆分文件内容为行（保留空行），去掉末尾的空元素
// （文件以 \n 结尾的常规情况）。行内保留原始字节（含 ANSI 与行尾 \r）。
func splitLogLines(data []byte) []string {
	var out []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, string(data))
			break
		}
		out = append(out, string(data[:i]))
		data = data[i+1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// readLogTail 从日志文件集合尾部倒读最后 n 行（按时间从旧到新拼接）；
// n <= 0 表示读取全部。文件缺失/不可读自动跳过。
// 实现：逐文件从新到旧收集尾部行（chunk 内部保持时间顺序），
// 最后只反转文件级顺序（新文件在前 → 旧文件在前）。
func readLogTail(paths []string, n int) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	var chunks [][]string
	need := n
	for i := len(paths) - 1; i >= 0; i-- {
		if n > 0 && need <= 0 {
			break
		}
		data, err := os.ReadFile(paths[i])
		if err != nil {
			continue
		}
		lines := splitLogLines(data)
		if len(lines) == 0 {
			continue
		}
		take := need
		if n <= 0 || len(lines) < take {
			take = len(lines)
		}
		chunks = append(chunks, lines[len(lines)-take:])
		if n > 0 {
			need -= take
		}
	}
	var out []string
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.Join(out, "\n") + "\n", nil
}

// readLogSince 读取修改时间晚于 since（unix 毫秒）的日志文件内容，
// 按时间顺序拼接（断线补发用，进程已退出时的近似实现）。
func readLogSince(paths []string, sinceMs int64) (string, error) {
	since := time.UnixMilli(sinceMs)
	var sb strings.Builder
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		sb.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			sb.WriteByte('\n')
		}
	}
	return sb.String(), nil
}

// handleInstanceLogs 读取实例历史日志。
// GET /api/instance/logs?uuid&daemonId&tail=<n>&since=<unix_ms>
//   - tail：返回最后 N 行（默认 1000；显式 0 表示全部）
//   - since：返回该时间点之后追加的行（断线重连补发）
//
// 响应 data 为日志字符串（保留 ANSI）。
func (d *Daemon) handleInstanceLogs(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	since := int64(atoiDefault(queryParam(r, "since"), 0))
	paths := d.logFilePaths(inst.InstanceUuid)

	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()

	var out string
	if queryParam(r, "since") != "" {
		// 断线补发：运行中优先精确行缓冲（时间戳精确到行）
		if proc != nil && proc.IsRunning() && proc.lines != nil {
			out = proc.lines.since(since)
		} else {
			out, err = readLogSince(paths, since)
		}
	} else {
		// 历史日志：文件优先（持久历史），未落盘（-instance-log=false）
		// 或文件缺失时回退到行缓冲
		tail := atoiDefault(queryParam(r, "tail"), logTailDefault)
		if len(paths) > 0 {
			out, err = readLogTail(paths, tail)
		}
		if out == "" && proc != nil && proc.lines != nil {
			out = proc.lines.tail(tail)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}
	writeOK(w, out)
}

// handleInstanceLogsClear 清空实例日志（客户端「清空日志」按钮）。
// DELETE /api/instance/logs?uuid&daemonId
func (d *Daemon) handleInstanceLogsClear(w http.ResponseWriter, r *http.Request) {
	inst, err := d.requireInstance(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst.mu.Lock()
	proc := inst.Proc
	inst.mu.Unlock()

	if proc != nil {
		// 运行中：行缓冲清空 + 通过 fileLogger 清空文件（避免与写句柄冲突）
		if proc.lines != nil {
			proc.lines.clear()
		}
		if proc.log != nil {
			if err := proc.log.Clear(); err != nil {
				writeError(w, http.StatusInternalServerError, "清空日志失败: "+err.Error())
				return
			}
		}
	} else {
		// 已停止：直接删除日志文件与轮转文件
		for _, p := range d.logFilePaths(inst.InstanceUuid) {
			_ = os.Remove(p)
		}
	}
	writeOK(w, true)
}
