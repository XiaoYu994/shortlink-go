# 0033. 按任务类型（Kind）隔离抢占，防止滚动发布旧版 Worker 误杀新任务

- 状态：Accepted
- 日期：2026-11-27
- 相关：`0020`, `0022`, `0029`, `0031`

## 背景

在 [ADR 0020](0020-background-jobs-and-async-risk-scan.md) 与 [ADR 0022](0022-postgres-query-and-lease-hardening.md) 建立的 `jobs` Outbox 队列中，抢占链路存在一个在滚动发布（或灰度发布）时会确定性触发的缺陷：

1. **起点**：`job.ClaimParams` 未传递当前 `Runner` 已注册的任务类型集合，PostgreSQL 两条抢占 SQL（`claimJobsNoFairnessSQL` 与 `claimJobsFairSQL`）只要 `status = 'pending' AND run_at <= now()` 就一律抢占。
2. **中转**：`job.Runner.invokeHandler` 查不到 `j.Kind` 时返回 `job.Permanent(fmt.Errorf("unknown job kind %q", j.Kind))`。
3. **终点**：`executeJob` 见 `IsPermanent(err)` 立即调用 `r.queue.Dead(...)` 打入死信终态。

一旦新版本 `console serve` 率先上线并入队新类型任务（例如 `export.csv`），尚未退出的旧版 `console worker` 就会变成“碎纸机”：抢到新任务就判死刑进 `dead`，新版 `worker` 完全碰不到。此外，`idx_jobs_claimable (run_at, id)` 与 `idx_jobs_pending_tenant_run (tenant_id, run_at, id)` 均不含前导 `kind` 列，若直接在原 SQL 叠加 `kind = ANY(...)`，在未知 `kind` 大量积压时会退化为索引扫描 + 大量回表丢弃行，击穿 [ADR 0022](0022-postgres-query-and-lease-hardening.md) 建立的性能基线。

## 决策

### 1. 契约与调度层：`ClaimParams.Kinds []string` 白名单与纵深防御

- 在 `internal/job` 的 `ClaimParams` 新增 `Kinds []string`，契约明确：**`Kinds` 过滤空串后为空时，队列不得抢占任何任务**。
- `job.Runner` 启动时从已注册的 `handlers` 收集非空 key 排重排序存入 `r.kinds`：
  - 若 `len(r.kinds) == 0`，`pollLoop` 直接等待退出，不再无意义轮询 `Claim`；
  - 正常轮询每次向 `r.queue.Claim` 传入 `Kinds: r.kinds`；
  - `invokeHandler` 保留纵深防御：若自定义 `Queue` 越界吐出未注册 `j.Kind`，改为返回普通可重试错误（按指数退避 + `MaxAttempts` 兜底），不再使用 `Permanent` 瞬间秒杀进死信。
- **保留 `HandleDisabled` 分工边界**（[ADR 0029](0029-modular-feature-plugins.md)、[ADR 0031](0031-cache-and-worker-assembly-simplification.md)）：
  - **未知新 `kind`**（滚动发布新特性、或未来独立 worker 池消费的类型）：当前进程不认，通过 `ClaimParams.Kinds` 在 SQL 层直接跳过不抢；
  - **已明确关闭的已知内置特性**（如 `FEATURE_RISK_SCAN=false`）：由 `HandleDisabled` 注册 `job.Skip` 兜底处理器排空历史残留，避免永远停在 `pending` 持续触发 `shortlink_jobs_oldest_pending_age_seconds > 300` 误告警。

### 2. 存储层：迁移 `019` 三个 `kind` 前导部分索引与双路径 `unnest + LATERAL` 抢占 SQL

在 [migrations/019_jobs_claim_by_kind_indexes.up.sql](../../migrations/019_jobs_claim_by_kind_indexes.up.sql)（可逆迁移）删除旧索引 `idx_jobs_claimable`、`idx_jobs_pending_tenant_run` 与两列 `idx_jobs_pending_kind_run`，改为 3 个 `kind` 前导部分索引：

```sql
CREATE INDEX idx_jobs_pending_kind_run
    ON jobs (kind, run_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_jobs_pending_kind_tenant_run
    ON jobs (kind, tenant_id, run_at, id)
    WHERE status = 'pending' AND tenant_id IS NOT NULL;

CREATE INDEX idx_jobs_pending_kind_platform_run
    ON jobs (kind, run_at, id)
    WHERE status = 'pending' AND tenant_id IS NULL;
```

- **`Store.Claim`（[internal/postgres/job.go](../../internal/postgres/job.go)）**：用 `normalizeClaimKinds` 对 `p.Kinds` 去空、排序并去重（避免 `unnest` 重复 `kind` 导致同一行重复进入候选）；为空时零数据库往返直接返回 `nil, nil`。
- **`claimJobsNoFairnessSQL`（`MaxPerTenant <= 0`）**：经 `unnest($1::text[])` + `CROSS JOIN LATERAL` 分别沿 `idx_jobs_pending_kind_run` 按 `(run_at, id)` 取至多 `$2` 条并就地执行 `FOR UPDATE OF j SKIP LOCKED`（保证高并发 `Limit=1` 场景不因顶行被锁返回虚假空批），外层全局归并排序取前 `$2` 条更新。
- **`claimJobsFairSQL`（`MaxPerTenant > 0`）**：
  - `active_kind_tenants` 对每个声明的 `kind` 沿 `idx_jobs_pending_kind_tenant_run` 做递归松散索引扫描（显式带 `j.tenant_id IS NOT NULL` 匹配部分索引谓词），汇总去重出活跃租户；
  - `per_tenant_candidates` 对每个未满软上限租户做按 `kind` 嵌套 `LATERAL` + 合并裁顶，同时沿 `idx_jobs_pending_kind_platform_run` 合并平台级（`tenant_id IS NULL`）任务；
  - `ordered_candidates AS MATERIALIZED` 先固化有序候选集，再通过 `CROSS JOIN LATERAL (... FOR UPDATE OF j SKIP LOCKED)` 主键点查复核可见性并截断 `LIMIT $3`，避免 PostgreSQL 在候选估算偏大时退化为全表 `Hash Join + Seq Scan`。

## 不做什么（明确接受的风险边界）

1. **不为不同 `kind` 拆分多张队列表或独立表分区**：在当前单体 Outbox 架构下维持单一 `jobs` 表与统一 Reaper/清理周期；仅通过 `kind` 前导部分索引完成逻辑隔离。
2. **不自动把长期无人认领的 `pending` 任务标为 `dead`**：废弃 `kind` 若无人声明且未注册 `HandleDisabled`，将停留在 `pending` 并由现有告警 `ShortlinkJobsBacklog`（`shortlink_jobs_oldest_pending_age_seconds > 300`）暴露给运维处理，不做后台猜测性清理。

## 怎么验证

1. **合成积压 `EXPLAIN (ANALYZE, BUFFERS)` 实测（PostgreSQL 17，45,500 行 `jobs`：30,000 条未知 `export.csv` + 10,000 `link.risk_scan` + 5,000 `webhook.deliver` + 500 平台 `invitation.email`，20 个租户）**：
   - 无公平路径（`MaxPerTenant=0`）：`unnest + LATERAL` 命中 `idx_jobs_pending_kind_run`，执行时间 **`1.272 ms`**、共享缓冲 **`122`**（对比直接 `kind = ANY(...)` 在旧 `idx_jobs_claimable` 上因回表过滤 30,000 行未知任务耗时 **`8.469 ms`**、缓冲 `658`）；
   - 公平路径（`MaxPerTenant=4`）：若直接 `JOIN jobs`，优化器会退化为全表 `Seq Scan + Hash Join`（**`140.219 ms`**）；改用 `ordered_candidates AS MATERIALIZED` + `LATERAL` 主键点查后降至 **`~6.6–7.5 ms`**；若队列中只有 30,000 条未知 `export.csv`，老 worker 抢占仅 **`0.442 ms` / 6 shared hit buffers**；
   - `OldestPendingAges` 仍走 `Index Only Scan using idx_jobs_pending_kind_run`（`1.399 ms`，40 buffers）。
2. **单元与集成测试**：
   - `internal/job/runner_test.go`：覆盖滚动发布期间老 worker 跳过更早到期的 `export.csv` 并由新 worker 接盘、空 `handlers` 零抢占、自定义未过滤队列返回未知 `kind` 走 `Retry` 而非直接 `Dead`；
   - `internal/postgres/job_test.go`：覆盖 `MaxPerTenant = 0` 与 `MaxPerTenant > 0`（多租户 + 平台任务）下老 worker 绝不碰 `export.csv`、`Kinds` 为空/重复/空串归一、新 worker 接盘消费；
   - `internal/postgres/identityschema_test.go`：校验迁移 019 三个新部分索引存在且旧索引已删除。
3. **全量自检与基准回归**：`make selfcheck`（含 `019` `up -> down -> up` 可逆迁移、`golangci-lint`、`-race` 全量单测/集成测试与 `smoke.sh`）以及 `BenchmarkJobClaimSkipLocked` 8/32/64 并发压测全通过。

## 后果

- **收益**：滚动发布或灰度上线新增任务类型时，新老 `worker` 可安全混跑，彻底消灭“旧 worker 抢到新任务即标 `dead`”；同时即使未知或低优先级任务积压上万条，已知 `kind` 仍走精确部分索引定位。
- **代价**：`jobs` 写入/状态变更多维护 1 个 `pending` 部分索引（3 个替代原 3 个，`done`/`dead`/`running` 不入索引）；`claimJobsFairSQL` 结构相对复杂，后续改动须配合 `EXPLAIN (ANALYZE, BUFFERS)` 复核计划。
