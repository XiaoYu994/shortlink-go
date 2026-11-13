# shortlink

用来学 Go 的短链接服务。配套笔记：Notion《Go 学习笔记：从 Java 到 Go（短链接 SaaS 项目）》。

## 快速开始

```bash
# 先按 docs/runbook.md 准备 .env、DATABASE_URL 和 REDIS_URL
make run
curl -i localhost:8080/healthz
```

创建短链：

```bash
curl -i -X POST localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

响应里的 `code` 拼到 `/r/{code}` 就是短链：

```bash
curl -i localhost:8080/r/AbC123xy
```

## 验证

```bash
make deps-up      # 使用项目自带 compose 时：只启动 PostgreSQL 和 Redis
make selfcheck    # 格式、vet、单测、lint、build、集成测试、冒烟测试
```

`make deps-up` 只适用于项目自带的 compose；本机已有 PostgreSQL/Redis 时跳过它，
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

## 代码约定

见 [AGENTS.md](AGENTS.md)。
