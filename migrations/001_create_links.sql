-- 短链表。表名按 PostgreSQL 习惯用复数 links。
-- 首次 make up 时由 PostgreSQL 容器自动执行（挂载到 /docker-entrypoint-initdb.d）。
-- 之后的表结构变更改用迁移工具（goose 或 golang-migrate）管理。

CREATE TABLE IF NOT EXISTS links (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       TEXT        NOT NULL UNIQUE,
    url        TEXT        NOT NULL,
    clicks     BIGINT      NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,              -- NULL 表示永不过期
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- id 由数据库自增，只作内部主键；对外短码由应用随机生成后写入 code。
