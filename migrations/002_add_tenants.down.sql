-- 索引先于列删除。先删列的话，依赖它的索引会一起消失，后面的 DROP INDEX 会失败。
DROP INDEX idx_links_tenant_id;
DROP INDEX idx_api_keys_tenant_id;

ALTER TABLE links DROP COLUMN tenant_id;

DROP TABLE api_keys;
DROP TABLE tenants;
