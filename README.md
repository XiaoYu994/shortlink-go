# shortlink

用来学 Go 的短链接服务。配套笔记：Notion《Go 学习笔记：从 Java 到 Go（短链接 SaaS 项目）》。

## 快速开始

```bash
# 先按 docs/runbook.md 准备 .env、DATABASE_URL 和 REDIS_ADDR
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
