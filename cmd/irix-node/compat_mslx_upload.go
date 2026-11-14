// MSLX 分片上传会话（面板「创建服务端」里上传核心/整合包用的临时通道）。
//
// 面板在这条链路上依次调用：
//
//	POST /api/files/upload/init               → {uploadId, resumed, receivedChunks}
//	POST /api/files/upload/chunk/{uploadId}   → multipart：index、file、可选 crc32c
//	GET  /api/files/upload/status/{uploadId}  → {receivedChunks:[...]}
//	POST /api/files/upload/finish/{uploadId}  → {totalChunks, chunkSize?, totalSize?, blake3?}
//	POST /api/files/upload/delete/{uploadId}
//
// 合并后的文件落在 {data}/compat-mslx-uploads/{uploadId}.bin，随后由
// createServer 的 coreFileKey / packageFileKey 取用。
//
// 与 MSLX 的差异（已在其响应/日志里说明）：BLAKE3 需要额外依赖，本节点不引入，
// 因此只做**分片 CRC32C** 与**总大小**校验；blake3 字段接收但不比对。
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"irix/node/internal/timesys"
)

// mslxUploadDirName 上传临时目录名（位于数据目录下）。
const mslxUploadDirName = "compat-mslx-uploads"

// mslxUploadLifetime 会话有效期（与 MSLX 一致：2 小时）。
const mslxUploadLifetime = 2 * time.Hour

// mslxUploadMaxSize 单文件上限（512 MiB，与核心下载上限一致）。
const mslxUploadMaxSize = 512 << 20

// mslxUploadSession 一次分片上传会话。
type mslxUploadSession struct {
	id        string
	createdAt time.Time
	chunks    map[int]string // 分片索引 → 分片文件路径
	merged    string         // 合并后的文件路径（finish 后填写）
	size      int64
}

// mslxUploadRegistry 会话表（内存）。
type mslxUploadRegistry struct {
	mu    sync.Mutex
	items map[string]*mslxUploadSession
}

// mslxUploadDir 上传临时目录（不存在时创建）。
func (ms *mslxCompat) mslxUploadDir() (string, error) {
	dir := filepath.Join(ms.d.DataDir, mslxUploadDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// newUploadSession 创建会话。
func (ms *mslxCompat) newUploadSession() (*mslxUploadSession, error) {
	if _, err := ms.mslxUploadDir(); err != nil {
		return nil, err
	}
	s := &mslxUploadSession{
		id:        randomToken(16), // 32 位十六进制，与面板约定的 uploadId 格式一致
		createdAt: timesys.Now(),
		chunks:    map[int]string{},
	}
	ms.uploads.mu.Lock()
	if ms.uploads.items == nil {
		ms.uploads.items = map[string]*mslxUploadSession{}
	}
	// 顺手清理过期会话，避免长期占盘
	for id, item := range ms.uploads.items {
		if timesys.Now().Sub(item.createdAt) > mslxUploadLifetime {
			ms.dropUploadLocked(item)
			delete(ms.uploads.items, id)
		}
	}
	ms.uploads.items[s.id] = s
	ms.uploads.mu.Unlock()
	return s, nil
}

// getUploadSession 取会话。
func (ms *mslxCompat) getUploadSession(id string) *mslxUploadSession {
	ms.uploads.mu.Lock()
	defer ms.uploads.mu.Unlock()
	s, ok := ms.uploads.items[id]
	if !ok {
		return nil
	}
	if timesys.Now().Sub(s.createdAt) > mslxUploadLifetime {
		ms.dropUploadLocked(s)
		delete(ms.uploads.items, id)
		return nil
	}
	return s
}

// dropUploadLocked 删除会话的磁盘文件（调用方持有 uploads 锁）。
func (ms *mslxCompat) dropUploadLocked(s *mslxUploadSession) {
	if s == nil {
		return
	}
	for _, p := range s.chunks {
		_ = os.Remove(p)
	}
	if s.merged != "" {
		_ = os.Remove(s.merged)
	}
}

// uploadFilePath 返回合并后的文件路径（未完成时返回空）。
func (ms *mslxCompat) uploadFilePath(id string) string {
	s := ms.getUploadSession(id)
	if s == nil || s.merged == "" {
		return ""
	}
	if _, err := os.Stat(s.merged); err != nil {
		return ""
	}
	return s.merged
}

// handleUpload 处理 /api/files/upload/*。
func (ms *mslxCompat) handleUpload(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		mslxFail(w, http.StatusNotFound, "未知上传接口")
		return
	}
	switch segs[0] {
	case "init":
		ms.handleUploadInit(w, r)
	case "chunk":
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少 uploadId")
			return
		}
		ms.handleUploadChunk(w, r, segs[1])
	case "status":
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少 uploadId")
			return
		}
		ms.handleUploadStatus(w, r, segs[1])
	case "finish":
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少 uploadId")
			return
		}
		ms.handleUploadFinish(w, r, segs[1])
	case "delete":
		if len(segs) < 2 {
			mslxFail(w, http.StatusBadRequest, "缺少 uploadId")
			return
		}
		ms.handleUploadDelete(w, r, segs[1])
	default:
		mslxFail(w, http.StatusNotFound, "未知上传接口："+strings.Join(segs, "/"))
	}
}

// handleUploadInit 获取上传凭证。
func (ms *mslxCompat) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	s, err := ms.newUploadSession()
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建上传会话失败: "+err.Error())
		return
	}
	mslxOK(w, map[string]any{
		"uploadId":       s.id,
		"resumed":        false,
		"receivedChunks": []int{},
	}, "获取成功")
}

// handleUploadChunk 接收一个分片（multipart：index、file、crc32c）。
func (ms *mslxCompat) handleUploadChunk(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	s := ms.getUploadSession(id)
	if s == nil {
		mslxFail(w, http.StatusForbidden, "上传凭证无效或已过期，请刷新页面重试")
		return
	}
	dir, err := ms.mslxUploadDir()
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "上传目录不可用: "+err.Error())
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		mslxFail(w, http.StatusBadRequest, "解析分片请求失败: "+err.Error())
		return
	}
	index, err := strconv.Atoi(strings.TrimSpace(r.FormValue("index")))
	if err != nil || index < 0 || index > 1_000_000 {
		mslxFail(w, http.StatusBadRequest, "分片索引超出范围")
		return
	}
	crcWanted := strings.ToLower(strings.TrimSpace(r.FormValue("crc32c")))
	file, _, err := r.FormFile("file")
	if err != nil {
		mslxFail(w, http.StatusBadRequest, "缺少分片文件（表单字段 file）")
		return
	}
	defer file.Close()

	chunkPath := filepath.Join(dir, fmt.Sprintf("%s_%d.part", id, index))
	tmp := chunkPath + ".uploading"
	out, err := os.Create(tmp)
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "写入分片失败: "+err.Error())
		return
	}
	hasher := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(file, mslxUploadMaxSize+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusInternalServerError, "写入分片失败")
		return
	}
	if n > mslxUploadMaxSize {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusRequestEntityTooLarge, "分片超过 512 MiB 上限")
		return
	}
	if crcWanted != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != crcWanted {
			_ = os.Remove(tmp)
			mslxFail(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("分片 %d CRC32C 校验失败（期望 %s，实际 %s）", index, crcWanted, got))
			return
		}
	}
	if err := os.Rename(tmp, chunkPath); err != nil {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusInternalServerError, "落盘分片失败: "+err.Error())
		return
	}
	ms.uploads.mu.Lock()
	s.chunks[index] = chunkPath
	ms.uploads.mu.Unlock()
	mslxOK(w, map[string]any{"index": index, "size": n, "crc32c": crcWanted, "validation": "done"}, "上传完成")
}

// handleUploadStatus 已接收分片列表。
func (ms *mslxCompat) handleUploadStatus(w http.ResponseWriter, r *http.Request, id string) {
	s := ms.getUploadSession(id)
	if s == nil {
		mslxFail(w, http.StatusForbidden, "上传凭证无效或已过期")
		return
	}
	ms.uploads.mu.Lock()
	indexes := make([]int, 0, len(s.chunks))
	for idx := range s.chunks {
		indexes = append(indexes, idx)
	}
	ms.uploads.mu.Unlock()
	sort.Ints(indexes)
	mslxOK(w, map[string]any{"receivedChunks": indexes, "uploadId": id}, "获取成功")
}

// handleUploadFinish 合并分片。
func (ms *mslxCompat) handleUploadFinish(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		mslxFail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	s := ms.getUploadSession(id)
	if s == nil {
		mslxFail(w, http.StatusForbidden, "凭证无效或已过期")
		return
	}
	var req struct {
		TotalChunks int    `json:"totalChunks"`
		ChunkSize   *int   `json:"chunkSize"`
		TotalSize   *int64 `json:"totalSize"`
		Blake3      string `json:"blake3"`
	}
	if err := parseJSONBody(r, &req); err != nil {
		mslxFail(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if req.TotalChunks < 0 || req.TotalChunks > 1_000_001 {
		mslxFail(w, http.StatusBadRequest, "分片总数超出范围")
		return
	}
	ms.uploads.mu.Lock()
	chunks := make([]string, 0, len(s.chunks))
	missing := []int{}
	for i := 0; i < req.TotalChunks; i++ {
		p, ok := s.chunks[i]
		if !ok {
			missing = append(missing, i)
			continue
		}
		chunks = append(chunks, p)
	}
	ms.uploads.mu.Unlock()
	if len(missing) > 0 {
		mslxFail(w, http.StatusBadRequest,
			fmt.Sprintf("分片集合不完整，缺少 %d 个分片（首个缺失索引 %d）", len(missing), missing[0]))
		return
	}

	dir, err := ms.mslxUploadDir()
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "上传目录不可用")
		return
	}
	merged := filepath.Join(dir, id+".bin")
	tmp := merged + ".merging"
	out, err := os.Create(tmp)
	if err != nil {
		mslxFail(w, http.StatusInternalServerError, "创建合并文件失败: "+err.Error())
		return
	}
	var total int64
	for _, p := range chunks {
		f, openErr := os.Open(p)
		if openErr != nil {
			out.Close()
			_ = os.Remove(tmp)
			mslxFail(w, http.StatusInternalServerError, "读取分片失败: "+openErr.Error())
			return
		}
		n, copyErr := io.Copy(out, io.LimitReader(f, mslxUploadMaxSize+1))
		f.Close()
		if copyErr != nil {
			out.Close()
			_ = os.Remove(tmp)
			mslxFail(w, http.StatusInternalServerError, "合并分片失败")
			return
		}
		total += n
		if total > mslxUploadMaxSize {
			out.Close()
			_ = os.Remove(tmp)
			mslxFail(w, http.StatusRequestEntityTooLarge, "文件超过 512 MiB 上限")
			return
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusInternalServerError, "写入合并文件失败")
		return
	}
	if req.TotalSize != nil && total != *req.TotalSize {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusBadRequest,
			fmt.Sprintf("合并文件大小不匹配，期望 %d，实际 %d", *req.TotalSize, total))
		return
	}
	if err := os.Rename(tmp, merged); err != nil {
		_ = os.Remove(tmp)
		mslxFail(w, http.StatusInternalServerError, "落盘合并文件失败: "+err.Error())
		return
	}
	ms.uploads.mu.Lock()
	s.merged = merged
	s.size = total
	for _, p := range s.chunks {
		_ = os.Remove(p)
	}
	s.chunks = map[int]string{}
	ms.uploads.mu.Unlock()

	// blake3：本节点不引入 BLAKE3 依赖，接收字段但不比对（已在文档说明）
	mslxOK(w, map[string]any{"uploadId": id, "size": total, "blake3": nil}, "上传完成")
}

// handleUploadDelete 删除上传会话。
func (ms *mslxCompat) handleUploadDelete(w http.ResponseWriter, r *http.Request, id string) {
	ms.uploads.mu.Lock()
	s := ms.uploads.items[id]
	if s != nil {
		ms.dropUploadLocked(s)
		delete(ms.uploads.items, id)
	}
	ms.uploads.mu.Unlock()
	mslxOK(w, true, "已删除上传会话")
}

// errUploadNotFound 上传文件不存在。
var errUploadNotFound = errors.New("上传文件不存在或已过期，请重新上传")
