-- 阶段 15：租户 Webhook 订阅配置与投递明细表（ADR 0027）。
-- 沿用 ADR 0010：不建数据库外键与级联，引用完整性由应用层在事务内保证。
-- webhooks.consecutive_failures 不建索引，仅更新状态列时保留 PostgreSQL HOT Update 优化。

CREATE TABLE webhooks (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id            BIGINT      NOT NULL,
    url                  TEXT        NOT NULL,
    description          TEXT        NOT NULL DEFAULT '',
    events               TEXT[]      NOT NULL,
    secret_ciphertext    BYTEA       NOT NULL,
    secret_prefix        TEXT        NOT NULL,
    enabled              BOOLEAN     NOT NULL DEFAULT TRUE,
    consecutive_failures INT         NOT NULL DEFAULT 0,
    disabled_reason      TEXT        NOT NULL DEFAULT '',
    disabled_at          TIMESTAMPTZ,
    created_by           BIGINT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_webhooks_tenant_positive CHECK (tenant_id > 0),
    CONSTRAINT ck_webhooks_url_length CHECK (octet_length(url) BETWEEN 1 AND 2048),
    CONSTRAINT ck_webhooks_description_length CHECK (octet_length(description) <= 200),
    CONSTRAINT ck_webhooks_events_nonempty CHECK (cardinality(events) BETWEEN 1 AND 16),
    CONSTRAINT ck_webhooks_events_valid CHECK (
        events <@ ARRAY['link.created', 'link.updated', 'link.deleted', 'link.banned', 'link.unbanned']::text[]
    ),
    CONSTRAINT ck_webhooks_secret_ciphertext_len CHECK (octet_length(secret_ciphertext) >= 29),
    CONSTRAINT ck_webhooks_secret_prefix_len CHECK (octet_length(secret_prefix) BETWEEN 6 AND 24),
    CONSTRAINT ck_webhooks_consecutive_failures_nonneg CHECK (consecutive_failures >= 0),
    CONSTRAINT ck_webhooks_disabled_consistency CHECK (
        (enabled = TRUE AND disabled_reason = '' AND disabled_at IS NULL)
        OR (enabled = FALSE AND disabled_reason IN ('manual', 'consecutive_failures') AND disabled_at IS NOT NULL)
    ),
    CONSTRAINT ck_webhooks_created_by_positive CHECK (created_by IS NULL OR created_by > 0)
);

CREATE INDEX idx_webhooks_tenant_id
    ON webhooks (tenant_id, id DESC);

CREATE INDEX idx_webhooks_tenant_enabled
    ON webhooks (tenant_id)
    WHERE enabled = TRUE;

CREATE TABLE webhook_deliveries (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        BIGINT      NOT NULL,
    webhook_id       BIGINT      NOT NULL,
    job_id           BIGINT      NOT NULL,
    delivery_id      TEXT        NOT NULL,
    event            TEXT        NOT NULL,
    attempt          INT         NOT NULL,
    status           TEXT        NOT NULL,
    http_status      INT,
    duration_ms      INT         NOT NULL,
    error_message    TEXT        NOT NULL DEFAULT '',
    response_snippet TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_webhook_deliveries_tenant_positive CHECK (tenant_id > 0),
    CONSTRAINT ck_webhook_deliveries_webhook_positive CHECK (webhook_id > 0),
    CONSTRAINT ck_webhook_deliveries_job_positive CHECK (job_id > 0),
    CONSTRAINT ck_webhook_deliveries_delivery_id_len CHECK (octet_length(delivery_id) BETWEEN 1 AND 64),
    CONSTRAINT ck_webhook_deliveries_event_valid CHECK (
        event IN ('link.created', 'link.updated', 'link.deleted', 'link.banned', 'link.unbanned', 'webhook.ping')
    ),
    CONSTRAINT ck_webhook_deliveries_attempt_positive CHECK (attempt >= 1),
    CONSTRAINT ck_webhook_deliveries_status CHECK (status IN ('success', 'retry', 'failed')),
    CONSTRAINT ck_webhook_deliveries_http_status CHECK (http_status IS NULL OR (http_status BETWEEN 100 AND 599)),
    CONSTRAINT ck_webhook_deliveries_duration_nonneg CHECK (duration_ms >= 0),
    CONSTRAINT ck_webhook_deliveries_error_len CHECK (octet_length(error_message) <= 512),
    CONSTRAINT ck_webhook_deliveries_response_len CHECK (octet_length(response_snippet) <= 1024)
);

CREATE INDEX idx_webhook_deliveries_tenant_webhook_id
    ON webhook_deliveries (tenant_id, webhook_id, id DESC);

CREATE INDEX idx_webhook_deliveries_created_at_id
    ON webhook_deliveries (created_at, id);
