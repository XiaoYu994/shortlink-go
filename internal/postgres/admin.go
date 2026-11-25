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
		用 CASE 而不是读-改-写：没传的字段保持原值，UPDATE 本身是原子的。
		$3/$5/$7 是「有没有传这个字段」，$4/$6/$8 是新值；清空 expires_at 时 $6 是 NULL。
		重复停用保留第一次的时间；启用把 disabled_at 清成 NULL。
		WHERE 排除已封禁的行：被封禁的链接对租户只读，修改一律不落库。
		只有本次修改了 url（$3 为真）且更新成功时，才在同一条多 CTE 语句里入队异步风控（ADR 0020）；
		只改 expires_at 或 disabled 不入队。
		RETURNING 只带 links 的列，clicks_today 在 CTE 外面补。
	*/
	updateLinkSQL = `WITH target AS (
    SELECT ` + linkColumns + `, clicks, (banned_at IS NOT NULL) AS is_banned
    FROM links
    WHERE code = $1 AND tenant_id = $2
    FOR UPDATE
),
updated AS (
    UPDATE links l
    SET url = CASE WHEN $3 THEN $4::text ELSE l.url END,
        expires_at = CASE WHEN $5 THEN $6::timestamptz ELSE l.expires_at END,
        disabled_at = CASE WHEN NOT $7 THEN l.disabled_at
                           WHEN $8 THEN COALESCE(l.disabled_at, now())
                           ELSE NULL END,
        updated_at = now()
    FROM target t
    WHERE l.id = t.id AND NOT t.is_banned
    RETURNING l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id,
              l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, l.clicks
),
enqueued AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $13::text,
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('link_id', u.id, 'code', u.code, 'url', u.url)
    FROM updated u
    WHERE $3
),
reconciled AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $16::text,
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('code', u.code)
    FROM updated u
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT u.tenant_id,
           $9::text,
           $10::bigint,
           $14::text,
           $15::text,
           u.code,
           $11::text,
           $12::jsonb
    FROM updated u
)
SELECT COALESCE(u.id, t.id),
       COALESCE(u.code, t.code),
       COALESCE(u.url, t.url),
       CASE WHEN u.id IS NOT NULL THEN u.expires_at ELSE t.expires_at END,
       COALESCE(u.created_at, t.created_at),
       COALESCE(u.tenant_id, t.tenant_id),
       CASE WHEN u.id IS NOT NULL THEN u.disabled_at ELSE t.disabled_at END,
       COALESCE(u.banned_at, t.banned_at),
       COALESCE(u.ban_reason, t.ban_reason),
       COALESCE(u.ban_source, t.ban_source),
       COALESCE(u.clicks, t.clicks),
       COALESCE(d.clicks, 0),
       t.is_banned
FROM target t
LEFT JOIN updated u ON u.id = t.id
LEFT JOIN link_clicks_daily d ON d.link_id = u.id AND d.day = (now() AT TIME ZONE 'UTC')::date`

	/*
		Ban 按短码操作，不带租户。COALESCE 让重复封禁保留第一次的时间、
		原因和来源，命令可以放心重跑；仅当从非封禁变为封禁时写入 link.ban 审计事件。
		RETURNING 要带完整状态，缓存层据此决定解封后写正缓存还是负缓存。
	*/
	banLinkSQL = `WITH prev AS (
    SELECT id, (banned_at IS NULL) AS newly_banned
    FROM links
    WHERE code = $1
    FOR UPDATE
),
updated AS (
    UPDATE links l
    SET banned_at = COALESCE(l.banned_at, now()),
        ban_reason = COALESCE(l.ban_reason, $2::text),
        ban_source = COALESCE(l.ban_source, $3::text),
        updated_at = CASE WHEN l.banned_at IS NULL THEN now() ELSE l.updated_at END
    FROM prev p
    WHERE l.id = p.id
    RETURNING l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id,
              l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, p.newly_banned
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT u.tenant_id,
           $4::text,
           $5::bigint,
           $7::text,
           $8::text,
           u.code,
           $6::text,
           jsonb_build_object(
               'link_id', u.id,
               'url', u.url,
               'reason', u.ban_reason,
               'source', u.ban_source
           )
    FROM updated u
    WHERE u.newly_banned
),
reconciled AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $9::text,
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('code', u.code)
    FROM updated u
)
SELECT ` + linkColumns + ` FROM updated`

	/*
		banLinkIfURLSQL 给异步风控使用：同一语句加 FOR UPDATE 锁并判断 URL 是否仍等于送审 URL（ADR 0020）。
		不存在返回零行（ErrNotFound），URL 已变更返回 url_matched=false（ErrURLChanged），
		消除先 UPDATE 再补查的并发窗口。
	*/
	banLinkIfURLSQL = `WITH target AS (
    SELECT ` + linkColumns + `, (url = $4) AS url_matched, (banned_at IS NULL) AS newly_banned
    FROM links
    WHERE code = $1
    FOR UPDATE
),
updated AS (
    UPDATE links l
    SET banned_at = COALESCE(l.banned_at, now()),
        ban_reason = COALESCE(l.ban_reason, $2::text),
        ban_source = COALESCE(l.ban_source, $3::text),
        updated_at = CASE WHEN l.banned_at IS NULL THEN now() ELSE l.updated_at END
    FROM target t
    WHERE l.id = t.id AND t.url_matched
    RETURNING l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id,
              l.disabled_at, l.banned_at, l.ban_reason, l.ban_source, t.newly_banned
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT u.tenant_id,
           $5::text,
           $6::bigint,
           $8::text,
           $9::text,
           u.code,
           $7::text,
           jsonb_build_object(
               'link_id', u.id,
               'url', u.url,
               'reason', u.ban_reason,
               'source', u.ban_source
           )
    FROM updated u
    WHERE u.newly_banned
),
reconciled AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $10::text,
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('code', u.code)
    FROM updated u
)
SELECT COALESCE(u.id, t.id),
       COALESCE(u.code, t.code),
       COALESCE(u.url, t.url),
       COALESCE(u.expires_at, t.expires_at),
       COALESCE(u.created_at, t.created_at),
       COALESCE(u.tenant_id, t.tenant_id),
       COALESCE(u.disabled_at, t.disabled_at),
       COALESCE(u.banned_at, t.banned_at),
       COALESCE(u.ban_reason, t.ban_reason),
       COALESCE(u.ban_source, t.ban_source),
       t.url_matched
FROM target t
LEFT JOIN updated u ON u.id = t.id`

	// Unban 把三个封禁字段一起清空。没被封禁的链接解封是空操作，不重复写入审计日志。
	unbanLinkSQL = `WITH prev AS (
    SELECT id, (banned_at IS NOT NULL) AS was_banned, ban_reason, ban_source
    FROM links
    WHERE code = $1
    FOR UPDATE
),
updated AS (
    UPDATE links l
    SET banned_at = NULL,
        ban_reason = NULL,
        ban_source = NULL,
        updated_at = CASE WHEN l.banned_at IS NOT NULL THEN now() ELSE l.updated_at END
    FROM prev p
    WHERE l.id = p.id
    RETURNING l.id, l.code, l.url, l.expires_at, l.created_at, l.tenant_id,
              l.disabled_at, l.banned_at, l.ban_reason, l.ban_source,
              p.was_banned, p.ban_reason AS previous_reason, p.ban_source AS previous_source
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT u.tenant_id,
           $2::text,
           $3::bigint,
           $5::text,
           $6::text,
           u.code,
           $4::text,
           jsonb_strip_nulls(jsonb_build_object(
               'link_id', u.id,
               'url', u.url,
               'previous_reason', u.previous_reason,
               'previous_source', u.previous_source
           ))
    FROM updated u
    WHERE u.was_banned
),
reconciled AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $7::text,
           NULLIF(u.tenant_id, 0),
           jsonb_build_object('code', u.code)
    FROM updated u
)
SELECT ` + linkColumns + ` FROM updated`

	enqueueDeleteJobsSQL = `INSERT INTO jobs (kind, tenant_id, payload)
VALUES ($1, NULLIF($2::bigint, 0), jsonb_build_object('code', $3::text)),
       ($4, NULLIF($2::bigint, 0), jsonb_build_object('link_id', $5::bigint))`

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

	if opts.Sort == link.SortClicksToday {
		return s.listByClicksToday(ctx, opts)
	}

	query, args, err := buildListLinksQuery(opts.Sort, opts.Cursor, opts.TenantID, opts.Limit, opts.Status, opts.Query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanAdminLinks(rows, opts.Limit)
}

func (s *Store) listByClicksToday(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	// 游标已经落入今日 0 点击段时，跳过 link_clicks_daily 直接按 links (tenant_id, id DESC) 翻页。
	if opts.Cursor != nil && opts.Cursor.ClicksToday <= 0 {
		afterID := opts.Cursor.ID
		query, args, err := buildUnclickedTodayQuery(&afterID, opts.TenantID, opts.Limit, opts.Status, opts.Query)
		if err != nil {
			return nil, err
		}
		return queryAdminLinks(ctx, s.db, opts.Limit, query, args...)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query, args, err := buildClickedTodayQuery(opts.Cursor, opts.TenantID, opts.Limit, opts.Status, opts.Query)
	if err != nil {
		return nil, err
	}
	result, err := queryAdminLinks(ctx, tx, opts.Limit, query, args...)
	if err != nil {
		return nil, err
	}

	if remaining := opts.Limit - len(result); remaining > 0 {
		unclickedQuery, unclickedArgs, err := buildUnclickedTodayQuery(nil, opts.TenantID, remaining, opts.Status, opts.Query)
		if err != nil {
			return nil, err
		}
		more, err := queryAdminLinks(ctx, tx, remaining, unclickedQuery, unclickedArgs...)
		if err != nil {
			return nil, err
		}
		result = append(result, more...)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("list links: %w", err)
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
	metaJSON, err := marshalAuditMetadata(auditMeta)
	if err != nil {
		return nil, fmt.Errorf("update link: marshal audit metadata: %w", err)
	}
	actorKind, actorID, reqID, err := requireAuditMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}

	row := s.db.QueryRowContext(ctx, updateLinkSQL,
		code, tenantID, hasURL, newURL, hasExpires, newExpiry, hasDisabled, disabledValue,
		actorKind, actorID, reqID, string(metaJSON),
		job.KindLinkRiskScan, string(audit.ActionLinkUpdate), string(audit.TargetLink),
		job.KindCacheReconcile)
	var (
		got      link.AdminLink
		isBanned bool
	)
	base, err := scanLink(row, &got.Clicks, &got.ClicksToday, &isBanned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}
	if isBanned {
		return nil, link.ErrBanned
	}
	got.Link = *base
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id     int64
		urlStr string
		banned bool
	)
	err = tx.QueryRowContext(ctx,
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

	if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if _, err := tx.ExecContext(ctx, enqueueDeleteJobsSQL,
		job.KindCacheReconcile, tenantID, code, job.KindLinkPurgeStats, id); err != nil {
		return fmt.Errorf("delete link: enqueue jobs: %w", err)
	}
	if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionLinkDelete, audit.TargetLink, code, map[string]any{
		"link_id": id,
		"url":     urlStr,
	}); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	return nil
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

	actorKind, actorID, reqID, err := requireAuditMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}

	if expectedURL == "" {
		got, err := scanLink(s.db.QueryRowContext(ctx, banLinkSQL,
			code, string(reason), string(source), actorKind, actorID, reqID,
			string(audit.ActionLinkBan), string(audit.TargetLink), job.KindCacheReconcile))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, link.ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("ban link: %w", err)
		}
		return got, nil
	}

	var urlMatched bool
	got, err := scanLink(s.db.QueryRowContext(ctx, banLinkIfURLSQL,
		code, string(reason), string(source), expectedURL, actorKind, actorID, reqID,
		string(audit.ActionLinkBan), string(audit.TargetLink), job.KindCacheReconcile), &urlMatched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ban link: %w", err)
	}
	if !urlMatched {
		return nil, link.ErrURLChanged
	}
	return got, nil
}

/*
Unban 按短码解封，把三个封禁字段一起清空，不要求租户 ID。
没被封禁的链接解封是空操作，仍然返回这一行；短码不存在返回 link.ErrNotFound。
解封后链接可能仍然停用或过期，能不能跳转由调用方看 Servable。
*/
func (s *Store) Unban(ctx context.Context, code string) (*link.Link, error) {
	actorKind, actorID, reqID, err := requireAuditMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("unban link: %w", err)
	}
	got, err := scanLink(s.db.QueryRowContext(ctx, unbanLinkSQL,
		code, actorKind, actorID, reqID, string(audit.ActionLinkUnban), string(audit.TargetLink),
		job.KindCacheReconcile))
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
