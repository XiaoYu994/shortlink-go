package postgres

import (
	"context"
	"testing"
	"time"

	"shortlink/internal/click"
)

func TestRecordClicksWritesAggregates(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	first := createLink(t, store, tenantID, "https://example.com/clicks-1")
	second := createLink(t, store, tenantID, "https://example.com/clicks-2")

	day1 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	visitorA := []byte("0123456789abcdef")
	visitorB := []byte("fedcba9876543210")

	clicks := []click.Click{
		{LinkID: first.ID, At: day1, Referrer: "news.ycombinator.com", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: visitorA},
		// 同一访客同一天重复访问：点击数加两次，访客只算一次。
		{LinkID: first.ID, At: day1.Add(time.Hour), Referrer: "news.ycombinator.com", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: visitorA},
		{LinkID: first.ID, At: day1, Referrer: "example.com", Device: click.DeviceMobile, Client: click.ClientBrowser, VisitorHash: visitorB},
		{LinkID: first.ID, At: day2, Referrer: click.ReferrerDirect, Device: click.DeviceBot, Client: click.ClientBot, VisitorHash: visitorA},
		{LinkID: second.ID, At: day1, Referrer: click.ReferrerOther, Device: click.DeviceOther, Client: click.ClientOther},
		// 已删除或不存在的链接不产生统计行。
		{LinkID: 999_999, At: day1, Referrer: "gone.example", Device: click.DeviceDesktop, Client: click.ClientBrowser},
	}
	if err := store.RecordClicks(ctx, clicks); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	assertLinkClicks(t, store, first.ID, 4)
	assertLinkClicks(t, store, second.ID, 1)

	assertDailyClicks(t, store, first.ID, "2026-11-10", 3)
	assertDailyClicks(t, store, first.ID, "2026-11-11", 1)
	assertDailyClicks(t, store, second.ID, "2026-11-10", 1)

	assertDimensionClicks(t, store, first.ID, "2026-11-10", "referrer", "news.ycombinator.com", 2)
	assertDimensionClicks(t, store, first.ID, "2026-11-10", "referrer", "example.com", 1)
	assertDimensionClicks(t, store, first.ID, "2026-11-11", "referrer", click.ReferrerDirect, 1)
	assertDimensionClicks(t, store, first.ID, "2026-11-10", "device", click.DeviceDesktop, 2)
	assertDimensionClicks(t, store, first.ID, "2026-11-10", "device", click.DeviceMobile, 1)
	assertDimensionClicks(t, store, first.ID, "2026-11-11", "device", click.DeviceBot, 1)
	assertDimensionClicks(t, store, first.ID, "2026-11-10", "client", click.ClientBrowser, 3)
	assertDimensionClicks(t, store, first.ID, "2026-11-11", "client", click.ClientBot, 1)

	assertVisitorRows(t, store, first.ID, "2026-11-10", 2)
	assertVisitorRows(t, store, first.ID, "2026-11-11", 1)
	assertVisitorRows(t, store, second.ID, "2026-11-10", 0)

	// 第二批继续累加，不覆盖已有行。
	if err := store.RecordClicks(ctx, []click.Click{
		{LinkID: first.ID, At: day1, Referrer: "news.ycombinator.com", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: visitorA},
	}); err != nil {
		t.Fatalf("second RecordClicks: %v", err)
	}
	assertLinkClicks(t, store, first.ID, 5)
	assertDailyClicks(t, store, first.ID, "2026-11-10", 4)
	assertDimensionClicks(t, store, first.ID, "2026-11-10", "referrer", "news.ycombinator.com", 3)

	// 删除链接后，新批次由 JOIN links 过滤，不再写统计。
	if err := store.Delete(ctx, tenantID, second.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.RecordClicks(ctx, []click.Click{{
		LinkID: second.ID, At: day1, Referrer: "late.example", Device: click.DeviceDesktop, Client: click.ClientBrowser,
	}}); err != nil {
		t.Fatalf("RecordClicks after delete: %v", err)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM link_dimension_daily WHERE link_id = $1`, second.ID).Scan(&rows); err != nil {
		t.Fatalf("count deleted link dimensions: %v", err)
	}
	if rows != 0 {
		t.Errorf("deleted link dimension rows = %d, want 0", rows)
	}
}

func TestRecordClicksIgnoresInvalidDimensions(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	l := createLink(t, store, tenantID, "https://example.com/invalid")

	longValue := make([]byte, 256)
	for i := range longValue {
		longValue[i] = 'a'
	}
	if err := store.RecordClicks(ctx, []click.Click{{
		LinkID:      l.ID,
		At:          time.Now().UTC(),
		Referrer:    string(longValue),
		Device:      "",
		Client:      click.ClientBrowser,
		VisitorHash: []byte("short"),
	}}); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	assertLinkClicks(t, store, l.ID, 1)
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM link_dimension_daily WHERE link_id = $1`, l.ID).Scan(&rows); err != nil {
		t.Fatalf("count dimensions: %v", err)
	}
	if rows != 1 {
		t.Errorf("dimension rows = %d, want 1 (only client)", rows)
	}
	var visitors int
	if err := store.db.QueryRow(`SELECT count(*) FROM link_visitors_daily WHERE link_id = $1`, l.ID).Scan(&visitors); err != nil {
		t.Fatalf("count visitors: %v", err)
	}
	if visitors != 0 {
		t.Errorf("visitor rows = %d, want 0 for invalid hash", visitors)
	}
}

func assertLinkClicks(t *testing.T, store *Store, id, want int64) {
	t.Helper()
	var got int64
	if err := store.db.QueryRow(`SELECT clicks FROM `+linksTable+` WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("select clicks: %v", err)
	}
	if got != want {
		t.Errorf("links.clicks[%d] = %d, want %d", id, got, want)
	}
}

func assertDailyClicks(t *testing.T, store *Store, linkID int64, day string, want int64) {
	t.Helper()
	var got int64
	err := store.db.QueryRow(`
SELECT clicks FROM link_clicks_daily WHERE link_id = $1 AND day = $2::date`, linkID, day).Scan(&got)
	if err != nil {
		t.Fatalf("select daily clicks: %v", err)
	}
	if got != want {
		t.Errorf("daily clicks[%d %s] = %d, want %d", linkID, day, got, want)
	}
}

func assertDimensionClicks(t *testing.T, store *Store, linkID int64, day, dimension, value string, want int64) {
	t.Helper()
	var got int64
	err := store.db.QueryRow(`
SELECT clicks FROM link_dimension_daily
WHERE link_id = $1 AND day = $2::date AND dimension = $3 AND value = $4`,
		linkID, day, dimension, value).Scan(&got)
	if err != nil {
		t.Fatalf("select dimension clicks: %v", err)
	}
	if got != want {
		t.Errorf("dimension[%d %s %s=%s] = %d, want %d", linkID, day, dimension, value, got, want)
	}
}

func assertVisitorRows(t *testing.T, store *Store, linkID int64, day string, want int) {
	t.Helper()
	var got int
	err := store.db.QueryRow(`
SELECT count(*) FROM link_visitors_daily WHERE link_id = $1 AND day = $2::date`, linkID, day).Scan(&got)
	if err != nil {
		t.Fatalf("count visitor rows: %v", err)
	}
	if got != want {
		t.Errorf("visitor rows[%d %s] = %d, want %d", linkID, day, got, want)
	}
}
