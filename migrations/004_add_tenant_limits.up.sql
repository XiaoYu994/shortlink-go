-- 租户的链接配额和管理 API 限流。每租户独立配置，不用全局环境变量。
-- link_count 是租户当前拥有的链接数，创建时加一、删除时减一，和链接的增删在同一条语句或同一个事务里。
-- 迁移按 links 回填，已有链接都计入；过期链接仍然存在、仍然可管理，也占配额（ADR 0011）。
-- 默认值让已有租户和之后新建的租户一开始就有限制，运营用 console tenant set 调整。
-- rate_limit_per_second 为 0 表示不限制；进程内令牌桶的突发容量等于每秒速率。

ALTER TABLE tenants
    ADD COLUMN link_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN link_quota BIGINT NOT NULL DEFAULT 1000,
    ADD COLUMN rate_limit_per_second INTEGER NOT NULL DEFAULT 10;

UPDATE tenants t
SET link_count = (SELECT count(*) FROM links l WHERE l.tenant_id = t.id);

ALTER TABLE tenants
    ADD CONSTRAINT tenants_link_count_nonnegative CHECK (link_count >= 0),
    ADD CONSTRAINT tenants_link_quota_nonnegative CHECK (link_quota >= 0),
    ADD CONSTRAINT tenants_rate_limit_nonnegative CHECK (rate_limit_per_second >= 0);

COMMENT ON COLUMN tenants.link_count IS '租户当前拥有的链接数，含已过期链接；创建加一、删除减一';
COMMENT ON COLUMN tenants.link_quota IS '租户最多能拥有的链接数，0 表示不能再创建';
COMMENT ON COLUMN tenants.rate_limit_per_second IS '管理 API 每租户每秒请求数，0 表示不限制';
