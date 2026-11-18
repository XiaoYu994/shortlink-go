-- 链接状态：租户停用和平台封禁（ADR 0016）。过期仍然由 expires_at 算出来，不落状态。
-- 停用和封禁存时间戳而不是布尔值，和 api_keys.revoked_at 一致，顺带记下发生时间。
-- 封禁的三个字段同有同无：num_nulls 只能是 0（全有）或 3（全无），只填一部分会被拒绝。
-- 原因是封闭类别，因为会原样出现在租户的 API 响应里；来源只给平台看。
-- 不建索引：跳转按 code 的唯一索引找到行再过滤，列表不按状态筛选。

ALTER TABLE links
    ADD COLUMN disabled_at TIMESTAMPTZ,
    ADD COLUMN banned_at   TIMESTAMPTZ,
    ADD COLUMN ban_reason  TEXT,
    ADD COLUMN ban_source  TEXT,
    ADD CONSTRAINT ck_links_ban_fields CHECK (num_nulls(banned_at, ban_reason, ban_source) IN (0, 3)),
    ADD CONSTRAINT ck_links_ban_reason CHECK (ban_reason IN ('phishing', 'malware', 'adult', 'gambling', 'drugs', 'spam', 'other')),
    ADD CONSTRAINT ck_links_ban_source CHECK (ban_source IN ('operator', 'risk'));

COMMENT ON COLUMN links.disabled_at IS '租户停用的时间；为空表示没有停用';
COMMENT ON COLUMN links.banned_at IS '平台封禁的时间；为空表示没有封禁';
COMMENT ON COLUMN links.ban_reason IS '封禁原因类别，和 banned_at 同时有值';
COMMENT ON COLUMN links.ban_source IS '封禁来源：operator 人工，risk 风控；和 banned_at 同时有值';
