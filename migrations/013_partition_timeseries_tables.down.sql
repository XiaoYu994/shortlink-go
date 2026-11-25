-- 将四张按月分区表还原为普通单表，保持与 003 / 011 迁移完全一致的约束与索引。
SET lock_timeout = '3s';

CREATE TABLE link_clicks_daily_plain (
    tenant_id BIGINT NOT NULL,
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    clicks    BIGINT NOT NULL,
    CONSTRAINT link_clicks_daily_plain_pkey PRIMARY KEY (link_id, day),
    CONSTRAINT ck_link_clicks_daily_positive CHECK (clicks > 0)
);

CREATE TABLE link_dimension_daily_plain (
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    dimension TEXT   NOT NULL,
    value     TEXT   NOT NULL,
    clicks    BIGINT NOT NULL,
    CONSTRAINT link_dimension_daily_plain_pkey PRIMARY KEY (link_id, day, dimension, value),
    CONSTRAINT ck_link_dimension_daily_dimension
        CHECK (dimension IN ('referrer', 'device', 'client')),
    CONSTRAINT ck_link_dimension_daily_value_length
        CHECK (octet_length(value) BETWEEN 1 AND 255),
    CONSTRAINT ck_link_dimension_daily_positive CHECK (clicks > 0)
);

CREATE TABLE link_visitors_daily_plain (
    link_id      BIGINT NOT NULL,
    day          DATE   NOT NULL,
    visitor_hash BYTEA  NOT NULL,
    CONSTRAINT link_visitors_daily_plain_pkey PRIMARY KEY (link_id, day, visitor_hash),
    CONSTRAINT ck_link_visitors_daily_hash_length
        CHECK (octet_length(visitor_hash) = 16)
);

CREATE TABLE audit_logs_plain (
    id BIGINT GENERATED ALWAYS AS IDENTITY,
    tenant_id BIGINT NOT NULL,
    actor_kind TEXT NOT NULL,
    actor_id BIGINT,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT audit_logs_plain_pkey PRIMARY KEY (id),
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

INSERT INTO link_clicks_daily_plain (tenant_id, link_id, day, clicks)
SELECT tenant_id, link_id, day, clicks FROM link_clicks_daily;

INSERT INTO link_dimension_daily_plain (link_id, day, dimension, value, clicks)
SELECT link_id, day, dimension, value, clicks FROM link_dimension_daily;

INSERT INTO link_visitors_daily_plain (link_id, day, visitor_hash)
SELECT link_id, day, visitor_hash FROM link_visitors_daily;

INSERT INTO audit_logs_plain (id, tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at)
OVERRIDING SYSTEM VALUE
SELECT id, tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at
FROM audit_logs;

DROP TABLE link_clicks_daily;
ALTER TABLE link_clicks_daily_plain RENAME TO link_clicks_daily;
ALTER TABLE link_clicks_daily RENAME CONSTRAINT link_clicks_daily_plain_pkey TO link_clicks_daily_pkey;

DROP TABLE link_dimension_daily;
ALTER TABLE link_dimension_daily_plain RENAME TO link_dimension_daily;
ALTER TABLE link_dimension_daily RENAME CONSTRAINT link_dimension_daily_plain_pkey TO link_dimension_daily_pkey;

DROP TABLE link_visitors_daily;
ALTER TABLE link_visitors_daily_plain RENAME TO link_visitors_daily;
ALTER TABLE link_visitors_daily RENAME CONSTRAINT link_visitors_daily_plain_pkey TO link_visitors_daily_pkey;

DROP TABLE audit_logs;
ALTER TABLE audit_logs_plain RENAME TO audit_logs;
ALTER TABLE audit_logs RENAME CONSTRAINT audit_logs_plain_pkey TO audit_logs_pkey;

SELECT setval(
    pg_get_serial_sequence('audit_logs', 'id'),
    COALESCE((SELECT MAX(id) FROM audit_logs), 1),
    (SELECT COUNT(*) > 0 FROM audit_logs)
);

CREATE INDEX idx_link_clicks_daily_tenant_day_clicks
    ON link_clicks_daily (tenant_id, day, clicks DESC, link_id DESC);

CREATE INDEX idx_audit_logs_tenant_id ON audit_logs (tenant_id, id DESC);
CREATE INDEX idx_audit_logs_tenant_action_id ON audit_logs (tenant_id, action, id DESC);

COMMENT ON TABLE link_clicks_daily IS '每条链接每天的点击数';
COMMENT ON COLUMN link_clicks_daily.tenant_id IS '冗余列，写库时由 links 填充，服务按租户的今日排序';
COMMENT ON TABLE link_dimension_daily IS '按天、按维度的点击聚合，dimension 为 referrer、device、client';
COMMENT ON TABLE link_visitors_daily IS '按天去重的访客哈希，每行 16 字节';
COMMENT ON INDEX idx_link_clicks_daily_tenant_day_clicks IS '按租户和日期取今日 Top N';
