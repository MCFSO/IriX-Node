# 加密保险库（Vault）

可选功能（默认关闭，由用户显式开启）：用「TOTP + 密码 + 客户端证书（P12/GPG 在
客户端转换为标准 PEM）」三重认证保护节点数据。设计文档：`docs/vault-design.md`。

```bash
irix-node -tls-mode auto -vault       # 开启 TLS（自签）与加密保险库
irix-node -tls-mode manual -tls-cert x.pem -tls-key x.key -vault   # 正式证书
```

- 开启后 `instances.json` 与 `vaultFiles=true` 实例的文件区以 AES-256-GCM 加密
  存储；未解锁（`POST /api/vault/unlock`）时数据面 API 返回 403。
- 每次解锁需 TOTP 验证码 + 账号密码 + 证书私钥对挑战签名（私钥不出设备）。
- **开启 vault 强制要求 TLS**（`tls-mode=off` 时拒绝启动）；TLS 本身默认关闭，
  由用户显式开启（等保二级部署请开启）。
- 实例文件区加密粒度：`vaultFiles` 字段（默认 false 保持明文；true = 启停物化
  加密），可用 `-vault-default-files-mode materialize` 让新实例默认开启。
- 忘记密码/丢失恢复令牌 = 数据永久不可恢复（初始化时一次性显示恢复令牌，请
  物理保管）。
