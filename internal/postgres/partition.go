package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

type partitionKeyKind int

const (
	partitionKeyDate partitionKeyKind = iota + 1
	partitionKeyTimestamptz

	partitionLockTimeoutSQL = `SET LOCAL lock_timeout = '3s'`

	listChildPartitionsSQL = `SELECT c.relname
FROM pg_inherits i
JOIN pg_class p ON p.oid = i.inhparent
JOIN pg_namespace pn ON pn.oid = p.relnamespace
JOIN pg_class c ON c.oid = i.inhrelid
WHERE pn.nspname = current_schema()
  AND p.relname = $1
ORDER BY c.relname`
)

type partitionedTableSpec struct {
	name    string
	keyKind partitionKeyKind
	pattern *regexp.Regexp
}

var (
	statsPartitionSpecs = []partitionedTableSpec{
		newPartitionSpec("link_clicks_daily", partitionKeyDate),
		newPartitionSpec("link_dimension_daily", partitionKeyDate),
		newPartitionSpec("link_visitors_daily", partitionKeyDate),
	}
	auditPartitionSpecs = []partitionedTableSpec{
		newPartitionSpec("audit_logs", partitionKeyTimestamptz),
	}
)

func newPartitionSpec(name string, keyKind partitionKeyKind) partitionedTableSpec {
	return partitionedTableSpec{
		name:    name,
		keyKind: keyKind,
		pattern: regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `_(\d{4})_(\d{2})$`),
	}
}

func utcMonthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func utcDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

/*
EnsureStatsPartitions 为三张点击统计按月范围分区表预创建从当月起到未来 monthsAhead 个月的月分区。
每条 DDL 在独立事务内设置 SET LOCAL lock_timeout = '3s' 并使用 CREATE TABLE IF NOT EXISTS，
避免长期等锁拖垮在线流量，且支持多副本并发幂等重跑（ADR 0026、ADR 0029）。
*/
func (s *Store) EnsureStatsPartitions(ctx context.Context, now time.Time, monthsAhead int) (int, error) {
	return s.ensurePartitionsForSpecs(ctx, statsPartitionSpecs, now, monthsAhead)
}

/*
EnsureAuditPartitions 为审计日志分区表 audit_logs 预创建从当月起到未来 monthsAhead 个月的月分区。
与统计分区拆开，使关闭 FEATURE_ANALYTICS 时审计分区仍持续自动维护（ADR 0029）。
*/
func (s *Store) EnsureAuditPartitions(ctx context.Context, now time.Time, monthsAhead int) (int, error) {
	return s.ensurePartitionsForSpecs(ctx, auditPartitionSpecs, now, monthsAhead)
}

func (s *Store) ensurePartitionsForSpecs(ctx context.Context, specs []partitionedTableSpec, now time.Time, monthsAhead int) (int, error) {
	if monthsAhead < 0 {
		return 0, fmt.Errorf("ensure monthly partitions: negative monthsAhead %d", monthsAhead)
	}
	base := utcMonthStart(now)
	var created int
	for _, spec := range specs {
		for offset := 0; offset <= monthsAhead; offset++ {
			monthStart := base.AddDate(0, offset, 0)
			didCreate, err := s.ensureSingleMonthPartition(ctx, spec, monthStart)
			if err != nil {
				return created, err
			}
			if didCreate {
				created++
			}
		}
	}
	return created, nil
}

func (s *Store) ensureSingleMonthPartition(ctx context.Context, spec partitionedTableSpec, monthStart time.Time) (bool, error) {
	monthEnd := monthStart.AddDate(0, 1, 0)
	childName := fmt.Sprintf("%s_%04d_%02d", spec.name, monthStart.Year(), int(monthStart.Month()))

	var fromLiteral, toLiteral string
	switch spec.keyKind {
	case partitionKeyDate:
		fromLiteral = monthStart.Format("2006-01-02")
		toLiteral = monthEnd.Format("2006-01-02")
	case partitionKeyTimestamptz:
		fromLiteral = monthStart.Format("2006-01-02 15:04:05+00")
		toLiteral = monthEnd.Format("2006-01-02 15:04:05+00")
	default:
		return false, fmt.Errorf("ensure partition %s: unknown key kind %d", childName, spec.keyKind)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("ensure partition %s: begin: %w", childName, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)`, childName).Scan(&existing); err != nil {
		return false, fmt.Errorf("ensure partition %s: check exists: %w", childName, err)
	}
	if existing.Valid {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, partitionLockTimeoutSQL); err != nil {
		return false, fmt.Errorf("ensure partition %s: lock_timeout: %w", childName, err)
	}
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		childName, spec.name, fromLiteral, toLiteral,
	)
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return false, fmt.Errorf("ensure partition %s: create: %w", childName, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("ensure partition %s: commit: %w", childName, err)
	}
	return true, nil
}

/*
DropExpiredStatsPartitions 按天数保留期整月 DROP TABLE 过期统计分区（link_clicks_daily、link_dimension_daily、link_visitors_daily）。
只有整个分区上界 monthEnd <= cutoff 的历史 YYYY_MM 月分区才会被删除；_default 分区永不删除（ADR 0026）。
*/
func (s *Store) DropExpiredStatsPartitions(ctx context.Context, now time.Time, retentionDays int) (int64, error) {
	return s.dropExpiredPartitionsForSpecs(ctx, statsPartitionSpecs, now, retentionDays)
}

/*
DropExpiredAuditPartitions 按天数保留期整月 DROP TABLE 过期审计分区（audit_logs）。
只有整个分区上界 monthEnd <= cutoff 的历史 YYYY_MM 月分区才会被删除；_default 分区永不删除（ADR 0026）。
*/
func (s *Store) DropExpiredAuditPartitions(ctx context.Context, now time.Time, retentionDays int) (int64, error) {
	return s.dropExpiredPartitionsForSpecs(ctx, auditPartitionSpecs, now, retentionDays)
}

func (s *Store) dropExpiredPartitionsForSpecs(ctx context.Context, specs []partitionedTableSpec, now time.Time, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, fmt.Errorf("drop expired partitions: retentionDays must be positive, got %d", retentionDays)
	}
	cutoff := utcDayStart(now).AddDate(0, 0, -retentionDays)

	var dropped int64
	for _, spec := range specs {
		n, err := s.dropExpiredForSpec(ctx, spec, cutoff)
		dropped += n
		if err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

func (s *Store) dropExpiredForSpec(ctx context.Context, spec partitionedTableSpec, cutoff time.Time) (int64, error) {
	candidates, err := s.listExpiredPartitions(ctx, spec, cutoff)
	if err != nil {
		return 0, err
	}

	var dropped int64
	for _, childName := range candidates {
		if err := s.dropSinglePartition(ctx, childName); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

func (s *Store) listExpiredPartitions(ctx context.Context, spec partitionedTableSpec, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, listChildPartitionsSQL, spec.name)
	if err != nil {
		return nil, fmt.Errorf("list partitions for %s: %w", spec.name, err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []string
	for rows.Next() {
		var childName string
		if err := rows.Scan(&childName); err != nil {
			return nil, fmt.Errorf("scan partition for %s: %w", spec.name, err)
		}
		monthEnd, ok := parseMonthlyPartitionEnd(spec.pattern, childName)
		if !ok {
			// 非 YYYY_MM 格式（例如 *_default）一律跳过，绝不误删兜底分区。
			continue
		}
		if !monthEnd.After(cutoff) {
			candidates = append(candidates, childName)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate partitions for %s: %w", spec.name, err)
	}
	return candidates, nil
}

func parseMonthlyPartitionEnd(pattern *regexp.Regexp, childName string) (time.Time, bool) {
	m := pattern.FindStringSubmatch(childName)
	if m == nil {
		return time.Time{}, false
	}
	year, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	month, err := strconv.Atoi(m[2])
	if err != nil || month < 1 || month > 12 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0), true
}

func (s *Store) dropSinglePartition(ctx context.Context, childName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("drop partition %s: begin: %w", childName, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, partitionLockTimeoutSQL); err != nil {
		return fmt.Errorf("drop partition %s: lock_timeout: %w", childName, err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+childName); err != nil {
		return fmt.Errorf("drop partition %s: %w", childName, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("drop partition %s: commit: %w", childName, err)
	}
	return nil
}
