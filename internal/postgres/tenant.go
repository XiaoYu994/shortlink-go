package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"shortlink/internal/audit"
)

/*
TenantLimits 是租户配额和限流的运维视图。
LinkUsage 是本自然月（UTC）已创建的链接数，删除不返还；LinkQuota 是每月上限，
RateLimit 是管理 API 每秒请求数，0 表示不限制。
*/
type TenantLimits struct {
	ID        int64
	Name      string
	LinkUsage int64
	LinkQuota int64
	RateLimit int
}

/*
SetTenantLimitsParams 只修改非 nil 的字段。
LinkQuota 调低到本月 LinkUsage 以下时，本月不能再创建，下月恢复；已有链接不受影响。
*/
type SetTenantLimitsParams struct {
	LinkQuota *int64
	RateLimit *int
}

var (
	// ErrNoLimitChange 表示既没给链接配额也没给速率限制。
	ErrNoLimitChange = errors.New("set tenant limits: provide link_quota or rate_limit")
	// ErrNegativeLimit 表示配额或速率限制是负数。
	ErrNegativeLimit = errors.New("set tenant limits: limits must not be negative")
)

const (
	/*
		SELECT 和 UPDATE ... RETURNING 都要回显「有效已用量」：跨月后、当月第一次
		创建前，直接读 link_usage 会看到上个月的数。UPDATE 的 RETURNING 里求值的是
		更新后的行，link_usage 和 link_usage_month 没有被 SET 改动，结果和 SELECT 一致。
	*/
	tenantLimitColumns = `id, name, (` + effectiveUsageSQL + `) AS link_usage, link_quota, rate_limit_per_second`

	getTenantLimitsSQL = `SELECT ` + tenantLimitColumns + ` FROM tenants WHERE id = $1`

	lockTenantLimitsSQL = `SELECT link_quota, rate_limit_per_second FROM tenants WHERE id = $1 FOR UPDATE`

	/*
		和 Update 链接一样用 CASE：没传的字段保持原值，UPDATE 本身是原子的。
		$2/$4 是「有没有传这个字段」，$3/$5 是新值。
	*/
	updateTenantLimitsSQL = `UPDATE tenants
SET link_quota = CASE WHEN $2 THEN $3::bigint ELSE link_quota END,
    rate_limit_per_second = CASE WHEN $4 THEN $5::integer ELSE rate_limit_per_second END
WHERE id = $1
RETURNING ` + tenantLimitColumns
)

/*
GetTenantLimits 读一个租户的配额和限流配置。
LinkUsage 用「有效已用量」：记录的月份早于当前月时返回 0，不返回上个月的数。
租户不存在返回 ErrTenantNotFound，避免把写错的 ID 看成全是默认值。
*/
func (s *Store) GetTenantLimits(ctx context.Context, tenantID int64) (TenantLimits, error) {
	if tenantID <= 0 {
		return TenantLimits{}, ErrTenantNotFound
	}
	if err := ctx.Err(); err != nil {
		return TenantLimits{}, err
	}
	got, err := scanTenantLimits(s.db.QueryRowContext(ctx, getTenantLimitsSQL, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return TenantLimits{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantLimits{}, fmt.Errorf("get tenant limits: %w", err)
	}
	return got, nil
}

/*
SetTenantLimits 改一个租户的链接配额和/或速率限制，返回修改后的值。
速率限制改完后，下一次鉴权读到的就是新值，限流中间件跟着调整，不用重启进程。
*/
func (s *Store) SetTenantLimits(ctx context.Context, tenantID int64, p SetTenantLimitsParams) (TenantLimits, error) {
	if tenantID <= 0 {
		return TenantLimits{}, ErrTenantNotFound
	}
	if p.LinkQuota == nil && p.RateLimit == nil {
		return TenantLimits{}, ErrNoLimitChange
	}
	if (p.LinkQuota != nil && *p.LinkQuota < 0) || (p.RateLimit != nil && *p.RateLimit < 0) {
		return TenantLimits{}, ErrNegativeLimit
	}
	if err := ctx.Err(); err != nil {
		return TenantLimits{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TenantLimits{}, fmt.Errorf("set tenant limits: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		prevQuota     int64
		prevRateLimit int
	)
	err = tx.QueryRowContext(ctx, lockTenantLimitsSQL, tenantID).Scan(&prevQuota, &prevRateLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantLimits{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantLimits{}, fmt.Errorf("set tenant limits: %w", err)
	}

	var quota, rate any
	if p.LinkQuota != nil {
		quota = *p.LinkQuota
	}
	if p.RateLimit != nil {
		rate = *p.RateLimit
	}
	got, err := scanTenantLimits(tx.QueryRowContext(ctx, updateTenantLimitsSQL,
		tenantID, p.LinkQuota != nil, quota, p.RateLimit != nil, rate))
	if errors.Is(err, sql.ErrNoRows) {
		return TenantLimits{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantLimits{}, fmt.Errorf("set tenant limits: %w", err)
	}

	auditMeta := map[string]any{}
	if p.LinkQuota != nil {
		auditMeta["previous_link_quota"] = prevQuota
		auditMeta["link_quota"] = got.LinkQuota
	}
	if p.RateLimit != nil {
		auditMeta["previous_rate_limit_per_second"] = prevRateLimit
		auditMeta["rate_limit_per_second"] = got.RateLimit
	}
	if err := insertAuditLogTx(ctx, tx, tenantID, audit.OperatorMeta(""), audit.ActionTenantLimitsUpdate, audit.TargetOrg, strconv.FormatInt(tenantID, 10), auditMeta); err != nil {
		return TenantLimits{}, fmt.Errorf("set tenant limits: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TenantLimits{}, fmt.Errorf("set tenant limits: commit: %w", err)
	}
	return got, nil
}

func scanTenantLimits(scanner rowScanner) (TenantLimits, error) {
	var got TenantLimits
	err := scanner.Scan(&got.ID, &got.Name, &got.LinkUsage, &got.LinkQuota, &got.RateLimit)
	return got, err
}
