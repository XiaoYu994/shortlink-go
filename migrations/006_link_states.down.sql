-- 删列时三个 CHECK 约束一起消失。
ALTER TABLE links
    DROP COLUMN disabled_at,
    DROP COLUMN banned_at,
    DROP COLUMN ban_reason,
    DROP COLUMN ban_source;
