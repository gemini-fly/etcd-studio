# Etcd Studio

[![CI](https://github.com/gemini-fly/etcd-studio/actions/workflows/ci.yml/badge.svg)](https://github.com/gemini-fly/etcd-studio/actions/workflows/ci.yml)
[![CodeQL](https://github.com/gemini-fly/etcd-studio/actions/workflows/codeql.yml/badge.svg)](https://github.com/gemini-fly/etcd-studio/actions/workflows/codeql.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

一个使用 Go 编写的轻量 etcd Key-Value 管理页面。后端通过官方 etcd v3 客户端读写数据，前端资源直接嵌入 Go 二进制，无需单独安装 Node.js 或部署静态站点。

## 功能

- 按 Key 前缀查询，按字典序分页浏览
- 新建、查看、编辑和删除 Key-Value
- 修改前在独立历史存储中持久化旧 Value；即使 etcd 历史被压缩也可预览并回滚
- 首次启动可选择本地文件、PostgreSQL 或 MySQL 保存 Value 历史，并在页面测试连接
- 可在“系统设置”中查看初始化的历史文件或数据库连接信息，并配置每个 Key 的保留版本数量
- 独立审计页记录 Key、集群配置和系统设置的变更，默认保留 90 天
- 首次启动自动创建临时 `admin`，密码只在终端显示一次；首次登录必须修改为强密码
- 管理员可在“用户管理”中维护本地用户、角色、启停状态和 LDAP 连接配置
- 使用 `mod_revision` 做乐观并发校验，避免静默覆盖并发修改
- 显示创建版本、修改版本、版本次数和 Lease 信息
- 支持 UTF-8 与 Base64 Value，二进制数据不会被错误转码
- 页面管理多个独立集群；每个集群支持多个成员 endpoint、用户名/密码及 mTLS
- 多节点集群实时检测各配置节点状态，并在顶部标识当前 Raft Leader 和部分异常状态
- 响应式中文管理界面
- 前端资源内嵌、CSP 等安全响应头、请求体大小限制和优雅退出

## 快速启动

要求 Go 1.25+，以及至少一个可访问的 etcd v3 集群。

```bash
go run ./cmd/etcd-studio
```

首次启动会在终端输出一次性管理员凭据：用户名固定为 `admin`，密码由安全随机数生成，明文不会写入文件。浏览器打开 [http://127.0.0.1:8080](http://127.0.0.1:8080)，使用临时密码登录后必须先设置新的强密码，之后才能访问任何平台功能。然后选择 Value 历史存储，再点击“集群管理”添加 etcd 连接。默认仅监听本机回环地址。

强密码至少 10 个字符，必须同时包含大写字母、小写字母、数字和特殊字符，不能包含空格，且受 bcrypt 的 72 字节上限约束。临时密码只在首次创建 `AUTH_FILE` 时显示；服务重启不会再次输出。容器后台运行时可在首次启动日志中查看，例如 `docker logs <容器名>`。

登录后，管理员可以进入“用户管理”：

- 选择“仅本地账户”“仅 LDAP”或“本地账户 + LDAP”。双模式会在登录页显示两个独立入口。
- 创建本地管理员或操作员，修改显示名称、角色、密码、启停状态与集群权限。管理员始终可访问全部集群；操作员只能选择被分配的集群并管理其中的 Key，未分配集群时登录后列表为空，同时不能查看或修改用户、认证、集群连接和系统设置。系统会阻止删除当前账户或停用最后一个本地管理员。
- 配置 LDAP/LDAPS/StartTLS、Bind 账号、Base DN、过滤器或用户 DN 模板，并在保存前测试连接。
- 用 LDAP 管理员名单授予管理权限；LDAP 管理员可访问全部集群，不在名单内的普通 LDAP 用户默认为无集群权限的操作员。

历史存储选择：

- 本地文件：适合单实例和本地测试，默认使用 `./data/history.jsonl`；页面指定的文件必须位于 `HISTORY_FILE` 所在目录内。
- PostgreSQL：适合多实例/高可用部署，默认端口 `5432`。
- MySQL：适合复用已有 MySQL 基础设施，默认端口 `3306`。

数据库需要预先创建；连接账号需要建表、查询和写入权限。Etcd Studio 保存配置时会自动创建 `etcd_studio_value_history` 和 `etcd_studio_audit_log` 表。存储类型一旦保存，页面不会直接切换，避免不同后端中的历史记录被无意分散。

顶部“系统设置”会只读展示首次初始化的存储类型和时间；数据库模式还会展示地址、端口、数据库名、账号、TLS/SSL 模式及密码是否已配置，但不会返回密码内容。首次配置或系统设置都可以指定独立历史存储中每个 Key 最多保留的版本数，范围为 `0–10000`。超过数量时，本地文件会安全重写，PostgreSQL/MySQL 会删除更旧的行；`0` 表示全部保留。调小数量会立即清理，已经清理的历史不会因之后调大数量而恢复。该策略不修改 etcd 自身的 MVCC 历史，后者仍由 etcd compaction 控制。

“审计日志”记录成功的 Key 新建、修改、删除和回滚，以及登录、用户、集群配置和系统设置变更，可按日期范围、集群、操作类型、操作者或对象筛选。审计记录不包含 Value、登录密码、数据库密码、etcd 密码或证书内容，默认保留最近 90 天并在写入及服务启动时清理过期数据。本地模式写入与历史文件同目录的 `history.audit.jsonl`；PostgreSQL/MySQL 模式写入共享的 `etcd_studio_audit_log` 表，因此多实例可以看到同一份审计数据。操作者显示已登录的本地或 LDAP 用户。

每个页面配置代表一个独立 etcd 集群。一个集群可以填写多个 Endpoint（每行一个），这些地址必须是同一集群的成员节点，etcd 客户端会使用它们进行故障切换。顶部连接状态会并发检测这些 Endpoint：全部正常时显示当前 Raft Leader，部分不可用时显示橙色“部分异常”。管理员可以看到 Leader Endpoint；操作员只看到“节点 N”，不会获得集群连接地址。不同集群需要分别创建配置。

构建单文件程序：

```bash
go build -o bin/etcd-studio ./cmd/etcd-studio
./bin/etcd-studio
```

公开版本也可以直接安装：

```bash
go install github.com/gemini-fly/etcd-studio/cmd/etcd-studio@latest
```

## 配置

全部配置通过环境变量提供：

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | Web 服务监听地址 |
| `CLUSTERS_FILE` | `./data/clusters.json` | 页面集群配置的持久化文件 |
| `HISTORY_CONFIG_FILE` | 与 `CLUSTERS_FILE` 同目录的 `history-storage.json` | 页面选择的历史存储类型及连接配置 |
| `HISTORY_FILE` | 与 `CLUSTERS_FILE` 同目录的 `history.jsonl` | 首次启动向导中的默认本地历史路径 |
| `AUTH_FILE` | 与 `CLUSTERS_FILE` 同目录的 `auth.json` | 本地用户及 LDAP 登录配置；页面初始化与维护 |
| `ETCD_DIAL_TIMEOUT` | `5s` | 连接超时时间，使用 Go duration 格式 |

Endpoint、用户名、密码及 TLS 文件路径均在页面中维护。mTLS 文件路径是 Etcd Studio 后端进程所看到的本地路径；容器部署时需要把证书目录以只读方式挂载到容器。

自定义配置文件位置：

```bash
CLUSTERS_FILE=/var/lib/etcd-studio/clusters.json \
go run ./cmd/etcd-studio
```

## Docker

```bash
docker build -t etcd-studio:local .
docker run --rm -p 8080:8080 \
  -v etcd-studio-data:/data \
  etcd-studio:local
```

如果构建环境无法访问默认 Go 模块代理，可以只在构建时覆盖代理地址：

```bash
docker build --build-arg GOPROXY=https://your-trusted-proxy.example,direct -t etcd-studio:local .
```

容器内默认监听 `0.0.0.0:8080`，集群配置保存到 `/data/clusters.json`，认证配置保存到 `/data/auth.json`，历史存储选择保存到 `/data/history-storage.json`；选择本地文件时 Value 历史默认保存到 `/data/history.jsonl`。填写 Endpoint、LDAP 或数据库地址时必须使用容器能够访问的地址；Docker Desktop 访问宿主机可以使用 `host.docker.internal`。

## 生产部署基线

- Etcd Studio 不应直接裸露在公网；应置于 HTTPS 反向代理和网络访问控制之后。
- 使用持久化且受限的数据目录，禁止把 `data/`、数据库转储、证书和 `.env` 提交到仓库。
- 为 etcd、LDAP 和历史数据库使用独立的最小权限账号，并定期轮换凭据。
- 备份历史数据库或本地历史文件，并在升级前验证恢复流程。
- 及时安装安全更新；漏洞请按照 [SECURITY.md](SECURITY.md) 私密报告。

## 验证

```bash
go test ./...
go vet ./...
```

测试覆盖临时管理员生成、强密码策略、首次登录强制改密、本地密码哈希、登录会话、用户权限与密码脱敏，以及多集群持久化与文件权限、首次存储配置、可配置历史保留与文件安全重写、90 天审计保留与分页、本地历史持久化与损坏恢复、PostgreSQL/MySQL 连接和自动建表、压缩后回滚、集群隔离参数、二进制 Value、并发冲突、删除语义、嵌入页面和配置校验。

## 安全说明

Etcd Studio 使用最长 12 小时的服务端会话和 `HttpOnly`、`SameSite=Strict` Cookie。服务重启会清除现有会话，用户需要重新登录。生产环境仍应通过 HTTPS 和可信反向代理对外提供服务；若 TLS 在代理终止，请正确设置 `X-Forwarded-Proto: https`，使会话 Cookie 带上 `Secure` 属性。

本地账户密码使用 bcrypt 哈希，哈希及 LDAP 配置保存在 `AUTH_FILE`。该文件以 `0600` 权限原子写入，接口不会返回本地密码哈希、临时密码或 LDAP Bind 密码。临时管理员会话在完成改密前只能调用改密、状态和退出接口。LDAP 用户密码只用于当次目录 Bind，不会落盘。自定义 LDAP CA 文件必须放在 `AUTH_FILE` 所在目录内（容器部署可挂载到 `/data`）。`AUTH_FILE` 仍可能包含 LDAP Bind 密码，应放在受控的数据目录并使用磁盘加密。当前会话保存在单个 Etcd Studio 进程内；如果将 Etcd Studio 自身部署为多个实例，需要额外使用会话粘滞或共享会话方案，并统一安全管理认证配置。

页面提交的集群密码保存在 `CLUSTERS_FILE` 中，以便服务重启后自动重连。文件由程序以 `0600` 权限原子写入，集群查询 API 只返回“是否已配置密码”，不会回传密码内容。请仍然把该文件视为敏感凭据文件：不要提交到 Git、打进镜像或放在共享目录。生产环境可进一步把数据目录放在加密磁盘上。

通过 Etcd Studio 修改、删除或回滚 Key 前，当前 Key 和 Value 会先保存到已配置的历史后端，写入失败时 etcd 操作会被中止。本地文件支持任意二进制 Key/Value，以 `0600` 权限保存并在每条记录后同步到磁盘；PostgreSQL/MySQL 使用共享表和唯一版本约束。Value 可能包含密码、令牌等敏感配置，因此应采用严格的数据库访问控制、备份和磁盘加密策略。

数据库账号和密码保存在 `HISTORY_CONFIG_FILE` 中，以便服务重启后恢复连接。该文件以 `0600` 权限原子写入，历史存储状态 API 只返回是否配置了密码，不会回传密码。它仍是敏感凭据文件，不应提交到 Git、打进镜像或放在共享目录。

写操作使用加载时的 `mod_revision` 进行比较：数据被其他客户端更新后，保存或删除会返回 HTTP `409`，页面提示刷新后重试。新建 Key 使用期望版本 `0`，因此不会覆盖已经存在的同名 Key。

回滚弹窗会合并独立历史后端与 etcd 当前仍可读取的 MVCC 历史，按 `mod_revision` 去重并倒序展示；可分页加载更早版本、预览完整 Value，并明确选择目标版本。确认时服务端会重新读取目标版本并再次比较当前 `mod_revision`，不会接受浏览器直接提交的 Value。如果 etcd 历史已被 compact，则仍可选择文件或数据库中的持久化快照。绕过 Etcd Studio 直接写入 etcd 的操作无法在发生前自动备份，但 Etcd Studio 下一次修改该 Key 时会先保存当时的 Value。

## 项目结构

```text
cmd/etcd-studio/       程序入口与优雅退出
internal/app/          HTTP API、嵌入页面及接口测试
internal/app/web/      原生 HTML/CSS/JavaScript 管理界面
internal/config/       环境变量配置与校验
internal/store/        多集群注册表、Value 本地历史、etcd v3 数据访问和并发控制
```

## License

Etcd Studio 按 [Apache License 2.0](LICENSE) 开源发布，版权及声明信息见 [NOTICE](NOTICE)。贡献代码也将按照同一许可证发布。
