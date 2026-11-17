-- 三列上的 CHECK 约束是单列约束，会随列一起删除。
ALTER TABLE tenants
    DROP COLUMN link_count,
    DROP COLUMN link_quota,
    DROP COLUMN rate_limit_per_second;
