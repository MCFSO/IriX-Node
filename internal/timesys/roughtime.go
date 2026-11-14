// roughtime.go — Roughtime 协议实现（客户端侧）。
//
// 节点的时间唯一来源是 Roughtime（RFC 10049）：客户端用自己的随机 nonce
// 向若干互相独立的服务器查询，校验 Ed25519 签名与 Merkle 包含证明后，
// 得到「服务器在 (MIDP-RADI, MIDP+RADI) 之间签名」的时间区间。
// 该区间是后续时钟纪律（clock.go）唯一可采信的外部真值。
//
// 同时支持两种线上格式：
//   - legacy：已部署的公网服务器（Cloudflare / Netnod / int08h 等）实际使用
//     的格式：64 字节 nonce、SHA-512 完整 64 字节 Merkle 节点、MIDP/RADI
//     为微秒、请求只含 NONC + PAD，证书上下文带 "--"；
//   - rfc10049：RFC 10049 规定的版本 1：32 字节 nonce、SHA-512 截断 32 字节
//     节点、MIDP/RADI 为秒、请求含 VER/SRV/TYPE/ZZZZ。
//
// 两者都由 auto 模式自动探测（先 legacy，失败再 RFC 10049），成功后按服务器
// 缓存，避免每轮都多付一次超时。
package timesys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// 线上格式常量。
const (
	// roughtimeMagic 是 Roughtime 数据包的 8 字节魔数（ASCII "ROUGHTIM"，
	// 按小端序解释为 uint64）。
	roughtimeMagic uint64 = 0x4d49544847554f52
	// packetHeaderLen = 魔数 8 字节 + 消息长度 4 字节。
	packetHeaderLen = 12
	// minRequestMessageLen RFC 要求 UDP 请求消息至少 1024 字节（防放大）。
	minRequestMessageLen = 1024
	// maxResponseMessageLen 客户端接受的最大响应消息（防内存放大）。
	maxResponseMessageLen = 8192
	// maxMessageTags 单条消息最多 tag 数（协议实现上限，防解析放大）。
	maxMessageTags = 64
	// maxPathChunks Merkle 路径最多 32 个节点（RFC 限制）。
	maxPathChunks = 32

	nonceLenLegacy = 64
	nonceLenRFC    = 32
	hashLenLegacy  = 64
	hashLenRFC     = 32

	ed25519SignatureLen = ed25519.SignatureSize
	ed25519PublicKeyLen = ed25519.PublicKeySize
)

// 线上 tag。tag 为 uint32，首字符位于最低字节（小端序自然字节序）。
var (
	tagSIG  = makeTag("SIG\x00")
	tagVER  = makeTag("VER\x00")
	tagSRV  = makeTag("SRV\x00")
	tagNONC = makeTag("NONC")
	tagDELE = makeTag("DELE")
	tagTYPE = makeTag("TYPE")
	tagPATH = makeTag("PATH")
	tagRADI = makeTag("RADI")
	tagPUBK = makeTag("PUBK")
	tagMIDP = makeTag("MIDP")
	tagSREP = makeTag("SREP")
	tagVERS = makeTag("VERS")
	tagMINT = makeTag("MINT")
	tagROOT = makeTag("ROOT")
	tagCERT = makeTag("CERT")
	tagMAXT = makeTag("MAXT")
	tagINDX = makeTag("INDX")
	tagZZZZ = makeTag("ZZZZ")
	// tagPAD 是部署版协议的填充 tag（"PAD\xff"）。
	tagPAD = makeTag("PAD\xff")
)

// 签名上下文（含结尾 0 字节）。两种格式只差在 DELE 的 "--" 后缀上。
var (
	contextDelegationLegacy = []byte("RoughTime v1 delegation signature--\x00")
	contextDelegationRFC    = []byte("RoughTime v1 delegation signature\x00")
	contextResponse         = []byte("RoughTime v1 response signature\x00")
)

// makeTag 将最多 4 个字节（不足补 0）编码为 tag。
func makeTag(s string) uint32 {
	var b [4]byte
	copy(b[:], s)
	return binary.LittleEndian.Uint32(b[:])
}

// Profile Roughtime 线上格式。
type Profile string

const (
	// ProfileAuto 自动探测（先 legacy，后 RFC 10049）。
	ProfileAuto Profile = "auto"
	// ProfileLegacy 已部署的公网服务器格式（64 字节 nonce、微秒时间戳）。
	ProfileLegacy Profile = "legacy"
	// ProfileRFC10049 RFC 10049 版本 1（32 字节 nonce、秒级时间戳）。
	ProfileRFC10049 Profile = "rfc10049"
)

// ParseProfile 解析配置中的协议名（空 = auto）。
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case "", ProfileAuto:
		return ProfileAuto, nil
	case ProfileLegacy, ProfileRFC10049:
		return Profile(s), nil
	default:
		return "", fmt.Errorf("未知的 Roughtime 协议格式 %q（须为 auto / legacy / rfc10049）", s)
	}
}

// Server 一台受信任的 Roughtime 服务器。
type Server struct {
	Name      string  `json:"name"`               // 展示名（为空时用地址）
	Address   string  `json:"address"`            // host:port（UDP）
	PublicKey string  `json:"publicKey"`          // Ed25519 长期公钥（base64）
	Profile   Profile `json:"protocol,omitempty"` // 线上格式，空 = auto
}

// DisplayName 返回展示名。
func (s Server) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Address
}

// decodeServerKey 解码服务器长期公钥。
func decodeServerKey(text string) (ed25519.PublicKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("Roughtime 服务器公钥为空")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(text); err == nil {
			if len(raw) != ed25519PublicKeyLen {
				return nil, fmt.Errorf("Roughtime 公钥长度无效: %d（需 %d 字节）", len(raw), ed25519PublicKeyLen)
			}
			return ed25519.PublicKey(raw), nil
		}
	}
	return nil, errors.New("Roughtime 公钥不是合法 base64")
}

// Result 一次成功查询的结果。
type Result struct {
	Server    string        // 服务器展示名
	Address   string        // 服务器地址
	Profile   Profile       // 实际使用的线上格式
	Midpoint  time.Time     // 服务器保证时间区间中点
	Radius    time.Duration // 服务器给出的不确定度（半宽）
	RoundTrip time.Duration // 本地观测到的往返时延
	MonoSent  time.Time     // 发出请求时的系统时间（含单调读数）
	MonoRecv  time.Time     // 收到响应时的系统时间（含单调读数）
	Raw       []byte        // 原始响应（故障取证/日志用，不含敏感信息）
	Request   []byte        // 原始请求（Merkle 叶子输入/已知答案测试用）

	nonce []byte // 本次请求的 nonce（legacy 的 Merkle 叶子输入）
}

// rawLeafInput 返回该格式 Merkle 叶子的原始输入（RFC 为整个请求包，legacy 为 nonce）。
func (r *Result) rawLeafInput() []byte {
	if r.Profile == ProfileRFC10049 {
		return r.Request
	}
	return r.nonce
}

// MonoMid 返回请求/响应中点对应的系统单调时间。
func (r *Result) MonoMid() time.Time {
	return r.MonoSent.Add(r.RoundTrip / 2)
}

// Uncertainty 把服务器半径与链路往返误差合成为单次测量的不确定度。
func (r *Result) Uncertainty() time.Duration {
	if r == nil {
		return 0
	}
	return r.Radius + r.RoundTrip/2 + time.Millisecond
}

// Client Roughtime UDP 客户端。
type Client struct {
	Timeout time.Duration // 单次查询超时（默认 1.5s）
}

// Query 向一台服务器查询时间。ProfileAuto 时依次尝试 legacy 与 RFC 10049。
//
// 校验顺序（任一步失败即拒绝该响应，符合 RFC 10049 §5.4）：
//  1. CERT 的 DELE 签名用服务器长期公钥验证；
//  2. MIDP 落在 DELE 的 MINT..MAXT 区间内；
//  3. INDX/PATH 证明请求 nonce 位于签名覆盖的 Merkle 根中；
//  4. 顶层 SIG 用 DELE 中的临时公钥验证 SREP；
//  5. NONC 与本地 nonce 一致、TYPE/VER 正确、时间量级合理。
func (c *Client) Query(ctx context.Context, srv Server) (*Result, error) {
	pub, err := decodeServerKey(srv.PublicKey)
	if err != nil {
		return nil, err
	}
	profile := srv.Profile
	if profile == "" {
		profile = ProfileAuto
	}
	profiles := []Profile{profile}
	if profile == ProfileAuto {
		profiles = []Profile{ProfileLegacy, ProfileRFC10049}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	var errs []error
	for _, p := range profiles {
		res, err := c.queryProfile(ctx, srv, pub, p, timeout)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("查询 %s 失败: %w", srv.DisplayName(), errors.Join(errs...))
}

// queryProfile 用指定格式查询一次；域名会解析为多个地址依次尝试
// （IPv4 优先，IPv6 兜底），避免双栈网络里 AAAA 记录不可达时整轮超时。
func (c *Client) queryProfile(ctx context.Context, srv Server, pub ed25519.PublicKey, profile Profile, timeout time.Duration) (*Result, error) {
	nonce := make([]byte, nonceLenLegacy)
	if profile == ProfileRFC10049 {
		nonce = make([]byte, nonceLenRFC)
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}
	request, err := buildRequest(profile, nonce, pub)
	if err != nil {
		return nil, err
	}

	addresses := resolveServerAddresses(ctx, srv.Address)
	deadline := time.Now().Add(timeout)
	var errs []error
	for i, addr := range addresses {
		if ctx.Err() != nil {
			break
		}
		remain := time.Until(deadline)
		if remain < 50*time.Millisecond {
			break
		}
		attempt := remain
		if i < len(addresses)-1 {
			// 多地址：首个地址只给一半预算，给另一协议族留出重试时间。
			attempt = remain / 2
			if attempt < 200*time.Millisecond {
				attempt = remain
			}
		}
		res, err := c.queryAddress(ctx, srv, addr, request, nonce, profile, pub, attempt)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr, err))
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("没有可用的服务器地址（DNS 解析或超时）")
	}
	return nil, errors.Join(errs...)
}

// queryAddress 向单个具体地址发起一次查询。
func (c *Client) queryAddress(ctx context.Context, srv Server, addr string, request, nonce []byte, profile Profile, pub ed25519.PublicKey, timeout time.Duration) (*Result, error) {
	dialer := net.Dialer{Timeout: timeout}
	sent := time.Now()
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	buf := make([]byte, maxResponseMessageLen)
	n, err := conn.Read(buf)
	recv := time.Now()
	if err != nil {
		return nil, fmt.Errorf("等待响应失败: %w", err)
	}
	raw := buf[:n]
	midpoint, radius, err := verifyResponse(raw, request, nonce, profile, pub)
	if err != nil {
		return nil, err
	}
	return &Result{
		Server:    srv.DisplayName(),
		Address:   srv.Address,
		Profile:   profile,
		Midpoint:  midpoint,
		Radius:    radius,
		RoundTrip: recv.Sub(sent),
		MonoSent:  sent,
		MonoRecv:  recv,
		Raw:       raw,
		Request:   request,
		nonce:     nonce,
	}, nil
}

// resolveServerAddresses 把 host:port 解析为可尝试的地址列表：IPv4 在前、
// IPv6 在后；IP 字面量原样返回；解析失败时回退原地址（交给拨号器处理）。
func resolveServerAddresses(ctx context.Context, address string) []string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return []string{address}
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{address}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil || len(ips) == 0 {
		return []string{address}
	}
	var v4, v6 []string
	for _, ip := range ips {
		hostPort := net.JoinHostPort(ip.IP.String(), port)
		if ip.IP.To4() != nil {
			v4 = append(v4, hostPort)
		} else {
			v6 = append(v6, hostPort)
		}
	}
	out := append(v4, v6...)
	if len(out) == 0 {
		return []string{address}
	}
	return out
}

// buildRequest 构造请求数据包（消息至少 1024 字节）。
func buildRequest(profile Profile, nonce []byte, pub ed25519.PublicKey) ([]byte, error) {
	switch profile {
	case ProfileLegacy:
		if len(nonce) != nonceLenLegacy {
			return nil, fmt.Errorf("legacy nonce 长度无效: %d", len(nonce))
		}
		// NONC(64) + PAD(填充到 1024)
		pad := minRequestMessageLen - (8*2 + len(nonce))
		if pad < 4 || pad%4 != 0 {
			return nil, errors.New("legacy 请求填充长度无效")
		}
		msg := encodeMessage([]tagValue{
			{tagNONC, nonce},
			{tagPAD, make([]byte, pad)},
		})
		return wrapPacket(msg), nil
	case ProfileRFC10049:
		if len(nonce) != nonceLenRFC {
			return nil, fmt.Errorf("RFC 10049 nonce 长度无效: %d", len(nonce))
		}
		// VER(4) + SRV(32) + NONC(32) + TYPE(4) + ZZZZ(填充到 1024)
		sum := sha512.Sum512(append([]byte{0xff}, pub...))
		pad := minRequestMessageLen - (8*5 + 4 + 32 + 32 + 4)
		if pad < 4 || pad%4 != 0 {
			return nil, errors.New("RFC 10049 请求填充长度无效")
		}
		msg := encodeMessage([]tagValue{
			{tagVER, uint32Bytes(1)},     // 支持的版本：1
			{tagSRV, sum[:32]},           // 期望的服务器长期公钥指纹
			{tagNONC, nonce},             // 32 字节 nonce
			{tagTYPE, uint32Bytes(0)},    // 请求
			{tagZZZZ, make([]byte, pad)}, // 填充
		})
		return wrapPacket(msg), nil
	default:
		return nil, fmt.Errorf("不支持的 Roughtime 格式: %q", profile)
	}
}

// wrapPacket 加上 "ROUGHTIM" 魔数与消息长度。
func wrapPacket(msg []byte) []byte {
	out := make([]byte, packetHeaderLen+len(msg))
	binary.LittleEndian.PutUint64(out[0:8], roughtimeMagic)
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(msg)))
	copy(out[12:], msg)
	return out
}

// tagValue 消息中的一个 (tag, value)。
type tagValue struct {
	tag   uint32
	value []byte
}

// encodeMessage 按 tag 升序编码一条 Roughtime 消息。
// 调用方必须按 tag 升序给出（协议要求），值长度必须为 4 的倍数。
func encodeMessage(pairs []tagValue) []byte {
	header := 8 * len(pairs)
	total := header
	for _, p := range pairs {
		total += len(p.value)
	}
	out := make([]byte, total)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(pairs)))
	off := header
	for i, p := range pairs {
		if i > 0 {
			binary.LittleEndian.PutUint32(out[4+4*(i-1):], uint32(off-header))
		}
		binary.LittleEndian.PutUint32(out[4+4*(len(pairs)-1)+4*i:], p.tag)
		copy(out[off:], p.value)
		off += len(p.value)
	}
	return out
}

// uint32Bytes 返回 uint32 的小端字节。
func uint32Bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// uint64Bytes 返回 uint64 的小端字节。
func uint64Bytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// parsedMessage 解析后的 Roughtime 消息。
type parsedMessage struct {
	tags   []uint32
	values [][]byte
}

// get 读取指定 tag 的值。
func (m *parsedMessage) get(tag uint32) ([]byte, bool) {
	for i, t := range m.tags {
		if t == tag {
			return m.values[i], true
		}
	}
	return nil, false
}

// require 读取必需 tag，缺失时返回错误。
func (m *parsedMessage) require(tag uint32, name string) ([]byte, error) {
	v, ok := m.get(tag)
	if !ok {
		return nil, fmt.Errorf("响应缺少 %s 标签", name)
	}
	return v, nil
}

// parseMessage 严格解析一条 Roughtime 消息：tag 必须严格升序、
// 偏移必须 4 字节对齐且不越界、值区长度必须为 4 的倍数。
func parseMessage(data []byte) (*parsedMessage, error) {
	if len(data) < 4 {
		return nil, errors.New("消息过短")
	}
	n := binary.LittleEndian.Uint32(data[0:4])
	if n == 0 || n > maxMessageTags {
		return nil, fmt.Errorf("消息 tag 数非法: %d", n)
	}
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("消息长度 %d 不是 4 的倍数", len(data))
	}
	header := 8 * int(n)
	if len(data) < header {
		return nil, errors.New("消息头越界")
	}
	valuesLen := len(data) - header
	offsets := make([]uint32, n)
	for i := 1; i < int(n); i++ {
		offsets[i] = binary.LittleEndian.Uint32(data[4+4*(i-1):])
	}
	tags := make([]uint32, n)
	for i := 0; i < int(n); i++ {
		tags[i] = binary.LittleEndian.Uint32(data[4+4*(int(n)-1)+4*i:])
	}
	for i := 0; i < int(n); i++ {
		if i > 0 && tags[i] <= tags[i-1] {
			return nil, errors.New("消息 tag 未严格升序")
		}
		if offsets[i] > uint32(valuesLen) || offsets[i]%4 != 0 {
			return nil, fmt.Errorf("消息偏移非法: %d", offsets[i])
		}
		if i > 0 && offsets[i] < offsets[i-1] {
			return nil, errors.New("消息偏移递减")
		}
	}
	values := make([][]byte, n)
	for i := 0; i < int(n); i++ {
		end := uint32(valuesLen)
		if i+1 < int(n) {
			end = offsets[i+1]
		}
		values[i] = data[header+int(offsets[i]) : header+int(end)]
	}
	return &parsedMessage{tags: tags, values: values}, nil
}

// parsePacket 校验包头并解析消息。
func parsePacket(packet []byte) (*parsedMessage, []byte, error) {
	if len(packet) < packetHeaderLen {
		return nil, nil, errors.New("响应数据包过短")
	}
	if binary.LittleEndian.Uint64(packet[0:8]) != roughtimeMagic {
		return nil, nil, errors.New("响应魔数错误（不是 Roughtime 数据包）")
	}
	msgLen := binary.LittleEndian.Uint32(packet[8:12])
	if int(msgLen) != len(packet)-packetHeaderLen {
		return nil, nil, fmt.Errorf("响应长度声明 %d 与实际 %d 不一致", msgLen, len(packet)-packetHeaderLen)
	}
	if msgLen > maxResponseMessageLen {
		return nil, nil, fmt.Errorf("响应消息过大: %d", msgLen)
	}
	msgData := packet[packetHeaderLen:]
	msg, err := parseMessage(msgData)
	if err != nil {
		return nil, nil, err
	}
	return msg, msgData, nil
}

// verifyResponse 校验响应，返回被签名覆盖的时间中点与半径。
func verifyResponse(packet, request, nonce []byte, profile Profile, pub ed25519.PublicKey) (time.Time, time.Duration, error) {
	msg, _, err := parsePacket(packet)
	if err != nil {
		return time.Time{}, 0, err
	}
	// TYPE：RFC 10049 响应必须为 1；部署版没有该 tag，有则同样校验。
	if v, ok := msg.get(tagTYPE); ok {
		if len(v) != 4 || binary.LittleEndian.Uint32(v) != 1 {
			return time.Time{}, 0, errors.New("响应 TYPE 非法")
		}
	}
	nonceResp, err := msg.require(tagNONC, "NONC")
	if err != nil {
		return time.Time{}, 0, err
	}
	if subtle.ConstantTimeCompare(nonceResp, nonce) != 1 {
		return time.Time{}, 0, errors.New("响应 nonce 与请求不一致（可能被重放/串扰）")
	}

	// 1) 长期公钥验证 DELE 证书。
	certData, err := msg.require(tagCERT, "CERT")
	if err != nil {
		return time.Time{}, 0, err
	}
	cert, err := parseMessage(certData)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("CERT 消息非法: %w", err)
	}
	deleData, err := cert.require(tagDELE, "DELE")
	if err != nil {
		return time.Time{}, 0, err
	}
	certSig, err := cert.require(tagSIG, "SIG")
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(certSig) != ed25519SignatureLen {
		return time.Time{}, 0, fmt.Errorf("证书签名长度非法: %d", len(certSig))
	}
	context := contextDelegationRFC
	if profile == ProfileLegacy {
		context = contextDelegationLegacy
	}
	if !ed25519.Verify(pub, append(append([]byte{}, context...), deleData...), certSig) {
		return time.Time{}, 0, errors.New("DELE 证书签名无效（服务器公钥不匹配或被篡改）")
	}

	dele, err := parseMessage(deleData)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("DELE 消息非法: %w", err)
	}
	onlineKey, err := dele.require(tagPUBK, "PUBK")
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(onlineKey) != ed25519PublicKeyLen {
		return time.Time{}, 0, fmt.Errorf("临时公钥长度非法: %d", len(onlineKey))
	}
	mintRaw, err := dele.require(tagMINT, "MINT")
	if err != nil {
		return time.Time{}, 0, err
	}
	maxtRaw, err := dele.require(tagMAXT, "MAXT")
	if err != nil {
		return time.Time{}, 0, err
	}
	mint, err := decodeTimestamp(profile, mintRaw)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("MINT 非法: %w", err)
	}
	maxt, err := decodeTimestamp(profile, maxtRaw)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("MAXT 非法: %w", err)
	}
	if !mint.Before(maxt) {
		return time.Time{}, 0, errors.New("DELE 有效期非法（MINT 不早于 MAXT）")
	}

	// 2) 临时公钥验证 SREP。
	srepData, err := msg.require(tagSREP, "SREP")
	if err != nil {
		return time.Time{}, 0, err
	}
	topSig, err := msg.require(tagSIG, "SIG")
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(topSig) != ed25519SignatureLen {
		return time.Time{}, 0, fmt.Errorf("响应签名长度非法: %d", len(topSig))
	}
	if !ed25519.Verify(ed25519.PublicKey(onlineKey), append(append([]byte{}, contextResponse...), srepData...), topSig) {
		return time.Time{}, 0, errors.New("SREP 响应签名无效")
	}

	srep, err := parseMessage(srepData)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("SREP 消息非法: %w", err)
	}
	if profile == ProfileRFC10049 {
		// RFC 10049：SREP 内的 VER 必须是 1，VERS 必须包含 1。
		if v, ok := srep.get(tagVER); ok {
			if len(v) != 4 || binary.LittleEndian.Uint32(v) != 1 {
				return time.Time{}, 0, errors.New("SREP VER 非法")
			}
		}
		if v, ok := srep.get(tagVERS); ok {
			if !containsUint32(v, 1) {
				return time.Time{}, 0, errors.New("SREP VERS 不含版本 1")
			}
		}
	}
	midRaw, err := srep.require(tagMIDP, "MIDP")
	if err != nil {
		return time.Time{}, 0, err
	}
	radRaw, err := srep.require(tagRADI, "RADI")
	if err != nil {
		return time.Time{}, 0, err
	}
	root, err := srep.require(tagROOT, "ROOT")
	if err != nil {
		return time.Time{}, 0, err
	}
	midpoint, err := decodeTimestamp(profile, midRaw)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("MIDP 非法: %w", err)
	}
	radius, err := decodeRadius(profile, radRaw)
	if err != nil {
		return time.Time{}, 0, err
	}

	// 3) 时间量级与半径合理性（拒绝垃圾/故障服务器）。
	if midpoint.Year() < 2000 || midpoint.Year() > 2200 {
		return time.Time{}, 0, fmt.Errorf("服务器时间量级异常: %s", midpoint.UTC().Format(time.RFC3339))
	}
	if radius <= 0 || radius > time.Hour {
		return time.Time{}, 0, fmt.Errorf("服务器时间半径异常: %s", radius)
	}
	if midpoint.Before(mint) || midpoint.After(maxt) {
		return time.Time{}, 0, errors.New("MIDP 落在 DELE 有效期之外（证书已过期或被回溯）")
	}

	// 4) Merkle 包含证明：nonce（或整个请求包）到 ROOT。
	indxRaw, err := msg.require(tagINDX, "INDX")
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(indxRaw) != 4 {
		return time.Time{}, 0, fmt.Errorf("INDX 长度非法: %d", len(indxRaw))
	}
	index := binary.LittleEndian.Uint32(indxRaw)
	path, err := msg.require(tagPATH, "PATH")
	if err != nil {
		return time.Time{}, 0, err
	}
	rootLen := hashLenRFC
	leafInput := request
	if profile == ProfileLegacy {
		rootLen = hashLenLegacy
		leafInput = nonce
	}
	if len(root) != rootLen {
		return time.Time{}, 0, fmt.Errorf("ROOT 长度非法: %d", len(root))
	}
	if err := verifyMerklePath(profile, leafInput, path, index, root); err != nil {
		return time.Time{}, 0, err
	}
	return midpoint, radius, nil
}

// verifyMerklePath 按 RFC 10049 §5.3.1 从叶子重算 Merkle 根并与 ROOT 比较。
//
// RFC 10049 的文字（bit=0 时节点在左）与已部署实现（bit=0 时当前节点在左）
// 方向相反。为兼容两种实现，先按已部署方向校验；RFC 格式再尝试 RFC 文字方向。
// 两个方向都只是「沿 path 重算到已签名根」的两种父子顺序，接受任一方向不削弱
// 包含证明（伪造仍需要 SHA-512 碰撞）。
func verifyMerklePath(profile Profile, leafInput, path []byte, index uint32, root []byte) error {
	chunk := hashLenRFC
	if profile == ProfileLegacy {
		chunk = hashLenLegacy
	}
	if len(path)%chunk != 0 {
		return fmt.Errorf("PATH 长度 %d 不是哈希长度 %d 的倍数", len(path), chunk)
	}
	if len(path)/chunk > maxPathChunks {
		return fmt.Errorf("PATH 节点过多: %d", len(path)/chunk)
	}
	h, leftover, err := merkleRoot(profile, leafInput, path, index, false)
	if err != nil {
		return err
	}
	if leftover == 0 && bytes.Equal(h, root) {
		return nil
	}
	if profile == ProfileRFC10049 {
		// RFC 文字方向（bit=0 → H(node || hash)）。
		if alt, altLeft, altErr := merkleRoot(profile, leafInput, path, index, true); altErr == nil &&
			altLeft == 0 && bytes.Equal(alt, root) {
			return nil
		}
	}
	if leftover != 0 {
		return errors.New("PATH 未覆盖 INDX 的全部比特（树高度与索引不一致）")
	}
	return errors.New("Merkle 包含证明失败（nonce 不在服务器签名覆盖的树中）")
}

// merkleRoot 按指定方向重算根，返回根、消耗完 path 后剩余的索引位移。
// rfcLiteral=true 表示 bit=0 时 PATH 节点在左（RFC 10049 文字方向）。
func merkleRoot(profile Profile, leafInput, path []byte, index uint32, rfcLiteral bool) ([]byte, uint32, error) {
	chunk := hashLenRFC
	useRequestPacket := true
	if profile == ProfileLegacy {
		chunk = hashLenLegacy
		useRequestPacket = false
	}
	var leaf []byte
	sum := sha512.Sum512(append([]byte{0x00}, leafInput...))
	if useRequestPacket {
		leaf = sum[:hashLenRFC]
	} else {
		leaf = sum[:]
	}
	h := leaf
	for i := 0; i < len(path)/chunk; i++ {
		sibling := path[i*chunk : (i+1)*chunk]
		buf := make([]byte, 0, 1+2*chunk)
		buf = append(buf, 0x01)
		siblingOnRight := index&1 == 0
		if rfcLiteral {
			siblingOnRight = index&1 != 0
		}
		if siblingOnRight {
			buf = append(buf, h...)
			buf = append(buf, sibling...)
		} else {
			buf = append(buf, sibling...)
			buf = append(buf, h...)
		}
		s := sha512.Sum512(buf)
		if chunk == hashLenRFC {
			h = s[:hashLenRFC]
		} else {
			h = s[:]
		}
		index >>= 1
	}
	return h, index, nil
}

// decodeTimestamp 解码 8 字节时间戳。
//
// 线上单位按数值量级自适应：秒（约 1.7e9）、毫秒（1.7e12）、微秒（1.7e15）、
// 纳秒（1.7e18）之间相差三个数量级以上，不存在歧义。这样既能读 RFC 10049
// 的秒级时间戳，也能读沿用旧部署「微秒」单位、但请求/响应结构已是新版的服务
// 器（公网上确实存在这种混合实现）。单位只影响解读，签名覆盖原始数值，
// 因而不会削弱完整性校验。
func decodeTimestamp(profile Profile, raw []byte) (time.Time, error) {
	if len(raw) != 8 {
		return time.Time{}, fmt.Errorf("时间戳长度 %d（需 8）", len(raw))
	}
	v := binary.LittleEndian.Uint64(raw)
	switch {
	case v < 100_000_000_000: // < 1e11：秒
		return time.Unix(int64(v), 0).UTC(), nil
	case v < 100_000_000_000_000: // < 1e14：毫秒（拆分秒/纳秒，避免乘法溢出）
		return time.Unix(int64(v/1000), int64(v%1000)*int64(time.Millisecond)).UTC(), nil
	case v < 100_000_000_000_000_000: // < 1e17：微秒
		return time.Unix(int64(v/1_000_000), int64(v%1_000_000)*int64(time.Microsecond)).UTC(), nil
	default: // 纳秒
		if v > 1<<62 {
			return time.Time{}, errors.New("时间戳溢出")
		}
		return time.Unix(0, int64(v)).UTC(), nil
	}
}

// decodeRadius 解码 4 字节半径。
//
// legacy 协议规定 RADI 为微秒，直接按微秒解读；RFC 10049 为秒，但公网上存在
// 「新版结构 + 旧单位」的混合实现，因此 RFC 格式下按量级自适应：秒解读超过
// 1 小时（>3600）时改按微秒。这样既不会把 legacy 的 1 秒半径误读成 11 天，
// 也不会把混合实现的 1e6 微秒误读成 11 天。
func decodeRadius(profile Profile, raw []byte) (time.Duration, error) {
	if len(raw) != 4 {
		return 0, fmt.Errorf("RADI 长度 %d（需 4）", len(raw))
	}
	v := binary.LittleEndian.Uint32(raw)
	if v == 0 {
		return 0, errors.New("服务器时间半径为零")
	}
	if profile == ProfileLegacy {
		us := time.Duration(v) * time.Microsecond
		if us <= 0 || us > time.Hour {
			return 0, fmt.Errorf("服务器时间半径超出可接受范围: %d", v)
		}
		return us, nil
	}
	if sec := time.Duration(v) * time.Second; sec > 0 && sec <= time.Hour {
		return sec, nil
	}
	if us := time.Duration(v) * time.Microsecond; us > 0 && us <= time.Hour {
		return us, nil
	}
	return 0, fmt.Errorf("服务器时间半径超出可接受范围: %d", v)
}

// containsUint32 判断 4 字节小端整数序列中是否包含 v。
func containsUint32(raw []byte, v uint32) bool {
	if len(raw)%4 != 0 {
		return false
	}
	for i := 0; i+4 <= len(raw); i += 4 {
		if binary.LittleEndian.Uint32(raw[i:i+4]) == v {
			return true
		}
	}
	return false
}

// timestampToWire 把时间编码为指定格式的 8 字节时间戳（测试/自建服务器用）。
func timestampToWire(profile Profile, t time.Time) []byte {
	if profile == ProfileLegacy {
		return uint64Bytes(uint64(t.UnixMicro()))
	}
	return uint64Bytes(uint64(t.Unix()))
}

// radiusToWire 把半径编码为指定格式的 4 字节字段（测试/自建服务器用）。
func radiusToWire(profile Profile, d time.Duration) []byte {
	if profile == ProfileLegacy {
		return uint32Bytes(uint32(d / time.Microsecond))
	}
	return uint32Bytes(uint32(d / time.Second))
}
