package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

func TestStatsAggregates(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	otherTenant := insertTenant(t, store, "other")

	l := createLink(t, store, tenantID, "https://example.com/stats")
	day1 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	visitorA := []byte("0123456789abcdef")
	visitorB := []byte("fedcba9876543210")

	clicks := []click.Click{
		{LinkID: l.ID, At: day1, Referrer: "a.example", Device: click.DeviceMobile, Client: click.ClientBrowser, VisitorHash: visitorA},
		{LinkID: l.ID, At: day1, Referrer: "a.example", Device: click.DeviceMobile, Client: click.ClientBrowser, VisitorHash: visitorB},
		{LinkID: l.ID, At: day1, Referrer: "b.example", Device: click.DeviceDesktop, Client: click.ClientBrowser, VisitorHash: visitorA},
		{LinkID: l.ID, At: day2, Referrer: "b.example", Device: click.DeviceBot, Client: click.ClientBot, VisitorHash: visitorA},
		{LinkID: l.ID, At: day2, Referrer: "b.example", Device: click.DeviceBot, Client: click.ClientBot, VisitorHash: visitorA},
	}
	if err := store.RecordClicks(ctx, clicks); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	from := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	stats, err := store.Stats(ctx, tenantID, l.Code, from, to)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalClicks != 5 || stats.RangeClicks != 5 {
		t.Errorf("total/range clicks = %d/%d, want 5/5", stats.TotalClicks, stats.RangeClicks)
	}
	if len(stats.Clicks) != 2 || !stats.Clicks[0].Day.Equal(from) || stats.Clicks[0].Clicks != 3 ||
		stats.Clicks[1].Day.Format("2006-01-02") != "2026-11-09" || stats.Clicks[1].Clicks != 2 {
		t.Errorf("clicks = %+v, want 3 on day 1 and 2 on day 2", stats.Clicks)
	}
	if stats.UniqueVisitors != 3 {
		t.Errorf("unique visitors = %d, want 3 (2 + 1)", stats.UniqueVisitors)
	}
	if len(stats.DailyVisitors) != 2 || stats.DailyVisitors[0].Visitors != 2 || stats.DailyVisitors[1].Visitors != 1 {
		t.Errorf("daily visitors = %+v, want 2 and 1", stats.DailyVisitors)
	}
	if len(stats.Referrers) != 2 || stats.Referrers[0].Value != "b.example" || stats.Referrers[0].Clicks != 3 ||
		stats.Referrers[1].Value != "a.example" || stats.Referrers[1].Clicks != 2 {
		t.Errorf("referrers = %+v, want b.example 3, a.example 2", stats.Referrers)
	}
	if len(stats.Devices) != 3 || stats.Devices[0].Value != click.DeviceBot || stats.Devices[0].Clicks != 2 {
		t.Errorf("devices = %+v, want bot 2 first", stats.Devices)
	}
	if len(stats.Clients) != 2 || stats.Clients[0].Value != click.ClientBrowser || stats.Clients[0].Clicks != 3 {
		t.Errorf("clients = %+v, want browser 3 first", stats.Clients)
	}

	// 其他租户和不存在的短码都返回 ErrNotFound。
	if _, err := store.Stats(ctx, otherTenant, l.Code, from, to); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("other tenant error = %v, want ErrNotFound", err)
	}
	if _, err := store.Stats(ctx, tenantID, "missing1", from, to); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("missing link error = %v, want ErrNotFound", err)
	}

	// 时间范围外没有行，统计仍返回链接的终身累计。
	empty, err := store.Stats(ctx, tenantID, l.Code,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Stats empty range: %v", err)
	}
	if empty.TotalClicks != 5 || empty.RangeClicks != 0 || len(empty.Clicks) != 0 || empty.UniqueVisitors != 0 {
		t.Errorf("empty range = %+v, want total 5 and no rows", empty)
	}
}

func TestStatsReferrersTopTen(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	l := createLink(t, store, tenantID, "https://example.com/top")

	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var clicks []click.Click
	for i := range 12 {
		referrer := fmt.Sprintf("site-%02d.example", i)
		n := 1
		if i == 11 {
			n = 5 // site-11 最容易排第一
		}
		for range n {
			clicks = append(clicks, click.Click{
				LinkID: l.ID, At: day, Referrer: referrer,
				Device: click.DeviceDesktop, Client: click.ClientBrowser,
			})
		}
	}
	if err := store.RecordClicks(ctx, clicks); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	stats, err := store.Stats(ctx, tenantID, l.Code, day, day)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Referrers) != 10 {
		t.Fatalf("referrers = %d, want 10", len(stats.Referrers))
	}
	if stats.Referrers[0].Value != "site-11.example" || stats.Referrers[0].Clicks != 5 {
		t.Errorf("top referrer = %+v, want site-11.example 5", stats.Referrers[0])
	}
	// 其余都是 1 次，按取值升序，site-00 到 site-08。
	for i, got := range stats.Referrers[1:] {
		want := fmt.Sprintf("site-%02d.example", i)
		if got.Value != want || got.Clicks != 1 {
			t.Errorf("referrer[%d] = %+v, want %s 1", i+1, got, want)
		}
	}
}

func TestStatsIncludesExpiredLink(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	l := createLink(t, store, tenantID, "https://example.com/expired-stats")

	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if err := store.RecordClicks(ctx, []click.Click{{
		LinkID: l.ID, At: day, Referrer: "example.com",
		Device: click.DeviceDesktop, Client: click.ClientBrowser,
	}}); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE `+linksTable+` SET expires_at = now() - interval '1 minute' WHERE id = $1`, l.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}

	stats, err := store.Stats(ctx, tenantID, l.Code, day, day)
	if err != nil {
		t.Fatalf("Stats expired link: %v", err)
	}
	if stats.TotalClicks != 1 || stats.RangeClicks != 1 {
		t.Errorf("total/range = %d/%d, want 1/1", stats.TotalClicks, stats.RangeClicks)
	}
}
