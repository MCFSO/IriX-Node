<!--
请使用 Conventional Commits 风格撰写 PR 标题，例如：
  feat: 账户管理与权限系统
  fix: 相对 -data 路径下集群同步区双重拼接
  docs: 新增安全策略 SECURITY.md
  style: 修复 gofmt 格式
  refactor: ...
-->

## 变更说明

<!-- 这个 PR 做了什么？为什么要做？关联哪个 Issue（如 #123）？ -->

-

## 变更类型

<!-- 勾选适用的类型 -->

- [ ] Bug 修复（fix）
- [ ] 新功能（feat）
- [ ] 文档（docs）
- [ ] 重构（refactor）
- [ ] 格式 / 样式（style）
- [ ] 测试 / CI（test / ci）
- [ ] 其他（请说明）

## 关联 Issue

<!-- 如有关联，填写 Closes #编号 或 相关 #编号 -->

-

## 自检清单

<!-- 提交前请逐项确认，CI 会自动校验其中多数项 -->

- [ ] 已通过 `gofmt -l .`（无未格式化文件）
- [ ] 已通过 `go vet ./...`
- [ ] 已通过 `go build .`
- [ ] 已通过 `go test ./...`（含 `-race -short`）
- [ ] 新增 / 修改了 `RegisterRoutes` 中的路由时，相邻调用了 `perm(组名, 路由模式, 中文描述)` 纳入端点权限目录（`qa_perms_test.go` 会反向扫描校验漏标注）
- [ ] 新增了平台相关能力时，已补齐 `_windows` / `_linux` / `_bsd` / `_other` 对应文件（构建标签区分）
- [ ] 代码注释、错误消息、文档均为中文
- [ ] 未引入 `go.mod` 之外的第三方依赖（依赖最小化红线：仅 DB 驱动 / Redis / x-crypto 允许）
- [ ] 涉及安全相关变更（认证 / 权限 / Vault / 集群传输）时，已同步更新 `docs/` 与 `README.md` 相关说明
- [ ] 涉及安全漏洞修复时，已遵循 `SECURITY.md` 的协同披露流程

## 补充说明

<!-- 验收要点、破坏性变更、回滚方案、截图等 -->
