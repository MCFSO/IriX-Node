// probe_public_test.go — 可选的公网连通性探测（默认跳过）：
//
//	RT_PROBE=1 go test ./internal/timesys -run TestProbePublicServers -v
//
// 用于人工验证内置服务器列表的协议/公钥仍然有效，不作为 CI 依赖。
package timesys

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

// TestProbePublicServers 探测内置服务器并打印各格式的探测结果与 Merkle 方向。
func TestProbePublicServers(t *testing.T) {
	if os.Getenv("RT_PROBE") != "1" {
		t.Skip("设置 RT_PROBE=1 才探测公网服务器")
	}
	for _, srv := range DefaultServers() {
		for _, profile := range []Profile{ProfileLegacy, ProfileRFC10049} {
			srv.Profile = profile
			client := &Client{Timeout: 2 * time.Second}
			res, err := client.Query(context.Background(), srv)
			if err != nil {
				t.Logf("%-16s %-9s 失败: %v", srv.DisplayName(), profile, err)
				continue
			}
			t.Logf("%-16s %-9s 成功: mid=%s radius=%s rtt=%s", srv.DisplayName(), profile,
				res.Midpoint.UTC().Format(time.RFC3339Nano), res.Radius, res.RoundTrip.Round(time.Millisecond))
			checkMerkleOrientation(t, res)
		}
	}
}

// checkMerkleOrientation 对真实响应尝试两种 Merkle 方向并记录哪一种匹配。
func checkMerkleOrientation(t *testing.T, res *Result) {
	t.Helper()
	msg, _, err := parsePacket(res.Raw)
	if err != nil {
		return
	}
	srepData, _ := msg.get(tagSREP)
	srep, err := parseMessage(srepData)
	if err != nil {
		return
	}
	root, _ := srep.get(tagROOT)
	path, _ := msg.get(tagPATH)
	indxRaw, _ := msg.get(tagINDX)
	if len(path) == 0 || len(indxRaw) != 4 {
		t.Logf("  单叶响应（PATH 空），无法判断方向")
		return
	}
	index := binary.LittleEndian.Uint32(indxRaw)
	for _, currentLeft := range []bool{true, false} {
		got := rootOrientation(res, path, index, currentLeft)
		if string(got) == string(root) {
			t.Logf("  Merkle 方向: currentLeft=%v 匹配（PATH 节点 %d）", currentLeft, len(path)/hashLenFor(res.Profile))
		}
	}
}

// rootOrientation 按方向计算根。
func rootOrientation(res *Result, path []byte, index uint32, currentLeft bool) []byte {
	chunk := hashLenFor(res.Profile)
	var h []byte
	sum := sha512.Sum512(append([]byte{0x00}, res.rawLeafInput()...))
	if chunk == hashLenRFC {
		h = sum[:hashLenRFC]
	} else {
		h = sum[:]
	}
	for i := 0; i+chunk <= len(path); i += chunk {
		sibling := path[i : i+chunk]
		buf := []byte{0x01}
		if (index&1 == 0) == currentLeft {
			buf = append(append(buf, h...), sibling...)
		} else {
			buf = append(append(buf, sibling...), h...)
		}
		s := sha512.Sum512(buf)
		if chunk == hashLenRFC {
			h = s[:hashLenRFC]
		} else {
			h = s[:]
		}
		index >>= 1
	}
	return h
}

// hashLenFor 返回格式对应的哈希长度。
func hashLenFor(profile Profile) int {
	if profile == ProfileLegacy {
		return hashLenLegacy
	}
	return hashLenRFC
}
