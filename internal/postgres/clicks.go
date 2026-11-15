package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
	"unicode/utf8"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

/*
点击写入的日聚合语句。

先在 Go 里按主键去重聚合，同一批里同一条链接、同一天、同一个维度只出现一行；
unnest 的多个数组按下标配对，长度必须一致。
JOIN links 同时做两件事：只给仍然存在的链接写统计，并填 link_clicks_daily.tenant_id。
ON CONFLICT 把新点击数加到已有行上，而不是覆盖。
*/
const (
	recordClicksDailySQL = `INSERT INTO link_clicks_daily (tenant_id, link_id, day, clicks)
SELECT l.tenant_id, d.link_id, d.day::date, d.clicks
FROM unnest($1::bigint[], $2::text[], $3::bigint[]) AS d(link_id, day, clicks)
JOIN links l ON l.id = d.link_id
ON CONFLICT (link_id, day) DO UPDATE
SET clicks = link_clicks_daily.clicks + EXCLUDED.clicks`

	recordClicksDimensionSQL = `INSERT INTO link_dimension_daily (link_id, day, dimension, value, clicks)
SELECT d.link_id, d.day::date, d.dimension, d.value, d.clicks
FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::bigint[])
    AS d(link_id, day, dimension, value, clicks)
JOIN links l ON l.id = d.link_id
ON CONFLICT (link_id, day, dimension, value) DO UPDATE
SET clicks = link_dimension_daily.clicks + EXCLUDED.clicks`

	/*
		访客哈希用十六进制字符串传入，在 SQL 里 decode 成 bytea。
		这样不依赖驱动对 bytea[] 的数组编码；一批最多 100 个哈希，开销可以忽略。
	*/
	recordClicksVisitorsSQL = `INSERT INTO link_visitors_daily (link_id, day, visitor_hash)
SELECT v.link_id, v.day::date, decode(v.hash, 'hex')
FROM unnest($1::bigint[], $2::text[], $3::text[]) AS v(link_id, day, hash)
JOIN links l ON l.id = v.link_id
ON CONFLICT DO NOTHING`

	/*
		同一行在 data 里只出现一次，调用方已经按 id 加总。
	*/
	addClicksSQL = `UPDATE links AS l
SET clicks = l.clicks + data.n
FROM unnest($1::bigint[], $2::bigint[]) AS data(id, n)
WHERE l.id = data.id`
)

/*
维度名同时用于写入和统计读取；两边共用常量，改名时不会一边改一边漏。
取值和迁移里的 CHECK 约束保持一致。
*/
const (
	dimensionReferrer = "referrer"
	dimensionDevice   = "device"
	dimensionClient   = "client"
)

type clickDayKey struct {
	linkID int64
	day    string
}

type clickDimensionKey struct {
	linkID    int64
	day       string
	dimension string
	value     string
}

type clickVisitorKey struct {
	linkID int64
	day    string
	hash   string
}

/*
RecordClicks 把一个点击批次写进三张日聚合表和 links.clicks，全部在同一个事务里。
任何一步失败都回滚，这批点击整体丢掉，和 ADR 0007 的失败语义一致。

已经删除的链接由 JOIN links 过滤掉，不会产生统计行；links.clicks 的 UPDATE
本来也匹配不到行。删除和点击并发时仍可能留下孤儿统计行，见 ADR 0012 的对账 SQL。
*/
func (s *Store) RecordClicks(ctx context.Context, clicks []click.Click) error {
	daily, dimensions, visitors, totals := aggregateClicks(clicks)
	if len(daily) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record clicks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertDailyClicks(ctx, tx, daily); err != nil {
		return err
	}
	if err := insertDimensionClicks(ctx, tx, dimensions); err != nil {
		return err
	}
	if err := insertVisitorClicks(ctx, tx, visitors); err != nil {
		return err
	}
	if err := addLinkClicks(ctx, tx, totals); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record clicks: %w", err)
	}
	return nil
}

// aggregateClicks 在 Go 里按键去重聚合；visitor 只关心哈希是否出现过。
func aggregateClicks(clicks []click.Click) (
	daily map[clickDayKey]int64,
	dimensions map[clickDimensionKey]int64,
	visitors map[clickVisitorKey]struct{},
	totals map[int64]int64,
) {
	daily = make(map[clickDayKey]int64)
	dimensions = make(map[clickDimensionKey]int64)
	visitors = make(map[clickVisitorKey]struct{})
	totals = make(map[int64]int64)

	for _, c := range clicks {
		if c.LinkID <= 0 {
			continue
		}
		at := c.At
		if at.IsZero() {
			// Recorder 会补时间；直接调用存储的测试也兜底一次，避免落到公元 1 年。
			at = time.Now().UTC()
		}
		day := link.DayKey(at)
		key := clickDayKey{linkID: c.LinkID, day: day}
		daily[key]++
		totals[c.LinkID]++

		for _, dim := range [...]struct{ name, value string }{
			{dimensionReferrer, c.Referrer},
			{dimensionDevice, c.Device},
			{dimensionClient, c.Client},
		} {
			if !validDimensionValue(dim.value) {
				continue
			}
			dimensions[clickDimensionKey{
				linkID: c.LinkID, day: day, dimension: dim.name, value: dim.value,
			}]++
		}

		if len(c.VisitorHash) == 16 {
			visitors[clickVisitorKey{
				linkID: c.LinkID, day: day, hash: hex.EncodeToString(c.VisitorHash),
			}] = struct{}{}
		}
	}
	return daily, dimensions, visitors, totals
}

/*
validDimensionValue 挡住空值和超长值；CHECK 约束要求 1 到 255 字节。
无效 UTF-8 会让 PostgreSQL 拒绝整批写入，直接在 Go 里跳过这一个维度。
*/
func validDimensionValue(v string) bool {
	return v != "" && len(v) <= 255 && utf8.ValidString(v)
}

func insertDailyClicks(ctx context.Context, tx *sql.Tx, daily map[clickDayKey]int64) error {
	ids := make([]int64, 0, len(daily))
	days := make([]string, 0, len(daily))
	counts := make([]int64, 0, len(daily))
	for key, n := range daily {
		ids = append(ids, key.linkID)
		days = append(days, key.day)
		counts = append(counts, n)
	}
	if _, err := tx.ExecContext(ctx, recordClicksDailySQL, ids, days, counts); err != nil {
		return fmt.Errorf("record clicks daily: %w", err)
	}
	return nil
}

func insertDimensionClicks(ctx context.Context, tx *sql.Tx, dimensions map[clickDimensionKey]int64) error {
	if len(dimensions) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(dimensions))
	days := make([]string, 0, len(dimensions))
	names := make([]string, 0, len(dimensions))
	values := make([]string, 0, len(dimensions))
	counts := make([]int64, 0, len(dimensions))
	for key, n := range dimensions {
		ids = append(ids, key.linkID)
		days = append(days, key.day)
		names = append(names, key.dimension)
		values = append(values, key.value)
		counts = append(counts, n)
	}
	if _, err := tx.ExecContext(ctx, recordClicksDimensionSQL, ids, days, names, values, counts); err != nil {
		return fmt.Errorf("record dimension clicks: %w", err)
	}
	return nil
}

func insertVisitorClicks(ctx context.Context, tx *sql.Tx, visitors map[clickVisitorKey]struct{}) error {
	if len(visitors) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(visitors))
	days := make([]string, 0, len(visitors))
	hashes := make([]string, 0, len(visitors))
	for key := range visitors {
		ids = append(ids, key.linkID)
		days = append(days, key.day)
		hashes = append(hashes, key.hash)
	}
	if _, err := tx.ExecContext(ctx, recordClicksVisitorsSQL, ids, days, hashes); err != nil {
		return fmt.Errorf("record visitor clicks: %w", err)
	}
	return nil
}

func addLinkClicks(ctx context.Context, tx *sql.Tx, totals map[int64]int64) error {
	ids := make([]int64, 0, len(totals))
	counts := make([]int64, 0, len(totals))
	for id, n := range totals {
		ids = append(ids, id)
		counts = append(counts, n)
	}
	if _, err := tx.ExecContext(ctx, addClicksSQL, ids, counts); err != nil {
		return fmt.Errorf("add clicks: %w", err)
	}
	return nil
}
