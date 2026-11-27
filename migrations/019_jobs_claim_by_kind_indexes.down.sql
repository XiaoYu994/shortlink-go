DROP INDEX idx_jobs_pending_kind_platform_run;
DROP INDEX idx_jobs_pending_kind_tenant_run;
DROP INDEX idx_jobs_pending_kind_run;

-- 恢复迁移 009 与迁移 010 的原始 jobs 待执行索引。
CREATE INDEX idx_jobs_claimable
    ON jobs (run_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_jobs_pending_tenant_run
    ON jobs (tenant_id, run_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_jobs_pending_kind_run
    ON jobs (kind, run_at)
    WHERE status = 'pending';

COMMENT ON INDEX idx_jobs_pending_tenant_run IS '支撑按租户松散索引扫描与 LATERAL 公平抢占，消除积压窗口全扫';
COMMENT ON INDEX idx_jobs_pending_kind_run IS '支撑按任务类型松散索引扫描最老到期 pending 任务';
