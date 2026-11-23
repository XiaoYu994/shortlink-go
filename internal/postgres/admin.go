package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shortlink/internal/link"
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
	adminLinkColumns = `l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id, l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, l.clicks, COALESCE(d.clicks, 0)`
	todayJoin        = ` LEFT JOIN link_clicks_daily d ON d.link_id = l.id AND d.day = (now() AT TIME ZONE 'UTC')::date`

	/*
		linkColumns 是 scanLink 读取的十列，顺序不能改。
		管理写的 RETURNING 共用这一份，避免几处各写一遍后和扫描顺序漂移。
	*/
	linkColumns = `id, code, url, expires_at, created_at, tenant_id, disabled_at, banned_at, ban_reason, ban_source`

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
		$3/$5/$7 是「有没有传这个字段」，$4/$6/$8 是新值；清空 expires_at 时 $6 是 NULL。
		重复停用保留第一次的时间；启用把 disabled_at 清成 NULL。
		WHERE 排除已封禁的行：被封禁的链接对租户只读，修改一律不落库。
		只有本次修改了 url（$3 为真）且更新成功时，才在同一条多 CTE 语句里入队异步风控（ADR 0020）；
		只改 expires_at 或 disabled 不入队。
		RETURNING 只带 links 的列，clicks_today 在 CTE 外面补。
	*/
	updateLinkSQL = `WITH updated AS (
    UPDATE links
    SET url = CASE WHEN $3 THEN $4::text ELSE url END,
        expires_at = CASE WHEN $5 THEN $6::timestamptz ELSE expires_at END,
        disabled_at = CASE WHEN NOT $7 THEN disabled_at
                           WHEN $8 THEN COALESCE(disabled_at, now())
                           ELSE NULL END
    WHERE code = $1 AND tenant_id = $2 AND banned_at IS NULL
    RETURNING ` + linkColumns + `, clicks
),
enqueued AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT 'link.risk_scan',
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('link_id', u.id, 'code', u.code, 'url', u.url)
    FROM updated u
    WHERE $3
)
SELECT u.id, u.code, u.url, u.expires_at, u.created_at, u.tenant_id,
       u.disabled_at, u.banned_at, u.ban_reason, u.ban_source,
       u.clicks, COALESCE(d.clicks, 0)
FROM updated u
LEFT JOIN link_clicks_daily d ON d.link_id = u.id AND d.day = (now() AT TIME ZONE 'UTC')::date`

	/*
		补查为什么没更新到行：短码是否存在、是否已封禁。
		这是 RevokeAPIKey 区分「不存在」和「已吊销」的同一个写法。
		tenant_id 仍然参与条件，其他租户的链接一律当不存在。
	*/
	updateLinkMissSQL = `SELECT banned_at IS NOT NULL FROM links WHERE code = $1 AND tenant_id = $2`

	/*
		Ban 按短码操作，不带租户。COALESCE 让重复封禁保留第一次的时间、
		原因和来源，命令可以放心重跑；要改原因先解封再封禁。
		RETURNING 要带完整状态，缓存层据此决定解封后写正缓存还是负缓存。
	*/
	banLinkSQL = `UPDATE links
SET banned_at = COALESCE(banned_at, now()),
    ban_reason = COALESCE(ban_reason, $2),
    ban_source = COALESCE(ban_source, $3)
WHERE code = $1
RETURNING ` + linkColumns

	/*
		banLinkIfURLSQL 给异步风控使用：仅当当前 URL 仍等于送审 URL 时才封禁（ADR 0020）。
		租户在扫描完成前把违规 URL 改成合规 URL 时，旧扫描任务不会误封新链接。
	*/
	banLinkIfURLSQL = `UPDATE links
SET banned_at = COALESCE(banned_at, now()),
    ban_reason = COALESCE(ban_reason, $2),
    ban_source = COALESCE(ban_source, $3)
WHERE code = $1 AND url = $4
RETURNING ` + linkColumns

	linkExistsByCodeSQL = `SELECT 1 FROM links WHERE code = $1`

	// Unban 把三个封禁字段一起清空。没被封禁的链接解封是空操作，仍然返回这一行。
	unbanLinkSQL = `UPDATE links
SET banned_at = NULL, ban_reason = NULL, ban_source = NULL
WHERE code = $1
RETURNING ` + linkColumns
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
	if u.Disabled != nil {
		hasDisabled = true
		disabledValue = *u.Disabled
	}

	row := s.db.QueryRowContext(ctx, updateLinkSQL,
		code, tenantID, hasURL, newURL, hasExpires, newExpiry, hasDisabled, disabledValue)
	got, err := scanAdminLink(row)
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("update link: %w", err)
	}

	// 没更新到行：区分不存在和已封禁，和 RevokeAPIKey 一样补查一次。
	var banned bool
	err = s.db.QueryRowContext(ctx, updateLinkMissSQL, code, tenantID).Scan(&banned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}
	if !banned {
		return nil, fmt.Errorf("update link: row was not updated")
	}
	return nil, link.ErrBanned
}

/*
Delete 硬删除链接和它的统计行，放在同一个事务里。
已封禁的链接返回 link.ErrBanned，不删任何东西：链接这一行就是封禁记录，
删掉之后违规 URL、原因和归属租户都没有依据。其他租户的链接仍是 link.ErrNotFound。

先 FOR UPDATE 锁住这一条链接并读出是否封禁，再按固定顺序删三张统计表：
link_clicks_daily、link_dimension_daily、link_visitors_daily。
RecordClicks 用同一顺序：先按 id 锁 links，再写这三张表。
顺序相反时，删除持有链接、点击持有日表行，两边互相等待，
PostgreSQL 会判死锁并回滚其中一方。

删除不返还本月配额，也不再更新 tenants 行（ADR 0015），
所以删除不会和同租户的创建争租户行锁。
*/
func (s *Store) Delete(ctx context.Context, tenantID int64, code string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id     int64
		banned bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, banned_at IS NOT NULL FROM links WHERE code = $1 AND tenant_id = $2 FOR UPDATE`,
		code, tenantID).Scan(&id, &banned)
	if errors.Is(err, sql.ErrNoRows) {
		return link.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if banned {
		return link.ErrBanned
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

	var row *sql.Row
	if expectedURL == "" {
		row = s.db.QueryRowContext(ctx, banLinkSQL, code, string(reason), string(source))
	} else {
		row = s.db.QueryRowContext(ctx, banLinkIfURLSQL, code, string(reason), string(source), expectedURL)
	}
	got, err := scanLink(row)
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	if expectedURL == "" {
		return nil, link.ErrNotFound
	}

	// 未命中时补查短码是否存在，区分已删除和 URL 已变更。
	var exists int
	err = s.db.QueryRowContext(ctx, linkExistsByCodeSQL, code).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	return nil, link.ErrURLChanged
}

/*
Unban 按短码解封，把三个封禁字段一起清空，不要求租户 ID。
没被封禁的链接解封是空操作，仍然返回这一行；短码不存在返回 link.ErrNotFound。
解封后链接可能仍然停用或过期，能不能跳转由调用方看 Servable。
*/
func (s *Store) Unban(ctx context.Context, code string) (*link.Link, error) {
	got, err := scanLink(s.db.QueryRowContext(ctx, unbanLinkSQL, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
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
