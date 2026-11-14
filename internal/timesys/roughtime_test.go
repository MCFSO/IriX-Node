// roughtime_test.go — Roughtime 协议、Merkle 证明与客户端端到端测试。
package timesys

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"testing"
	"time"
)

// TestMessageRoundTrip 验证消息编解码与严格校验。
func TestMessageRoundTrip(t *testing.T) {
	msg := encodeMessage([]tagValue{
		{tagNONC, []byte("12345678")},
		{tagMIDP, uint64Bytes(42)},
	})
	parsed, err := parseMessage(msg)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if v, ok := parsed.get(tagNONC); !ok || string(v) != "12345678" {
		t.Fatalf("NONC 解析错误: %q", v)
	}
	if v, ok := parsed.get(tagMIDP); !ok || len(v) != 8 || v[0] != 42 {
		t.Fatalf("MIDP 解析错误: %v", v)
	}

	// tag 乱序必须拒绝。
	bad := append([]byte(nil), msg...)
	// 第二条 tag 换成更小的 tag（NONC 之后放 SIG）
	badTagPos := 4 + 4*(2-1) + 4
	copy(bad[badTagPos:], uint32Bytes(tagSIG))
	if _, err := parseMessage(bad); err == nil {
		t.Fatal("乱序 tag 未被拒绝")
	}
	// 偏移越界必须拒绝。
	bad2 := append([]byte(nil), msg...)
	copy(bad2[4:], uint32Bytes(4096))
	if _, err := parseMessage(bad2); err == nil {
		t.Fatal("越界偏移未被拒绝")
	}
}

// TestMerklePath 构造 4 叶完整树，验证每个叶子的包含证明（两种格式）。
func TestMerklePath(t *testing.T) {
	for _, profile := range []Profile{ProfileLegacy, ProfileRFC10049} {
		t.Run(string(profile), func(t *testing.T) {
			leaves := [][]byte{
				[]byte("leaf-0-payload"),
				[]byte("leaf-1-payload"),
				[]byte("leaf-2-payload"),
				[]byte("leaf-3-payload"),
			}
			root, paths := buildTestTree(profile, leaves)
			flatten := func(chunks [][]byte) []byte {
				var out []byte
				for _, c := range chunks {
					out = append(out, c...)
				}
				return out
			}
			for i := range leaves {
				if err := verifyMerklePath(profile, leaves[i], flatten(paths[i]), uint32(i), root); err != nil {
					t.Fatalf("叶子 %d 证明校验失败: %v", i, err)
				}
			}
			// 篡改路径必须失败。
			bad := append([]byte(nil), flatten(paths[1])...)
			bad[0] ^= 0xff
			if err := verifyMerklePath(profile, leaves[1], bad, 1, root); err == nil {
				t.Fatal("篡改后的路径未被拒绝")
			}
			// 索引与路径不匹配必须失败（用非补码的索引，避免与 RFC 文字方向混淆）。
			if err := verifyMerklePath(profile, leaves[0], flatten(paths[0]), 2, root); err == nil {
				t.Fatal("索引不匹配未被拒绝")
			}
		})
	}
}

// buildTestTree 构造 4 叶 Merkle 树并返回根与每叶的路径。
func buildTestTree(profile Profile, leaves [][]byte) ([]byte, [][][]byte) {
	hash := func(b []byte) []byte {
		sum := sha512.Sum512(b)
		if profile == ProfileLegacy {
			return append([]byte(nil), sum[:]...)
		}
		return append([]byte(nil), sum[:hashLenRFC]...)
	}
	node := func(left, right []byte) []byte {
		buf := make([]byte, 0, 1+len(left)+len(right))
		buf = append(buf, 0x01)
		buf = append(buf, left...)
		buf = append(buf, right...)
		return hash(buf)
	}
	level := make([][]byte, len(leaves))
	for i, leaf := range leaves {
		level[i] = hash(append([]byte{0x00}, leaf...))
	}
	paths := make([][][]byte, len(leaves))
	positions := make([]int, len(leaves))
	for i := range positions {
		positions[i] = i
	}
	for len(level) > 1 {
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, node(level[i], level[i+1]))
		}
		for j := range leaves {
			pos := positions[j]
			if pos%2 == 0 {
				if pos+1 < len(level) {
					paths[j] = append(paths[j], level[pos+1])
				}
			} else {
				paths[j] = append(paths[j], level[pos-1])
			}
			positions[j] = pos / 2
		}
		level = next
	}
	return level[0], paths
}

// TestQueryBothProfiles 端到端验证两种线上格式。
func TestQueryBothProfiles(t *testing.T) {
	cases := []struct {
		profile Profile
		offset  time.Duration
		radius  time.Duration
	}{
		{ProfileLegacy, 0, 20 * time.Millisecond},
		{ProfileLegacy, 3 * time.Second, 50 * time.Millisecond},
		{ProfileRFC10049, 0, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(string(tc.profile), func(t *testing.T) {
			srv := startMockServer(t, mockOptions{Profile: tc.profile, Offset: tc.offset, Radius: tc.radius})
			client := &Client{Timeout: 500 * time.Millisecond}
			before := time.Now()
			res, err := client.Query(context.Background(), Server{
				Name: "mock", Address: srv.Address(), PublicKey: srv.PublicKeyBase64(), Profile: tc.profile,
			})
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if res.Profile != tc.profile {
				t.Fatalf("协议格式 = %q，期望 %q", res.Profile, tc.profile)
			}
			want := before.Add(tc.offset)
			if d := res.Midpoint.Sub(want); d < -2*time.Second || d > 2*time.Second {
				t.Fatalf("MIDP 偏差过大: %v（期望约 %s）", d, want)
			}
			if res.Radius != tc.radius {
				t.Fatalf("半径 = %v，期望 %v", res.Radius, tc.radius)
			}
			if res.RoundTrip < 0 {
				t.Fatalf("往返时延不能为负: %v", res.RoundTrip)
			}
		})
	}
}

// TestQueryAutoProfile 验证 auto 模式对旧格式与 RFC 格式的自动探测。
func TestQueryAutoProfile(t *testing.T) {
	for _, profile := range []Profile{ProfileLegacy, ProfileRFC10049} {
		srv := startMockServer(t, mockOptions{Profile: profile, Radius: time.Second})
		client := &Client{Timeout: 150 * time.Millisecond}
		res, err := client.Query(context.Background(), Server{
			Name: "auto", Address: srv.Address(), PublicKey: srv.PublicKeyBase64(),
		})
		if err != nil {
			t.Fatalf("%s: auto 查询失败: %v", profile, err)
		}
		if res.Profile != profile {
			t.Fatalf("%s: 探测结果 = %q", profile, res.Profile)
		}
	}
}

// TestQueryMerklePath 服务器批量签名（PATH 非空）时仍能验证包含证明。
func TestQueryMerklePath(t *testing.T) {
	for _, profile := range []Profile{ProfileLegacy, ProfileRFC10049} {
		t.Run(string(profile), func(t *testing.T) {
			radius := time.Second
			if profile == ProfileLegacy {
				radius = 20 * time.Millisecond
			}
			srv := startMockServer(t, mockOptions{
				Profile: profile, Radius: radius, Leaves: 8, LeafIndex: 5,
			})
			client := &Client{Timeout: 500 * time.Millisecond}
			res, err := client.Query(context.Background(), Server{
				Address: srv.Address(), PublicKey: srv.PublicKeyBase64(), Profile: profile,
			})
			if err != nil {
				t.Fatalf("带 PATH 的响应校验失败: %v", err)
			}
			msg, _, err := parsePacket(res.Raw)
			if err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			path, _ := msg.get(tagPATH)
			indx, _ := msg.get(tagINDX)
			if len(path) != 3*hashLenFor(profile) {
				t.Fatalf("PATH 长度 = %d，期望 3 个节点", len(path))
			}
			if got := binary.LittleEndian.Uint32(indx); got != 5 {
				t.Fatalf("INDX = %d，期望 5", got)
			}
		})
	}
}

// TestDecodeTimestampNoOverflow 大数值时间戳不得因乘法溢出得到负数/远古时间。
func TestDecodeTimestampNoOverflow(t *testing.T) {
	values := []uint64{
		9_000_000_000_000_000,  // 微秒上界附近
		9_999_999_999_999_999,  // 接近 1e16
		99_999_999_999_999_999, // 接近 1e17
		1 << 40,
	}
	for _, v := range values {
		for _, profile := range []Profile{ProfileLegacy, ProfileRFC10049} {
			got, err := decodeTimestamp(profile, uint64Bytes(v))
			if err != nil {
				continue // 溢出被拒绝也是可接受结果
			}
			if got.Year() < 1970 {
				t.Fatalf("v=%d profile=%s 解码得到 %s（疑似溢出）", v, profile, got)
			}
		}
	}
}

// TestQueryRejectsInvalid 验证各类伪造/损坏响应都被拒绝。
func TestQueryRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		opts mockOptions
	}{
		{"错误长期密钥", mockOptions{WrongKey: true}},
		{"错误nonce", mockOptions{WrongNonce: true}},
		{"错误Merkle根", mockOptions{BadRoot: true}},
		{"MIDP超出证书有效期", mockOptions{BadMidp: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startMockServer(t, tc.opts)
			client := &Client{Timeout: 200 * time.Millisecond}
			_, err := client.Query(context.Background(), Server{
				Address: srv.Address(), PublicKey: srv.PublicKeyBase64(), Profile: ProfileLegacy,
			})
			if err == nil {
				t.Fatal("非法响应未被拒绝")
			}
		})
	}
}

// TestQuerySilentTimeout 验证服务器不响应时按超时失败。
func TestQuerySilentTimeout(t *testing.T) {
	srv := startMockServer(t, mockOptions{Silent: true})
	client := &Client{Timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := client.Query(context.Background(), Server{
		Address: srv.Address(), PublicKey: srv.PublicKeyBase64(), Profile: ProfileLegacy,
	}); err == nil {
		t.Fatal("无响应应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效: %v", elapsed)
	}
}
