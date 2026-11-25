-- 阶段 16A：PostgreSQL 行级安全（RLS）作为多租户隔离第二道防线（ADR 0028）。
-- 1. 幂等创建实例级受限非属主角色 shortlink_tenant（NOSUPERUSER NOBYPASSRLS），由应用在事务内 SET LOCAL ROLE 降权；
-- 2. 在 7 张含 tenant_id NOT NULL 的租户业务/控制表开启 ENABLE ROW LEVEL SECURITY（不加 FORCE，以免阻塞托管库非超管表属主在白名单跨租户路径的查询）；
-- 3. 策略采用零 OR 分支的 fail-closed 写法：漏设 app.tenant_id 时读返回 0 行、写抛 42501。

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'shortlink_tenant') THEN
        CREATE ROLE shortlink_tenant NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
    EXECUTE format('GRANT shortlink_tenant TO %I', current_user);
END $$;

GRANT USAGE ON SCHEMA public TO shortlink_tenant;

GRANT SELECT, INSERT, UPDATE, DELETE ON links, api_keys, memberships, invitations, webhooks TO shortlink_tenant;
GRANT SELECT, INSERT ON audit_logs, webhook_deliveries TO shortlink_tenant;
GRANT SELECT, UPDATE ON tenants TO shortlink_tenant;
GRANT SELECT ON users, link_clicks_daily, link_dimension_daily, link_visitors_daily TO shortlink_tenant;
GRANT INSERT, SELECT (id, kind, dedup_key, status) ON jobs TO shortlink_tenant;

ALTER TABLE links ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON links
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_keys
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invitations
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_logs
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE webhooks ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_deliveries
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);
