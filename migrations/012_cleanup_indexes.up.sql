-- 周期清理索引加固（批次 1）：支撑 dead jobs 与过期/撤销/被取代 invitations 的分批索引清理，
-- 避免随时间积累膨胀，并保证清理 SQL 按索引走 O(limit) 路径（不使用 OR 退化全表扫）。
SET lock_timeout = '3s';

CREATE INDEX idx_jobs_finished_dead
    ON jobs (finished_at, id)
    WHERE status = 'dead';

CREATE INDEX idx_invitations_expires_at
    ON invitations (expires_at, id)
    WHERE expires_at IS NOT NULL;

CREATE INDEX idx_invitations_revoked_at
    ON invitations (revoked_at, id)
    WHERE revoked_at IS NOT NULL;

CREATE INDEX idx_invitations_superseded_at
    ON invitations (superseded_at, id)
    WHERE superseded_at IS NOT NULL;
