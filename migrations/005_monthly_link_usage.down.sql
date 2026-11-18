-- 回到 ADR 0014 的存量口径：列名和约束名改回去，删掉月份列。
-- link_usage 变回 link_count 后按 links 重新数一遍，包含已过期链接。
-- 存量本来就能从 links 数出来，回滚后的计数是准的；丢掉的是本月用量和它所属的月份。

ALTER TABLE tenants DROP COLUMN link_usage_month;
ALTER TABLE tenants RENAME CONSTRAINT tenants_link_usage_nonnegative TO tenants_link_count_nonnegative;
ALTER TABLE tenants RENAME COLUMN link_usage TO link_count;
UPDATE tenants t
SET link_count = (SELECT count(*) FROM links l WHERE l.tenant_id = t.id);
COMMENT ON COLUMN tenants.link_count IS '租户当前拥有的链接数，含已过期链接；创建加一、删除减一';
COMMENT ON COLUMN tenants.link_quota IS '租户最多能拥有的链接数，0 表示不能再创建';
