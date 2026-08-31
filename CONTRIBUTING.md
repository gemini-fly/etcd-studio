# Contributing to Etcd Studio

感谢你为 Etcd Studio 做出贡献。提交代码前，请先搜索现有 Issue，避免重复工作。较大的功能或行为变更建议先创建 Feature Request，说明使用场景、权限边界和兼容性影响。

## 本地开发

项目要求 Go 1.25+。克隆仓库后运行：

```bash
go mod download
go test ./...
go vet ./...
go build ./cmd/etcd-studio
node --check internal/app/web/app.js
```

数据库集成测试默认跳过。需要验证 PostgreSQL、MySQL 或真实 etcd 时，分别设置测试文件中记录的环境变量；测试环境不得使用生产凭据或生产数据。

## 提交要求

- 一个 Pull Request 聚焦一个问题，并说明行为变化和验证方法。
- 新功能和错误修复应包含覆盖真实调用链的测试。
- 不得提交 `data/`、`.env`、证书、数据库转储、etcd 快照、构建产物或任何凭据。
- 涉及认证、权限、审计、历史回滚或敏感信息展示的改动，需要明确说明安全影响。
- 保持 API 向后兼容；必须破坏兼容时，请在 Pull Request 中显著标注。

## 许可

提交贡献即表示你同意该贡献按照项目的 Apache License 2.0 许可发布。
