package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/job"
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

func createLink(t testing.TB, store *Store, tenantID int64, rawURL string) *link.Link {
	t.Helper()
	l := mustLink(t, rawURL, tenantID)
	if err := store.Create(testAuditCtx(context.Background()), l); err != nil {
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
	ctx := testAuditCtx(context.Background())
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
	ctx := testAuditCtx(context.Background())
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

/*
TestAdminListClicksTodayMixedZeroPagination 验证两段式今日点击排序（ADR 0022）：
覆盖同点击数按 id 降序消歧、同一页跨「今日有点击 -> 今日 0 点击」边界，以及纯 0 点击段继续翻页。
*/
func TestAdminListClicksTodayMixedZeroPagination(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	l1 := createLink(t, store, tenantID, "https://example.com/z1")
	l2 := createLink(t, store, tenantID, "https://example.com/c2")
	l3 := createLink(t, store, tenantID, "https://example.com/c3")
	l4 := createLink(t, store, tenantID, "https://example.com/z4")
	l5 := createLink(t, store, tenantID, "https://example.com/c5")
	l6 := createLink(t, store, tenantID, "https://example.com/z6")

	setTodayClicks(t, store, tenantID, l2.ID, 7)
	setTodayClicks(t, store, tenantID, l3.ID, 3)
	setTodayClicks(t, store, tenantID, l5.ID, 3)

	// 期望全量顺序：l2(7), l5(3), l3(3), l6(0), l4(0), l1(0)
	wantIDs := []int64{l2.ID, l5.ID, l3.ID, l6.ID, l4.ID, l1.ID}
	wantToday := []int64{7, 3, 3, 0, 0, 0}

	// 按每页 2 条翻完所有页：page1 纯有点击，page2 跨有点击与 0 点击，page3 纯 0 点击。
	var (
		gotIDs   []int64
		gotToday []int64
		cursor   *link.ListCursor
	)
	for range 4 {
		page, err := store.List(ctx, link.ListOptions{
			TenantID: tenantID, Limit: 2, Sort: link.SortClicksToday, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("List clicks_today cursor=%+v: %v", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			gotIDs = append(gotIDs, item.ID)
			gotToday = append(gotToday, item.ClicksToday)
		}
		last := page[len(page)-1]
		cursor = &link.ListCursor{ID: last.ID, ClicksToday: last.ClicksToday}
	}

	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("collected %d links (%v), want %d (%v)", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] || gotToday[i] != wantToday[i] {
			t.Errorf("pos %d = (id=%d today=%d), want (id=%d today=%d)",
				i, gotIDs[i], gotToday[i], wantIDs[i], wantToday[i])
		}
	}
}

func TestAdminUpdate(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
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

	// 验证 links.updated_at：元数据修改与状态切换推进时间，点击计数更新与幂等封禁不改动。
	var beforeUpdated time.Time
	if _, err := store.db.ExecContext(ctx, `UPDATE links SET updated_at = now() - interval '1 hour' WHERE id = $1`, l.ID); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM links WHERE id = $1`, l.ID).Scan(&beforeUpdated); err != nil {
		t.Fatalf("read backdated updated_at: %v", err)
	}
	if err := store.RecordClicks(ctx, []click.Click{{LinkID: l.ID, At: time.Now().UTC(), Referrer: "direct", Device: click.DeviceDesktop, Client: click.ClientBrowser}}); err != nil {
		t.Fatalf("RecordClicks: %v", err)
	}
	var afterClicks time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM links WHERE id = $1`, l.ID).Scan(&afterClicks); err != nil {
		t.Fatalf("read updated_at after clicks: %v", err)
	}
	if !afterClicks.Equal(beforeUpdated) {
		t.Errorf("RecordClicks changed updated_at: %v -> %v", beforeUpdated, afterClicks)
	}
	if _, err := store.Ban(ctx, l.Code, link.BanReasonSpam, link.BanSourceOperator); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	var afterBan time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM links WHERE id = $1`, l.ID).Scan(&afterBan); err != nil {
		t.Fatalf("read updated_at after ban: %v", err)
	}
	if !afterBan.After(beforeUpdated) {
		t.Errorf("Ban did not advance updated_at: before=%v after=%v", beforeUpdated, afterBan)
	}
	if _, err := store.Ban(ctx, l.Code, link.BanReasonPhishing, link.BanSourceRisk); err != nil {
		t.Fatalf("idempotent Ban: %v", err)
	}
	var afterIdempotentBan time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM links WHERE id = $1`, l.ID).Scan(&afterIdempotentBan); err != nil {
		t.Fatalf("read updated_at after idempotent ban: %v", err)
	}
	if !afterIdempotentBan.Equal(afterBan) {
		t.Errorf("idempotent Ban changed updated_at: %v -> %v", afterBan, afterIdempotentBan)
	}
	if _, err := store.Unban(ctx, l.Code); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	var afterUnban time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM links WHERE id = $1`, l.ID).Scan(&afterUnban); err != nil {
		t.Fatalf("read updated_at after unban: %v", err)
	}
	if afterUnban.Before(afterBan) {
		t.Errorf("Unban moved updated_at backwards: ban=%v unban=%v", afterBan, afterUnban)
	}
}

func TestAdminDeleteRemovesStats(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
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
	var jobKinds []string
	rows, err := store.db.QueryContext(ctx, `SELECT kind FROM jobs WHERE payload->>'code' = $1 OR (payload->>'link_id')::bigint = $2`, l.Code, l.ID)
	if err != nil {
		t.Fatalf("query delete jobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan job kind: %v", err)
		}
		jobKinds = append(jobKinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate delete jobs: %v", err)
	}
	if !slices.Contains(jobKinds, job.KindCacheReconcile) || !slices.Contains(jobKinds, job.KindLinkPurgeStats) {
		t.Fatalf("delete jobs = %v, want cache.reconcile and link.purge_stats", jobKinds)
	}
	for {
		n, err := store.PurgeLinkStatsBatch(ctx, l.ID, 100)
		if err != nil {
			t.Fatalf("PurgeLinkStatsBatch: %v", err)
		}
		if n < 100 {
			break
		}
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
