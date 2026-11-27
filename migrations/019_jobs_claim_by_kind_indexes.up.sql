-- 按任务类型（kind）隔离抢占的部分索引（ADR 0033）。
-- 消除滚动发布新增任务 kind 时旧版 worker 误抢并标 dead 的问题，同时避免过滤 kind 时回表扫描积压任务。

DROP INDEX idx_jobs_claimable;
DROP INDEX idx_jobs_pending_tenant_run;
DROP INDEX idx_jobs_pending_kind_run;

-- 1. 服务无单租户软上限时的按 kind 抢占（MaxPerTenant <= 0）以及 OldestPendingAges 松散索引扫描。
CREATE INDEX idx_jobs_pending_kind_run
    ON jobs (kind, run_at, id)
    WHERE status = 'pending';

-- 2. 服务按 (kind, tenant_id) 的递归松散索引扫描与 LATERAL 公平抢占（仅覆盖租户级任务）。
CREATE INDEX idx_jobs_pending_kind_tenant_run
    ON jobs (kind, tenant_id, run_at, id)
    WHERE status = 'pending' AND tenant_id IS NOT NULL;

-- 3. 服务平台级任务（tenant_id IS NULL），避免 0 条平台任务时跨过上万条租户行。
CREATE INDEX idx_jobs_pending_kind_platform_run
    ON jobs (kind, run_at, id)
    WHERE status = 'pending' AND tenant_id IS NULL;

COMMENT ON INDEX idx_jobs_pending_kind_run IS '支撑无租户上限按 kind 抢占（run_at, id 有序）及 OldestPendingAges 松散索引扫描';
COMMENT ON INDEX idx_jobs_pending_kind_tenant_run IS '支撑按 kind + tenant_id 递归松散索引扫描与 LATERAL 公平抢占';
COMMENT ON INDEX idx_jobs_pending_kind_platform_run IS '支撑平台级（tenant_id IS NULL）pending 任务按 kind 抢占';
