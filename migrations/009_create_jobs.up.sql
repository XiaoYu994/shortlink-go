-- 通用可靠后台任务表（PostgreSQL Outbox / Job 队列，阶段 12、ADR 0020）。
-- 不建数据库外键与级联，引用完整性由应用层在事务内保证（ADR 0010）。
-- 第一期用于异步 URL 风控（link.risk_scan），后续复用承载 Webhook 投递等可靠后台任务。

CREATE TABLE jobs (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         TEXT        NOT NULL,
    tenant_id    BIGINT,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    dedup_key    TEXT,
    status       TEXT        NOT NULL DEFAULT 'pending',
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 5,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at    TIMESTAMPTZ,
    locked_until TIMESTAMPTZ,
    locked_by    TEXT,
    last_error   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    CONSTRAINT ck_jobs_kind_length CHECK (octet_length(kind) BETWEEN 1 AND 128),
    CONSTRAINT ck_jobs_tenant_id_positive CHECK (tenant_id IS NULL OR tenant_id > 0),
    CONSTRAINT ck_jobs_status CHECK (status IN ('pending', 'running', 'done', 'dead')),
    CONSTRAINT ck_jobs_attempts CHECK (attempts >= 0 AND max_attempts > 0),
    CONSTRAINT ck_jobs_running_lock CHECK (
        status <> 'running' OR (locked_at IS NOT NULL AND locked_until IS NOT NULL AND locked_by IS NOT NULL)
    ),
    CONSTRAINT ck_jobs_finished_at CHECK (
        (status IN ('done', 'dead')) = (finished_at IS NOT NULL)
    )
);

-- 抢占只扫待执行行，历史 done/dead 行不拖慢索引。
CREATE INDEX idx_jobs_claimable
    ON jobs (run_at, id)
    WHERE status = 'pending';

-- 租约回收只扫运行中行。
CREATE INDEX idx_jobs_lease_expired
    ON jobs (locked_until)
    WHERE status = 'running';

-- 按租户统计当前运行中任务数，支撑公平调度软上限。
CREATE INDEX idx_jobs_running_tenant
    ON jobs (tenant_id)
    WHERE status = 'running' AND tenant_id IS NOT NULL;

-- 可选去重：同一类型和去重键在未完成时最多保留一条活跃任务。
CREATE UNIQUE INDEX uq_jobs_dedup_active
    ON jobs (kind, dedup_key)
    WHERE dedup_key IS NOT NULL AND status IN ('pending', 'running');

-- 支撑历史 done 任务的定期分批清理。
CREATE INDEX idx_jobs_finished_done
    ON jobs (finished_at, id)
    WHERE status = 'done';

COMMENT ON TABLE jobs IS 'PostgreSQL Outbox 通用后台任务队列；业务写与入队在同一事务提交';
COMMENT ON COLUMN jobs.kind IS '任务类型，例如 link.risk_scan';
COMMENT ON COLUMN jobs.tenant_id IS '触发任务的租户编号；平台级任务为空，用于抢占公平调度软上限';
COMMENT ON COLUMN jobs.dedup_key IS '可选去重键；非空时同 kind 未完成任务唯一';
COMMENT ON COLUMN jobs.locked_until IS 'worker 租约到期时刻；到期后由 Reaper 回收';
COMMENT ON COLUMN jobs.finished_at IS '进入 done 或 dead 终态的时间；非终态为空';
