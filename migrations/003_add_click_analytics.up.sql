-- 点击日聚合（ADR 0012）。看板只需要日粒度的四个维度，不保留单次点击明细。
-- 三张表都不建 link_id 外键；删除链接时在应用层同一事务清理统计行（ADR 0010）。
-- day 是 UTC 日期，由应用按 Click.At.UTC() 计算，不依赖数据库会话时区。

CREATE TABLE link_clicks_daily (
    tenant_id BIGINT NOT NULL,
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    clicks    BIGINT NOT NULL,
    PRIMARY KEY (link_id, day),
    CONSTRAINT ck_link_clicks_daily_positive CHECK (clicks > 0)
);

-- 冗余 tenant_id 由写库时的 JOIN links 填充，服务按租户的今日 Top N 排序。
CREATE INDEX idx_link_clicks_daily_tenant_day_clicks
    ON link_clicks_daily (tenant_id, day, clicks DESC, link_id DESC);

CREATE TABLE link_dimension_daily (
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    dimension TEXT   NOT NULL,
    value     TEXT   NOT NULL,
    clicks    BIGINT NOT NULL,
    PRIMARY KEY (link_id, day, dimension, value),
    CONSTRAINT ck_link_dimension_daily_dimension
        CHECK (dimension IN ('referrer', 'device', 'client')),
    CONSTRAINT ck_link_dimension_daily_value_length
        CHECK (octet_length(value) BETWEEN 1 AND 255),
    CONSTRAINT ck_link_dimension_daily_positive CHECK (clicks > 0)
);

CREATE TABLE link_visitors_daily (
    link_id      BIGINT NOT NULL,
    day          DATE   NOT NULL,
    visitor_hash BYTEA  NOT NULL,
    PRIMARY KEY (link_id, day, visitor_hash),
    CONSTRAINT ck_link_visitors_daily_hash_length
        CHECK (octet_length(visitor_hash) = 16)
);

-- 管理列表按历史累计排序：links.clicks 是终身累计的读模型。
CREATE INDEX idx_links_tenant_clicks ON links (tenant_id, clicks DESC, id DESC);

COMMENT ON TABLE link_clicks_daily IS '每条链接每天的点击数';
COMMENT ON COLUMN link_clicks_daily.tenant_id IS '冗余列，写库时由 links 填充，服务按租户的今日排序';
COMMENT ON TABLE link_dimension_daily IS '按天、按维度的点击聚合，dimension 为 referrer、device、client';
COMMENT ON TABLE link_visitors_daily IS '按天去重的访客哈希，每行 16 字节';
COMMENT ON INDEX idx_link_clicks_daily_tenant_day_clicks IS '按租户和日期取今日 Top N';
COMMENT ON INDEX idx_links_tenant_clicks IS '管理列表按历史累计点击排序';
