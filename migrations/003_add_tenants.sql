-- 租户和 API Key。管理 API 用密钥哈希换租户，跳转接口不鉴权。
-- 明文密钥不入库。expires_at 为空表示不过期，revoked_at 为空表示仍有效。
-- 鉴权不区分过期、吊销和密钥不存在。external_id 留给以后的平台租户，现在可以为空。
-- 已有链接挂到 legacy 租户上，避免 tenant_id 设成 NOT NULL 时失败。

CREATE TABLE tenants (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL,
    external_id TEXT UNIQUE
);

CREATE TABLE api_keys (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  BIGINT NOT NULL REFERENCES tenants (id),
    key_hash   BYTEA  NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

ALTER TABLE links ADD COLUMN tenant_id BIGINT;

INSERT INTO tenants (name)
SELECT 'legacy'
WHERE EXISTS (SELECT 1 FROM links WHERE tenant_id IS NULL);

UPDATE links
SET tenant_id = (SELECT id FROM tenants WHERE name = 'legacy')
WHERE tenant_id IS NULL;

ALTER TABLE links
    ALTER COLUMN tenant_id SET NOT NULL,
    ADD CONSTRAINT fk_links_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id);

COMMENT ON TABLE tenants IS '短链租户';
COMMENT ON COLUMN tenants.external_id IS '外部系统的租户编号，没有时为空';
COMMENT ON TABLE api_keys IS '租户的 API Key 哈希，不存明文';
COMMENT ON COLUMN api_keys.expires_at IS '为空表示不过期；到期时刻本身算过期';
COMMENT ON COLUMN api_keys.revoked_at IS '非空表示已吊销';
COMMENT ON COLUMN links.tenant_id IS '链接所属租户，由鉴权决定，不从请求 JSON 读取';
