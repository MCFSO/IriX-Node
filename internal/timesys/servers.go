// servers.go — 内置默认 Roughtime 服务器列表。
//
// 公钥是各服务器的**长期 Ed25519 公钥**（信任根），地址是 UDP 端口。
// 默认列表覆盖多个互相独立的运营方（Cloudflare / int08h / roughtime.se 等），
// 满足「至少三台、不由同一方运营」的客户端要求。
//
// 运维者可在 config.json 的 time.servers 中替换/追加（例如使用内网自建
// Roughtime 服务器）。公钥一旦变更必须显式更新，否则该服务器的时间会被
// 签名校验拒绝（这是设计使然，不是故障）。
package timesys

// DefaultServers 返回内置默认服务器列表。
//
// 公钥与地址为 2026 年的社区维护快照；公网服务器可能下线、迁移或更换密钥，
// 运维者应通过 `GET /api/time` 观察成功率，并按需在 config.json 中替换。
func DefaultServers() []Server {
	return []Server{
		{Name: "cloudflare", Address: "roughtime.cloudflare.com:2003", PublicKey: "0GD7c3yP8xEc4Zl2zeuN2SlLvDVVocjsPSL8/Rl/7zg=", Profile: ProfileAuto},
		{Name: "int08h", Address: "roughtime.int08h.com:2002", PublicKey: "AW5uAoTSTDfG5NfY1bTh08GUnOqlRb+HVhbJ3ODJvsE=", Profile: ProfileAuto},
		{Name: "roughtime.se", Address: "roughtime.se:2002", PublicKey: "S3AzfZJ5CjSdkJ21ZJGbxqdYP/SoE8fXKY0+aicsehI=", Profile: ProfileAuto},
		{Name: "rough.time.nl", Address: "rough.time.nl:2002", PublicKey: "v2CievhgKsxzlWPwIkYFUXeA51Akhkv5uhJCj1/kbiY=", Profile: ProfileAuto},
		{Name: "time.teax.dev", Address: "time.teax.dev:2002", PublicKey: "84pMADvKUcSOq5RNbVRjVrjiU16Dxo2XV2Qkm+4DRTg=", Profile: ProfileAuto},
		{Name: "sturdy", Address: "roughtime.sturdystatistics.com:2002", PublicKey: "NqIjwLopQn6yQChtE21Mb97dAbAPe5UOuTa0tOakgD8=", Profile: ProfileAuto},
	}
}
