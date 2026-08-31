# 功能

## 能力概览

- **概览**：`GET /api/overview` 返回主机信息、内存、实例统计。
- **实例管理**：列表 / 详情 / 创建 / 更新 / 删除，启动 / 停止 / 重启 / 强制终止，
  命令下发（标准输入），输出日志（环形缓冲）。
- **文件管理**：列表 / 读写 / 删除 / 移动 / 复制 / 压缩 / 解压 / 新建目录 /
  新建文件，以及带票据的下载 / 上传直连通道。
- **审计日志**：每次 API 请求（时间、来源 IP、方法、路径与参数、状态码、耗时、
  请求体）落盘到 `{data}/logs/audit.log`，`apikey` 自动打码；下载/上传直连通道
  同样在审计范围内。
- 实例配置与状态持久化在 `{data}/instances.json`。

## 数据目录结构

```
{data}/
  instances.json   # 实例配置列表（保险库模式下加密存储，见下）
  auth.hash        # 配对码 SHA-256 哈希（首次启动生成）
  vault/           # 加密保险库（-vault 开启时）
    vault.json     # 用户/密钥包裹/恢复令牌哈希/迁移标记
    objects/       # 密文对象（文件名随机化，无明文痕迹）
    index.json.enc # 加密索引（文件名/大小/时间不泄露）
  tls/             # 自签 TLS 证书（-tls-mode auto 时）
  backup/audit/    # 审计日志轮转归档（防覆盖丢失）
  logs/            # 实例日志 {uuid}.log（-instance-log 开启时）
                   # 审计日志 audit.log（-audit-log 开启时）
```
