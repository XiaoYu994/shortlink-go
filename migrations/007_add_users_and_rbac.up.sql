-- 用户、OIDC 身份、组织成员关系、服务端会话、邀请，以及 API Key 权限范围（ADR 0018）。
-- 不建数据库外键与级联，引用完整性由应用层在事务内保证，引用列显式建索引（ADR 0010）。
-- 第一版不按邮箱自动关联不同提供方的账号，所以 users.email 不设唯一约束，身份以 (issuer, subject) 为准。

CREATE TABLE users (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      TEXT        NOT NULL DEFAULT '',
    name       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_users_email CHECK (
        octet_length(email) <= 320
        AND email = lower(email)
        AND btrim(email) = email
    ),
    CONSTRAINT ck_users_name_length CHECK (octet_length(name) <= 255)
);

CREATE TABLE user_identities (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT      NOT NULL,
    issuer         TEXT        NOT NULL,
    subject        TEXT        NOT NULL,
    email          TEXT        NOT NULL DEFAULT '',
    email_verified BOOLEAN     NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_user_identities_issuer_subject UNIQUE (issuer, subject),
    CONSTRAINT ck_user_identities_issuer_length CHECK (octet_length(issuer) BETWEEN 1 AND 512),
    CONSTRAINT ck_user_identities_subject_length CHECK (octet_length(subject) BETWEEN 1 AND 255),
    CONSTRAINT ck_user_identities_email CHECK (
        octet_length(email) <= 320
        AND email = lower(email)
        AND btrim(email) = email
    )
);

CREATE INDEX idx_user_identities_user_id ON user_identities (user_id, id);

CREATE TABLE memberships (
    user_id    BIGINT      NOT NULL,
    tenant_id  BIGINT      NOT NULL,
    role       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, tenant_id),
    CONSTRAINT ck_memberships_role CHECK (role IN ('owner', 'admin', 'member', 'viewer'))
);

CREATE INDEX idx_memberships_tenant_id ON memberships (tenant_id, user_id);

CREATE TABLE sessions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT      NOT NULL,
    token_hash BYTEA       NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT ck_sessions_token_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_sessions_expires_after_created CHECK (expires_at > created_at)
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id, id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE TABLE invitations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   BIGINT      NOT NULL,
    email       TEXT        NOT NULL,
    role        TEXT        NOT NULL,
    invited_by  BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    accepted_at TIMESTAMPTZ,
    accepted_by BIGINT,
    revoked_at  TIMESTAMPTZ,
    CONSTRAINT ck_invitations_email CHECK (
        octet_length(email) BETWEEN 1 AND 320
        AND email = lower(email)
        AND btrim(email) = email
    ),
    CONSTRAINT ck_invitations_role CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    CONSTRAINT ck_invitations_expires_after_created CHECK (expires_at IS NULL OR expires_at > created_at),
    CONSTRAINT ck_invitations_accepted_fields CHECK (num_nulls(accepted_at, accepted_by) IN (0, 2)),
    CONSTRAINT ck_invitations_state CHECK (NOT (accepted_at IS NOT NULL AND revoked_at IS NOT NULL))
);

-- 同一租户对同一邮箱只保留一条待兑现邀请；已兑现或已撤销后可再次邀请。
CREATE UNIQUE INDEX uq_invitations_pending_tenant_email
    ON invitations (tenant_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- 首次 OIDC 登录按已验证邮箱查找待兑现邀请。
CREATE INDEX idx_invitations_pending_email
    ON invitations (email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX idx_invitations_tenant_id ON invitations (tenant_id, id);

-- 存量密钥统一回填全部可用 scope；created_by 为空表示运维命令签发。
ALTER TABLE api_keys
    ADD COLUMN scopes     TEXT[] NOT NULL DEFAULT ARRAY['links:read', 'links:write', 'stats:read'],
    ADD COLUMN created_by BIGINT,
    ADD CONSTRAINT ck_api_keys_scopes_valid CHECK (
        cardinality(scopes) > 0
        AND array_position(scopes, NULL) IS NULL
        AND scopes <@ ARRAY['links:read', 'links:write', 'stats:read']::text[]
    );

CREATE INDEX idx_api_keys_created_by
    ON api_keys (created_by, id)
    WHERE created_by IS NOT NULL;

COMMENT ON TABLE users IS '平台用户主档；不按邮箱唯一约束合并账号';
COMMENT ON COLUMN users.email IS '最近同步的小写邮箱，可为空串';
COMMENT ON TABLE user_identities IS '用户绑定的外部 OIDC 身份，以 (issuer, subject) 唯一标识';
COMMENT ON TABLE memberships IS '用户与租户（组织）的多对多成员关系及角色';
COMMENT ON TABLE sessions IS '服务端会话，只存 token 的 SHA-256 哈希';
COMMENT ON COLUMN sessions.revoked_at IS '非空表示已主动登出或被吊销';
COMMENT ON TABLE invitations IS '组织成员邀请，由运维命令或管理员登记，首次 OIDC 登录按已验证邮箱兑现';
COMMENT ON COLUMN invitations.invited_by IS '发起邀请的用户编号；为空表示运维命令创建';
COMMENT ON COLUMN api_keys.scopes IS 'API Key 授权范围，存量密钥默认拥有全部链接和统计权限';
COMMENT ON COLUMN api_keys.created_by IS '签发密钥的用户编号；为空表示运维命令签发';
COMMENT ON INDEX uq_invitations_pending_tenant_email IS '防止同一租户给同一邮箱重复发出待兑现邀请';
COMMENT ON INDEX idx_invitations_pending_email IS '登录回调按已验证邮箱查找待兑现邀请';
COMMENT ON INDEX idx_sessions_expires_at IS '支撑过期会话定期清理';
