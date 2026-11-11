-- 短链表。
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

-- 短码由 id 做 Base62 编码得到，所以要先拿到 id 再写入 code：
--   SELECT nextval(pg_get_serial_sequence('links', 'id'));
--   INSERT INTO links (id, code, url) OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3);
