ALTER TABLE api_keys
    DROP CONSTRAINT ck_api_keys_hash_length;

ALTER TABLE links
    DROP COLUMN updated_at;

ALTER TABLE tenants
    DROP CONSTRAINT ck_tenants_name_length,
    DROP COLUMN created_at;

DROP INDEX idx_memberships_tenant_owners;

-- 回滚前把因过期重邀关闭的行回填到 revoked_at，避免重建 007 的部分唯一索引时冲突。
UPDATE invitations
SET revoked_at = superseded_at,
    superseded_at = NULL
WHERE superseded_at IS NOT NULL
  AND revoked_at IS NULL
  AND accepted_at IS NULL;

DROP INDEX idx_invitations_pending_email;
CREATE INDEX idx_invitations_pending_email
    ON invitations (email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

DROP INDEX uq_invitations_pending_tenant_email;
CREATE UNIQUE INDEX uq_invitations_pending_tenant_email
    ON invitations (tenant_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

ALTER TABLE invitations
    DROP CONSTRAINT ck_invitations_state,
    ADD CONSTRAINT ck_invitations_state CHECK (NOT (accepted_at IS NOT NULL AND revoked_at IS NOT NULL)),
    DROP COLUMN superseded_at;

ALTER TABLE jobs
    DROP CONSTRAINT ck_jobs_lock_version_nonnegative,
    DROP COLUMN lock_version;

DROP INDEX idx_jobs_pending_kind_run;
DROP INDEX idx_jobs_pending_tenant_run;
DROP INDEX idx_sessions_revoked_at;
