-- 短链表。表名按 PostgreSQL 习惯用复数 links。
-- 版本由 schema_migrations 记录，所以不用 IF NOT EXISTS：
-- 建到一半失败时应该停住，而不是下次被当成已经完成。
-- code 格式和 internal/idgen 的 CodeLength、alphabet 保持一致。
-- 以后改短码格式时新增迁移，不能只改 Go 里的 ValidCode。

CREATE TABLE links (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       TEXT        NOT NULL UNIQUE,
    url        TEXT        NOT NULL,
    clicks     BIGINT      NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,              -- NULL 表示永不过期
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_links_code_format CHECK (code ~ '^[0-9A-Za-z]{8}$'),
    CONSTRAINT ck_links_url_length CHECK (octet_length(url) BETWEEN 1 AND 2048),
    CONSTRAINT ck_links_clicks_nonnegative CHECK (clicks >= 0)
);

COMMENT ON TABLE links IS '短链映射表';
COMMENT ON COLUMN links.id IS '内部主键';
COMMENT ON COLUMN links.code IS '对外短码';
COMMENT ON COLUMN links.url IS '原始 URL';
COMMENT ON COLUMN links.clicks IS '点击次数';
COMMENT ON COLUMN links.expires_at IS '过期时间';
COMMENT ON COLUMN links.created_at IS '创建时间';

-- clicks 由 ClickRecorder 异步批量维护，跳转请求里不直接 UPDATE。
-- 过期行留在表里。GetByCode 用 expires_at > now() 把它滤成找不到，缓存 TTL 取剩余寿命。
-- id 只作内部主键；code 由应用随机生成。
