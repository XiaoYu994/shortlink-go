# 0026. 时序统计表与审计日志的按月范围分区及保留期治理

- 状态：已采纳
- 日期：2026-11-24
- 相关：`0003`, `0012`, `0020`, `0022`, `0023`, `0025`

## 背景

在全量架构审查中，`cmd/console worker` 原先仅周期清理 `sessions` 与 `done` 状态 `jobs`，而四张只增不减的明细/日聚合表没有保留期治理：

1. **三张点击日统计表**（`link_clicks_daily`、`link_dimension_daily`、`link_visitors_daily`）：其中 `link_visitors_daily` 每个 `(link_id, day, visitor_hash)` 占一行，随长尾跳转持续膨胀；若按行 `DELETE` 清理历史天，会产生海量死元组并拖垮 `autovacuum`。
2. **审计日志表**（`audit_logs`）：记录组织全量管理操作，长期只增不减会抬高全表体积与索引深度，且若保留在单表中，后续合规保留期到期清理同样面临大表行级 `DELETE`。

## 决策

### 1. PostgreSQL 原生声明式按月范围分区（`RANGE`）

- 迁移 [013_partition_timeseries_tables.up.sql](../../migrations/013_partition_timeseries_tables.up.sql) 在 `SET lock_timeout = '3s'` 保护下将四张表转换为声明式分区表：
  - `link_clicks_daily`、`link_dimension_daily`、`link_visitors_daily`：按 UTC `DATE` 列 `day` 分区；三张表的现有复合主键均已包含 `day`，无需改动列结构。
  - `audit_logs`：按 `created_at` 分区；PostgreSQL 要求全局唯一/主键必须包含分区键，因此主表主键扩展为 `PRIMARY KEY (id, created_at)`，保留原有 `id BIGINT GENERATED ALWAYS AS IDENTITY` 及既有二次索引 `(tenant_id, id DESC)`、`(tenant_id, action, id DESC)`。
- 迁移初始化当前 UTC 月前 3 个月至未来 2 个月（`[now_utc - 3m .. now_utc + 2m]`）的月分区，并以 `YYYY_MM` 命名（如 `audit_logs_2026_11`），边界严格使用 UTC `DATE` 或 `+00` 显式 UTC `TIMESTAMPTZ`。
- 同时为每张主表建立 `_default` 兜底分区（如 `audit_logs_default`），确保 worker 尚未追平或异常历史/未来时间戳写入绝不会因找不到分区而直接失败。

### 2. 自动预建与整月 `DROP TABLE` 保留期清理

- [`internal/postgres/partition.go`](../../internal/postgres/partition.go) 暴露两类分区治理方法，所有 DDL 均在独立短事务内设置 `SET LOCAL lock_timeout = '3s'` 防长锁阻塞线上流量：
  - `EnsureMonthlyPartitions(ctx, now, monthsAhead)`：按当月到未来 `monthsAhead`（默认 2 个月）幂等执行 `CREATE TABLE IF NOT EXISTS <tbl>_YYYY_MM PARTITION OF <tbl> ...`。
  - `DropExpiredStatsPartitions(ctx, now, retentionDays)`（默认 `WORKER_STATS_RETENTION_DAYS=90`）与 `DropExpiredAuditPartitions(ctx, now, retentionDays)`（默认 `WORKER_AUDIT_RETENTION_DAYS=180`）：只扫描 `pg_inherits` 下当前 schema 的直系子分区，严格匹配正则 `^<parent>_(\d{4})_(\d{2})$`，仅当该月结束上界 `monthEnd <= cutoff`（整个分区全部过期）时执行 `DROP TABLE IF EXISTS <child>`。**`_default` 分区永不删除**。
- `cmd/console worker` 启动时执行一次 `EnsureMonthlyPartitions` 预建未来 2 个月分区，并在周期任务 `partitions.maintain`（间隔 `WORKER_CLEANUP_INTERVAL`，默认 1 小时）内循环预建与整月回收。

### 3. 配合异步清理护栏

- 由于范围分区表的 `ctid` 跨分区不唯一，[ADR 0025](0025-reconcile-cache-async-purge-and-audit-hardening.md) 配套实现的 `link.purge_stats`（`PurgeLinkStatsBatch`）统一基于完整主键元组删除，绝不依赖 `ctid`。

## 不做什么

- **不引入 `pg_partman` 或 TimescaleDB 外部扩展**：保持 PostgreSQL 16/17 原生零扩展依赖与简洁的容器/本地开发栈；百行 Go 分区维护逻辑完全覆盖当前需求，且迁移 `013.down.sql` 可完整无损回滚为普通表。
- **不在业务请求路径做分区探测或自动建表**：所有 DDL 收敛在迁移工具与异步 `console worker`。
- **不删除或截断 `_default` 分区**：异常时间落入 `_default` 后只可通过人工/运维窗口迁移或观察，绝不允许定时任务误删数据。

## 后果

- 历史数据淘汰代价从千万级行扫 `DELETE` + `VACUUM` 降为瞬时 `DROP TABLE` 元数据释放与磁盘回收。
- 按月区间查询（如 `Stats` 的最多 90 天窗口）自动触发分区裁剪，跳过无关历史月分区。
- 运维侧需确保 `console worker` 持续运行以提前创建未来月分区；若长期停机导致新月份落入 `_default`，在创建对应月分区前需先处理 `_default` 中跨边界数据（或由运维窗口拆分）。

## 验证标准

1. `scripts/prepare-test-db.sh` 验证迁移 `013` 的 `up -> down -> up` 全程无错且数据与索引结构对称还原。
2. `internal/postgres/partition_test.go` 验证 4 张表均为分区表且挂有 `_default`，验证 `EnsureMonthlyPartitions` 幂等、过期月分区删除且 `_default` 与保留期内分区完好。
3. 在跨 3 个月的千行级合成数据上执行真实 `EXPLAIN (ANALYZE, BUFFERS)`，确认日统计范围查命中当月分区并裁剪历史月分区、`audit_logs` 游标查询走 `Merge Append` + `Index Scan`、`PurgeLinkStatsBatch` 走主键索引扫描。
