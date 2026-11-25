-- API Key 元数据（name、last_used_at）与短链列表状态筛选索引（阶段 14 收口）。
-- 严禁给 api_keys.last_used_at 建索引：保留 PostgreSQL HOT Update 优化，避免热路径鉴权更新额外放大索引写入。

ALTER TABLE api_keys
    ADD COLUMN name         TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_used_at TIMESTAMPTZ,
    ADD CONSTRAINT ck_api_keys_name_length CHECK (octet_length(name) <= 100 AND btrim(name) = name);

-- 针对非活跃状态筛选的部分索引，条件与 link.Link.Status(now) 优先级严格对齐（banned > disabled > expired > active）。
CREATE INDEX idx_links_tenant_banned
    ON links (tenant_id, id DESC)
    WHERE banned_at IS NOT NULL;

CREATE INDEX idx_links_tenant_disabled
    ON links (tenant_id, id DESC)
    WHERE banned_at IS NULL AND disabled_at IS NOT NULL;

CREATE INDEX idx_links_tenant_expiring
    ON links (tenant_id, id DESC)
    WHERE banned_at IS NULL AND disabled_at IS NULL AND expires_at IS NOT NULL;

COMMENT ON COLUMN api_keys.name IS 'API Key 名称或备注，可选，默认为空串';
COMMENT ON COLUMN api_keys.last_used_at IS 'API Key 最近一次通过鉴权的时间（带 5 分钟节流更新）；不建索引以保留 HOT Update';
COMMENT ON INDEX idx_links_tenant_banned IS '按租户快速筛选已被封禁的短链';
COMMENT ON INDEX idx_links_tenant_disabled IS '按租户快速筛选已停用（且未封禁）的短链';
COMMENT ON INDEX idx_links_tenant_expiring IS '按租户快速筛选带过期时间的活跃候选短链（服务 status=expired 筛选）';
