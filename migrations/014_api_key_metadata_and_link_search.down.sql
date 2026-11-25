DROP INDEX IF EXISTS idx_links_tenant_expiring;
DROP INDEX IF EXISTS idx_links_tenant_disabled;
DROP INDEX IF EXISTS idx_links_tenant_banned;

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS ck_api_keys_name_length,
    DROP COLUMN IF EXISTS last_used_at,
    DROP COLUMN IF EXISTS name;
