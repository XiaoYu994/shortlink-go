# 0022. PostgreSQL 查询路径、索引与后台任务租约加固

- 状态：Accepted
- 日期：2026-11-24
- 关联：修订 [ADR 0012](0012-click-analytics.md)（今日点击排序查询）、[ADR 0018](0018-users-orgs-and-rbac.md)（邀请过期重邀与会话清理）、[ADR 0020](0020-background-jobs-and-async-risk-scan.md)（任务抢占、最老等待统计与租约 Fencing）

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 今日点击排序（`sort=clicks_today`） | 改为只读 `REPEATABLE READ` 事务内的两段式有序索引查询：先以 `link_clicks_daily` 走 `idx_link_clicks_daily_tenant_day_clicks`，不足 `limit` 或游标落入 `0` 点击段再按 `idx_links_tenant_id` 逆序补齐 |
| 过期与已吊销会话清理 | 新增部分索引 `idx_sessions_revoked_at (revoked_at) WHERE revoked_at IS NOT NULL`，将 `CleanupExpiredSessions` 拆成按 `expires_at` 和按 `revoked_at` 的两条独立 `DELETE`，彻底消除 `OR` 导致的全表扫描 |
| 任务公平抢占与积压指标 | 新增部分索引 `idx_jobs_pending_tenant_run (tenant_id, run_at, id)` 与 `idx_jobs_pending_kind_run (kind, run_at)`；`MaxPerTenant <= 0` 走极简快路径，`MaxPerTenant > 0` 与 `OldestPendingAges` 改用递归松散索引扫描 + `LATERAL`，消除积压下的全表窗口排序 |
| 租约 Fencing Token 与分批回收 | `jobs` 表新增单调递增 `lock_version` 列；`Complete`/`Retry`/`Dead` 校验 `(id, status='running', locked_by, lock_version)`，0 行命中返回 `job.ErrLeaseLost` 防止同 `WorkerID` ABA 覆盖；`ReapExpired` 增加 `LIMIT` 分批回收并统一用 `locked_until <= now()` |
| 应用层租户引用兜底 | 移除 `insertLinkSQL` 里的 `OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = $4)` 后门，`Store.Create` 对缺失或非法 `tenant_id` 统一返回 `ErrTenantNotFound`，不再静默写入孤儿链接和任务 |
| 过期邀请重邀语义 | `invitations` 新增 `superseded_at` 并更新状态互斥约束与部分唯一索引；过期重邀写 `superseded_at` 而不再覆盖 `revoked_at`，保留历史 `expired` 展示语义 |
| 大组织成员锁与元字段收口 | 新增 `idx_memberships_tenant_owners` 部分索引；补齐 `tenants.created_at`、`ck_tenants_name_length`、`links.updated_at`、`api_keys.ck_api_keys_hash_length`；`Stats` 合并 `device`/`client` 维度查询减少一次 DB 往返 |

---

## 背景

在完成阶段 13 后对全部 9 份迁移文件与 `internal/postgres/` 生产 SQL 做了一次面向生产的架构复审，并用 PostgreSQL 17 `EXPLAIN (ANALYZE, BUFFERS)` 在合成规模数据（2 万链接、5 万活跃会话、3 万积压任务、5 千组织成员）上做了实测，发现 4 个 P0 级隐患和若干 P1 边界瑕疵（完整执行计划证据见 [2026-11-24 数据库审查复盘](../postmortems/2026-11-24-postgres-schema-and-query-audit.md)）：

1. **死索引 + 全租户排序**：迁移 003 专门为今日排序建了 `idx_link_clicks_daily_tenant_day_clicks`，但 `admin.go` 的 `LEFT JOIN ... ORDER BY COALESCE(d.clicks, 0) DESC` 完全用不上它，每次查询都在全表扫 `links` 并做内存排序。
2. **`OR` 破坏会话清理索引**：`cleanupExpiredSessionsSQL` 把有索引的 `expires_at` 和无索引的 `revoked_at` 用 `OR` 拼在一起，PG 放弃索引退化为全表扫描（5 万活跃会话全扫滤掉 49,995 行）。
3. **队列积压 `O(N log N)` 死亡螺旋**：`claimJobsSQL` 的 `eligible` CTE 对全表到期 `pending` 行算 `row_number() OVER (PARTITION BY tenant_id ...)`，3 万积压行单次抢占就要双次全表扫 + 5.7 MB 内存排序哈希耗时 42.7 ms；`oldestPendingAgesSQL` 同样全表扫描回表 `GROUP BY`。
4. **同 `WorkerID` 租约 ABA 覆盖与无上限回收**：`Complete`/`Retry`/`Dead` 仅校验 `locked_by`（进程级固定 `WorkerID`），同一 worker 进程内旧协程卡顿超租约后被同进程新协程重抢，旧协程晚到仍会命中 `WHERE` 覆盖新执行状态；同时 `ReapExpired` 没有 `LIMIT`，宕机恢复时单事务可能锁住全部过期行。
5. **P1 边界缺陷**：`insertLinkSQL` 为旧测试保留的 `OR NOT EXISTS (SELECT 1 FROM tenants ...)` 后门会绕过配额静默制造孤儿数据；过期邀请重邀借写 `revoked_at` 导致历史状态从 `expired` 变成 `revoked`；`lockMemberScopeSQL` 的 `OR role = 'owner'` 在大组织下会扫全组织成员；基础元字段与长度约束存在遗漏。

---

## 决策

### 1. 迁移 010（`migrations/010_postgres_query_and_lease_hardening.{up,down}.sql`）

- 新增 4 个针对性部分索引：`idx_sessions_revoked_at`、`idx_jobs_pending_tenant_run`、`idx_jobs_pending_kind_run`、`idx_memberships_tenant_owners`。
- `jobs` 新增 `lock_version BIGINT NOT NULL DEFAULT 0` 及非负约束 `ck_jobs_lock_version_nonnegative`。
- `invitations` 新增 `superseded_at TIMESTAMPTZ`，将 `ck_invitations_state` 升级为 `num_nulls(accepted_at, revoked_at, superseded_at) >= 2`，并同步重建 `uq_invitations_pending_tenant_email` 与 `idx_invitations_pending_email` 过滤条件为 `WHERE accepted_at IS NULL AND revoked_at IS NULL AND superseded_at IS NULL`。
- `tenants` 补 `created_at` 与 `ck_tenants_name_length CHECK (octet_length(btrim(name)) BETWEEN 1 AND 100)`；`links` 补 `updated_at`；`api_keys` 补 `ck_api_keys_hash_length CHECK (octet_length(key_hash) = 32)`。
- `down.sql` 在重建旧索引前将 `superseded_at` 无损回填到 `revoked_at`（同语句置空 `superseded_at` 以满足互斥约束），确保 `up -> down -> up` 在有数据的库上也能干净回滚。

### 2. 今日点击排序两段式索引查询（`internal/postgres/admin.go`）

- 当游标已落入 `ClicksToday <= 0` 时，直接查询 `links` 逆序 `id < $2` 且 `NOT EXISTS` 今日点击行，跳过 `link_clicks_daily`。
- 其余情况在同一只读 `REPEATABLE READ` 事务内执行（保证两段共用同一 `now()` UTC 日期和 MVCC 快照）：
  1. 以 `link_clicks_daily` 为驱动表按 `(tenant_id, day, clicks DESC, link_id DESC)` 走 `idx_link_clicks_daily_tenant_day_clicks` 并 `JOIN links`；
  2. 仅当第一段不足 `limit` 时，再从 `links` 沿 `idx_links_tenant_id` 逆序补足今日 0 点击行。

### 3. 会话清理拆分为两条独立索引语句（`internal/postgres/session.go`）

- `CleanupExpiredSessions(ctx, retention, limit)` 先按 `expires_at < cutoff ORDER BY expires_at LIMIT $2` 删除（命中 `idx_sessions_expires_at`），若实际删除行数小于 `limit`，再以剩余配额按 `revoked_at IS NOT NULL AND revoked_at < cutoff ORDER BY revoked_at LIMIT $2` 删除（命中 `idx_sessions_revoked_at`）。

### 4. 任务抢占、积压统计与 Fencing Token（`internal/job`、`internal/postgres/job.go`）

- **抢占分流**：`MaxPerTenant <= 0` 走极简 `idx_jobs_claimable` 快路径；`MaxPerTenant > 0` 用递归 CTE 沿 `idx_jobs_pending_tenant_run` 跳扫活跃租户（单租户积压再多也只花 1 次 B-Tree 查找跳过），再通过 `CROSS JOIN LATERAL (... LIMIT GREATEST($2 - running_cnt, 0))` 合并 `tenant_id IS NULL` 平台任务，最后在外层 `ORDER BY run_at, id LIMIT $1 FOR UPDATE OF j SKIP LOCKED`。
- **最老等待统计**：`OldestPendingAges` 改用递归 CTE 沿 `idx_jobs_pending_kind_run` 跳扫不同 `kind`，再 `LATERAL` 取最早到期单行，复杂度从 `O(积压量)` 降为 `O(kind 数)`。
- **单调 Fencing Token**：每次 `Claim` 原子执行 `lock_version = j.lock_version + 1`；`Complete`/`Retry`/`Dead` 全部校验 `AND lock_version = $3`，影响行数为 0 时返回 `job.ErrLeaseLost`；`Runner.executeJob` 识别 `ErrLeaseLost` 后仅记告警日志，不再重复累计 `processed_total` 指标。
- **分批回收**：`ReapExpired` 新增 `limit` 参数（SQL 改为 `locked_until <= now() ORDER BY locked_until LIMIT $2 FOR UPDATE SKIP LOCKED`），`Runner.reapOnce` 按 `ReapBatchSize`（默认 500）循环排空。

### 5. 引用完整性、邀请语义与杂项收口

- `insertLinkSQL` 移除 `OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = $4)`，`Store.Create` 对 `TenantID <= 0` 或库中不存在的租户返回 `ErrTenantNotFound`。
- `expireStaleInvitationsSQL` 改写 `superseded_at = now()`，`redeemInvitationsSQL` 增加 `AND i.superseded_at IS NULL` 保持与部分索引一致，`RevokeInvitation` 将 `superseded_at` 纳入非待兑现拦截。
- `CreateOrgForUser` 改用 `tenants.created_at` 作为组织创建时间；`IssueAPIKey` 创建新租户时复用 `identity.NormalizeOrgName` 校验名称；`links.updated_at` 仅在管理写与状态变更时更新（点击计数不改此列）；`Stats` 将 `device` 与 `client` 两次固定维度查询合并为一次。

---

## 不做什么

以下属于规模化或契约变更范畴，按“先满足当前真实边界、达到触发条件再演进”的原则本次明确不做：

- **不做三张统计日表的按月声明式分区与历史日 UV 固化**：当前没有跨月大表归档或千万级单链 UV 压力；当单表超过千万行或统计 `count(*)` 成为慢查询时另立 ADR 引入按 `day` 分区或列式存储。
- **不做爆款短链软删除 / 异步分批清理统计行**：当前单条链接访客行数远未触及 5 秒事务超时；出现大流量删除超时时改为软删 + `jobs` 异步清理。
- **不做 `ListAPIKeys` / `ListMembers` / `ListInvitations` 游标分页**：改动会变更 HTTP API 契约与前端列表交互，待单个组织历史记录规模增长后再扩展。
- **不对 `tenants.link_usage` / `link_usage_month` 建索引**：刻意保持这两列无索引以让 PostgreSQL 在高频创建短链时走 HOT（Heap-Only Tuple）Update，减轻表和索引膨胀。
- **暂不往 `link_dimension_daily` / `link_visitors_daily` 加冗余 `tenant_id`**：留到阶段 16 设计 PostgreSQL RLS 时与 RLS 策略和数据库角色划分一并定案。

---

## 怎么验证

1. **迁移可逆性**：`make test-db` 执行 `up -> down -> up` 验证迁移 `010` 升到 10、降到空、再升到 10 全部成功；`cmd/migrate` 的 `TestMigrationFilesPaired` 锁定 10 对迁移文件齐全。
2. **执行计划实测**：在同一组 2 万链接 / 5 万会话 / 3 万积压任务 / 5 千成员合成数据集上运行 `EXPLAIN (ANALYZE, BUFFERS)`，验证所有 5 条查询从全表扫描切换为索引扫描（详细前后对比见 [2026-11-24 审查复盘](../postmortems/2026-11-24-postgres-schema-and-query-audit.md)）。
3. **新增单元与集成回归测试**：
   - `internal/job`：`TestRunnerStaleLeaseLostDoesNotOverwriteSameWorkerReclaim`、`TestRunnerReaperDrainsAcrossBatches`（`testing/synctest`）；
   - `internal/postgres`：`TestJobLeaseFencingPreventsSameWorkerABAAndReapBatchLimit`、`TestJobClaimFairnessIncludesPlatformJobsAndSkipsFuture`、`TestAdminListClicksTodayMixedZeroPagination`、`TestMigration010IndexesAndConstraints`，以及更新后的 `TestTenantReferenceShape`、`TestCreateInvitation`、`TestJobReapAndCleanup`。
4. **全量自检**：`make selfcheck`（含 `gofmt`、`go vet`、`golangci-lint`、单测 `-race`、集成测试 `-race` 与端到端 `smoke.sh`）全部通过。

---

## 后果

- **收益**：彻底消除高基数/高积压场景下的 4 类指数级退化与并发覆盖风险；`EXPLAIN` 实测缓冲区读取降低 68%–99.4%、单次查询提速 10×–94×；应用层租户隔离与邀请历史状态更严谨。
- **代价**：`jobs` 表新增 2 个部分索引、`sessions` 与 `memberships` 各增 1 个部分索引；由于全部带严格 `WHERE` 过滤条件（只覆盖 `pending` / 已吊销 / `owner` 极小子集），写放大和索引体积极小。`job.Queue` 接口增加 `lockVersion` 与 `ReapExpired` 的 `limit` 参数，仅影响内部包 `internal/job` 与 `internal/postgres`。
