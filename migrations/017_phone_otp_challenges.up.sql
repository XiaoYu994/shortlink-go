-- 手机号短信验证码登录挑战表（ADR 0030）。
-- 属于登录前平台级认证表（不含 tenant_id），不授予 shortlink_tenant 权限也不挂租户 RLS。
-- 验证码绝不明文落库，仅存 16 字节随机盐与 HMAC-SHA256 摘要。

CREATE TABLE phone_otp_challenges (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    phone          TEXT        NOT NULL,
    code_salt      BYTEA       NOT NULL,
    code_hash      BYTEA       NOT NULL,
    attempts       INT         NOT NULL DEFAULT 0,
    max_attempts   INT         NOT NULL DEFAULT 5,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    CONSTRAINT ck_phone_otp_phone CHECK (
        octet_length(phone) BETWEEN 8 AND 16
        AND phone ~ '^\+[1-9][0-9]{6,14}$'
    ),
    CONSTRAINT ck_phone_otp_salt_len CHECK (octet_length(code_salt) = 16),
    CONSTRAINT ck_phone_otp_hash_len CHECK (octet_length(code_hash) = 32),
    CONSTRAINT ck_phone_otp_attempts CHECK (attempts >= 0 AND max_attempts > 0),
    CONSTRAINT ck_phone_otp_expires_after_created CHECK (expires_at > created_at),
    CONSTRAINT ck_phone_otp_terminal_state CHECK (NOT (consumed_at IS NOT NULL AND invalidated_at IS NOT NULL))
);

-- 同一手机号最多同时存在一条未消费且未作废的活跃挑战，防止并发双发留下多条有效验证码。
CREATE UNIQUE INDEX uq_phone_otp_active_phone
    ON phone_otp_challenges (phone)
    WHERE consumed_at IS NULL AND invalidated_at IS NULL;

-- 支撑单手机号发送冷却期与小时滑动窗口频控查询。
CREATE INDEX idx_phone_otp_phone_created_at
    ON phone_otp_challenges (phone, created_at DESC);

-- 支撑 console worker 按过期时间分批清理历史挑战记录（O(limit)）。
CREATE INDEX idx_phone_otp_expires_at
    ON phone_otp_challenges (expires_at);

COMMENT ON TABLE phone_otp_challenges IS '手机号短信验证码登录挑战记录（仅存随机盐与 HMAC-SHA256 摘要，不含 tenant_id）';
COMMENT ON COLUMN phone_otp_challenges.consumed_at IS '核验通过并消费的时间；非空后不可再次使用';
COMMENT ON COLUMN phone_otp_challenges.invalidated_at IS '被重发覆盖、过期扫描或达到最大试错次数作废的时间';
COMMENT ON INDEX uq_phone_otp_active_phone IS '保证同一手机号同时只有一条活跃验证码挑战';
COMMENT ON INDEX idx_phone_otp_phone_created_at IS '支撑发码冷却与小时频控窗口查询';
COMMENT ON INDEX idx_phone_otp_expires_at IS '支撑过期验证码挑战定期分批清理';
