-- 已有库升级：给 links 补数据约束。
-- 新库由 001 建表后，PostgreSQL 初始化时也会按文件名顺序执行本文件。
-- code 格式和 internal/idgen 的 CodeLength、alphabet 保持一致；
-- 以后改短码格式时新增迁移，不能只改 Go 里的 ValidCode。
ALTER TABLE links
    ADD CONSTRAINT ck_links_code_format CHECK (code ~ '^[0-9A-Za-z]{8}$'),
    ADD CONSTRAINT ck_links_url_length CHECK (octet_length(url) BETWEEN 1 AND 2048),
    ADD CONSTRAINT ck_links_clicks_nonnegative CHECK (clicks >= 0);
