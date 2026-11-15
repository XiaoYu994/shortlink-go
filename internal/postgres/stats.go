package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shortlink/internal/link"
)

/*
Stats 读取一条链接的日统计。

整个查询在一个只读的 REPEATABLE READ 事务里：先确认链接存在并取终身累计，
再读三张日表，同一批点击的各维度在响应里是同一个快照。
链接按 code + tenant_id 查，过期链接可以看，已删除或其他租户的链接返回 ErrNotFound。
*/
func (s *Store) Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("link stats: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stats := &link.Stats{
		Code: code,
		From: link.DayStart(from),
		To:   link.DayStart(to),
	}
	var linkID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id, clicks FROM links WHERE code = $1 AND tenant_id = $2`,
		code, tenantID).Scan(&linkID, &stats.TotalClicks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("link stats: %w", err)
	}

	fromDay, toDay := link.DayKey(stats.From), link.DayKey(stats.To)
	if err := readDailyClicks(ctx, tx, linkID, fromDay, toDay, stats); err != nil {
		return nil, err
	}
	if err := readDimensionClicks(ctx, tx, linkID, fromDay, toDay, stats); err != nil {
		return nil, err
	}
	if err := readDailyVisitors(ctx, tx, linkID, fromDay, toDay, stats); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("link stats: %w", err)
	}
	return stats, nil
}

func readDailyClicks(ctx context.Context, tx *sql.Tx, linkID int64, from, to string, stats *link.Stats) error {
	rows, err := tx.QueryContext(ctx, `
SELECT day, clicks
FROM link_clicks_daily
WHERE link_id = $1 AND day BETWEEN $2::date AND $3::date
ORDER BY day`, linkID, from, to)
	if err != nil {
		return fmt.Errorf("link stats daily clicks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			day    time.Time
			clicks int64
		)
		if err := rows.Scan(&day, &clicks); err != nil {
			return fmt.Errorf("link stats daily clicks: %w", err)
		}
		stats.Clicks = append(stats.Clicks, link.DayClicks{Day: link.DayStart(day), Clicks: clicks})
		stats.RangeClicks += clicks
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("link stats daily clicks: %w", err)
	}
	return nil
}

/*
readDimensionClicks 分三次读三类维度。
参考来源只取点击数最多的 10 个：host 来自客户端，是唯一能被任意撑大的维度，
不能先全部拉到 Go 再截断。设备、客户端类别固定，取全部。
点击数相同时按取值升序，保证结果稳定。
不合成 other 汇总行：other 是存下来的真实取值，和普通 host 一样参与排序。
*/
func readDimensionClicks(ctx context.Context, tx *sql.Tx, linkID int64, from, to string, stats *link.Stats) error {
	var err error
	if stats.Referrers, err = readDimension(ctx, tx, linkID, from, to, dimensionReferrer, 10); err != nil {
		return err
	}
	if stats.Devices, err = readDimension(ctx, tx, linkID, from, to, dimensionDevice, 0); err != nil {
		return err
	}
	stats.Clients, err = readDimension(ctx, tx, linkID, from, to, dimensionClient, 0)
	return err
}

// readDimension 按维度汇总日期范围内的点击数；limit <= 0 表示不限制。
func readDimension(ctx context.Context, tx *sql.Tx, linkID int64, from, to, dimension string, limit int) ([]link.DimensionCount, error) {
	query := `
SELECT value, sum(clicks)
FROM link_dimension_daily
WHERE link_id = $1 AND day BETWEEN $2::date AND $3::date AND dimension = $4
GROUP BY value
ORDER BY sum(clicks) DESC, value`
	args := []any{linkID, from, to, dimension}
	if limit > 0 {
		query += ` LIMIT $5`
		args = append(args, limit)
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("link stats %s: %w", dimension, err)
	}
	defer func() { _ = rows.Close() }()

	var result []link.DimensionCount
	for rows.Next() {
		var count link.DimensionCount
		if err := rows.Scan(&count.Value, &count.Clicks); err != nil {
			return nil, fmt.Errorf("link stats %s: %w", dimension, err)
		}
		result = append(result, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("link stats %s: %w", dimension, err)
	}
	return result, nil
}

func readDailyVisitors(ctx context.Context, tx *sql.Tx, linkID int64, from, to string, stats *link.Stats) error {
	rows, err := tx.QueryContext(ctx, `
SELECT day, count(*)
FROM link_visitors_daily
WHERE link_id = $1 AND day BETWEEN $2::date AND $3::date
GROUP BY day
ORDER BY day`, linkID, from, to)
	if err != nil {
		return fmt.Errorf("link stats visitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			day      time.Time
			visitors int64
		)
		if err := rows.Scan(&day, &visitors); err != nil {
			return fmt.Errorf("link stats visitors: %w", err)
		}
		stats.DailyVisitors = append(stats.DailyVisitors, link.DayVisitors{Day: link.DayStart(day), Visitors: visitors})
		stats.UniqueVisitors += visitors
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("link stats visitors: %w", err)
	}
	return nil
}
