-- PostgreSQL 查询路径、索引与任务租约加固（ADR 0022）。
-- 修复代码审查与 EXPLAIN 剖析发现的索引失效、队列积压窗口全扫、同 WorkerID 租约 ABA 覆盖，
-- 并收口 invitations 过期重邀语义、大组织 owner 锁索引与基础元字段/长度约束。

-- 1. 已吊销会话清理索引：配合拆分为两条独立清理 SQL，消除 OR 导致的 sessions 全表扫描。
CREATE INDEX idx_sessions_revoked_at
    ON sessions (revoked_at)
    WHERE revoked_at IS NOT NULL;

-- 2. jobs 队列积压安全索引与单调递增 Fencing Token（lock_version）。
-- idx_jobs_pending_tenant_run 服务按租户的松散索引扫描 + LATERAL 公平抢占，替代全量窗口排序；
-- idx_jobs_pending_kind_run 服务 OldestPendingAges 的 O(kind 数) 松散索引扫描。
CREATE INDEX idx_jobs_pending_tenant_run
    ON jobs (tenant_id, run_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_jobs_pending_kind_run
    ON jobs (kind, run_at)
    WHERE status = 'pending';

ALTER TABLE jobs
    ADD COLUMN lock_version BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_jobs_lock_version_nonnegative CHECK (lock_version >= 0);

-- 3. 邀请过期后重邀改写 superseded_at，不再借写 revoked_at 造成历史状态从 expired 变 revoked。
ALTER TABLE invitations
    ADD COLUMN superseded_at TIMESTAMPTZ,
    DROP CONSTRAINT ck_invitations_state,
    ADD CONSTRAINT ck_invitations_state CHECK (
        num_nulls(accepted_at, revoked_at, superseded_at) >= 2
    );

DROP INDEX uq_invitations_pending_tenant_email;
CREATE UNIQUE INDEX uq_invitations_pending_tenant_email
    ON invitations (tenant_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL AND superseded_at IS NULL;

DROP INDEX idx_invitations_pending_email;
CREATE INDEX idx_invitations_pending_email
    ON invitations (email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL AND superseded_at IS NULL;

-- 4. 成员变更事务中按组织锁 owner 行的部分索引，避免大组织下 OR role = 'owner' 扫全组织成员。
CREATE INDEX idx_memberships_tenant_owners
    ON memberships (tenant_id, user_id)
    WHERE role = 'owner';

-- 5. 基础元字段与约束一致性收口。
ALTER TABLE tenants
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT ck_tenants_name_length CHECK (octet_length(btrim(name)) BETWEEN 1 AND 100);

ALTER TABLE links
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE api_keys
    ADD CONSTRAINT ck_api_keys_hash_length CHECK (octet_length(key_hash) = 32);

COMMENT ON INDEX idx_sessions_revoked_at IS '支撑已吊销会话的独立分批清理，避免 OR 破坏 idx_sessions_expires_at';
COMMENT ON INDEX idx_jobs_pending_tenant_run IS '支撑按租户松散索引扫描与 LATERAL 公平抢占，消除积压窗口全扫';
COMMENT ON INDEX idx_jobs_pending_kind_run IS '支撑按任务类型松散索引扫描最老到期 pending 任务';
COMMENT ON COLUMN jobs.lock_version IS '单调递增的租约代次（Fencing Token），每次抢占加一且永不回退，防陈旧回写覆盖';
COMMENT ON COLUMN invitations.superseded_at IS '已过期邀请因重邀被系统关闭的时间，与管理员主动撤销 revoked_at 区分';
COMMENT ON INDEX idx_memberships_tenant_owners IS '支撑成员权限变更事务只锁组织 owner 行';
COMMENT ON COLUMN tenants.created_at IS '租户（组织）创建时间';
COMMENT ON COLUMN links.updated_at IS '短链元数据最近更新时间（点击计数更新不改此列）';
