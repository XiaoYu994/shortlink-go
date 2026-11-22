package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"shortlink/internal/link"
)

func TestStatsResponseZeroFill(t *testing.T) {
	from := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	var gotFrom, gotTo time.Time
	admin := &stubAdmin{statsFn: func(_ context.Context, tenantID int64, code string, f, tt time.Time) (*link.Stats, error) {
		if tenantID != 1 || code != "AbC123xy" {
			t.Errorf("Stats(%d, %q), want (1, AbC123xy)", tenantID, code)
		}
		gotFrom, gotTo = f, tt
		return &link.Stats{
			Code:           code,
			TotalClicks:    100,
			RangeClicks:    7,
			From:           from,
			To:             to,
			Clicks:         []link.DayClicks{{Day: from, Clicks: 5}, {Day: to, Clicks: 2}},
			Referrers:      []link.DimensionCount{{Value: "news.ycombinator.com", Clicks: 4}},
			Devices:        []link.DimensionCount{{Value: "mobile", Clicks: 6}},
			Clients:        []link.DimensionCount{{Value: "bot", Clicks: 1}},
			DailyVisitors:  []link.DayVisitors{{Day: from, Visitors: 3}},
			UniqueVisitors: 3,
		}, nil
	}}
	handler := newAdminTestServer(admin).Routes()

	rec := doAdminRequest(t, handler, http.MethodGet,
		"/api/v1/links/AbC123xy/stats?from=2026-11-08T00:00:00Z&to=2026-11-10T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Errorf("range = %v..%v, want %v..%v", gotFrom, gotTo, from, to)
	}

	var resp linkStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Clicks) != 3 || len(resp.DailyVisitors) != 3 {
		t.Fatalf("days = %d/%d, want 3/3", len(resp.Clicks), len(resp.DailyVisitors))
	}
	if resp.Clicks[0].Clicks != 5 || resp.Clicks[1].Clicks != 0 || resp.Clicks[2].Clicks != 2 {
		t.Errorf("clicks = %+v, want 5,0,2", resp.Clicks)
	}
	if resp.DailyVisitors[0].Visitors != 3 || resp.DailyVisitors[1].Visitors != 0 {
		t.Errorf("visitors = %+v, want 3,0,...", resp.DailyVisitors)
	}
	if len(resp.Devices) != 5 || len(resp.Clients) != 4 {
		t.Fatalf("device/client categories = %d/%d, want 5/4", len(resp.Devices), len(resp.Clients))
	}
	if resp.Devices[0].Device != "desktop" || resp.Devices[0].Clicks != 0 ||
		resp.Devices[1].Device != "mobile" || resp.Devices[1].Clicks != 6 {
		t.Errorf("devices = %+v, want desktop 0, mobile 6", resp.Devices)
	}
	if resp.Clients[0].Client != "browser" || resp.Clients[1].Client != "bot" || resp.Clients[1].Clicks != 1 {
		t.Errorf("clients = %+v, want browser 0, bot 1", resp.Clients)
	}
	if len(resp.Referrers) != 1 || resp.Referrers[0].Referrer != "news.ycombinator.com" {
		t.Errorf("referrers = %+v", resp.Referrers)
	}
	if resp.UniqueVisitors != 3 || resp.TotalClicks != 100 || resp.RangeClicks != 7 {
		t.Errorf("totals = %+v, want total 100 range 7 unique 3", resp)
	}
	if resp.From != "2026-11-08T00:00:00Z" || resp.To != "2026-11-10T00:00:00Z" {
		t.Errorf("from/to = %q/%q", resp.From, resp.To)
	}
}

func TestStatsDefaultRange(t *testing.T) {
	admin := &stubAdmin{statsFn: func(_ context.Context, _ int64, _ string, from, to time.Time) (*link.Stats, error) {
		if got := to.Sub(from); got != 7*24*time.Hour {
			t.Errorf("range = %v, want 7 days", got)
		}
		if to.Hour() != 0 || to.Minute() != 0 || to.Location() != time.UTC {
			t.Errorf("to = %v, want UTC midnight", to)
		}
		return &link.Stats{From: from, To: to}, nil
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).Routes(), http.MethodGet, "/api/v1/links/AbC123xy/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

func TestStatsBadRange(t *testing.T) {
	admin := &stubAdmin{statsFn: func(context.Context, int64, string, time.Time, time.Time) (*link.Stats, error) {
		t.Fatal("store was called for an invalid range")
		return nil, nil
	}}
	handler := newAdminTestServer(admin).Routes()

	tests := []struct{ name, query, errSub string }{
		{"invalid from", "?from=2026-11-01", "invalid from"},
		{"invalid to", "?to=tomorrow", "invalid to"},
		{"from after to", "?from=2026-11-10T00:00:00Z&to=2026-11-08T00:00:00Z", "from must be before to"},
		{"same day", "?from=2026-11-08T00:00:00Z&to=2026-11-08T00:00:00Z", "from must be before to"},
		{"too wide", "?from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z", "must not exceed 90 days"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doAdminRequest(t, handler, http.MethodGet, "/api/v1/links/AbC123xy/stats"+tt.query, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.errSub) {
				t.Errorf("body = %s, want substring %q", rec.Body.String(), tt.errSub)
			}
		})
	}
}

func TestStatsNotFound(t *testing.T) {
	admin := &stubAdmin{statsFn: func(context.Context, int64, string, time.Time, time.Time) (*link.Stats, error) {
		return nil, link.ErrNotFound
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).Routes(), http.MethodGet, "/api/v1/links/AbC123xy/stats", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
