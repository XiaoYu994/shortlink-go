package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/webhook"
)

/*
管理接口的查询一律按 code + tenant_id 定位，并且不按 expires_at、disabled_at
或 banned_at 过滤：管理员能看到、修改和删除过期和停用的链接，被封禁的链接
照常返回但只能读。跳转视角仍然只看可跳转的链接。见 ADR 0011、ADR 0016。

clicks_today 用左连接取 UTC 今天的行；没有点击时补 0。
左连接不会过滤掉链接，所以列表和详情仍然包含过期链接。
封禁的三个字段同有同无，由数据库 CHECK 保证。
*/
const (
	adminLinkColumns     = `l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id, l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, l.clicks, COALESCE(d.clicks, 0)`
	zeroTodayLinkColumns = `l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id, l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, l.clicks, 0::bigint`
	todayJoin            = ` LEFT JOIN link_clicks_daily d ON d.link_id = l.id AND d.day = (now() AT TIME ZONE 'UTC')::date`

	/*
		linkColumns 是 scanLink 读取的十列，顺序不能改。
		管理写的 RETURNING 共用这一份，避免几处各写一遍后和扫描顺序漂移。
	*/
	linkColumns = `id, code, url, expires_at, created_at, tenant_id, disabled_at, banned_at, ban_reason, ban_source`

	/*
		statusFilter* 与 link.Link.Status(now) 的优先级严格一致（banned > disabled > expired > active），
		拆出独立条件避免用 OR 混合不同列破坏部分索引（迁移 014）与租户索引命中。
		按今日点击排序拆成两段有序索引扫描，消除旧版 LEFT JOIN + COALESCE 导致的整租户全量排序（ADR 0022）：
		1. 今日有点击段：以 link_clicks_daily 为驱动表，直接走 idx_link_clicks_daily_tenant_day_clicks；
		2. 今日 0 点击段：仅在第一段不足 limit 或游标已落入 clicks_today = 0 时，沿 idx_links_tenant_id 逆序补齐。
	*/
	statusFilterBannedSQL   = ` AND l.banned_at IS NOT NULL`
	statusFilterDisabledSQL = ` AND l.banned_at IS NULL AND l.disabled_at IS NOT NULL`
	statusFilterExpiredSQL  = ` AND l.banned_at IS NULL AND l.disabled_at IS NULL AND l.expires_at IS NOT NULL AND l.expires_at <= now()`
	statusFilterActiveSQL   = ` AND l.banned_at IS NULL AND l.disabled_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > now())`

	listLinksBaseSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1`

	listClickedTodayBaseSQL = `SELECT ` + adminLinkColumns + `
FROM link_clicks_daily d
JOIN links l ON l.id = d.link_id
WHERE d.tenant_id = $1
  AND d.day = (now() AT TIME ZONE 'UTC')::date
  AND l.tenant_id = $1`

	listUnclickedTodayBaseSQL = `SELECT ` + zeroTodayLinkColumns + `
FROM links l
WHERE l.tenant_id = $1`

	notExistsClickedTodaySQL = `
  AND NOT EXISTS (
      SELECT 1
      FROM link_clicks_daily d
      WHERE d.link_id = l.id
        AND d.day = (now() AT TIME ZONE 'UTC')::date
  )`

	getForTenantSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.code = $1 AND l.tenant_id = $2`

	/*
		用 CASE 而不是读-改-写：没传的字段保持原值；$1/$3/$5 表示是否传了对应字段，$2/$4/$6 是新值。
		清空 expires_at 时 $4 为 NULL；重复停用保留第一次的时间，启用把 disabled_at 清成 NULL。
		调用方先在事务内 FOR UPDATE 校验归属与封禁状态，再执行单表更新并依次入队任务、扇出 Webhook 与写审计。
	*/
	lockLinkForTenantUpdateSQL = `SELECT id, (banned_at IS NOT NULL)
FROM links
WHERE code = $1 AND tenant_id = $2
FOR UPDATE`

	updateLinkRowSQL = `UPDATE links
SET url = CASE WHEN $1 THEN $2::text ELSE url END,
    expires_at = CASE WHEN $3 THEN $4::timestamptz ELSE expires_at END,
    disabled_at = CASE WHEN NOT $5 THEN disabled_at
                       WHEN $6 THEN COALESCE(disabled_at, now())
                       ELSE NULL END,
    updated_at = now()
WHERE id = $7
RETURNING ` + linkColumns + `, clicks, updated_at`

	selectTodayLinkClicksSQL = `SELECT COALESCE((
    SELECT clicks
    FROM link_clicks_daily
    WHERE link_id = $1
      AND day = (now() AT TIME ZONE 'UTC')::date
), 0)`

	// Ban/Unban 按短码操作，不带租户；先 FOR UPDATE 锁主行再决定是否更新与写副作用。
	lockLinkByCodeForUpdateSQL = `SELECT ` + linkColumns + `
FROM links
WHERE code = $1
FOR UPDATE`

	banLinkRowSQL = `UPDATE links
SET banned_at = now(),
    ban_reason = $1,
    ban_source = $2,
    updated_at = now()
WHERE id = $3
RETURNING ` + linkColumns

	unbanLinkRowSQL = `UPDATE links
SET banned_at = NULL,
    ban_reason = NULL,
    ban_source = NULL,
    updated_at = now()
WHERE id = $1
RETURNING ` + linkColumns + `, updated_at`

	/*
		按完整主键元组分批删除统计行，严禁使用 ctid：
		三张日表转为范围分区表后 ctid 跨分区不唯一（ADR 0025、ADR 0026）。
	*/
	purgeClicksDailyBatchSQL = `DELETE FROM link_clicks_daily
WHERE (link_id, day) IN (
    SELECT link_id, day
    FROM link_clicks_daily
    WHERE link_id = $1
    LIMIT $2
)`

	purgeDimensionDailyBatchSQL = `DELETE FROM link_dimension_daily
WHERE (link_id, day, dimension, value) IN (
    SELECT link_id, day, dimension, value
    FROM link_dimension_daily
    WHERE link_id = $1
    LIMIT $2
)`

	purgeVisitorsDailyBatchSQL = `DELETE FROM link_visitors_daily
WHERE (link_id, day, visitor_hash) IN (
    SELECT link_id, day, visitor_hash
    FROM link_visitors_daily
    WHERE link_id = $1
    LIMIT $2
)`
)

// rowScanner 让 QueryRow 和 Rows 共用一段扫描逻辑。
type rowScanner interface {
	Scan(dest ...any) error
}

/*
GetForTenant 按短码和租户查一条链接，包含已过期链接。
不存在和其他租户的链接都返回 link.ErrNotFound，不透露链接是否存在。
*/
func (s *Store) GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error) {
	if tenantID <= 0 {
		return nil, link.ErrNotFound
	}
	var got *link.AdminLink
	err := s.withTenantReadTx(ctx, tenantID, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, getForTenantSQL, code, tenantID)
		scanned, scanErr := scanAdminLink(row)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return link.ErrNotFound
		}
		if scanErr != nil {
			return fmt.Errorf("get link for tenant: %w", scanErr)
		}
		got = scanned
		return nil
	})
	if err != nil {
		return nil, err
	}
	return got, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func appendLinkFilters(b *strings.Builder, args *[]any, status link.Status, query string) error {
	if status != "" {
		switch status {
		case link.StatusBanned:
			b.WriteString(statusFilterBannedSQL)
		case link.StatusDisabled:
			b.WriteString(statusFilterDisabledSQL)
		case link.StatusExpired:
			b.WriteString(statusFilterExpiredSQL)
		case link.StatusActive:
			b.WriteString(statusFilterActiveSQL)
		default:
			return fmt.Errorf("list links: invalid status %q", status)
		}
	}
	if query != "" {
		*args = append(*args, "%"+likeEscaper.Replace(query)+"%")
		idx := len(*args)
		fmt.Fprintf(b, ` AND (l.code ILIKE $%d ESCAPE '\' OR l.url ILIKE $%d ESCAPE '\')`, idx, idx)
	}
	return nil
}

func buildListLinksQuery(sort link.Sort, cursor *link.ListCursor, tenantID int64, limit int, status link.Status, query string) (string, []any, error) {
	var b strings.Builder
	var args []any
	var orderBy string

	switch sort {
	case link.SortID:
		b.WriteString(listLinksBaseSQL)
		args = []any{tenantID}
		if cursor != nil {
			args = append(args, cursor.ID)
			fmt.Fprintf(&b, " AND l.id < $%d", len(args))
		}
		orderBy = "\nORDER BY l.id DESC\nLIMIT $%d"
	case link.SortClicks:
		b.WriteString(listLinksBaseSQL)
		args = []any{tenantID}
		if cursor != nil {
			args = append(args, cursor.Clicks, cursor.ID)
			fmt.Fprintf(&b, " AND (l.clicks, l.id) < ($%d, $%d)", len(args)-1, len(args))
		}
		orderBy = "\nORDER BY l.clicks DESC, l.id DESC\nLIMIT $%d"
	default:
		return "", nil, fmt.Errorf("list links: unknown sort %q", sort)
	}

	if err := appendLinkFilters(&b, &args, status, query); err != nil {
		return "", nil, err
	}
	args = append(args, limit)
	fmt.Fprintf(&b, orderBy, len(args))
	return b.String(), args, nil
}

func buildClickedTodayQuery(cursor *link.ListCursor, tenantID int64, limit int, status link.Status, query string) (string, []any, error) {
	var b strings.Builder
	b.WriteString(listClickedTodayBaseSQL)
	args := []any{tenantID}
	if cursor != nil {
		args = append(args, cursor.ClicksToday, cursor.ID)
		fmt.Fprintf(&b, "\n  AND (d.clicks, d.link_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	if err := appendLinkFilters(&b, &args, status, query); err != nil {
		return "", nil, err
	}
	args = append(args, limit)
	fmt.Fprintf(&b, "\nORDER BY d.clicks DESC, d.link_id DESC\nLIMIT $%d", len(args))
	return b.String(), args, nil
}

func buildUnclickedTodayQuery(afterID *int64, tenantID int64, limit int, status link.Status, query string) (string, []any, error) {
	var b strings.Builder
	b.WriteString(listUnclickedTodayBaseSQL)
	args := []any{tenantID}
	if afterID != nil {
		args = append(args, *afterID)
		fmt.Fprintf(&b, "\n  AND l.id < $%d", len(args))
	}
	b.WriteString(notExistsClickedTodaySQL)
	if err := appendLinkFilters(&b, &args, status, query); err != nil {
		return "", nil, err
	}
	args = append(args, limit)
	fmt.Fprintf(&b, "\nORDER BY l.id DESC\nLIMIT $%d", len(args))
	return b.String(), args, nil
}

/*
List 返回租户的链接，包含已过期链接，按 opts.Sort 固定降序，并支持按状态和关键词筛选。
调用方要判断有没有下一页时把 Limit 多要一条；这里不额外查询总数。
排序字段会随点击变化，sort=clicks / clicks_today 的翻页可能重复或漏项，
默认 sort=id 保持稳定。见 ADR 0011。
*/
func (s *Store) List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("list links: limit must be positive")
	}
	if opts.Status != "" && !opts.Status.Valid() {
		return nil, fmt.Errorf("list links: invalid status %q", opts.Status)
	}
	if opts.TenantID <= 0 {
		return []link.AdminLink{}, nil
	}

	if opts.Sort == link.SortClicksToday {
		return s.listByClicksToday(ctx, opts)
	}

	query, args, err := buildListLinksQuery(opts.Sort, opts.Cursor, opts.TenantID, opts.Limit, opts.Status, opts.Query)
	if err != nil {
		return nil, err
	}
	var out []link.AdminLink
	if err := s.withTenantReadTx(ctx, opts.TenantID, func(tx *sql.Tx) error {
		items, qErr := queryAdminLinks(ctx, tx, opts.Limit, query, args...)
		if qErr != nil {
			return qErr
		}
		out = items
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) listByClicksToday(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	// 游标已经落入今日 0 点击段时，跳过 link_clicks_daily 直接按 links (tenant_id, id DESC) 翻页。
	if opts.Cursor != nil && opts.Cursor.ClicksToday <= 0 {
		afterID := opts.Cursor.ID
		query, args, err := buildUnclickedTodayQuery(&afterID, opts.TenantID, opts.Limit, opts.Status, opts.Query)
		if err != nil {
			return nil, err
		}
		var out []link.AdminLink
		if err := s.withTenantReadTx(ctx, opts.TenantID, func(tx *sql.Tx) error {
			items, qErr := queryAdminLinks(ctx, tx, opts.Limit, query, args...)
			if qErr != nil {
				return qErr
			}
			out = items
			return nil
		}); err != nil {
			return nil, err
		}
		return out, nil
	}

	var result []link.AdminLink
	err := s.withTenantTx(ctx, opts.TenantID, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		query, args, err := buildClickedTodayQuery(opts.Cursor, opts.TenantID, opts.Limit, opts.Status, opts.Query)
		if err != nil {
			return err
		}
		items, err := queryAdminLinks(ctx, tx, opts.Limit, query, args...)
		if err != nil {
			return err
		}
		result = items

		if remaining := opts.Limit - len(result); remaining > 0 {
			unclickedQuery, unclickedArgs, err := buildUnclickedTodayQuery(nil, opts.TenantID, remaining, opts.Status, opts.Query)
			if err != nil {
				return err
			}
			more, err := queryAdminLinks(ctx, tx, remaining, unclickedQuery, unclickedArgs...)
			if err != nil {
				return err
			}
			result = append(result, more...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type queryContexter interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryAdminLinks(ctx context.Context, q queryContexter, capHint int, query string, args ...any) ([]link.AdminLink, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAdminLinks(rows, capHint)
}

func scanAdminLinks(rows *sql.Rows, capHint int) ([]link.AdminLink, error) {
	result := make([]link.AdminLink, 0, capHint)
	for rows.Next() {
		got, err := scanAdminLink(rows)
		if err != nil {
			return nil, fmt.Errorf("list links: %w", err)
		}
		result = append(result, *got)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	return result, nil
}

type cacheReconcilePayload struct {
	Code string `json:"code"`
}

type linkPurgeStatsPayload struct {
	LinkID int64 `json:"link_id"`
}

type linkUpdatedWebhookData struct {
	LinkID        int64      `json:"link_id"`
	Code          string     `json:"code"`
	URL           string     `json:"url"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	DisabledAt    *time.Time `json:"disabled_at,omitempty"`
	ChangedFields []string   `json:"changed_fields"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type linkDeletedWebhookData struct {
	LinkID    int64     `json:"link_id"`
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	DeletedAt time.Time `json:"deleted_at"`
}

type linkBannedWebhookData struct {
	LinkID    int64          `json:"link_id"`
	Code      string         `json:"code"`
	URL       string         `json:"url"`
	BannedAt  time.Time      `json:"banned_at"`
	BanReason link.BanReason `json:"ban_reason"`
	BanSource link.BanSource `json:"ban_source"`
}

type linkUnbannedWebhookData struct {
	LinkID         int64          `json:"link_id"`
	Code           string         `json:"code"`
	URL            string         `json:"url"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	DisabledAt     *time.Time     `json:"disabled_at,omitempty"`
	PreviousReason link.BanReason `json:"previous_reason,omitempty"`
	PreviousSource link.BanSource `json:"previous_source,omitempty"`
	UnbannedAt     time.Time      `json:"unbanned_at"`
}

/*
Update 修改 URL、expires_at 和/或停用状态，返回修改后的链接。
链接不存在、已删除或属于其他租户都返回 link.ErrNotFound；已封禁返回 link.ErrBanned，
不落任何修改。停用的链接仍然可以改 URL 和过期时间。
把 expires_at 改到将来可以让过期链接重新生效；只改 URL 时过期状态不变。
*/
func (s *Store) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	if u.Empty() {
		return nil, fmt.Errorf("update link: no fields to update")
	}

	var (
		hasURL        bool
		newURL        any
		hasExpires    bool
		newExpiry     any
		hasDisabled   bool
		disabledValue bool
	)
	metaFields := make([]string, 0, 3)
	auditMeta := map[string]any{}
	if u.URL != nil {
		hasURL = true
		newURL = *u.URL
		metaFields = append(metaFields, "url")
		auditMeta["url"] = *u.URL
	}
	if u.ClearExpiresAt {
		hasExpires = true
		newExpiry = nil
		metaFields = append(metaFields, "expires_at")
		auditMeta["clear_expires_at"] = true
	} else if u.ExpiresAt != nil {
		hasExpires = true
		newExpiry = u.ExpiresAt.UTC()
		metaFields = append(metaFields, "expires_at")
		auditMeta["expires_at"] = u.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if u.Disabled != nil {
		hasDisabled = true
		disabledValue = *u.Disabled
		metaFields = append(metaFields, "disabled")
		auditMeta["disabled"] = disabledValue
	}
	auditMeta["fields"] = metaFields
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}
	if tenantID <= 0 {
		return nil, link.ErrNotFound
	}

	var got link.AdminLink
	err := s.withTenantTx(ctx, tenantID, nil, func(tx *sql.Tx) error {
		var (
			id       int64
			isBanned bool
		)
		err := tx.QueryRowContext(ctx, lockLinkForTenantUpdateSQL, code, tenantID).Scan(&id, &isBanned)
		if errors.Is(err, sql.ErrNoRows) {
			return link.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		if isBanned {
			return link.ErrBanned
		}

		var updatedAt time.Time
		base, err := scanLink(tx.QueryRowContext(ctx, updateLinkRowSQL,
			hasURL, newURL, hasExpires, newExpiry, hasDisabled, disabledValue, id,
		), &got.Clicks, &updatedAt)
		if err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		updatedAt = updatedAt.UTC()

		if err := tx.QueryRowContext(ctx, selectTodayLinkClicksSQL, id).Scan(&got.ClicksToday); err != nil {
			return fmt.Errorf("update link: read today clicks: %w", err)
		}
		if hasURL {
			if err := insertJobTx(ctx, tx, job.KindLinkRiskScan, tenantID, linkRiskScanPayload{
				LinkID: base.ID,
				Code:   base.Code,
				URL:    base.URL,
			}); err != nil {
				return fmt.Errorf("update link: %w", err)
			}
		}
		if err := insertJobTx(ctx, tx, job.KindCacheReconcile, tenantID, cacheReconcilePayload{
			Code: base.Code,
		}); err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		if err := enqueueWebhookEventTx(ctx, tx, tenantID, webhook.EventLinkUpdated, updatedAt, linkUpdatedWebhookData{
			LinkID:        base.ID,
			Code:          base.Code,
			URL:           base.URL,
			ExpiresAt:     base.ExpiresAt,
			DisabledAt:    base.DisabledAt,
			ChangedFields: metaFields,
			UpdatedAt:     updatedAt,
		}); err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionLinkUpdate, audit.TargetLink, base.Code, auditMeta); err != nil {
			return fmt.Errorf("update link: %w", err)
		}
		got.Link = *base
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &got, nil
}

/*
Delete 在事务内先锁并删除 links 主行、写审计日志，并同事务入队 cache.reconcile 与 link.purge_stats。
三张日统计表的残留行由 worker 分批异步回收，避免热点短链删表拉长数据库行锁（ADR 0025）。
已封禁的链接返回 link.ErrBanned，不删任何东西：链接这一行就是封禁记录，
删掉之后违规 URL、原因和归属租户都没有依据。其他租户的链接仍是 link.ErrNotFound。
*/
func (s *Store) Delete(ctx context.Context, tenantID int64, code string) error {
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if tenantID <= 0 {
		return link.ErrNotFound
	}
	return s.withTenantTx(ctx, tenantID, nil, func(tx *sql.Tx) error {
		var (
			id     int64
			urlStr string
			banned bool
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, url, banned_at IS NOT NULL FROM links WHERE code = $1 AND tenant_id = $2 FOR UPDATE`,
			code, tenantID).Scan(&id, &urlStr, &banned)
		if errors.Is(err, sql.ErrNoRows) {
			return link.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		if banned {
			return link.ErrBanned
		}

		var deletedAt time.Time
		if err := tx.QueryRowContext(ctx, `DELETE FROM links WHERE id = $1 RETURNING now()`, id).Scan(&deletedAt); err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		deletedAt = deletedAt.UTC()

		if err := insertJobTx(ctx, tx, job.KindCacheReconcile, tenantID, cacheReconcilePayload{
			Code: code,
		}); err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		if err := insertJobTx(ctx, tx, job.KindLinkPurgeStats, tenantID, linkPurgeStatsPayload{
			LinkID: id,
		}); err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		if err := enqueueWebhookEventTx(ctx, tx, tenantID, webhook.EventLinkDeleted, deletedAt, linkDeletedWebhookData{
			LinkID:    id,
			Code:      code,
			URL:       urlStr,
			DeletedAt: deletedAt,
		}); err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionLinkDelete, audit.TargetLink, code, map[string]any{
			"link_id": id,
			"url":     urlStr,
		}); err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		return nil
	})
}

// PurgeLinkStatsBatch 按完整主键元组分批清理已删除链接的三张日统计表残留行。
func (s *Store) PurgeLinkStatsBatch(ctx context.Context, linkID int64, limit int) (int64, error) {
	if linkID <= 0 || limit <= 0 {
		return 0, nil
	}
	var deleted int64
	for _, query := range []string{purgeClicksDailyBatchSQL, purgeDimensionDailyBatchSQL, purgeVisitorsDailyBatchSQL} {
		remaining := limit - int(deleted)
		if remaining <= 0 {
			break
		}
		res, err := s.db.ExecContext(ctx, query, linkID, remaining)
		if err != nil {
			return deleted, fmt.Errorf("purge link stats: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("purge link stats rows: %w", err)
		}
		deleted += n
	}
	return deleted, nil
}

/*
Ban 按短码封禁一条链接，不要求租户 ID：平台封禁的是短码指向的内容。
reason 和 source 先在 Go 里校验，非法值不落库；数据库 CHECK 兜底。
已封禁的链接再封一次，保留第一次的时间、原因和来源，照样返回这一行。
短码不存在返回 link.ErrNotFound。
返回的 Link 带完整状态（ExpiresAt、DisabledAt、Ban），
缓存层据此决定解封后写正缓存还是负缓存。
*/
func (s *Store) Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	return s.ban(ctx, code, "", reason, source)
}

/*
BanIfURL 仅当链接当前 URL 仍等于 expectedURL 时封禁，用于异步风控防并发改写误封（ADR 0020）。
链接已删除返回 link.ErrNotFound；链接仍在但 URL 已变更返回 link.ErrURLChanged。
*/
func (s *Store) BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	if expectedURL == "" {
		return nil, errors.New("ban link: expected URL is required")
	}
	return s.ban(ctx, code, expectedURL, reason, source)
}

func (s *Store) ban(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	if !reason.Valid() {
		return nil, fmt.Errorf("ban link: invalid reason %q", reason)
	}
	if !source.Valid() {
		return nil, fmt.Errorf("ban link: invalid source %q", source)
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanLink(tx.QueryRowContext(ctx, lockLinkByCodeForUpdateSQL, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	if expectedURL != "" && existing.URL != expectedURL {
		return nil, link.ErrURLChanged
	}

	got := existing
	newlyBanned := existing.Ban == nil
	if newlyBanned {
		got, err = scanLink(tx.QueryRowContext(ctx, banLinkRowSQL, string(reason), string(source), existing.ID))
		if err != nil {
			return nil, fmt.Errorf("ban link: %w", err)
		}
	}

	if err := insertJobTx(ctx, tx, job.KindCacheReconcile, got.TenantID, cacheReconcilePayload{
		Code: got.Code,
	}); err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	if newlyBanned {
		if err := insertAuditLogTx(ctx, tx, got.TenantID, 0, audit.ActionLinkBan, audit.TargetLink, got.Code, map[string]any{
			"link_id": got.ID,
			"url":     got.URL,
			"reason":  string(got.Ban.Reason),
			"source":  string(got.Ban.Source),
		}); err != nil {
			return nil, fmt.Errorf("ban link: %w", err)
		}
		if err := enqueueWebhookEventTx(ctx, tx, got.TenantID, webhook.EventLinkBanned, got.Ban.At, linkBannedWebhookData{
			LinkID:    got.ID,
			Code:      got.Code,
			URL:       got.URL,
			BannedAt:  got.Ban.At,
			BanReason: got.Ban.Reason,
			BanSource: got.Ban.Source,
		}); err != nil {
			return nil, fmt.Errorf("ban link: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	return got, nil
}

/*
Unban 按短码解封，把三个封禁字段一起清空，不要求租户 ID。
没被封禁的链接解封是空操作，仍然返回这一行；短码不存在返回 link.ErrNotFound。
解封后链接可能仍然停用或过期，能不能跳转由调用方看 Servable。
*/
func (s *Store) Unban(ctx context.Context, code string) (*link.Link, error) {
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanLink(tx.QueryRowContext(ctx, lockLinkByCodeForUpdateSQL, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}

	got := existing
	var (
		wasBanned  bool
		prevReason link.BanReason
		prevSource link.BanSource
		unbannedAt time.Time
	)
	if existing.Ban != nil {
		wasBanned = true
		prevReason = existing.Ban.Reason
		prevSource = existing.Ban.Source
		got, err = scanLink(tx.QueryRowContext(ctx, unbanLinkRowSQL, existing.ID), &unbannedAt)
		if err != nil {
			return nil, fmt.Errorf("unban link: %w", err)
		}
		unbannedAt = unbannedAt.UTC()
	}

	if err := insertJobTx(ctx, tx, job.KindCacheReconcile, got.TenantID, cacheReconcilePayload{
		Code: got.Code,
	}); err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}
	if wasBanned {
		auditMeta := map[string]any{
			"link_id": got.ID,
			"url":     got.URL,
		}
		if prevReason != "" {
			auditMeta["previous_reason"] = string(prevReason)
		}
		if prevSource != "" {
			auditMeta["previous_source"] = string(prevSource)
		}
		if err := insertAuditLogTx(ctx, tx, got.TenantID, 0, audit.ActionLinkUnban, audit.TargetLink, got.Code, auditMeta); err != nil {
			return nil, fmt.Errorf("unban link: %w", err)
		}
		if err := enqueueWebhookEventTx(ctx, tx, got.TenantID, webhook.EventLinkUnbanned, unbannedAt, linkUnbannedWebhookData{
			LinkID:         got.ID,
			Code:           got.Code,
			URL:            got.URL,
			ExpiresAt:      got.ExpiresAt,
			DisabledAt:     got.DisabledAt,
			PreviousReason: prevReason,
			PreviousSource: prevSource,
			UnbannedAt:     unbannedAt,
		}); err != nil {
			return nil, fmt.Errorf("unban link: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}
	return got, nil
}

/*
scanLink 读取链接的十列：id、code、url、expires_at、created_at、tenant_id、
disabled_at、banned_at、ban_reason、ban_source。返回的 Link 带完整状态：
时间统一转成 UTC，封禁字段同有同无，以 banned_at 是否为空判断有没有封禁。
extra 是调用方追加在后面的列，例如管理列表的两个计数。
*/
func scanLink(scanner rowScanner, extra ...any) (*link.Link, error) {
	var (
		got        link.Link
		expiresAt  sql.NullTime
		disabledAt sql.NullTime
		bannedAt   sql.NullTime
		banReason  sql.NullString
		banSource  sql.NullString
	)
	dest := []any{
		&got.ID, &got.Code, &got.URL, &expiresAt, &got.CreatedAt, &got.TenantID,
		&disabledAt, &bannedAt, &banReason, &banSource,
	}
	dest = append(dest, extra...)
	if err := scanner.Scan(dest...); err != nil {
		return nil, err
	}
	got.ExpiresAt = nullTimeUTC(expiresAt)
	got.DisabledAt = nullTimeUTC(disabledAt)
	if bannedAt.Valid {
		got.Ban = &link.Ban{
			At:     bannedAt.Time.UTC(),
			Reason: link.BanReason(banReason.String),
			Source: link.BanSource(banSource.String),
		}
	}
	got.CreatedAt = got.CreatedAt.UTC()
	return &got, nil
}

// scanAdminLink 读取链接和两个计数。列顺序和 adminLinkColumns 一致。
func scanAdminLink(scanner rowScanner) (*link.AdminLink, error) {
	var got link.AdminLink
	base, err := scanLink(scanner, &got.Clicks, &got.ClicksToday)
	if err != nil {
		return nil, err
	}
	got.Link = *base
	return &got, nil
}
