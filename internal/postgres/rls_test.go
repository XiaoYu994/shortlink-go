package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shortlink/internal/link"
)

func execExpectDenied(t *testing.T, ctx context.Context, tx *sql.Tx, label, query string, args ...any) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `SAVEPOINT rls_deny`); err != nil {
		t.Fatalf("%s: SAVEPOINT: %v", label, err)
	}
	_, execErr := tx.ExecContext(ctx, query, args...)
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT rls_deny`); err != nil {
		t.Fatalf("%s: ROLLBACK TO SAVEPOINT: %v", label, err)
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT rls_deny`); err != nil {
		t.Fatalf("%s: RELEASE SAVEPOINT: %v", label, err)
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](execErr)
	if !ok || pgErr.Code != "42501" {
		t.Fatalf("%s: err = %v, want SQLSTATE 42501 (insufficient_privilege)", label, execErr)
	}
}

func queryExpectDenied(t *testing.T, ctx context.Context, tx *sql.Tx, label, query string, args ...any) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, `SAVEPOINT rls_deny_q`); err != nil {
		t.Fatalf("%s: SAVEPOINT: %v", label, err)
	}
	var dummy int
	qErr := tx.QueryRowContext(ctx, query, args...).Scan(&dummy)
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT rls_deny_q`); err != nil {
		t.Fatalf("%s: ROLLBACK TO SAVEPOINT: %v", label, err)
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT rls_deny_q`); err != nil {
		t.Fatalf("%s: RELEASE SAVEPOINT: %v", label, err)
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](qErr)
	if !ok || pgErr.Code != "42501" {
		t.Fatalf("%s: err = %v, want SQLSTATE 42501 (insufficient_privilege)", label, qErr)
	}
}

func countTableRowsTx(t *testing.T, ctx context.Context, tx *sql.Tx, table string) (total int, distinctTenants []int64) {
	t.Helper()
	rows, err := tx.QueryContext(ctx, `SELECT tenant_id FROM `+table)
	if err != nil {
		t.Fatalf("SELECT %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[int64]bool{}
	for rows.Next() {
		var tid int64
		if err := rows.Scan(&tid); err != nil {
			t.Fatalf("scan %s.tenant_id: %v", table, err)
		}
		total++
		if !seen[tid] {
			seen[tid] = true
			distinctTenants = append(distinctTenants, tid)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return total, distinctTenants
}

func explainTxQuery(t *testing.T, ctx context.Context, tx *sql.Tx, sqlText string, args ...any) string {
	t.Helper()
	rows, err := tx.QueryContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)\n"+sqlText, args...)
	if err != nil {
		t.Fatalf("explain query: %v\n%s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

func TestRLSTenantIsolationOnAllProtectedTables(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := context.Background()

	var (
		tenantB                int64
		userA, userB           int64
		linkAID, linkBID       int64
		keyAID, keyBID         int64
		invAID, invBID         int64
		webhookAID, webhookBID int64
	)
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('tenant-b') RETURNING id`).Scan(&tenantB); err != nil {
		t.Fatalf("insert tenantB: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `INSERT INTO users (email, name) VALUES ('a@example.com', 'A') RETURNING id`).Scan(&userA); err != nil {
		t.Fatalf("insert userA: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `INSERT INTO users (email, name) VALUES ('b@example.com', 'B') RETURNING id`).Scan(&userB); err != nil {
		t.Fatalf("insert userB: %v", err)
	}

	seedTenant := func(tid, uid int64, suffix string) (linkID, keyID, invID, whID int64) {
		if err := store.db.QueryRowContext(ctx,
			`INSERT INTO links (code, url, tenant_id) VALUES ($1, $2, $3) RETURNING id`,
			"rls000"+suffix, "https://example.com/"+suffix, tid,
		).Scan(&linkID); err != nil {
			t.Fatalf("seed link %s: %v", suffix, err)
		}
		if err := store.db.QueryRowContext(ctx,
			`INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, decode(repeat($2, 32), 'hex'), ARRAY['links:read']) RETURNING id`,
			tid, suffix,
		).Scan(&keyID); err != nil {
			t.Fatalf("seed api_key %s: %v", suffix, err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'owner')`,
			uid, tid,
		); err != nil {
			t.Fatalf("seed membership %s: %v", suffix, err)
		}
		if err := store.db.QueryRowContext(ctx,
			`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, $2, 'member') RETURNING id`,
			tid, "inv-"+suffix+"@example.com",
		).Scan(&invID); err != nil {
			t.Fatalf("seed invitation %s: %v", suffix, err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO audit_logs (tenant_id, actor_kind, action, target_type, target_id, request_id, metadata) VALUES ($1, 'system', 'link.create', 'link', $2, 'req-1', '{}'::jsonb)`,
			tid, "code"+suffix,
		); err != nil {
			t.Fatalf("seed audit_log %s: %v", suffix, err)
		}
		if err := store.db.QueryRowContext(ctx,
			`INSERT INTO webhooks (tenant_id, url, events, secret_ciphertext, secret_prefix) VALUES ($1, $2, ARRAY['link.created'], decode(repeat('ab', 40), 'hex'), 'whsec_test01') RETURNING id`,
			tid, "https://hooks.example.com/"+suffix,
		).Scan(&whID); err != nil {
			t.Fatalf("seed webhook %s: %v", suffix, err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries (tenant_id, webhook_id, job_id, delivery_id, event, attempt, status, duration_ms) VALUES ($1, $2, $3, $4, 'link.created', 1, 'success', 10)`,
			tid, whID, whID+100, "dlv_"+suffix,
		); err != nil {
			t.Fatalf("seed delivery %s: %v", suffix, err)
		}
		return linkID, keyID, invID, whID
	}

	linkAID, keyAID, invAID, webhookAID = seedTenant(tenantA, userA, "aa")
	linkBID, keyBID, invBID, webhookBID = seedTenant(tenantB, userB, "bb")

	err := store.withTenantTx(ctx, tenantA, nil, func(tx *sql.Tx) error {
		// 1. 故意不写 WHERE tenant_id：7 张受保护表都只能读出 tenantA 自己的行。
		expectedCounts := map[string]int{
			"links":              1,
			"api_keys":           1,
			"memberships":        1,
			"invitations":        1,
			"audit_logs":         1,
			"webhooks":           1,
			"webhook_deliveries": 1,
		}
		for table, wantCount := range expectedCounts {
			total, tenantsSeen := countTableRowsTx(t, ctx, tx, table)
			if total != wantCount || len(tenantsSeen) != 1 || tenantsSeen[0] != tenantA {
				t.Fatalf("%s bare SELECT under tenantA = (total=%d, tenants=%v), want (%d, [%d])", table, total, tenantsSeen, wantCount, tenantA)
			}
		}

		// 2. 故意不带 tenant_id 的 UPDATE/DELETE 指向 tenantB 主键：影响行数必须为 0。
		zeroUpdates := []struct {
			label string
			sql   string
			args  []any
		}{
			{"update links B", `UPDATE links SET url = 'https://evil.example' WHERE id = $1`, []any{linkBID}},
			{"delete api_keys B", `DELETE FROM api_keys WHERE id = $1`, []any{keyBID}},
			{"delete memberships B", `DELETE FROM memberships WHERE user_id = $1`, []any{userB}},
			{"delete invitations B", `DELETE FROM invitations WHERE id = $1`, []any{invBID}},
			{"update webhooks B", `UPDATE webhooks SET url = 'https://evil.example' WHERE id = $1`, []any{webhookBID}},
		}
		for _, tc := range zeroUpdates {
			res, err := tx.ExecContext(ctx, tc.sql, tc.args...)
			if err != nil {
				t.Fatalf("%s failed: %v", tc.label, err)
			}
			if n, _ := res.RowsAffected(); n != 0 {
				t.Fatalf("%s RowsAffected = %d, want 0", tc.label, n)
			}
		}

		// 3. 越权 INSERT tenantB 行与把 tenantA 行的 tenant_id 改成 tenantB 均须报 42501。
		execExpectDenied(t, ctx, tx, "insert links B",
			`INSERT INTO links (code, url, tenant_id) VALUES ('evilcode', 'https://evil.example', $1)`, tenantB)
		execExpectDenied(t, ctx, tx, "insert api_keys B",
			`INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, decode(repeat('cc', 32), 'hex'), ARRAY['links:read'])`, tenantB)
		execExpectDenied(t, ctx, tx, "insert memberships B",
			`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'admin')`, userA, tenantB)
		execExpectDenied(t, ctx, tx, "insert invitations B",
			`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'evil@example.com', 'member')`, tenantB)
		execExpectDenied(t, ctx, tx, "insert audit_logs B",
			`INSERT INTO audit_logs (tenant_id, actor_kind, action, target_type, target_id, request_id, metadata) VALUES ($1, 'system', 'link.create', 'link', 'x', 'r1', '{}'::jsonb)`, tenantB)
		execExpectDenied(t, ctx, tx, "insert webhooks B",
			`INSERT INTO webhooks (tenant_id, url, events, secret_ciphertext, secret_prefix) VALUES ($1, 'https://evil.example', ARRAY['link.created'], decode(repeat('ab', 40), 'hex'), 'whsec_test02')`, tenantB)
		execExpectDenied(t, ctx, tx, "insert webhook_deliveries B",
			`INSERT INTO webhook_deliveries (tenant_id, webhook_id, job_id, delivery_id, event, attempt, status, duration_ms) VALUES ($1, $2, 999, 'dlv_evil', 'link.created', 1, 'success', 5)`, tenantB, webhookBID)

		execExpectDenied(t, ctx, tx, "steal link tenant_id", `UPDATE links SET tenant_id = $1 WHERE id = $2`, tenantB, linkAID)
		execExpectDenied(t, ctx, tx, "steal api_key tenant_id", `UPDATE api_keys SET tenant_id = $1 WHERE id = $2`, tenantB, keyAID)
		execExpectDenied(t, ctx, tx, "steal membership tenant_id", `UPDATE memberships SET tenant_id = $1 WHERE user_id = $2`, tenantB, userA)
		execExpectDenied(t, ctx, tx, "steal invitation tenant_id", `UPDATE invitations SET tenant_id = $1 WHERE id = $2`, tenantB, invAID)
		execExpectDenied(t, ctx, tx, "steal webhook tenant_id", `UPDATE webhooks SET tenant_id = $1 WHERE id = $2`, tenantB, webhookAID)

		// 4. 最小权限矩阵：audit_logs / webhook_deliveries 不可改删，sessions / user_identities 不可读。
		execExpectDenied(t, ctx, tx, "update audit_logs", `UPDATE audit_logs SET target_id = 'tamper'`)
		execExpectDenied(t, ctx, tx, "delete webhook_deliveries", `DELETE FROM webhook_deliveries`)
		queryExpectDenied(t, ctx, tx, "select sessions", `SELECT count(*) FROM sessions`)
		queryExpectDenied(t, ctx, tx, "select user_identities", `SELECT count(*) FROM user_identities`)
		return nil
	})
	if err != nil {
		t.Fatalf("withTenantTx error = %v", err)
	}

	// 5. 事务外使用 store.db 复核：被拦截的跨租户写/改/删均未产生任何持久化副作用。
	var gotLinkURL string
	if err := store.db.QueryRowContext(ctx, `SELECT url FROM links WHERE id = $1`, linkBID).Scan(&gotLinkURL); err != nil {
		t.Fatalf("verify tenantB link intact: %v", err)
	}
	if gotLinkURL != "https://example.com/bb" {
		t.Fatalf("tenantB link url = %q, want %q", gotLinkURL, "https://example.com/bb")
	}
	var gotWebhookURL string
	if err := store.db.QueryRowContext(ctx, `SELECT url FROM webhooks WHERE id = $1`, webhookBID).Scan(&gotWebhookURL); err != nil {
		t.Fatalf("verify tenantB webhook intact: %v", err)
	}
	if gotWebhookURL != "https://hooks.example.com/bb" {
		t.Fatalf("tenantB webhook url = %q, want %q", gotWebhookURL, "https://hooks.example.com/bb")
	}
	for _, check := range []struct {
		label string
		sql   string
		args  []any
		want  int
	}{
		{"tenantB api_key intact", `SELECT count(*) FROM api_keys WHERE id = $1 AND tenant_id = $2`, []any{keyBID, tenantB}, 1},
		{"tenantB membership intact", `SELECT count(*) FROM memberships WHERE user_id = $1 AND tenant_id = $2`, []any{userB, tenantB}, 1},
		{"tenantB invitation intact", `SELECT count(*) FROM invitations WHERE id = $1 AND tenant_id = $2`, []any{invBID, tenantB}, 1},
		{"no evil link inserted", `SELECT count(*) FROM links WHERE code = 'evilcode'`, nil, 0},
		{"no evil webhook delivery inserted", `SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'dlv_evil'`, nil, 0},
		{"no tampered audit log", `SELECT count(*) FROM audit_logs WHERE target_id = 'tamper'`, nil, 0},
	} {
		var cnt int
		if err := store.db.QueryRowContext(ctx, check.sql, check.args...).Scan(&cnt); err != nil {
			t.Fatalf("%s query: %v", check.label, err)
		}
		if cnt != check.want {
			t.Fatalf("%s count = %d, want %d", check.label, cnt, check.want)
		}
	}
}

func TestRLSFailClosedWithoutTenantSetting(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := context.Background()

	if _, err := store.db.ExecContext(ctx, `INSERT INTO links (code, url, tenant_id) VALUES ('rlsfail1', 'https://example.com/1', $1)`, tenantA); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO audit_logs (tenant_id, actor_kind, action, target_type, target_id, request_id, metadata) VALUES ($1, 'system', 'link.create', 'link', 'rlsfail1', 'req-1', '{}'::jsonb)`, tenantA); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	for _, emptyFirst := range []bool{false, true} {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, setTenantRoleSQL); err != nil {
			_ = tx.Rollback()
			t.Fatalf("SET LOCAL ROLE: %v", err)
		}
		if emptyFirst {
			if _, err := tx.ExecContext(ctx, setTenantIDSQL, ""); err != nil {
				_ = tx.Rollback()
				t.Fatalf("set empty app.tenant_id: %v", err)
			}
		}

		for _, table := range []string{"links", "api_keys", "memberships", "invitations", "audit_logs", "webhooks", "webhook_deliveries"} {
			total, _ := countTableRowsTx(t, ctx, tx, table)
			if total != 0 {
				_ = tx.Rollback()
				t.Fatalf("%s visible rows without tenant_id = %d, want 0 (fail-closed)", table, total)
			}
		}
		execExpectDenied(t, ctx, tx, "insert links fail-closed",
			`INSERT INTO links (code, url, tenant_id) VALUES ('rlsfail2', 'https://example.com/2', $1)`, tenantA)
		execExpectDenied(t, ctx, tx, "insert webhooks fail-closed",
			`INSERT INTO webhooks (tenant_id, url, events, secret_ciphertext, secret_prefix) VALUES ($1, 'https://hooks.example.com/a', ARRAY['link.created'], decode(repeat('ab', 40), 'hex'), 'whsec_fail01')`, tenantA)

		_ = tx.Rollback()
	}
}

func TestRLSAuditLogsDynamicPartitions(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := context.Background()

	var tenantB int64
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('part-b') RETURNING id`).Scan(&tenantB); err != nil {
		t.Fatalf("insert tenantB: %v", err)
	}

	now := time.Now().UTC()
	if _, err := store.EnsureMonthlyPartitions(ctx, now, 2); err != nil {
		t.Fatalf("EnsureMonthlyPartitions: %v", err)
	}

	timestamps := []time.Time{
		now,                                  // 当月分区
		utcMonthStart(now).AddDate(0, 2, 10), // EnsureMonthlyPartitions 动态新建的未来月分区
		time.Date(2099, 1, 15, 0, 0, 0, 0, time.UTC), // audit_logs_default 兜底分区
	}
	for i, ts := range timestamps {
		for _, tid := range []int64{tenantA, tenantB} {
			if _, err := store.db.ExecContext(ctx,
				`INSERT INTO audit_logs (tenant_id, actor_kind, action, target_type, target_id, request_id, metadata, created_at) VALUES ($1, 'system', 'link.create', 'link', $2, 'req-part', '{}'::jsonb, $3)`,
				tid, fmt.Sprintf("part_%d_%d", tid, i), ts,
			); err != nil {
				t.Fatalf("insert audit_log (%d, %v): %v", tid, ts, err)
			}
		}
	}

	err := store.withTenantReadTx(ctx, tenantA, func(tx *sql.Tx) error {
		total, seen := countTableRowsTx(t, ctx, tx, "audit_logs")
		if total != len(timestamps) || len(seen) != 1 || seen[0] != tenantA {
			t.Fatalf("audit_logs bare SELECT across partitions = (total=%d, seen=%v), want (%d, [%d])", total, seen, len(timestamps), tenantA)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withTenantReadTx: %v", err)
	}
}

func TestRLSPoolConnectionNoLeak(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")

	var tenantB int64
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('pool-b') RETURNING id`).Scan(&tenantB); err != nil {
		t.Fatalf("insert tenantB: %v", err)
	}

	singleDB, err := Open(ctx, dsn, PoolConfig{
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open single-conn pool: %v", err)
	}
	t.Cleanup(func() { _ = singleDB.Close() })
	singleStore := NewStore(singleDB)

	assertReset := func(step string) {
		t.Helper()
		var curUser, setting string
		if err := singleDB.QueryRowContext(ctx, `SELECT current_user, COALESCE(current_setting('app.tenant_id', true), '')`).Scan(&curUser, &setting); err != nil {
			t.Fatalf("%s inspect conn: %v", step, err)
		}
		if curUser == tenantRLSRole || setting != "" {
			t.Fatalf("%s leaked connection state: current_user=%q app.tenant_id=%q", step, curUser, setting)
		}
	}

	if err := singleStore.withTenantReadTx(ctx, tenantA, func(tx *sql.Tx) error {
		var curUser, setting string
		if err := tx.QueryRowContext(ctx, `SELECT current_user, current_setting('app.tenant_id', true)`).Scan(&curUser, &setting); err != nil {
			return err
		}
		if curUser != tenantRLSRole || setting != fmt.Sprintf("%d", tenantA) {
			t.Fatalf("in tenantA tx: user=%q setting=%q", curUser, setting)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenantA tx: %v", err)
	}
	assertReset("after tenantA commit")

	if err := singleStore.withTenantReadTx(ctx, tenantB, func(tx *sql.Tx) error {
		var setting string
		if err := tx.QueryRowContext(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&setting); err != nil {
			return err
		}
		if setting != fmt.Sprintf("%d", tenantB) {
			t.Fatalf("in tenantB tx: setting=%q, want %d", setting, tenantB)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenantB tx: %v", err)
	}
	assertReset("after tenantB commit")

	sentinelErr := errors.New("intentional rollback")
	if err := singleStore.withTenantTx(ctx, tenantA, nil, func(tx *sql.Tx) error {
		return sentinelErr
	}); !errors.Is(err, sentinelErr) {
		t.Fatalf("rolled-back tx err = %v, want %v", err, sentinelErr)
	}
	assertReset("after tenantA rollback")
}

func TestRLSExplainPlansPreserveIndexes(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	// LIFO：在 newTestStore 注册的 release/db.Close 之前清空合成数据，避免污染同一 TEST_DATABASE_URL 上后续运行的冒烟测试。
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `TRUNCATE links, memberships, audit_logs, webhooks, webhook_deliveries RESTART IDENTITY`)
	})

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO links (code, url, tenant_id)
SELECT 'rl' || lpad(g::text, 6, '0'), 'https://example.com/' || g, (g % 200) + 1
FROM generate_series(1, 10000) AS g;

INSERT INTO memberships (user_id, tenant_id, role)
SELECT g, (g % 200) + 1, 'member'
FROM generate_series(1, 5000) AS g;

INSERT INTO audit_logs (tenant_id, actor_kind, action, target_type, target_id, request_id, metadata)
SELECT (g % 200) + 1, 'system', 'link.create', 'link', 'c' || g, 'req-1', '{}'::jsonb
FROM generate_series(1, 10000) AS g;

INSERT INTO webhooks (tenant_id, url, events, secret_ciphertext, secret_prefix, enabled, disabled_reason, disabled_at)
SELECT (g % 200) + 1,
       'https://hooks.example.com/' || g,
       ARRAY['link.created', 'link.updated'],
       decode(repeat('ab', 40), 'hex'),
       'whsec_aabbcc',
       (g % 2 = 0),
       CASE WHEN g % 2 = 0 THEN '' ELSE 'manual' END,
       CASE WHEN g % 2 = 0 THEN NULL ELSE now() END
FROM generate_series(1, 1000) AS g;

INSERT INTO webhook_deliveries (tenant_id, webhook_id, job_id, delivery_id, event, attempt, status, duration_ms)
SELECT (g % 200) + 1, (g % 500) + 1, g, 'dlv_' || g, 'link.created', 1, 'success', 15
FROM generate_series(1, 30000) AS g;

ANALYZE links;
ANALYZE memberships;
ANALYZE audit_logs;
ANALYZE webhooks;
ANALYZE webhook_deliveries;
`); err != nil {
		t.Fatalf("seed synthetic RLS data: %v", err)
	}

	targetTenant := tenantID + 6
	err := store.withTenantReadTx(ctx, targetTenant, func(tx *sql.Tx) error {
		linksQuery, linksArgs, err := buildListLinksQuery(link.SortID, nil, targetTenant, 20, "", "", true)
		if err != nil {
			return err
		}
		linksPlan := explainTxQuery(t, ctx, tx, linksQuery, linksArgs...)
		if !strings.Contains(linksPlan, "idx_links_tenant_id") || strings.Contains(linksPlan, "Seq Scan on links") {
			t.Errorf("links list query missed idx_links_tenant_id under RLS:\n%s", linksPlan)
		}

		auditPlan := explainTxQuery(t, ctx, tx, listAuditLogsFirstSQL, targetTenant, 20)
		if !strings.Contains(auditPlan, "_tenant_id_id_idx") || !strings.Contains(auditPlan, "Index Scan using audit_logs_") || strings.Contains(auditPlan, "Seq Scan on audit_logs") {
			t.Errorf("audit_logs list query missed partition tenant_id index under RLS:\n%s", auditPlan)
		}

		hooksPlan := explainTxQuery(t, ctx, tx, `SELECT id FROM webhooks WHERE tenant_id = $1 AND enabled = TRUE AND 'link.created' = ANY(events)`, targetTenant)
		if !strings.Contains(hooksPlan, "idx_webhooks_tenant_enabled") || strings.Contains(hooksPlan, "Seq Scan on webhooks") {
			t.Errorf("webhooks fanout query missed idx_webhooks_tenant_enabled under RLS:\n%s", hooksPlan)
		}

		dlvPlan := explainTxQuery(t, ctx, tx, listWebhookDeliveriesFirstSQL, targetTenant, 7, 20)
		if !strings.Contains(dlvPlan, "idx_webhook_deliveries_tenant_webhook_id") || strings.Contains(dlvPlan, "Seq Scan on webhook_deliveries") {
			t.Errorf("webhook_deliveries query missed idx_webhook_deliveries_tenant_webhook_id under RLS:\n%s", dlvPlan)
		}

		memPlan := explainTxQuery(t, ctx, tx, listMembersSQL, targetTenant)
		if !strings.Contains(memPlan, "idx_memberships_tenant_id") || strings.Contains(memPlan, "Seq Scan on memberships") {
			t.Errorf("memberships list query missed idx_memberships_tenant_id under RLS:\n%s", memPlan)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withTenantReadTx: %v", err)
	}
}

func TestWithTenantTxGuardrails(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	for _, badTenantID := range []int64{0, -1} {
		calls := 0
		err := store.withTenantTx(ctx, badTenantID, nil, func(*sql.Tx) error {
			calls++
			return nil
		})
		if err == nil {
			t.Fatalf("withTenantTx(%d) error = nil, want error", badTenantID)
		}
		if calls != 0 {
			t.Fatalf("withTenantTx(%d) callback calls = %d, want 0", badTenantID, calls)
		}
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	calls := 0
	err := store.withTenantTx(canceledCtx, tenantID, nil, func(*sql.Tx) error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("withTenantTx(canceled) error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("withTenantTx(canceled) callback calls = %d, want 0", calls)
	}

	for _, table := range []string{"links", "api_keys", "memberships", "invitations", "audit_logs", "webhooks", "webhook_deliveries"} {
		var rlsEnabled, rlsForced bool
		if err := store.db.QueryRowContext(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = to_regclass($1)`,
			"public."+table,
		).Scan(&rlsEnabled, &rlsForced); err != nil {
			t.Fatalf("inspect pg_class %s: %v", table, err)
		}
		if !rlsEnabled || rlsForced {
			t.Fatalf("%s RLS flags = (enabled=%v, forced=%v), want (true, false)", table, rlsEnabled, rlsForced)
		}
	}
}
