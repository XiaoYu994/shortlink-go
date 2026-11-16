package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shortlink/internal/link"
)

/*
管理接口的查询一律按 code + tenant_id 定位，并且不按 expires_at 过滤：
管理员能看到、修改和删除过期链接，跳转视角仍然只看有效链接。见 ADR 0011。

clicks_today 用左连接取 UTC 今天的行；没有点击时补 0。
左连接不会过滤掉链接，所以列表和详情仍然包含过期链接。
*/
const (
	adminLinkColumns = `l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id, l.clicks, COALESCE(d.clicks, 0)`
	todayJoin        = ` LEFT JOIN link_clicks_daily d ON d.link_id = l.id AND d.day = (now() AT TIME ZONE 'UTC')::date`

	/*
		第一页和后续页分开写 SQL，不把游标藏在 ($2 = 0 OR ...) 里。
		带 OR 的谓词不是 sargable，PostgreSQL 可能退化成从租户最新一行往下扫；
		单独的 id < $2 / 行比较才能用索引定位到游标。
	*/
	listLinksFirstByIDSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1
ORDER BY l.id DESC
LIMIT $2`

	listLinksAfterIDSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1 AND l.id < $2
ORDER BY l.id DESC
LIMIT $3`

	listLinksFirstByClicksSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1
ORDER BY l.clicks DESC, l.id DESC
LIMIT $2`

	listLinksAfterClicksSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1 AND (l.clicks, l.id) < ($2, $3)
ORDER BY l.clicks DESC, l.id DESC
LIMIT $4`

	listLinksFirstByClicksTodaySQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1
ORDER BY COALESCE(d.clicks, 0) DESC, l.id DESC
LIMIT $2`

	listLinksAfterClicksTodaySQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.tenant_id = $1 AND (COALESCE(d.clicks, 0), l.id) < ($2, $3)
ORDER BY COALESCE(d.clicks, 0) DESC, l.id DESC
LIMIT $4`

	getForTenantSQL = `SELECT ` + adminLinkColumns + `
FROM links l` + todayJoin + `
WHERE l.code = $1 AND l.tenant_id = $2`

	/*
		用 CASE 而不是读-改-写：没传的字段保持原值，UPDATE 本身是原子的。
		$3/$5 是「有没有传这个字段」，$4/$6 是新值；清空 expires_at 时 $6 是 NULL。
		RETURNING 只带 links 的列，clicks_today 在 CTE 外面补。
	*/
	updateLinkSQL = `WITH updated AS (
    UPDATE links
    SET url = CASE WHEN $3 THEN $4::text ELSE url END,
        expires_at = CASE WHEN $5 THEN $6::timestamptz ELSE expires_at END
    WHERE code = $1 AND tenant_id = $2
    RETURNING id, code, url, expires_at, created_at, tenant_id, clicks
)
SELECT u.id, u.code, u.url, u.expires_at, u.created_at, u.tenant_id, u.clicks, COALESCE(d.clicks, 0)
FROM updated u
LEFT JOIN link_clicks_daily d ON d.link_id = u.id AND d.day = (now() AT TIME ZONE 'UTC')::date`
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
	row := s.db.QueryRowContext(ctx, getForTenantSQL, code, tenantID)
	got, err := scanAdminLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get link for tenant: %w", err)
	}
	return got, nil
}

/*
List 返回租户的链接，包含已过期链接，按 opts.Sort 固定降序。
调用方要判断有没有下一页时把 Limit 多要一条；这里不额外查询总数。
排序字段会随点击变化，sort=clicks / clicks_today 的翻页可能重复或漏项，
默认 sort=id 保持稳定。见 ADR 0011。
*/
func (s *Store) List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("list links: limit must be positive")
	}

	var (
		rows   *sql.Rows
		err    error
		cursor link.ListCursor
	)
	if opts.Cursor != nil {
		cursor = *opts.Cursor
	}
	switch opts.Sort {
	case link.SortID:
		if opts.Cursor == nil {
			rows, err = s.db.QueryContext(ctx, listLinksFirstByIDSQL, opts.TenantID, opts.Limit)
		} else {
			rows, err = s.db.QueryContext(ctx, listLinksAfterIDSQL, opts.TenantID, cursor.ID, opts.Limit)
		}
	case link.SortClicks:
		if opts.Cursor == nil {
			rows, err = s.db.QueryContext(ctx, listLinksFirstByClicksSQL, opts.TenantID, opts.Limit)
		} else {
			rows, err = s.db.QueryContext(ctx, listLinksAfterClicksSQL,
				opts.TenantID, cursor.Clicks, cursor.ID, opts.Limit)
		}
	case link.SortClicksToday:
		if opts.Cursor == nil {
			rows, err = s.db.QueryContext(ctx, listLinksFirstByClicksTodaySQL, opts.TenantID, opts.Limit)
		} else {
			rows, err = s.db.QueryContext(ctx, listLinksAfterClicksTodaySQL,
				opts.TenantID, cursor.ClicksToday, cursor.ID, opts.Limit)
		}
	default:
		return nil, fmt.Errorf("list links: unknown sort %q", opts.Sort)
	}
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]link.AdminLink, 0, opts.Limit)
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

/*
Update 修改 URL 和/或 expires_at，返回修改后的链接。
链接不存在、已删除或属于其他租户都返回 link.ErrNotFound。
把 expires_at 改到将来可以让过期链接重新生效；只改 URL 时过期状态不变。
*/
func (s *Store) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	if u.Empty() {
		return nil, fmt.Errorf("update link: no fields to update")
	}

	var (
		hasURL     bool
		newURL     any
		hasExpires bool
		newExpiry  any
	)
	if u.URL != nil {
		hasURL = true
		newURL = *u.URL
	}
	if u.ClearExpiresAt {
		hasExpires = true
		newExpiry = nil
	} else if u.ExpiresAt != nil {
		hasExpires = true
		newExpiry = u.ExpiresAt.UTC()
	}

	row := s.db.QueryRowContext(ctx, updateLinkSQL,
		code, tenantID, hasURL, newURL, hasExpires, newExpiry)
	got, err := scanAdminLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}
	return got, nil
}

/*
Delete 硬删除链接和它的统计行，放在同一个事务里。

先 FOR UPDATE 锁住这一条链接，再按固定顺序删三张统计表：
link_clicks_daily、link_dimension_daily、link_visitors_daily。
RecordClicks 用同一顺序：先按 id 锁 links，再写这三张表。
顺序相反时，删除持有链接、点击持有日表行，两边互相等待，
PostgreSQL 会判死锁并回滚其中一方。
*/
func (s *Store) Delete(ctx context.Context, tenantID int64, code string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM links WHERE code = $1 AND tenant_id = $2 FOR UPDATE`, code, tenantID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return link.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}

	for _, table := range []string{"link_clicks_daily", "link_dimension_daily", "link_visitors_daily"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE link_id = $1`, id); err != nil {
			return fmt.Errorf("delete link stats: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	return nil
}

// scanAdminLink 读取链接和两个计数。DATE 列扫描成 time.Time，统一转成 UTC。
func scanAdminLink(scanner rowScanner) (*link.AdminLink, error) {
	var (
		got       link.AdminLink
		expiresAt sql.NullTime
	)
	if err := scanner.Scan(
		&got.ID, &got.Code, &got.URL, &expiresAt, &got.CreatedAt,
		&got.TenantID, &got.Clicks, &got.ClicksToday,
	); err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		got.ExpiresAt = &t
	}
	got.CreatedAt = got.CreatedAt.UTC()
	return &got, nil
}
