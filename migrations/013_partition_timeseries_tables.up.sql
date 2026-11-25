-- 将三张日粒度点击统计表与 audit_logs 转为 PostgreSQL 声明式按月范围分区表（ADR 0026）。
-- 保留期与未来月分区由 cmd/console worker 周期创建并在保留期外直接 DROP TABLE 整月分区，
-- 避免大表逐行 DELETE 与 VACUUM 膨胀；同时保留 _default 兜底分区防异常时间戳写入失败。
SET lock_timeout = '3s';

-- 1. link_clicks_daily（按 day 月分区）
CREATE TABLE link_clicks_daily_part (
    tenant_id BIGINT NOT NULL,
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    clicks    BIGINT NOT NULL,
    CONSTRAINT link_clicks_daily_part_pkey PRIMARY KEY (link_id, day),
    CONSTRAINT ck_link_clicks_daily_positive CHECK (clicks > 0)
) PARTITION BY RANGE (day);

-- 2. link_dimension_daily（按 day 月分区）
CREATE TABLE link_dimension_daily_part (
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    dimension TEXT   NOT NULL,
    value     TEXT   NOT NULL,
    clicks    BIGINT NOT NULL,
    CONSTRAINT link_dimension_daily_part_pkey PRIMARY KEY (link_id, day, dimension, value),
    CONSTRAINT ck_link_dimension_daily_dimension
        CHECK (dimension IN ('referrer', 'device', 'client')),
    CONSTRAINT ck_link_dimension_daily_value_length
        CHECK (octet_length(value) BETWEEN 1 AND 255),
    CONSTRAINT ck_link_dimension_daily_positive CHECK (clicks > 0)
) PARTITION BY RANGE (day);

-- 3. link_visitors_daily（按 day 月分区）
CREATE TABLE link_visitors_daily_part (
    link_id      BIGINT NOT NULL,
    day          DATE   NOT NULL,
    visitor_hash BYTEA  NOT NULL,
    CONSTRAINT link_visitors_daily_part_pkey PRIMARY KEY (link_id, day, visitor_hash),
    CONSTRAINT ck_link_visitors_daily_hash_length
        CHECK (octet_length(visitor_hash) = 16)
) PARTITION BY RANGE (day);

-- 4. audit_logs（按 created_at 月分区，主键扩展为 (id, created_at) 以满足分区键约束）
CREATE TABLE audit_logs_part (
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
    CONSTRAINT audit_logs_part_pkey PRIMARY KEY (id, created_at),
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
) PARTITION BY RANGE (created_at);

-- 为 4 张表创建 [now_utc - 3m .. now_utc + 2m] 月分区及 DEFAULT 兜底分区。
DO $$
DECLARE
    base_month DATE := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
    m_offset   INT;
    m_start    DATE;
    m_end      DATE;
    suffix     TEXT;
    ts_start   TEXT;
    ts_end     TEXT;
BEGIN
    FOR m_offset IN -3..2 LOOP
        m_start := (base_month + (m_offset || ' months')::interval)::date;
        m_end := (m_start + interval '1 month')::date;
        suffix := to_char(m_start, 'YYYY_MM');
        ts_start := to_char(m_start, 'YYYY-MM-DD') || ' 00:00:00+00';
        ts_end := to_char(m_end, 'YYYY-MM-DD') || ' 00:00:00+00';

        EXECUTE format(
            'CREATE TABLE %I PARTITION OF link_clicks_daily_part FOR VALUES FROM (%L) TO (%L)',
            'link_clicks_daily_' || suffix, m_start, m_end
        );
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF link_dimension_daily_part FOR VALUES FROM (%L) TO (%L)',
            'link_dimension_daily_' || suffix, m_start, m_end
        );
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF link_visitors_daily_part FOR VALUES FROM (%L) TO (%L)',
            'link_visitors_daily_' || suffix, m_start, m_end
        );
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF audit_logs_part FOR VALUES FROM (%L) TO (%L)',
            'audit_logs_' || suffix, ts_start, ts_end
        );
    END LOOP;
END $$;

CREATE TABLE link_clicks_daily_default PARTITION OF link_clicks_daily_part DEFAULT;
CREATE TABLE link_dimension_daily_default PARTITION OF link_dimension_daily_part DEFAULT;
CREATE TABLE link_visitors_daily_default PARTITION OF link_visitors_daily_part DEFAULT;
CREATE TABLE audit_logs_default PARTITION OF audit_logs_part DEFAULT;

-- 迁移既有数据（小表停机/短锁窗口切换）。
INSERT INTO link_clicks_daily_part (tenant_id, link_id, day, clicks)
SELECT tenant_id, link_id, day, clicks FROM link_clicks_daily;

INSERT INTO link_dimension_daily_part (link_id, day, dimension, value, clicks)
SELECT link_id, day, dimension, value, clicks FROM link_dimension_daily;

INSERT INTO link_visitors_daily_part (link_id, day, visitor_hash)
SELECT link_id, day, visitor_hash FROM link_visitors_daily;

INSERT INTO audit_logs_part (id, tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at)
OVERRIDING SYSTEM VALUE
SELECT id, tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at
FROM audit_logs;

-- 切换表名并清理旧表，再按原索引名与主键名重建。
ALTER TABLE link_clicks_daily RENAME TO link_clicks_daily_old;
ALTER TABLE link_clicks_daily_part RENAME TO link_clicks_daily;
DROP TABLE link_clicks_daily_old;
ALTER TABLE link_clicks_daily RENAME CONSTRAINT link_clicks_daily_part_pkey TO link_clicks_daily_pkey;

ALTER TABLE link_dimension_daily RENAME TO link_dimension_daily_old;
ALTER TABLE link_dimension_daily_part RENAME TO link_dimension_daily;
DROP TABLE link_dimension_daily_old;
ALTER TABLE link_dimension_daily RENAME CONSTRAINT link_dimension_daily_part_pkey TO link_dimension_daily_pkey;

ALTER TABLE link_visitors_daily RENAME TO link_visitors_daily_old;
ALTER TABLE link_visitors_daily_part RENAME TO link_visitors_daily;
DROP TABLE link_visitors_daily_old;
ALTER TABLE link_visitors_daily RENAME CONSTRAINT link_visitors_daily_part_pkey TO link_visitors_daily_pkey;

ALTER TABLE audit_logs RENAME TO audit_logs_old;
ALTER TABLE audit_logs_part RENAME TO audit_logs;
DROP TABLE audit_logs_old;
ALTER TABLE audit_logs RENAME CONSTRAINT audit_logs_part_pkey TO audit_logs_pkey;

SELECT setval(
    pg_get_serial_sequence('audit_logs', 'id'),
    COALESCE((SELECT MAX(id) FROM audit_logs), 1),
    (SELECT COUNT(*) > 0 FROM audit_logs)
);

CREATE INDEX idx_link_clicks_daily_tenant_day_clicks
    ON link_clicks_daily (tenant_id, day, clicks DESC, link_id DESC);

CREATE INDEX idx_audit_logs_tenant_id ON audit_logs (tenant_id, id DESC);
CREATE INDEX idx_audit_logs_tenant_action_id ON audit_logs (tenant_id, action, id DESC);

COMMENT ON TABLE link_clicks_daily IS '每条链接每天的点击数（按 day 月分区）';
COMMENT ON COLUMN link_clicks_daily.tenant_id IS '冗余列，写库时由 links 填充，服务按租户的今日排序';
COMMENT ON TABLE link_dimension_daily IS '按天、按维度的点击聚合，dimension 为 referrer、device、client（按 day 月分区）';
COMMENT ON TABLE link_visitors_daily IS '按天去重的访客哈希，每行 16 字节（按 day 月分区）';
COMMENT ON INDEX idx_link_clicks_daily_tenant_day_clicks IS '按租户和日期取今日 Top N';
