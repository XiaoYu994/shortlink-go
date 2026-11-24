package pgredirect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/link"
	"shortlink/internal/pgdb"
)

func openTestRedirectStore(t *testing.T) (*Store, *sql.DB, int64) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("refusing to truncate non-test database %q: name must end with _test", dbName)
	}

	ctx := context.Background()
	db, err := pgdb.Open(ctx, dsn, pgdb.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	release, err := pgdb.HoldTestDBLock(ctx, db)
	if err != nil {
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(release)

	if _, err := db.ExecContext(ctx, `TRUNCATE links, api_keys, tenants,
    link_clicks_daily, link_dimension_daily, link_visitors_daily,
    users, user_identities, memberships, sessions, invitations, jobs, audit_logs RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	var tenantID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return NewStore(db), db, tenantID
}

func TestGetByCodeAndStates(t *testing.T) {
	store, db, tenantID := openTestRedirectStore(t)
	ctx := context.Background()

	var activeID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO links (code, url, tenant_id) VALUES ('active01', 'https://example.com/active', $1) RETURNING id`,
		tenantID,
	).Scan(&activeID); err != nil {
		t.Fatalf("insert active link: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO links (code, url, tenant_id, expires_at) VALUES ('expir001', 'https://example.com/exp', $1, now() - interval '1 minute')`,
		tenantID,
	); err != nil {
		t.Fatalf("insert expired link: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO links (code, url, tenant_id, disabled_at) VALUES ('disab001', 'https://example.com/dis', $1, now())`,
		tenantID,
	); err != nil {
		t.Fatalf("insert disabled link: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO links (code, url, tenant_id, banned_at, ban_reason, ban_source) VALUES ('bannd001', 'https://example.com/ban', $1, now(), 'phishing', 'operator')`,
		tenantID,
	); err != nil {
		t.Fatalf("insert banned link: %v", err)
	}

	got, err := store.GetByCode(ctx, "active01")
	if err != nil {
		t.Fatalf("GetByCode active: %v", err)
	}
	if got.ID != activeID || got.Code != "active01" || got.URL != "https://example.com/active" || got.TenantID != tenantID {
		t.Fatalf("got = %+v", got)
	}
	for _, hidden := range []string{"missing1", "expir001", "disab001", "bannd001"} {
		if _, err := store.GetByCode(ctx, hidden); !errors.Is(err, link.ErrNotFound) {
			t.Errorf("GetByCode(%q) err = %v, want ErrNotFound", hidden, err)
		}
	}
}

func TestRecordClicksAggregates(t *testing.T) {
	store, db, tenantID := openTestRedirectStore(t)
	ctx := context.Background()

	var linkID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO links (code, url, tenant_id) VALUES ('click001', 'https://example.com/click', $1) RETURNING id`,
		tenantID,
	).Scan(&linkID); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	day := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	hash := []byte("0123456789abcdef")
	if err := store.RecordClicks(ctx, []click.Click{
		{LinkID: linkID, At: day, Referrer: "example.com", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: hash},
		{LinkID: linkID, At: day.Add(time.Minute), Referrer: "example.com", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: hash},
	}); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	var total, daily, visitors int64
	if err := db.QueryRowContext(ctx, `SELECT clicks FROM links WHERE id = $1`, linkID).Scan(&total); err != nil {
		t.Fatalf("select total clicks: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT clicks FROM link_clicks_daily WHERE link_id = $1 AND day = '2026-11-10'`, linkID).Scan(&daily); err != nil {
		t.Fatalf("select daily clicks: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM link_visitors_daily WHERE link_id = $1 AND day = '2026-11-10'`, linkID).Scan(&visitors); err != nil {
		t.Fatalf("count visitors: %v", err)
	}
	if total != 2 || daily != 2 || visitors != 1 {
		t.Fatalf("counts = (total=%d daily=%d visitors=%d), want (2, 2, 1)", total, daily, visitors)
	}
}

// BenchmarkAggregateClicks 只量 Go 里的去重聚合，不连数据库。
func BenchmarkAggregateClicks(b *testing.B) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	clicks := make([]click.Click, 0, 100)
	for i := range 100 {
		clicks = append(clicks, click.Click{
			LinkID:      int64(i%50 + 1),
			At:          at,
			Referrer:    "news.ycombinator.com",
			Device:      click.DeviceDesktop,
			Client:      click.ClientBrowser,
			VisitorHash: fmt.Appendf(nil, "visitor-%08d", i),
		})
	}
	b.ReportAllocs()
	for b.Loop() {
		aggregateClicks(clicks)
	}
}
