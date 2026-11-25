-- 回滚阶段 16A：撤销 RLS 策略并收回当前库对 shortlink_tenant 的授权。
-- PostgreSQL 角色属于实例级对象（与同实例其它库如开发库共享），此处刻意不 DROP ROLE，避免阻断 test-db 升降级验证。

DROP POLICY IF EXISTS tenant_isolation ON webhook_deliveries;
ALTER TABLE webhook_deliveries DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON webhooks;
ALTER TABLE webhooks DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON audit_logs;
ALTER TABLE audit_logs DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON invitations;
ALTER TABLE invitations DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON memberships;
ALTER TABLE memberships DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON api_keys;
ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON links;
ALTER TABLE links DISABLE ROW LEVEL SECURITY;

REVOKE ALL PRIVILEGES ON
    links, api_keys, memberships, invitations, audit_logs, webhooks, webhook_deliveries,
    tenants, users, link_clicks_daily, link_dimension_daily, link_visitors_daily, jobs
    FROM shortlink_tenant;
REVOKE USAGE ON SCHEMA public FROM shortlink_tenant;
