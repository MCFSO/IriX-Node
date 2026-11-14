// rfc_vector_test.go — RFC 10049 附录 B 的真实响应作为已知答案向量，
// 固定消息格式、签名上下文与 Merkle 叶子算法，防止实现漂移。
//
// 向量取自 RFC 10049 附录 B 第一条记录（base64 原样内嵌，无运行时依赖）。
package timesys

import (
	"encoding/base64"
	"testing"
	"time"
)

// rfcVectorPublicKey 是 RFC 10049 附录 B 中该服务器的长期 Ed25519 公钥。
const rfcVectorPublicKey = "FnDyLV/68ephhLdFJbdEGCdkVvpXDaVe5PYvRDdlOOY="

// rfcVectorRequest 是附录 B 中的客户端请求（含 ROUGHTIM 包头）。
const rfcVectorRequest = "Uk9VR0hUSU0ABAAABQAAAAQAAAAkAAAARAAAAEgAAABWRVIAU1JWAE5PTkNUWVBFWlpaWgEAAACf" +
	"4gKLPdPfiNTv93lrhNqYgyehDgMyHFmA1BrAhM1QEDBh9lBlN6LUye6zghiqSWMwyNm0IucxQxW3" +
	"zTMrwj4dAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAA=="

// rfcVectorResponse 是附录 B 中的服务器响应（签名、Merkle 证明齐全）。
const rfcVectorResponse = "Uk9VR0hUSU2UAQAABwAAAEAAAABgAAAAZAAAAGQAAADAAAAAWAEAAFNJRwBOT05DVFlQRVBBVEhT" +
	"UkVQQ0VSVElORFhBWL64CToGs4v/4UtfN/80HLFiA09vGIDRP/zTjcTj8/1DlZWCsVja6RlfwaYn" +
	"c1wfJqThfhcuSDonrTGyKngBMGH2UGU3otTJ7rOCGKpJYzDI2bQi5zFDFbfNMyvCPh0BAAAABQAA" +
	"AAQAAAAIAAAAEAAAABQAAABWRVIAUkFESU1JRFBWRVJTUk9PVAEAAAADAAAAQ0u4aQAAAAABAAAA" +
	"c86AWYB/O3Kxzsx4d5P5cbSOftJUA8bWVtVrQ3tc+b0CAAAAQAAAAFNJRwBERUxFI2B5tbj5ePjV" +
	"KYE0PAL1NmgZOAsqh/E2from9Ol5BAnVcLje0C6exbXY8hE3dRvYV01Alru8Ocle+jOZT5r8AwMA" +
	"AAAgAAAAKAAAAFBVQktNSU5UTUFYVKqljhhqi4A54vW20e+slwViPyxybNnqKXzimIiIUHQMaBCv" +
	"aQAAAADYyd9pAAAAAAAAAAA="

// TestRFC10049KnownVector 用 RFC 官方示例验证完整校验链。
func TestRFC10049KnownVector(t *testing.T) {
	req := mustBase64(t, rfcVectorRequest)
	resp := mustBase64(t, rfcVectorResponse)
	pub := mustBase64(t, rfcVectorPublicKey)

	// 请求：5 个 tag，nonce 32 字节，TYPE=0，VER 含版本 1。
	reqMsg, _, err := parsePacket(req)
	if err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	if n, _ := reqMsg.get(tagNONC); len(n) != nonceLenRFC {
		t.Fatalf("请求 nonce 长度 = %d，期望 %d", len(n), nonceLenRFC)
	}
	if v, _ := reqMsg.get(tagTYPE); len(v) != 4 || !equalBytes(v, uint32Bytes(0)) {
		t.Fatalf("请求 TYPE 非 0: %x", v)
	}
	if v, _ := reqMsg.get(tagVER); !containsUint32(v, 1) {
		t.Fatalf("请求 VER 不含版本 1: %x", v)
	}
	if v, _ := reqMsg.get(tagSRV); len(v) != 32 {
		t.Fatalf("请求 SRV 长度 = %d", len(v))
	}
	if v, _ := reqMsg.get(tagZZZZ); len(v) == 0 {
		t.Fatal("请求缺少 ZZZZ 填充")
	}

	// 响应必须通过完整校验链（DELE/SREP 签名 + Merkle + 时间量级）。
	nonce, _ := reqMsg.get(tagNONC)
	mid, radius, err := verifyResponse(resp, req, nonce, ProfileRFC10049, pub)
	if err != nil {
		t.Fatalf("已知答案验证失败: %v", err)
	}
	if got, want := mid.Unix(), int64(1773685571); got != want {
		t.Fatalf("MIDP = %d，期望 %d", got, want)
	}
	if radius != 3*time.Second {
		t.Fatalf("RADI = %v，期望 3s", radius)
	}

	// 任意一位篡改都必须被拒绝。
	bad := append([]byte(nil), resp...)
	bad[len(bad)-1] ^= 0x01
	if _, _, err := verifyResponse(bad, req, nonce, ProfileRFC10049, pub); err == nil {
		t.Fatal("篡改后的响应未被拒绝")
	}
}

// mustBase64 解码 base64，失败即终止测试。
func mustBase64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return raw
}
