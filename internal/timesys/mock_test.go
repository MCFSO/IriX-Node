// mock_test.go — 测试用 Roughtime 服务器（同时实现 legacy 与 RFC 10049 两种
// 线上格式），用于端到端验证客户端校验、Merkle 证明与时间系统。
package timesys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockOptions 模拟服务器的行为选项。
type mockOptions struct {
	Profile    Profile
	Offset     time.Duration // 报告时间相对真实时间的偏移
	Radius     time.Duration // 报告半径
	WrongKey   bool          // 用另一把长期私钥签名（签名校验应失败）
	WrongNonce bool          // 回显错误 nonce
	BadRoot    bool          // 返回错误的 Merkle 根
	BadMidp    bool          // MIDP 落在 DELE 有效期之外
	Silent     bool          // 不响应
	Leaves     int           // Merkle 树叶数（2 的幂；默认 1，模拟服务器批量签名）
	LeafIndex  int           // 本次请求所在的叶子下标
}

// mockServer 测试服务器。
type mockServer struct {
	t       *testing.T
	conn    *net.UDPConn
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	online  ed25519.PublicKey
	onlineK ed25519.PrivateKey
	opts    mockOptions
	queries atomic.Int64
	stop    chan struct{}
	wg      sync.WaitGroup
}

// startMockServer 启动一台模拟服务器并返回其地址。
func startMockServer(t *testing.T, opts mockOptions) *mockServer {
	t.Helper()
	if opts.Profile == "" {
		opts.Profile = ProfileLegacy
	}
	if opts.Radius <= 0 {
		opts.Radius = 20 * time.Millisecond
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成长期密钥失败: %v", err)
	}
	onlinePub, onlinePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成临时密钥失败: %v", err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听 UDP 失败: %v", err)
	}
	m := &mockServer{
		t: t, conn: conn, pub: pub, priv: priv,
		online: onlinePub, onlineK: onlinePriv,
		opts: opts, stop: make(chan struct{}),
	}
	m.wg.Add(1)
	go m.serve()
	t.Cleanup(m.Close)
	return m
}

// PublicKeyBase64 返回长期公钥的 base64 文本。
func (m *mockServer) PublicKeyBase64() string {
	return encodeBase64(m.pub)
}

// Address 返回服务器 UDP 地址。
func (m *mockServer) Address() string { return m.conn.LocalAddr().String() }

// Close 关闭服务器。
func (m *mockServer) Close() {
	select {
	case <-m.stop:
		return
	default:
	}
	close(m.stop)
	_ = m.conn.Close()
	m.wg.Wait()
}

// serve 处理请求。
func (m *mockServer) serve() {
	defer m.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, addr, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m.queries.Add(1)
		if m.opts.Silent {
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		resp, err := m.respond(req)
		if err != nil {
			continue
		}
		_, _ = m.conn.WriteToUDP(resp, addr)
	}
}

// respond 构造响应。
func (m *mockServer) respond(req []byte) ([]byte, error) {
	msg, _, err := parsePacket(req)
	if err != nil {
		return nil, err
	}
	nonce, ok := msg.get(tagNONC)
	if !ok {
		return nil, fmt.Errorf("请求缺少 NONC")
	}
	if m.opts.Profile == ProfileRFC10049 {
		if typ, ok := msg.get(tagTYPE); !ok || len(typ) != 4 || binary.LittleEndian.Uint32(typ) != 0 {
			return nil, fmt.Errorf("RFC 请求 TYPE 非法")
		}
		if srv, ok := msg.get(tagSRV); !ok || !equalBytes(srv, srvHash(m.pub)) {
			return nil, fmt.Errorf("RFC 请求 SRV 不匹配")
		}
		if len(nonce) != nonceLenRFC {
			return nil, fmt.Errorf("RFC 请求 nonce 长度错误")
		}
	} else {
		if len(nonce) != nonceLenLegacy {
			return nil, fmt.Errorf("legacy 请求 nonce 长度错误")
		}
	}
	echo := nonce
	if m.opts.WrongNonce {
		echo = append([]byte(nil), nonce...)
		echo[0] ^= 0xff
	}

	now := time.Now().Add(m.opts.Offset)
	mid := now
	if m.opts.BadMidp {
		mid = now.Add(24 * time.Hour)
	}
	mint := now.Add(-time.Hour)
	maxt := now.Add(time.Hour)
	if m.opts.BadMidp {
		mint = now.Add(-time.Hour)
		maxt = now.Add(-time.Minute)
	}

	dele := encodeMessage([]tagValue{
		{tagPUBK, m.online},
		{tagMINT, timestampToWire(m.opts.Profile, mint)},
		{tagMAXT, timestampToWire(m.opts.Profile, maxt)},
	})
	key := m.priv
	if m.opts.WrongKey {
		otherPub, otherPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		_ = otherPub
		key = otherPriv
	}
	deleSig := ed25519.Sign(key, append(append([]byte{}, delegationContext(m.opts.Profile)...), dele...))
	cert := encodeMessage([]tagValue{
		{tagSIG, deleSig},
		{tagDELE, dele},
	})

	path := []byte{}
	index := uint32(0)
	var root []byte
	// 构造真实的 Merkle 树：可配置叶数，模拟服务器批量签名（PATH 非空）。
	leafCount := m.opts.Leaves
	if leafCount < 1 {
		leafCount = 1
	}
	// 强制为 2 的幂（完整树），便于服务端与客户端按 INDX 位对齐。
	for leafCount&(leafCount-1) != 0 {
		leafCount++
	}
	if m.opts.LeafIndex >= 0 && m.opts.LeafIndex < leafCount {
		index = uint32(m.opts.LeafIndex)
	}
	leafInput := nonce
	if m.opts.Profile == ProfileRFC10049 {
		leafInput = req
	}
	inputs := make([][]byte, leafCount)
	for i := range inputs {
		if i == int(index) {
			inputs[i] = leafInput
			continue
		}
		decoy := make([]byte, 64)
		_, _ = rand.Read(decoy)
		inputs[i] = decoy
	}
	root, path = buildMerkleTree(m.opts.Profile, inputs, int(index))
	if m.opts.BadRoot {
		root = append([]byte(nil), root...)
		root[0] ^= 0xff
	}

	var srepPairs []tagValue
	if m.opts.Profile == ProfileRFC10049 {
		srepPairs = []tagValue{
			{tagVER, uint32Bytes(1)},
			{tagRADI, radiusToWire(m.opts.Profile, m.opts.Radius)},
			{tagMIDP, timestampToWire(m.opts.Profile, mid)},
			{tagVERS, uint32Bytes(1)},
			{tagROOT, root},
		}
	} else {
		srepPairs = []tagValue{
			{tagRADI, radiusToWire(m.opts.Profile, m.opts.Radius)},
			{tagMIDP, timestampToWire(m.opts.Profile, mid)},
			{tagROOT, root},
		}
	}
	srep := encodeMessage(srepPairs)
	sigKey := m.onlineK
	if m.opts.WrongKey {
		_, wrong, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		sigKey = wrong
	}
	srepSig := ed25519.Sign(sigKey, append(append([]byte{}, contextResponse...), srep...))

	var top []tagValue
	indx := uint32Bytes(index)
	if m.opts.Profile == ProfileRFC10049 {
		top = []tagValue{
			{tagSIG, srepSig},
			{tagNONC, echo},
			{tagTYPE, uint32Bytes(1)},
			{tagPATH, path},
			{tagSREP, srep},
			{tagCERT, cert},
			{tagINDX, indx},
		}
	} else {
		top = []tagValue{
			{tagSIG, srepSig},
			{tagNONC, echo},
			{tagPATH, path},
			{tagSREP, srep},
			{tagCERT, cert},
			{tagINDX, indx},
		}
	}
	return wrapPacket(encodeMessage(top)), nil
}

// buildMerkleTree 按协议规则构造完整二叉 Merkle 树，返回根与 index 叶的 PATH。
// 叶数必须是 2 的幂（调用方补齐），这样 INDX 位与层级一一对应。
func buildMerkleTree(profile Profile, inputs [][]byte, index int) (root, path []byte) {
	chunk := hashLenRFC
	if profile == ProfileLegacy {
		chunk = hashLenLegacy
	}
	hash := func(b []byte) []byte {
		sum := sha512.Sum512(b)
		return sum[:chunk]
	}
	node := func(left, right []byte) []byte {
		buf := make([]byte, 0, 1+2*chunk)
		buf = append(buf, 0x01)
		buf = append(buf, left...)
		buf = append(buf, right...)
		return hash(buf)
	}
	level := make([][]byte, len(inputs))
	for i, in := range inputs {
		level[i] = hash(append([]byte{0x00}, in...))
	}
	pos := index
	for len(level) > 1 {
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, node(level[i], level[i+1]))
		}
		if pos%2 == 0 {
			path = append(path, level[pos+1]...)
		} else {
			path = append(path, level[pos-1]...)
		}
		pos /= 2
		level = next
	}
	return level[0], path
}

// delegationContext 返回与格式对应的证书签名上下文。
func delegationContext(profile Profile) []byte {
	if profile == ProfileLegacy {
		return contextDelegationLegacy
	}
	return contextDelegationRFC
}

// srvHash 计算 RFC 10049 的 SRV 值 H(0xff || 公钥)。
func srvHash(pub ed25519.PublicKey) []byte {
	sum := sha512.Sum512(append([]byte{0xff}, pub...))
	return sum[:32]
}

// encodeBase64 编码为标准 base64。
func encodeBase64(raw []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, (len(raw)+2)/3*4)
	for i := 0; i < len(raw); i += 3 {
		var b [3]byte
		n := copy(b[:], raw[i:])
		out = append(out, alphabet[b[0]>>2], alphabet[(b[0]&0x03)<<4|b[1]>>4])
		if n > 1 {
			out = append(out, alphabet[(b[1]&0x0f)<<2|b[2]>>6])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, alphabet[b[2]&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// equalBytes 比较字节串。
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
