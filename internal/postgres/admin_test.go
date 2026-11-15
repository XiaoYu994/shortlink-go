package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

func insertTenant(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var id int64
	if err := store.db.QueryRow(`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("insert tenant %q: %v", name, err)
	}
	return id
}

func createLink(t *testing.T, store *Store, tenantID int64, rawURL string) *link.Link {
	t.Helper()
	l := mustLink(t, rawURL, tenantID)
	if err := store.Create(context.Background(), l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return l
}

func setClicks(t *testing.T, store *Store, id, clicks int64) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE `+linksTable+` SET clicks = $2 WHERE id = $1`, id, clicks); err != nil {
		t.Fatalf("set clicks: %v", err)
	}
}

func setTodayClicks(t *testing.T, store *Store, tenantID, linkID, clicks int64) {
	t.Helper()
	if _, err := store.db.Exec(`
INSERT INTO link_clicks_daily (tenant_id, link_id, day, clicks)
VALUES ($1, $2, (now() AT TIME ZONE 'UTC')::date, $3)`, tenantID, linkID, clicks); err != nil {
		t.Fatalf("set today clicks: %v", err)
	}
}

func TestAdminGetForTenant(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	otherTenant := insertTenant(t, store, "other")

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	l, err := link.NewLink("https://example.com/tenant", &expires)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	l.TenantID = tenantID
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	setClicks(t, store, l.ID, 9)
	setTodayClicks(t, store, tenantID, l.ID, 2)

	got, err := store.GetForTenant(ctx, tenantID, l.Code)
	if err != nil {
		t.Fatalf("GetForTenant: %v", err)
	}
	if got.ID != l.ID || got.Clicks != 9 || got.ClicksToday != 2 || got.ExpiresAt == nil {
		t.Errorf("GetForTenant = %+v, want link 9 clicks, 2 today, expiry", got)
	}

	if _, err := store.GetForTenant(ctx, otherTenant, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("other tenant error = %v, want ErrNotFound", err)
	}

	// 过期链接在管理视角仍然可见，跳转视角查不到。
	if _, err := store.db.Exec(`UPDATE `+linksTable+` SET expires_at = now() - interval '1 minute' WHERE id = $1`, l.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
	if _, err := store.GetForTenant(ctx, tenantID, l.Code); err != nil {
		t.Errorf("GetForTenant expired link: %v", err)
	}
	if _, err := store.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode expired link error = %v, want ErrNotFound", err)
	}
}

func TestAdminListSortAndCursor(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	otherTenant := insertTenant(t, store, "other")

	first := createLink(t, store, tenantID, "https://example.com/1")
	second := createLink(t, store, tenantID, "https://example.com/2")
	third := createLink(t, store, tenantID, "https://example.com/3")
	createLink(t, store, otherTenant, "https://example.com/other")

	setClicks(t, store, first.ID, 5)
	setClicks(t, store, second.ID, 30)
	setClicks(t, store, third.ID, 10)
	setTodayClicks(t, store, tenantID, first.ID, 1)
	setTodayClicks(t, store, tenantID, second.ID, 7)
	setTodayClicks(t, store, tenantID, third.ID, 3)

	// 默认按 id 降序，游标稳定。
	page1, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 2, Sort: link.SortID})
	if err != nil {
		t.Fatalf("List id page 1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != third.ID || page1[1].ID != second.ID {
		t.Fatalf("id page 1 = %+v, want IDs %d,%d", page1, third.ID, second.ID)
	}
	page2, err := store.List(ctx, link.ListOptions{
		TenantID: tenantID, Limit: 2, Sort: link.SortID,
		Cursor: &link.ListCursor{ID: page1[1].ID},
	})
	if err != nil {
		t.Fatalf("List id page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != first.ID {
		t.Fatalf("id page 2 = %+v, want ID %d", page2, first.ID)
	}

	// 历史累计排序。
	byClicks, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 10, Sort: link.SortClicks})
	if err != nil {
		t.Fatalf("List clicks: %v", err)
	}
	if len(byClicks) != 3 || byClicks[0].ID != second.ID || byClicks[1].ID != third.ID || byClicks[2].ID != first.ID {
		t.Fatalf("clicks order = %+v, want IDs %d,%d,%d", byClicks, second.ID, third.ID, first.ID)
	}
	if byClicks[1].ClicksToday != 3 {
		t.Errorf("ClicksToday = %d, want 3", byClicks[1].ClicksToday)
	}
	// 用真实的 (clicks, id) 游标翻页。
	byClicksPage1, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 1, Sort: link.SortClicks})
	if err != nil {
		t.Fatalf("List clicks page 1: %v", err)
	}
	byClicksPage2, err := store.List(ctx, link.ListOptions{
		TenantID: tenantID, Limit: 2, Sort: link.SortClicks,
		Cursor: &link.ListCursor{ID: byClicksPage1[0].ID, Clicks: byClicksPage1[0].Clicks},
	})
	if err != nil {
		t.Fatalf("List clicks page 2: %v", err)
	}
	if len(byClicksPage2) != 2 || byClicksPage2[0].ID != third.ID || byClicksPage2[1].ID != first.ID {
		t.Fatalf("clicks page 2 = %+v, want IDs %d,%d", byClicksPage2, third.ID, first.ID)
	}

	// 今日排序。
	byToday, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 10, Sort: link.SortClicksToday})
	if err != nil {
		t.Fatalf("List clicks_today: %v", err)
	}
	if len(byToday) != 3 || byToday[0].ID != second.ID || byToday[1].ID != third.ID || byToday[2].ID != first.ID {
		t.Fatalf("clicks_today order = %+v, want IDs %d,%d,%d", byToday, second.ID, third.ID, first.ID)
	}
	todayPage2, err := store.List(ctx, link.ListOptions{
		TenantID: tenantID, Limit: 2, Sort: link.SortClicksToday,
		Cursor: &link.ListCursor{ID: byToday[1].ID, ClicksToday: byToday[1].ClicksToday},
	})
	if err != nil {
		t.Fatalf("List clicks_today page 2: %v", err)
	}
	if len(todayPage2) != 1 || todayPage2[0].ID != first.ID {
		t.Fatalf("clicks_today page 2 = %+v, want ID %d", todayPage2, first.ID)
	}
}

func TestAdminUpdate(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	otherTenant := insertTenant(t, store, "other")

	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	l, err := link.NewLink("https://example.com/old", &future)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	l.TenantID = tenantID
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newURL := "https://example.com/new"
	updated, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL})
	if err != nil {
		t.Fatalf("Update URL: %v", err)
	}
	if updated.URL != newURL {
		t.Errorf("URL = %q, want %q", updated.URL, newURL)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(future) {
		t.Errorf("ExpiresAt = %v, want unchanged %v", updated.ExpiresAt, future)
	}

	cleared, err := store.Update(ctx, tenantID, l.Code, link.Update{ClearExpiresAt: true})
	if err != nil {
		t.Fatalf("Update clear expiry: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", cleared.ExpiresAt)
	}

	newFuture := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	set, err := store.Update(ctx, tenantID, l.Code, link.Update{ExpiresAt: &newFuture})
	if err != nil {
		t.Fatalf("Update set expiry: %v", err)
	}
	if set.ExpiresAt == nil || !set.ExpiresAt.Equal(newFuture) {
		t.Errorf("ExpiresAt = %v, want %v", set.ExpiresAt, newFuture)
	}

	if _, err := store.Update(ctx, otherTenant, l.Code, link.Update{URL: &newURL}); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("other tenant update error = %v, want ErrNotFound", err)
	}
	if _, err := store.Update(ctx, tenantID, "missing1", link.Update{URL: &newURL}); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("missing update error = %v, want ErrNotFound", err)
	}
}

func TestAdminDeleteRemovesStats(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	otherTenant := insertTenant(t, store, "other")

	l := createLink(t, store, tenantID, "https://example.com/delete")
	day := time.Now().UTC()
	if err := store.RecordClicks(ctx, []click.Click{{
		LinkID: l.ID, At: day, Referrer: "example.com", Device: click.DeviceDesktop, Client: click.ClientBrowser,
		VisitorHash: []byte("0123456789abcdef"),
	}}); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}

	if err := store.Delete(ctx, otherTenant, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("other tenant delete error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, tenantID, l.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetForTenant(ctx, tenantID, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetForTenant after delete error = %v, want ErrNotFound", err)
	}
	for _, table := range []string{"link_clicks_daily", "link_dimension_daily", "link_visitors_daily"} {
		var n int
		if err := store.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE link_id = $1`, l.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d, want 0", table, n)
		}
	}
	if err := store.Delete(ctx, tenantID, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}
