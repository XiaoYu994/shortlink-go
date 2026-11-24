# PostgreSQL 索引失效、队列积压雪崩与租约 ABA 覆盖审查

- 日期：2026-11-24
- 来源：架构审查 + PostgreSQL 17 `EXPLAIN (ANALYZE, BUFFERS)` 实测
- 关联：[ADR 0022](../adr/0022-postgres-query-and-lease-hardening.md)、迁移 `010`、[ADR 0012](../adr/0012-click-analytics.md)、[ADR 0020](../adr/0020-background-jobs-and-async-risk-scan.md)

## 现象与影响

阶段 13 完成后，对全部 9 份数据库迁移与 `internal/postgres/` 的生产 SQL 进行全量架构审查，定位出 4 个 P0 级性能/并发隐患和 4 处 P1 级语义与约束边界缺陷：

1. **今日点击排序（`sort=clicks_today`）未命中专用复合索引**：每次翻页都全表扫该租户全部短链并做内存排序，`idx_link_clicks_daily_tenant_day_clicks` 沦为只增写放大、零读命中的死索引。
2. **过期会话定期清理（`CleanupExpiredSessions`）全表扫描**：活跃会话越多、可删行越少，每次定时清理成本越高。
3. **后台任务公平抢占（`Claim`）与最老等待指标（`OldestPendingAges`）在积压时呈超线性退化**：单租户突发积压数万条任务时，每次 worker 轮询都在全表扫描 + 内存排序哈希计算窗口函数，极易触发“积压 → 抢占变慢 → 积压扩大”的雪崩。
4. **后台任务租约回写存在同 `WorkerID` 的 ABA 覆盖风险，且 `ReapExpired` 无批量上限**：同进程内旧协程超租约恢复后可能覆盖同一进程新协程正在执行的任务；崩溃恢复时单事务可能锁住全部过期任务。
5. **P1 边界问题**：`Store.Create` 存在 `OR NOT EXISTS (SELECT 1 FROM tenants ...)` 后门会静默产生孤儿链接与任务；过期邀请重邀覆写 `revoked_at` 造成历史状态失真；大组织成员变更因 `OR role = 'owner'` 扫描全组织成员；部分元字段与长度 CHECK 缺失。

## 复现与证据

在测试库（PostgreSQL 17）灌入以下合成规模数据并执行 `ANALYZE`：

- `links`：租户 1 共 20,000 条链接，其中今日有点击 200 条写入 `link_clicks_daily`；
- `sessions`：50,000 条有效活跃会话，其中 5 条已吊销超过 7 天保留期；
- `jobs`：30,000 条到期 `pending` 任务（租户 1 占 29,950 条，租户 2–6 各 10 条），另 2 条租户 1 的 `running` 任务；
- `memberships`：租户 1 共 5,000 名成员（其中 2 名 `owner`）。

修前关键 `EXPLAIN (ANALYZE, BUFFERS)` 摘录：

```text
-- 1. listLinksFirstByClicksTodaySQL（20k links / 200 clicked）
Limit (actual time=7.066..7.104 rows=21) Buffers: shared hit=269
 -> Sort (Sort Key: COALESCE(d.clicks, 0) DESC, l.id DESC; top-N heapsort Memory: 30kB)
   -> Hash Left Join (actual rows=20000)
     -> Seq Scan on links l (Filter: tenant_id = 1; actual rows=20000; Buffers: shared hit=267)
     -> Hash -> Seq Scan on link_clicks_daily d (actual rows=200)
Execution Time: 7.428 ms

-- 2. cleanupExpiredSessionsSQL（50k active sessions / 5 old revoked）
Limit (actual time=2.914..2.916 rows=5) Buffers: shared hit=618
 -> Sort (Sort Key: expires_at, id)
   -> Seq Scan on sessions (Rows Removed by Filter: 49995; Buffers: shared hit=618)
Execution Time: 2.951 ms

-- 3. claimJobsSQL（30k pending jobs, MaxPerTenant=4, Limit=8）
CTE Scan on candidate (actual time=41.707..41.718 rows=8) Buffers: shared hit=934
 -> LockRows -> Sort -> Hash Join (Rows Removed by Join Filter: 29978)
   -> Seq Scan on jobs j (actual rows=30000; Buffers: shared hit=462)
   -> Hash (Memory Usage: 3303kB)
     -> WindowAgg (actual rows=30000)
       -> Sort (Sort Key: tenant_id, run_at, id; Memory: 2409kB)
         -> Hash Left Join -> Seq Scan on jobs j_1 (actual rows=30000; Buffers: shared hit=462)
Execution Time: 42.753 ms

-- 4. oldestPendingAgesSQL（30k pending jobs）
HashAggregate (Group Key: kind; Buffers: shared hit=462)
 -> Seq Scan on jobs (actual rows=30000; Buffers: shared hit=462)
Execution Time: 6.362 ms
```

同 `WorkerID` 租约 ABA 竞态时序：

```text
t0: Worker W1 协程 A 抢占 job#1 (locked_by='W1', attempts=1, lease=30s)
t1: 协程 A 外部 I/O 卡顿超过 30s，Reaper 回收 job#1 回到 pending
t2: 同一进程 Worker W1 协程 B 重新抢占 job#1 (locked_by='W1', attempts=2)
t3: 协程 A 恢复并执行 Complete(1, 'W1')
    旧 SQL 条件仅查 id=1 AND status='running' AND locked_by='W1' -> 误命中并覆盖协程 B！
```

## 根因

1. **表达式包装可空列破坏有序扫描**：`LEFT JOIN` 后的 `COALESCE(d.clicks, 0)` 是跨表可空表达式，优化器无法将其映射到 `idx_link_clicks_daily_tenant_day_clicks`，且查询未带 `d.tenant_id = $1` 过滤。
2. **`OR` 混用有索引列与无索引列**：`expires_at < cutoff OR (revoked_at IS NOT NULL AND revoked_at < cutoff)` 中 `revoked_at` 无索引，PostgreSQL 既无法范围扫描也无法 `BitmapOr`，被迫全表扫描。
3. **无界窗口函数 + 索引缺前导分组列**：`row_number() OVER (PARTITION BY tenant_id ...)` 必须先物化全量输入行再做分组排序，且 `idx_jobs_claimable (run_at, id)` 不含 `tenant_id` 与 `kind`。
4. **身份标识代替租约代次**：进程级 `WorkerID` 在单进程多协程重试场景下不具备单调唯一性，不能充当分布式租约的 Fencing Token。

## 修复方案

具体表结构迁移与代码改动见 [ADR 0022](../adr/0022-postgres-query-and-lease-hardening.md) 与迁移 `010`：

- `sort=clicks_today` 改为只读 `REPEATABLE READ` 事务内的两段式有序索引查询；
- `CleanupExpiredSessions` 拆为两条独立清理 SQL，并补 `idx_sessions_revoked_at` 部分索引；
- `Claim` 拆分无上限快路径与“递归松散索引扫描 + `LATERAL`”公平路径，`OldestPendingAges` 改用 `idx_jobs_pending_kind_run` 递归松散索引扫描；
- `jobs` 增加单调递增 `lock_version`，所有回写校验 `lock_version` 并在 0 行命中时返回 `job.ErrLeaseLost`，`ReapExpired` 增加 `LIMIT` 分批回收并统一用 `<= now()`；
- 同步移除 `insertLinkSQL` 孤儿后门、引入 `invitations.superseded_at`、补 `idx_memberships_tenant_owners` 与元字段/长度约束，并合并 `Stats` 中 `device`/`client` 两次查询。

## 验证对比

在完全相同的合成数据集与 `ANALYZE` 条件下重跑 `EXPLAIN (ANALYZE, BUFFERS)`：

| 查询 | 修前计划 / Buffers / 耗时 | 修后计划 / Buffers / 耗时 | 改善 |
| --- | --- | --- | --- |
| `listLinksFirstClickedToday` (20k links / 200 clicked) | Seq Scan `links` + Hash Left Join + Sort / **269** / **7.428 ms** | Index Only Scan `idx_link_clicks_daily_tenant_day_clicks` + Nested Loop PK / **86** / **0.139 ms** | **53× 提速**，Buffers -68% |
| `CleanupExpiredSessions` (50k active / 5 old revoked) | Seq Scan `sessions` (滤掉 49,995 行) + Sort / **618** / **2.951 ms** | 双路 Index Scan (`idx_sessions_expires_at` + `idx_sessions_revoked_at`) / **2 + 2 = 4** / **0.023 + 0.025 = 0.048 ms** | **61× 提速**，Buffers -99.4% |
| `Claim` 公平抢占 (30k pending, MaxPerTenant=4) | 双次 Seq Scan `jobs` + Sort 2.4 MB + WindowAgg + Hash 3.3 MB / **934** / **42.753 ms** | Recursive Loose Index Scan + LATERAL Index Only Scan (`idx_jobs_pending_tenant_run`) / **75** / **0.453 ms** | **94× 提速**，Buffers -92%，内存 5.7 MB → 26 KB |
| `OldestPendingAges` (30k pending) | Seq Scan `jobs` + HashAggregate / **462** / **6.362 ms** | Recursive Loose Index Scan + LATERAL Index Only Scan (`idx_jobs_pending_kind_run`) / **8** / **0.619 ms**（执行节点 0.100 ms） | **10× 提速**，Buffers -98.3% |
| `lockMemberScopeSQL` (5k members / 2 owners) | 扫全组织 5,000 行回表滤 `role='owner'` | `BitmapOr` (`idx_memberships_tenant_id` + `idx_memberships_tenant_owners`) 仅碰 4 行 / **10** / **0.105 ms** | 消除大组织全成员扫描 |

同时确认所有单元、集成与端到端回归测试通过（`make selfcheck`）。

## 防再犯护栏

1. **自动化回归测试**：
   - `TestJobLeaseFencingPreventsSameWorkerABAAndReapBatchLimit` 与 `TestRunnerStaleLeaseLostDoesNotOverwriteSameWorkerReclaim` 锁死同 `WorkerID` ABA 防护与分批回收行为；
   - `TestAdminListClicksTodayMixedZeroPagination` 锁死两段式今日排序跨零点击边界翻页；
   - `TestTenantReferenceShape` 锁死“DB 无外键但 `Store.Create` 拒绝缺失租户”；
   - `TestMigration010IndexesAndConstraints` 与 `TestMigrationFilesPaired` 锁死迁移索引、约束与成对回滚。
2. **工程约定沉淀回 `AGENTS.md`**：
   - 新增或改造带索引的热路径/清理 SQL，必须用真实量级数据跑 `EXPLAIN (ANALYZE, BUFFERS)` 验证命中预期索引，禁止用 `OR` 混合有索引列和无索引列；
   - 后台清理与租约回收 SQL 一律带 `LIMIT` 分批执行，分布式租约回写必须校验单调递增 Fencing Token；
   - 严禁给高频更新计数列（如 `tenants.link_usage`、`link_usage_month`）建索引，以保留 PostgreSQL HOT Update 优化。
