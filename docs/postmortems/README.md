# 问题复盘与调优记录（Postmortems）

本目录记录“向后看的真相”——深度排查、架构复审、性能剖析与并发故障复盘：出了什么问题、怎么用数据复现、根因是什么、修前修后对比，以及沉淀了什么防再犯护栏。

与 [ADR](../adr/README.md) 的分工：

- `docs/adr/` 记录**向前看的架构决定**（为什么选方案 A 而不是方案 B）。
- `docs/postmortems/` 记录**向后看的问题排查与验证证据**（现象、复现、`EXPLAIN`/`pprof` 数据、根因与护栏）。
- 不涉及架构变更或深层排障的普通小 bug，只写清晰的 Git Commit 正文和回归测试，不单独建文档。

## 命名

`YYYY-MM-DD-短标题.md`，例如 `2026-11-24-postgres-schema-and-query-audit.md`。

## 索引

| 日期 | 标题 | 来源 | 关联 |
| --- | --- | --- | --- |
| 2026-11-24 | [PostgreSQL 索引失效、队列积压雪崩与租约 ABA 覆盖审查](2026-11-24-postgres-schema-and-query-audit.md) | 架构审查 | [ADR 0022](../adr/0022-postgres-query-and-lease-hardening.md) |
| 2026-11-24 | [日志体系与可观测性架构审查](2026-11-24-logging-system-audit.md) | 架构审查 | [ADR 0005](../adr/0005-prometheus-before-tracing.md)、[ADR 0023](../adr/0023-audit-logs.md) |

## 模板

见 [template.md](template.md)。
