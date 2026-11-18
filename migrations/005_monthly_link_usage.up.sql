-- 链接配额从「当前拥有的链接数」改为「每个 UTC 自然月可创建的链接数」（ADR 0015）。
-- link_count 改名 link_usage，含义变成某个月已创建的链接数；link_usage_month 记录这个数属于哪个月。
-- 删除不再返还配额，用量没法再从 links 推算；回填只能数本月创建且还在的链接，少算的部分对租户有利。
-- link_usage_month 默认 1970-01-01 表示「还没用过」，比较时不用处理 NULL；CHECK 只允许每月 1 日。

ALTER TABLE tenants RENAME COLUMN link_count TO link_usage;
ALTER TABLE tenants RENAME CONSTRAINT tenants_link_count_nonnegative TO tenants_link_usage_nonnegative;
ALTER TABLE tenants
    ADD COLUMN link_usage_month DATE NOT NULL DEFAULT DATE '1970-01-01',
    ADD CONSTRAINT tenants_link_usage_month_first_day CHECK (EXTRACT(DAY FROM link_usage_month) = 1);

UPDATE tenants t
SET link_usage = (
        SELECT count(*) FROM links l
        WHERE l.tenant_id = t.id
          AND l.created_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
    ),
    link_usage_month = date_trunc('month', now() AT TIME ZONE 'UTC')::date;

COMMENT ON COLUMN tenants.link_usage IS '本月（UTC 自然月）已创建的链接数，删除不减；属于哪个月见 link_usage_month';
COMMENT ON COLUMN tenants.link_usage_month IS 'link_usage 所属月份的第一天（UTC）；早于当前月份时 link_usage 视为 0';
COMMENT ON COLUMN tenants.link_quota IS '租户每个自然月（UTC）最多能创建的链接数，0 表示不能创建';
