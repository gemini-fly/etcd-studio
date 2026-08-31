# Security Policy

## Supported versions

安全修复优先应用到默认分支和最新正式版本。旧版本可能需要先升级后才能获得修复。

## Reporting a vulnerability

请不要通过公开 Issue 报告漏洞。请在 GitHub 仓库的 **Security → Report a vulnerability** 中提交私密报告，并提供：

- 受影响的版本或提交；
- 可重复的最小步骤；
- 可能的影响和攻击前提；
- 已知的缓解措施（如有）。

报告中不要包含真实 etcd、LDAP 或数据库凭据，不要上传生产配置、Value、审计日志、证书或私钥。维护者确认问题并准备修复后，会协调披露时间。

## Deployment boundary

Etcd Studio 能够修改和删除 etcd 数据，不应直接裸露在公网。生产部署必须使用 HTTPS、可信反向代理、网络访问控制和最小权限的 etcd/数据库账户，并妥善保护持久化数据目录。
