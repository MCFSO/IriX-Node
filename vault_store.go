// vault_store.go — 加密存储层（M4）。
//
// 实现 docs/vault-design.md §8：密文对象（分块 AES-256-GCM）、块级随机读、
// 追加写崩溃语义（D11）、加密索引（独立 indexDEK，D12）、copy/move 语义
// （D14）、孤儿回收、加密层文件列表。
//
// 对象布局（版本 1，头部 81 字节）：
//
//	magic "IRIXVT01"(8) | version(1) | blockSize(4) |
//	dekNonce(12) | dekCipher(48，masterKey 包裹的 DEK，wrapBlob) |
//	blockCount(4) | lastBlockSize(4) | 块体...
//
// 块体：每块 = nonce(12) + 密文(blockSize+16)。全块补齐存储，逻辑长度
// 记录在索引 —— 追加写只 append 新块 + 原地更新头部 blockCount（两次
// fsync），永不原地改写已有块，崩溃安全由构造保证（D11 的孤儿尾截断兜底）。
// 代价：每文件最多浪费 blockSize-1 字节。
//
// 追加写：数据起点与块边界对齐（旧长度是 blockSize 整数倍）时走块追加；
// 否则回退整文件重写（新对象 + 原子换索引，崩溃安全）。加密层追加不是
// 热路径（运行中的实例进程写明文目录，停止时整树回收），此取舍可接受。
//
// 锁模型：store.mu 保护索引；masterKey 归 vault.mu 管（S7 生命周期锁）。
// 涉及密钥的读写操作先取 v.mu.RLock，保证与 lock() 的销毁互斥。

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 对象头与索引常量（docs/vault-design.md §8.4/§8.5）。
const (
	objectMagic      = "IRIXVT01"
	objectVersion    = 1
	objectHeaderSize = 8 + 1 + 4 + 12 + 48 + 4 + 4 // = 81
	objectBlockOver  = 12 + 16                     // 每块 nonce(12) + GCM tag(16)
	objectIDBytes    = 32                          // 对象 ID 随机字节（hex 64 位）

	indexFlushTick  = time.Second // 索引脏标记延迟落盘周期（D12）
	indexFlushBatch = 100         // 迁移游标落盘间隔（文件数）
	hashCalcMaxSize = 8 << 20     // 加密层列表 sha256 计算的单文件上限（防大文件解密开销）
)

// vaultIndexEntry 索引条目：明文逻辑路径 → 密文对象。Size=-1 表示目录。
type vaultIndexEntry struct {
	ID     string `json:"id"`
	Size   int64  `json:"size"`   // 逻辑大小（-1 = 目录）
	MTime  int64  `json:"mtime"`  // unix 秒
	Blocks int    `json:"blocks"` // 块数（目录 0）
}

// vaultStore 加密存储层（Daemon.vault.store）。
type vaultStore struct {
	d *Daemon

	mu         sync.RWMutex
	index      map[string]*vaultIndexEntry
	dirty      bool
	indexDEK   []byte   // 索引密钥（解锁后内存，锁定清零）
	pendingDel []string // 待删旧对象：延迟到下次索引成功落盘后删除（崩溃安全，见 writeFile）
	objectsDir string
	indexFile  string
	blockSize  int
}

// newVaultStore 创建加密存储层并启动脏索引落盘循环。
func newVaultStore(d *Daemon, objectsDir, indexFile string, blockSize int) *vaultStore {
	s := &vaultStore{
		d:          d,
		index:      map[string]*vaultIndexEntry{},
		objectsDir: objectsDir,
		indexFile:  indexFile,
		blockSize:  blockSize,
	}
	go s.flushLoop()
	return s
}

// ---------------------------------------------------------------------------
// 对象基础读写
// ---------------------------------------------------------------------------

// objectPath 对象文件路径。
func (s *vaultStore) objectPath(id string) string {
	return filepath.Join(s.objectsDir, id)
}

// objectHeader 解析后的对象头。
type objectHeader struct {
	blockSize     int
	dek           []byte
	blockCount    uint32
	lastBlockSize uint32
}

// readHeader 读取对象头并解开 DEK。调用方须已持有 v.mu.RLock。
// 顺带执行孤儿尾截断（D11：崩溃于「写块后、更新计数前」的残留）。
func (s *vaultStore) readHeader(v *vaultState, id string) (*objectHeader, error) {
	f, err := os.Open(s.objectPath(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, objectHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("对象头读取失败: %w", err)
	}
	if string(buf[:8]) != objectMagic || buf[8] != objectVersion {
		return nil, errors.New("对象格式无效")
	}
	h := &objectHeader{
		blockSize:     int(binary.BigEndian.Uint32(buf[9:13])),
		blockCount:    binary.BigEndian.Uint32(buf[73:77]),
		lastBlockSize: binary.BigEndian.Uint32(buf[77:81]),
	}
	if h.blockSize <= 0 || h.blockSize > 64<<20 {
		return nil, errors.New("对象块大小无效")
	}
	dek, err := gcmUnwrap(v.masterKey, buf[13:73]) // nonce(12)+ct(32)+tag(16) = 60B
	if err != nil {
		return nil, errors.New("对象 DEK 解开失败（数据被篡改或密钥不匹配）")
	}
	h.dek = dek
	// 孤儿尾截断：文件超出「头 + blockCount 个整块」的部分丢弃
	expect := int64(objectHeaderSize) + int64(h.blockCount)*(int64(h.blockSize)+objectBlockOver)
	if fi, err := f.Stat(); err == nil && fi.Size() > expect {
		if terr := f.Truncate(expect); terr == nil {
			s.d.auditLogf("vault.store 孤儿尾截断 id=%s 丢弃 %d 字节", id, fi.Size()-expect)
		}
	}
	return h, nil
}

// readBlock 读取并解密第 i 块（全块，含补齐）。
func (s *vaultStore) readBlock(id string, h *objectHeader, i int) ([]byte, error) {
	if uint32(i) >= h.blockCount {
		return nil, errors.New("块越界")
	}
	gcm, err := newGCM(h.dek)
	if err != nil {
		return nil, err
	}
	off := int64(objectHeaderSize) + int64(i)*(int64(h.blockSize)+objectBlockOver)
	f, err := os.Open(s.objectPath(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, objectBlockOver+h.blockSize)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, buf[:12], buf[12:], nil)
	if err != nil {
		return nil, errors.New("块解密失败（数据被篡改）")
	}
	return plain, nil
}

// newObject 创建新对象（新 DEK，全块补齐）。调用方须已持有 v.mu.RLock。
func (s *vaultStore) newObject(v *vaultState, data []byte) (string, error) {
	idBytes := make([]byte, objectIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes)

	dek, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	dekWrap, err := gcmWrap(v.masterKey, dek)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return "", err
	}
	blocks := (len(data) + s.blockSize - 1) / s.blockSize
	if blocks == 0 {
		blocks = 1 // 空文件也占一块，维持「全块」不变量
	}
	if err := os.MkdirAll(s.objectsDir, 0o700); err != nil {
		return "", err
	}
	tmp := s.objectPath(id) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	abort := func() {
		f.Close()
		_ = os.Remove(tmp)
	}
	header := make([]byte, objectHeaderSize)
	copy(header[:8], objectMagic)
	header[8] = objectVersion
	binary.BigEndian.PutUint32(header[9:13], uint32(s.blockSize))
	copy(header[13:73], dekWrap) // 60B wrapBlob
	binary.BigEndian.PutUint32(header[73:77], uint32(blocks))
	binary.BigEndian.PutUint32(header[77:81], uint32(s.blockSize))
	if _, err := f.Write(header); err != nil {
		abort()
		return "", err
	}
	for i := 0; i < blocks; i++ {
		start := i * s.blockSize
		end := start + s.blockSize
		if end > len(data) {
			end = len(data)
		}
		block := make([]byte, s.blockSize)
		copy(block, data[start:end]) // 尾部零填充（补齐）
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			abort()
			return "", err
		}
		out := make([]byte, 0, objectBlockOver+s.blockSize)
		out = append(out, nonce...)
		out = gcm.Seal(out, nonce, block, nil)
		if _, err := f.Write(out); err != nil {
			abort()
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		abort()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, s.objectPath(id)); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return id, nil
}

// deleteObject 删除对象文件（best-effort；孤儿回收兜底）。
func (s *vaultStore) deleteObject(id string) {
	if id == "" {
		return
	}
	_ = os.Remove(s.objectPath(id))
	_ = os.Remove(s.objectPath(id) + ".tmp")
}

// ---------------------------------------------------------------------------
// 逻辑文件操作
// ---------------------------------------------------------------------------

// logicalPath 构造逻辑路径 "/实例UUID/相对路径"（斜杠分隔）。
// rel 须已通过 NormalizePath 校验（禁止 .. 越界）。
func logicalPath(uuid, rel string) string {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	return "/" + uuid + "/" + rel
}

// logicalParent 返回逻辑路径父目录（"a/b/c" → "a/b"；根返回 "/"）。
func logicalParent(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// lockedErr 密钥未解锁时统一错误。
func lockedErr() error { return errors.New("vault locked") }

// readFile 读取逻辑文件全部内容（小文件场景；大文件用 readRange）。
func (s *vaultStore) readFile(v *vaultState, path string) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return nil, lockedErr()
	}
	s.mu.RLock()
	e := s.index[path]
	s.mu.RUnlock()
	if e == nil {
		return nil, os.ErrNotExist
	}
	if e.Size < 0 {
		return nil, errors.New("目标为目录")
	}
	h, err := s.readHeader(v, e.ID)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, e.Size)
	for i := 0; i < int(h.blockCount); i++ {
		block, err := s.readBlock(e.ID, h, i)
		if err != nil {
			return nil, err
		}
		out = append(out, block...)
	}
	if int64(len(out)) > e.Size {
		out = out[:e.Size]
	}
	return out, nil
}

// readRange 读取逻辑文件 [off, off+n) 区间（按块定位，只解密目标块）。
func (s *vaultStore) readRange(v *vaultState, path string, off, n int64) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return nil, lockedErr()
	}
	s.mu.RLock()
	e := s.index[path]
	s.mu.RUnlock()
	if e == nil {
		return nil, os.ErrNotExist
	}
	if e.Size < 0 {
		return nil, errors.New("目标为目录")
	}
	if off >= e.Size || n <= 0 {
		return []byte{}, nil
	}
	if off+n > e.Size {
		n = e.Size - off
	}
	h, err := s.readHeader(v, e.ID)
	if err != nil {
		return nil, err
	}
	first := int(off / int64(h.blockSize))
	last := int((off + n - 1) / int64(h.blockSize))
	out := make([]byte, 0, n)
	for i := first; i <= last; i++ {
		block, err := s.readBlock(e.ID, h, i)
		if err != nil {
			return nil, err
		}
		// 求 [off, off+n) 与块 [bs, be) 的交集
		bs := int64(i) * int64(h.blockSize)
		be := bs + int64(len(block))
		from := off
		if from < bs {
			from = bs
		}
		to := off + n
		if to > be {
			to = be
		}
		if from < to {
			out = append(out, block[from-bs:to-bs]...)
		}
	}
	return out, nil
}

// writeFile 全量写入逻辑文件：新对象 + 新 DEK → 换索引 → 删旧对象。
func (s *vaultStore) writeFile(v *vaultState, path string, data []byte) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return lockedErr()
	}
	id, err := s.newObject(v, data)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	s.mu.Lock()
	old := s.index[path]
	s.index[path] = &vaultIndexEntry{
		ID:     id,
		Size:   int64(len(data)),
		MTime:  now,
		Blocks: (len(data) + s.blockSize - 1) / s.blockSize,
	}
	if old == nil || old.Size < 0 {
		parent := logicalParent(path)
		if _, ok := s.index[parent]; !ok && parent != "/" {
			s.index[parent] = &vaultIndexEntry{ID: "", Size: -1, MTime: now}
		}
	}
	s.dirty = true
	s.mu.Unlock()
	// 旧对象延迟删除（崩溃安全）：立即删旧对象会在「索引脏落盘窗口」崩溃时
	// 留下引用已删对象的陈旧索引（数据丢失）；延迟到下次索引落盘成功后删除，
	// 崩溃时旧对象仍在盘上，与新索引状态一致（多余对象由孤儿回收兜底）。
	if old != nil && old.Size >= 0 {
		s.mu.Lock()
		s.pendingDel = append(s.pendingDel, old.ID)
		s.mu.Unlock()
	}
	return nil
}

// appendFile 追加写（D11）：
//   - 旧长度块对齐 → 块追加（新块到 EOF + fsync → 头部 blockCount 原地更新 + fsync
//     → 索引 size 脏落盘）；崩溃于任意点不产生半块：块未被计数则下次打开截断，
//     计数已更新而索引滞后则读取截到旧长度（一致）。
//   - 旧长度非块对齐 → 整文件重写（新对象原子换索引），正确性优先。
func (s *vaultStore) appendFile(v *vaultState, path string, data []byte) (int64, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return 0, lockedErr()
	}
	s.mu.RLock()
	e := s.index[path]
	s.mu.RUnlock()
	if e == nil || e.Size < 0 {
		return 0, os.ErrNotExist
	}
	if len(data) == 0 {
		return e.Size, nil
	}
	if e.Size%int64(s.blockSize) != 0 {
		// 非对齐：整文件重写
		old, err := s.readFile(v, path)
		if err != nil {
			return 0, err
		}
		merged := make([]byte, 0, len(old)+len(data))
		merged = append(merged, old...)
		merged = append(merged, data...)
		if err := s.writeFile(v, path, merged); err != nil {
			return 0, err
		}
		return int64(len(merged)), nil
	}
	h, err := s.readHeader(v, e.ID)
	if err != nil {
		return 0, err
	}
	gcm, err := newGCM(h.dek)
	if err != nil {
		return 0, err
	}
	newSize := e.Size + int64(len(data))
	needBlocks := (int(newSize) + s.blockSize - 1) / s.blockSize
	addBlocks := needBlocks - int(h.blockCount)
	if addBlocks <= 0 {
		return e.Size, nil // 数据已含于块内（块对齐时不发生，防御）
	}
	// 新块内容：数据按 blockSize 切块，末块补齐
	src := make([]byte, addBlocks*s.blockSize)
	copy(src, data)
	f, err := os.OpenFile(s.objectPath(e.ID), os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	blockEnd := int64(objectHeaderSize) + int64(h.blockCount)*(int64(h.blockSize)+objectBlockOver)
	if _, err := f.Seek(blockEnd, io.SeekStart); err != nil {
		return 0, err
	}
	for i := 0; i < addBlocks; i++ {
		block := src[i*s.blockSize : (i+1)*s.blockSize]
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return 0, err
		}
		out := make([]byte, 0, objectBlockOver+s.blockSize)
		out = append(out, nonce...)
		out = gcm.Seal(out, nonce, block, nil)
		if _, err := f.Write(out); err != nil {
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	// 提交点：头部 blockCount 原地更新（4 字节 + fsync）
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, h.blockCount+uint32(addBlocks))
	if _, err := f.WriteAt(hdr, 73); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	e.Size = newSize
	e.MTime = time.Now().Unix()
	e.Blocks = needBlocks
	s.dirty = true
	s.mu.Unlock()
	return newSize, nil
}

// remove 删除逻辑路径（目录 = 前缀下全部条目 + 目录条目）。
func (s *vaultStore) remove(v *vaultState, path string) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return lockedErr()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.index[path]
	if e == nil {
		return os.ErrNotExist
	}
	delIDs := []string{}
	if e.Size >= 0 {
		delIDs = append(delIDs, e.ID)
		delete(s.index, path)
	} else {
		prefix := path + "/"
		for p, en := range s.index {
			if strings.HasPrefix(p, prefix) {
				if en.Size >= 0 {
					delIDs = append(delIDs, en.ID)
				}
				delete(s.index, p)
			}
		}
		delete(s.index, path)
	}
	s.dirty = true
	s.mu.Unlock()
	for _, id := range delIDs {
		s.deleteObject(id)
	}
	s.mu.Lock()
	return nil
}

// copy 复制（D14）：新对象 + 新 DEK + 新索引项。
func (s *vaultStore) copy(v *vaultState, src, dst string) error {
	data, err := s.readFile(v, src)
	if err != nil {
		return err
	}
	return s.writeFile(v, dst, data)
}

// move 移动（D14）：仅索引项迁移，对象与 DEK 复用（无引用计数、无别名）。
func (s *vaultStore) move(src, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.index[src]
	if e == nil {
		return os.ErrNotExist
	}
	if e.Size >= 0 {
		s.index[dst] = e
		delete(s.index, src)
	} else {
		prefix := src + "/"
		moved := []string{}
		for p := range s.index {
			if strings.HasPrefix(p, prefix) {
				moved = append(moved, p)
			}
		}
		sort.Strings(moved)
		for _, p := range moved {
			s.index[dst+"/"+strings.TrimPrefix(p, prefix)] = s.index[p]
			delete(s.index, p)
		}
		s.index[dst] = e
		delete(s.index, src)
	}
	s.dirty = true
	return nil
}

// mkdir 新建目录条目（幂等；已存在报错）。
func (s *vaultStore) mkdir(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[path]; ok {
		return errors.New("已存在同名文件或目录")
	}
	s.index[path] = &vaultIndexEntry{ID: "", Size: -1, MTime: time.Now().Unix()}
	s.dirty = true
	return nil
}

// touch 创建空文件（幂等：已存在则更新 mtime）。
func (s *vaultStore) touch(v *vaultState, path string) error {
	if e := s.stat(path); e != nil {
		if e.Size < 0 {
			return errors.New("目标为目录")
		}
		s.mu.Lock()
		e.MTime = time.Now().Unix()
		s.dirty = true
		s.mu.Unlock()
		return nil
	}
	return s.writeFile(v, path, []byte{})
}

// listDir 列出逻辑目录（加密层）：目录在前按名称排序，条目与明文层同构。
// sha256 需解密计算，仅对 ≤ hashCalcMaxSize 的文件计算（大文件返回空串）。
func (s *vaultStore) listDir(v *vaultState, path string) ([]map[string]any, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.masterKey == nil {
		return nil, lockedErr()
	}
	s.mu.RLock()
	prefix := "/"
	if path != "/" {
		prefix = path + "/"
	}
	type entry struct {
		item map[string]any
		path string
		size int64
	}
	items := map[string]*entry{}
	for p, e := range s.index {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		item := map[string]any{
			"name":  rest,
			"size":  e.Size,
			"mtime": time.Unix(e.MTime, 0).Format(clusterMtimeFormat),
			"time":  time.Unix(e.MTime, 0).Format(fileTimeFormat),
			"mode":  0o644,
		}
		if e.Size < 0 {
			item["type"] = 0
			item["sha256"] = ""
		} else {
			item["type"] = 1
			item["sha256"] = ""
		}
		items[rest] = &entry{item: item, path: p, size: e.Size}
	}
	s.mu.RUnlock()

	// 文件 sha256（锁外解密，避免持锁做 IO）
	for _, en := range items {
		if en.size < 0 || en.size > hashCalcMaxSize {
			continue
		}
		if data, err := s.readFile(v, en.path); err == nil {
			sum := sha256.Sum256(data)
			en.item["sha256"] = hex.EncodeToString(sum[:])
		}
	}
	list := make([]map[string]any, 0, len(items))
	for _, en := range items {
		list = append(list, en.item)
	}
	sort.Slice(list, func(a, b int) bool {
		ta, tb := list[a]["type"].(int), list[b]["type"].(int)
		if ta != tb {
			return ta < tb
		}
		return list[a]["name"].(string) < list[b]["name"].(string)
	})
	return list, nil
}

// stat 返回逻辑路径条目（nil = 不存在）。
func (s *vaultStore) stat(path string) *vaultIndexEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.index[path]
}

// walkPrefix 遍历逻辑前缀下的文件条目（加密层物化/回收用；不持锁调用回调）。
func (s *vaultStore) walkPrefix(v *vaultState, prefix string, fn func(path string, e *vaultIndexEntry) error) error {
	s.mu.RLock()
	var paths []string
	for p, e := range s.index {
		if strings.HasPrefix(p, prefix) && e.Size >= 0 {
			paths = append(paths, p)
		}
	}
	s.mu.RUnlock()
	sort.Strings(paths)
	for _, p := range paths {
		s.mu.RLock()
		e := s.index[p]
		s.mu.RUnlock()
		if e != nil {
			if err := fn(p, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 索引持久化 / 孤儿回收 / 物化辅助
// ---------------------------------------------------------------------------

// load 解锁时加载索引：解 indexDEK → 解密索引文件 → 孤儿回收。
// 调用方（解锁处理器）须已持有 v.mu.Lock。
func (s *vaultStore) load(v *vaultState, masterKey []byte, indexDEKWrapB64 string) error {
	wrap, err := mustB64Strict(indexDEKWrapB64)
	if err != nil {
		return errors.New("indexDEKWrapB64 格式无效")
	}
	indexDEK, err := gcmUnwrap(masterKey, wrap)
	if err != nil {
		return errors.New("indexDEK 解开失败（元数据被篡改？）")
	}
	s.mu.Lock()
	s.indexDEK = indexDEK
	s.index = map[string]*vaultIndexEntry{}
	s.dirty = false
	s.mu.Unlock()

	raw, err := os.ReadFile(s.indexFile)
	if err == nil {
		plain, err := gcmUnwrap(indexDEK, raw)
		if err != nil {
			return errors.New("索引解密失败（数据被篡改？）")
		}
		var idx map[string]*vaultIndexEntry
		if err := json.Unmarshal(plain, &idx); err != nil {
			return fmt.Errorf("索引解析失败: %w", err)
		}
		s.mu.Lock()
		s.index = idx
		s.dirty = false
		s.mu.Unlock()
	} else if !os.IsNotExist(err) {
		return err
	}
	s.sweepOrphans()
	return nil
}

// sweepOrphans 删除对象目录中索引未引用的对象（D12 兜底；审计记录）。
func (s *vaultStore) sweepOrphans() {
	entries, err := os.ReadDir(s.objectsDir)
	if err != nil {
		return
	}
	s.mu.RLock()
	used := map[string]bool{}
	for _, e := range s.index {
		if e.ID != "" {
			used[e.ID] = true
		}
	}
	s.mu.RUnlock()
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || e.IsDir() {
			continue
		}
		if !used[name] {
			if err := os.Remove(filepath.Join(s.objectsDir, name)); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		s.d.auditLogf("vault.orphan.reclaim 回收孤儿对象 %d 个", removed)
	}
}

// flush 索引落盘（原子写 tmp+rename；需 indexDEK）。
func (s *vaultStore) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexDEK == nil {
		return errors.New("索引未加载")
	}
	if !s.dirty {
		return nil
	}
	data, err := json.Marshal(s.index)
	if err != nil {
		return err
	}
	blob, err := gcmWrap(s.indexDEK, data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.indexFile), 0o700); err != nil {
		return err
	}
	tmp := s.indexFile + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.indexFile); err != nil {
		return err
	}
	s.dirty = false
	// 索引已落盘：此时删除待删旧对象安全（崩溃时旧对象已不被新索引引用，
	// 磁盘索引也已更新，不再指向它们）
	for _, id := range s.pendingDel {
		s.deleteObject(id)
	}
	s.pendingDel = nil
	return nil
}

// flushLoop 脏标记延迟落盘（D12：≤1s；崩溃最多丢最近 1s 索引变更，孤儿回收兜底）。
func (s *vaultStore) flushLoop() {
	for {
		time.Sleep(indexFlushTick)
		s.mu.RLock()
		dirty := s.dirty && s.indexDEK != nil
		s.mu.RUnlock()
		if dirty {
			if err := s.flush(); err != nil {
				s.d.auditLogf("vault.store 索引落盘失败: %v", err)
			}
		}
	}
}

// zeroKeys 锁定前清零索引密钥（best-effort）。
func (s *vaultStore) zeroKeys() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexDEK != nil {
		zeroBytes(s.indexDEK)
		s.indexDEK = nil
	}
	s.dirty = false
}

// mustB64Strict base64 解码（失败返回错误）。
func mustB64Strict(s string) ([]byte, error) {
	return b64d(s)
}

// materialize 物化：把逻辑前缀下的文件树解密到明文目录（实例启动，D9）。
// 调用方须已持有 v.mu.RLock（或处于解锁会话上下文）。
func (s *vaultStore) materialize(v *vaultState, uuid, plainDir string) error {
	return s.walkPrefix(v, "/"+uuid+"/", func(path string, e *vaultIndexEntry) error {
		data, err := s.readFile(v, path)
		if err != nil {
			return fmt.Errorf("物化 %s 失败: %w", path, err)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, "/"+uuid+"/"), "/")
		full := filepath.Join(plainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, data, 0o644)
	})
}

// encryptBack 回收：把明文目录整树加密入库并删除明文（实例停止/崩溃恢复，D9）。
// 幂等：索引条目 size/mtime 一致的明文文件跳过（不重复加密）。
func (s *vaultStore) encryptBack(v *vaultState, uuid, plainDir string) error {
	var files []string
	err := filepath.Walk(plainDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		rel, err := filepath.Rel(plainDir, path)
		if err != nil {
			return err
		}
		logical := logicalPath(uuid, filepath.ToSlash(rel))
		fi, err := os.Stat(path)
		if err != nil {
			continue // 已被并发删除
		}
		if e := s.stat(logical); e != nil && e.Size == fi.Size() && e.MTime == fi.ModTime().Unix() {
			continue // 未变更，跳过
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("回收读取 %s 失败: %w", path, err)
		}
		if err := s.writeFile(v, logical, data); err != nil {
			return fmt.Errorf("回收加密 %s 失败: %w", path, err)
		}
	}
	// 删除明文（含目录，自底向上）
	dirs := []string{}
	_ = filepath.Walk(plainDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for _, p := range files {
		_ = os.Remove(p)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	root := filepath.Clean(plainDir)
	for _, dir := range dirs {
		if filepath.Clean(dir) == root {
			continue // 保留实例工作目录本身（实例配置指向它）
		}
		_ = os.Remove(dir)
	}
	return nil
}
