-- 短链表。表名按 PostgreSQL 习惯用复数 links。
-- 首次 make up 时由 PostgreSQL 容器自动执行（挂载到 /docker-entrypoint-initdb.d）。
-- 之后的表结构变更按编号新增 SQL 文件；正式迁移工具（goose 或 golang-migrate）以后再接入。

CREATE TABLE IF NOT EXISTS links (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       TEXT        NOT NULL UNIQUE,
    url        TEXT        NOT NULL,
    clicks     BIGINT      NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,              -- NULL 表示永不过期
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE links IS '短链映射表';
COMMENT ON COLUMN links.id IS '内部主键';
COMMENT ON COLUMN links.code IS '对外短码';
COMMENT ON COLUMN links.url IS '原始 URL';
COMMENT ON COLUMN links.clicks IS '点击次数';
COMMENT ON COLUMN links.expires_at IS '过期时间';
COMMENT ON COLUMN links.created_at IS '创建时间';

-- TODO(第 10 课): clicks 由 ClickRecorder 维护。
-- TODO: expires_at 的过期校验和缓存 TTL 联动还没实现。

-- id 只作内部主键；code 由应用随机生成。
