-- 阶段 14：组织操作审计日志表（ADR 0023）。
-- 沿用 ADR 0010：不建数据库外键，成员/短链/API Key 删除后历史审计记录依然保留且可追溯。
CREATE TABLE audit_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    actor_kind TEXT NOT NULL,
    actor_id BIGINT,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_audit_logs_tenant_positive CHECK (tenant_id > 0),
    CONSTRAINT ck_audit_logs_actor_kind CHECK (actor_kind IN ('user', 'api_key', 'operator', 'system')),
    CONSTRAINT ck_audit_logs_actor_consistency CHECK (
        (actor_kind IN ('user', 'api_key') AND actor_id IS NOT NULL AND actor_id > 0)
        OR (actor_kind IN ('operator', 'system') AND actor_id IS NULL)
    ),
    CONSTRAINT ck_audit_logs_action_nonempty CHECK (octet_length(action) BETWEEN 1 AND 64),
    CONSTRAINT ck_audit_logs_target_type_nonempty CHECK (octet_length(target_type) BETWEEN 1 AND 32),
    CONSTRAINT ck_audit_logs_target_id_nonempty CHECK (octet_length(target_id) BETWEEN 1 AND 128),
    CONSTRAINT ck_audit_logs_request_id_length CHECK (octet_length(request_id) <= 128),
    CONSTRAINT ck_audit_logs_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

-- 按租户倒序游标分页主索引。
CREATE INDEX idx_audit_logs_tenant_id ON audit_logs (tenant_id, id DESC);

-- 按动作筛选 + 倒序游标分页复合索引，避免扫描无关动作行或触发额外 Sort。
CREATE INDEX idx_audit_logs_tenant_action_id ON audit_logs (tenant_id, action, id DESC);
