-- 回到这一版之前：日聚合表和累计排序索引都不存在。
DROP INDEX idx_links_tenant_clicks;

DROP TABLE link_visitors_daily;
DROP TABLE link_dimension_daily;
DROP TABLE link_clicks_daily;
