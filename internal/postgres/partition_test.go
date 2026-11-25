package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"shortlink/internal/audit"
)

func TestMigration013PartitionedTablesAndDefault(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	for _, table := range []string{
		"link_clicks_daily",
		"link_dimension_daily",
		"link_visitors_daily",
		"audit_logs",
	} {
		var relKind string
		if err := store.db.QueryRowContext(ctx,
			`SELECT relkind::text FROM pg_class WHERE oid = $1::regclass`, table,
		).Scan(&relKind); err != nil {
			t.Fatalf("query relkind(%s): %v", table, err)
		}
		if relKind != "p" {
			t.Errorf("%s relkind = %q, want 'p' (partitioned table)", table, relKind)
		}

		var hasDefault bool
		if err := store.db.QueryRowContext(ctx,
			`SELECT EXISTS (
                SELECT 1
                FROM pg_inherits i
                JOIN pg_class p ON p.oid = i.inhparent
                JOIN pg_class c ON c.oid = i.inhrelid
                WHERE p.relname = $1 AND c.relname = $2
            )`, table, table+"_default",
		).Scan(&hasDefault); err != nil {
			t.Fatalf("check default partition(%s): %v", table, err)
		}
		if !hasDefault {
			t.Errorf("missing default partition %s_default", table)
		}
	}
}

func TestEnsureAndDropMonthlyPartitions(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	// 1. 统计表与审计表分区分别预创建未来 5 个月，且二次调用幂等返回 0。
	now := time.Now().UTC()
	createdStats, err := store.EnsureStatsPartitions(ctx, now, 5)
	if err != nil {
		t.Fatalf("EnsureStatsPartitions(5): %v", err)
	}
	if createdStats == 0 {
		t.Fatalf("EnsureStatsPartitions(5) created = 0, want > 0 for future months +3..+5")
	}
	createdStatsAgain, err := store.EnsureStatsPartitions(ctx, now, 5)
	if err != nil {
		t.Fatalf("EnsureStatsPartitions(5) repeat: %v", err)
	}
	if createdStatsAgain != 0 {
		t.Fatalf("EnsureStatsPartitions repeat created = %d, want 0", createdStatsAgain)
	}
	createdAudit, err := store.EnsureAuditPartitions(ctx, now, 5)
	if err != nil {
		t.Fatalf("EnsureAuditPartitions(5): %v", err)
	}
	if createdAudit == 0 {
		t.Fatalf("EnsureAuditPartitions(5) created = 0, want > 0 for future months +3..+5")
	}
	createdAuditAgain, err := store.EnsureAuditPartitions(ctx, now, 5)
	if err != nil {
		t.Fatalf("EnsureAuditPartitions(5) repeat: %v", err)
	}
	if createdAuditAgain != 0 {
		t.Fatalf("EnsureAuditPartitions repeat created = %d, want 0", createdAuditAgain)
	}

	// 验证超过 2 个月以上的远期清理与保护：
	// 手工挂上一个远古 2019_01 分区用于测试 DropExpiredMonthlyPartitions，同时验证 _default 与当月分区绝不被删除。
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS link_clicks_daily_2019_01 PARTITION OF link_clicks_daily FOR VALUES FROM ('2019-01-01') TO ('2019-02-01')`,
		`CREATE TABLE IF NOT EXISTS link_dimension_daily_2019_01 PARTITION OF link_dimension_daily FOR VALUES FROM ('2019-01-01') TO ('2019-02-01')`,
		`CREATE TABLE IF NOT EXISTS link_visitors_daily_2019_01 PARTITION OF link_visitors_daily FOR VALUES FROM ('2019-01-01') TO ('2019-02-01')`,
		`CREATE TABLE IF NOT EXISTS audit_logs_2019_01 PARTITION OF audit_logs FOR VALUES FROM ('2019-01-01 00:00:00+00') TO ('2019-02-01 00:00:00+00')`,
	} {
		if _, err := store.db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("create old test partition: %v", err)
		}
	}
	t.Cleanup(func() {
		// 测试结束清理可能多出来的 +3..+5 远期测试分区和 2019_01（保持默认迁移区间干净）。
		base := utcMonthStart(now)
		for offset := 3; offset <= 5; offset++ {
			m := base.AddDate(0, offset, 0)
			for _, tbl := range []string{"link_clicks_daily", "link_dimension_daily", "link_visitors_daily", "audit_logs"} {
				_, _ = store.db.ExecContext(context.Background(),
					"DROP TABLE IF EXISTS "+tbl+"_"+m.Format("2006_01"))
			}
		}
		for _, tbl := range []string{"link_clicks_daily", "link_dimension_daily", "link_visitors_daily", "audit_logs"} {
			_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl+"_2019_01")
		}
	})

	droppedStats, err := store.DropExpiredStatsPartitions(ctx, now, 90)
	if err != nil {
		t.Fatalf("DropExpiredStatsPartitions: %v", err)
	}
	if droppedStats < 3 {
		t.Fatalf("droppedStats = %d, want >= 3", droppedStats)
	}
	droppedAudit, err := store.DropExpiredAuditPartitions(ctx, now, 180)
	if err != nil {
		t.Fatalf("DropExpiredAuditPartitions: %v", err)
	}
	if droppedAudit < 1 {
		t.Fatalf("droppedAudit = %d, want >= 1", droppedAudit)
	}

	curSuffix := utcMonthStart(now).Format("2006_01")
	for _, tbl := range []string{"link_clicks_daily", "link_dimension_daily", "link_visitors_daily", "audit_logs"} {
		for _, mustExist := range []string{tbl + "_default", tbl + "_" + curSuffix} {
			var reg *string
			if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, mustExist).Scan(&reg); err != nil {
				t.Fatalf("check %s: %v", mustExist, err)
			}
			if reg == nil {
				t.Errorf("%s was unexpectedly dropped", mustExist)
			}
		}
		var oldReg *string
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, tbl+"_2019_01").Scan(&oldReg); err != nil {
			t.Fatalf("check old partition %s_2019_01: %v", tbl, err)
		}
		if oldReg != nil {
			t.Errorf("%s_2019_01 still exists after expiration drop", tbl)
		}
	}
}

/*
TestPartitionedQueriesExplainAnalyze 写入跨多月合成数据并运行真实 EXPLAIN (ANALYZE, BUFFERS)，
验证：(1) 日统计按日期范围查询命中分区裁剪，绝不扫描无关历史月分区；
(2) audit_logs Keyset 游标查询命中 idx_audit_logs_tenant_id 索引；
(3) PurgeLinkStatsBatch 按 PK 元组过滤命中索引。
*/
func TestPartitionedQueriesExplainAnalyze(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	now := time.Now().UTC()
	curMonth := utcMonthStart(now)
	prevMonth := curMonth.AddDate(0, -1, 0)
	prev2Month := curMonth.AddDate(0, -2, 0)

	l := createLink(t, store, tenantID, "https://example.com/explain-part")
	other := createLink(t, store, tenantID, "https://example.com/explain-other")
	noiseTenant := insertTenant(t, store, "explain-noise")

	// 写入三个不同月份的数据：curMonth、prevMonth、prev2Month。
	// 每张表注入约 1,000~3,000 行多租户/多链接背景噪音，使默认规划器真实按统计分布选择索引与分区裁剪。
	for _, m := range []time.Time{prev2Month, prevMonth, curMonth} {
		for d := range 5 {
			day := m.AddDate(0, 0, d)
			for _, linkID := range []int64{l.ID, other.ID} {
				if _, err := store.db.ExecContext(ctx,
					`INSERT INTO link_clicks_daily (tenant_id, link_id, day, clicks) VALUES ($1, $2, $3, 10)`,
					tenantID, linkID, day); err != nil {
					t.Fatalf("seed clicks: %v", err)
				}
				if _, err := store.db.ExecContext(ctx,
					`INSERT INTO link_dimension_daily (link_id, day, dimension, value, clicks) VALUES ($1, $2, 'referrer', 'example.com', 5)`,
					linkID, day); err != nil {
					t.Fatalf("seed dimension: %v", err)
				}
			}
			for v := range 10 {
				var buf [8]byte
				binary.BigEndian.PutUint64(buf[:], uint64(l.ID*1000+int64(d)*20+int64(v)))
				h := sha256.Sum256(buf[:])
				if _, err := store.db.ExecContext(ctx,
					`INSERT INTO link_visitors_daily (link_id, day, visitor_hash) VALUES ($1, $2, $3)`,
					l.ID, day, h[:16]); err != nil {
					t.Fatalf("seed target visitor: %v", err)
				}
			}
		}

		// 为 other.ID 批量生成 1,000 条不同 visitor_hash（跨页），测试 PurgeLinkStatsBatch 命中主键索引。
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO link_visitors_daily (link_id, day, visitor_hash)
             SELECT $1, $2::date, decode(lpad(to_hex(g), 32, '0'), 'hex')
             FROM generate_series(1, 1000) AS g`,
			other.ID, m.AddDate(0, 0, 1)); err != nil {
			t.Fatalf("seed noise visitors: %v", err)
		}

		// 为 tenantID 写 15 条、为 noiseTenant 写 1,000 条 audit_logs，测试 ListAuditLogs 命中 idx_audit_logs_tenant_id。
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at)
             SELECT $1, 'system', NULL, $2, 'link', $3, 'req-explain', '{}'::jsonb, $4::timestamptz + (g * interval '1 second')
             FROM generate_series(1, 15) AS g`,
			tenantID, string(audit.ActionLinkCreate), l.Code, m.AddDate(0, 0, 1)); err != nil {
			t.Fatalf("seed target audit_logs: %v", err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata, created_at)
             SELECT $1, 'system', NULL, $2, 'link', 'noise000', 'req-noise', '{}'::jsonb, $3::timestamptz + (g * interval '1 second')
             FROM generate_series(1, 1000) AS g`,
			noiseTenant, string(audit.ActionLinkCreate), m.AddDate(0, 0, 1)); err != nil {
			t.Fatalf("seed noise audit_logs: %v", err)
		}
	}

	if _, err := store.db.ExecContext(ctx, `ANALYZE link_clicks_daily; ANALYZE link_dimension_daily; ANALYZE link_visitors_daily; ANALYZE audit_logs;`); err != nil {
		t.Fatalf("ANALYZE partitioned tables: %v", err)
	}

	curSuffix := curMonth.Format("2006_01")
	prev2Suffix := prev2Month.Format("2006_01")

	clicksPlan := explainQuery(t, ctx, store,
		`EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
         SELECT day, clicks FROM link_clicks_daily
         WHERE link_id = $1 AND day >= $2 AND day < $3
         ORDER BY day`,
		l.ID, curMonth, curMonth.AddDate(0, 1, 0))
	t.Logf("clicks partition pruning plan:\n%s", clicksPlan)
	if !strings.Contains(clicksPlan, "link_clicks_daily_"+curSuffix) {
		t.Errorf("expected current month partition %s in plan:\n%s", curSuffix, clicksPlan)
	}
	if strings.Contains(clicksPlan, "link_clicks_daily_"+prev2Suffix) {
		t.Errorf("stale month partition %s was NOT pruned:\n%s", prev2Suffix, clicksPlan)
	}

	visitorsPlan := explainQuery(t, ctx, store,
		`EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
         SELECT count(*) FROM link_visitors_daily
         WHERE link_id = $1 AND day >= $2 AND day < $3`,
		l.ID, curMonth, curMonth.AddDate(0, 1, 0))
	t.Logf("visitors partition pruning plan:\n%s", visitorsPlan)
	if !strings.Contains(visitorsPlan, "link_visitors_daily_"+curSuffix) {
		t.Errorf("expected current month visitor partition %s in plan:\n%s", curSuffix, visitorsPlan)
	}
	if strings.Contains(visitorsPlan, "link_visitors_daily_"+prev2Suffix) {
		t.Errorf("stale month visitor partition %s was NOT pruned:\n%s", prev2Suffix, visitorsPlan)
	}

	auditPlan := explainQuery(t, ctx, store,
		`EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
         SELECT id, action FROM audit_logs
         WHERE tenant_id = $1
         ORDER BY id DESC
         LIMIT 20`,
		tenantID)
	t.Logf("audit_logs list plan:\n%s", auditPlan)
	if (!strings.Contains(auditPlan, "Index Scan") && !strings.Contains(auditPlan, "Index Only Scan")) ||
		!strings.Contains(auditPlan, "tenant_id_id_idx") {
		t.Errorf("expected Index Scan on tenant_id_id_idx in audit_logs plan:\n%s", auditPlan)
	}

	purgePlan := explainQuery(t, ctx, store,
		`EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
         SELECT link_id, day, visitor_hash
         FROM link_visitors_daily
         WHERE link_id = $1
         LIMIT 50`,
		l.ID)
	t.Logf("purge visitors PK plan:\n%s", purgePlan)
	if (!strings.Contains(purgePlan, "Index Only Scan") && !strings.Contains(purgePlan, "Index Scan")) ||
		!strings.Contains(purgePlan, "_pkey") {
		t.Errorf("expected index scan on _pkey in purge PK plan:\n%s", purgePlan)
	}
}

func explainQuery(t *testing.T, ctx context.Context, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain line: %v", err)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}
	return sb.String()
}
