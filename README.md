# shortlink

用来学 Go 的短链接服务。配套笔记：Notion《Go 学习笔记：从 Java 到 Go（短链接 SaaS 项目）》。

## 快速开始

```bash
# 先按 docs/runbook.md 准备 .env（DATABASE_URL、REDIS_URL、VISITOR_SALT），并启动数据库
make migrate
make run-redirector   # 终端 1：公开跳转，默认 :8080，用 ADDR 改
```

另开一个终端启动管理进程：

```bash
make run-console      # 管理 API，默认 :8081，用 CONSOLE_ADDR 改
curl -i localhost:8080/healthz
```

先签发一把密钥（明文只打印一次），再创建短链：

```bash
make console ARGS="key issue --name acme"
```

创建短链走 console：

```bash
curl -i -X POST localhost:8081/api/v1/links \
  -H 'Authorization: Bearer slk_...' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

响应里的 `code` 拼到 `/r/{code}` 就是短链，跳转走 redirector：

```bash
curl -i localhost:8080/r/AbC123xy
```

也可以通过本地 Dex 进行 OIDC 浏览器登录并用会话 Cookie 和 `X-Org-ID` 管理组织与短链，步骤见 [docs/runbook.md](docs/runbook.md)。

两个进程共用 PostgreSQL、Redis 和 `internal/` 里的代码，进程之间不互相调用。
部署时由反向代理把 `/r/` 转到 redirector，把 `/api/v1/` 转到 console；短链的公开地址不变。
宿主机上的进程用 `deploy/nginx.conf`（compose profile `edge`）。容器里跑两个进程用根目录的 `Dockerfile` 和 profile `app`，入口是 `deploy/nginx.stack.conf`。`/healthz` 只表示进程活着；`/readyz` 表示可以接流量，console 会探 PostgreSQL，redirector 启动完成就算就绪；这两个路径和 `/metrics` 都不经过公网入口。

## 验证

```bash
make deps-up      # 使用项目自带 compose 时：只启动 PostgreSQL、Redis 和 Dex
make selfcheck    # 格式、vet、单测、lint、build、集成测试、冒烟测试
```

`make deps-up` 只适用于项目自带的 compose；本机已有 PostgreSQL、Redis 和 Dex 时跳过它，
把 `.env` 指向已有实例即可。`make verify` 不启动 Docker，只跑代码检查；
`make selfcheck` 会重建测试库并启动真实进程验证 HTTP 行为。CI 配置见
`.github/workflows/ci.yml`，push 和 PR 会跑同样的检查。

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/README.md](docs/README.md) | 文档索引 |
| [docs/architecture.md](docs/architecture.md) | 架构、目录、依赖方向、请求流程 |
| [docs/api.md](docs/api.md) | HTTP API 契约和示例 |
| [docs/runbook.md](docs/runbook.md) | 本地启动、数据库、排错 |
| [docs/roadmap.md](docs/roadmap.md) | 学习路线和进度 |
| [docs/adr/README.md](docs/adr/README.md) | 架构决策记录 |
| [docs/postmortems/README.md](docs/postmortems/README.md) | 问题复盘与调优记录 |
| [web/README.md](web/README.md) | 前端控制台启动、目录与 React 学习指南 |

## 代码约定

- 全局协作方式、Go 约定与提交规范：见 [AGENTS.md](AGENTS.md)。
- 前端控制台（`web/`）开发规范：见 [web/AGENTS.md](web/AGENTS.md)。
