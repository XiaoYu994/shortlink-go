package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shortlink/internal/link"
)

func boolPtr(v bool) *bool { return &v }

/*
TestLinkStatesJumpAndAdminView 对过期、停用、封禁的八种组合验证两件事：
跳转查询能查到当且仅当管理视角读出的 Servable 为真；
管理视角照常返回，并带上对应的状态字段。
*/
func TestLinkStatesJumpAndAdminView(t *testing.T) {
	tests := []struct {
		name     string
		expired  bool
		disabled bool
		banned   bool
	}{
		{name: "active"},
		{name: "expired", expired: true},
		{name: "disabled", disabled: true},
		{name: "banned", banned: true},
		{name: "expired and disabled", expired: true, disabled: true},
		{name: "expired and banned", expired: true, banned: true},
		{name: "disabled and banned", disabled: true, banned: true},
		{name: "expired, disabled and banned", expired: true, disabled: true, banned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, tenantID := newTestStore(t)
			ctx := testAuditCtx(context.Background())
			l := createLink(t, store, tenantID, "https://example.com/states")

			if tt.expired {
				if _, err := store.db.ExecContext(ctx,
					`UPDATE links SET expires_at = now() - interval '1 minute' WHERE id = $1`, l.ID); err != nil {
					t.Fatalf("expire link: %v", err)
				}
			}
			if tt.disabled {
				if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(true)}); err != nil {
					t.Fatalf("disable link: %v", err)
				}
			}
			if tt.banned {
				if _, err := store.Ban(ctx, l.Code, link.BanReasonPhishing, link.BanSourceOperator); err != nil {
					t.Fatalf("ban link: %v", err)
				}
			}

			wantServable := !tt.expired && !tt.disabled && !tt.banned
			now := time.Now()
			if got, err := store.GetByCode(ctx, l.Code); wantServable {
				if err != nil {
					t.Fatalf("GetByCode: %v", err)
				}
				if !got.Servable(now) {
					t.Errorf("GetByCode returned a link that is not servable: %+v", got)
				}
			} else if !errors.Is(err, link.ErrNotFound) {
				t.Errorf("GetByCode error = %v, want link.ErrNotFound", err)
			}

			admin, err := store.GetForTenant(ctx, tenantID, l.Code)
			if err != nil {
				t.Fatalf("GetForTenant: %v", err)
			}
			if (admin.DisabledAt != nil) != tt.disabled {
				t.Errorf("DisabledAt = %v, want disabled=%v", admin.DisabledAt, tt.disabled)
			}
			if (admin.Ban != nil) != tt.banned {
				t.Errorf("Ban = %+v, want banned=%v", admin.Ban, tt.banned)
			}
			if (admin.ExpiresAt != nil) != tt.expired {
				t.Errorf("ExpiresAt = %v, want expired=%v", admin.ExpiresAt, tt.expired)
			}
			if tt.banned {
				if admin.Ban.Reason != link.BanReasonPhishing || admin.Ban.Source != link.BanSourceOperator {
					t.Errorf("Ban = %+v, want phishing/operator", admin.Ban)
				}
			}
			if got := admin.Servable(now); got != wantServable {
				t.Errorf("admin Servable = %v, want %v", got, wantServable)
			}
			wantStatus := link.StatusActive
			switch {
			case tt.banned:
				wantStatus = link.StatusBanned
			case tt.disabled:
				wantStatus = link.StatusDisabled
			case tt.expired:
				wantStatus = link.StatusExpired
			}
			if got := admin.Status(now); got != wantStatus {
				t.Errorf("admin Status = %q, want %q", got, wantStatus)
			}

			items, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 10, Sort: link.SortID})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(items) != 1 || items[0].Code != l.Code {
				t.Fatalf("List = %+v, want only %s", items, l.Code)
			}
			if got := items[0].Status(now); got != wantStatus {
				t.Errorf("List Status = %q, want %q", got, wantStatus)
			}
		})
	}
}

func TestUpdateDisabled(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	l := createLink(t, store, tenantID, "https://example.com/disable")

	first, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if first.DisabledAt == nil {
		t.Fatal("DisabledAt = nil after disable")
	}
	if _, err := store.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode while disabled error = %v, want ErrNotFound", err)
	}

	// 重复停用保留第一次的时间。
	second, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("disable again: %v", err)
	}
	if second.DisabledAt == nil || !second.DisabledAt.Equal(*first.DisabledAt) {
		t.Errorf("DisabledAt = %v, want unchanged %v", second.DisabledAt, first.DisabledAt)
	}

	// 停用期间仍然可以改 URL。
	newURL := "https://example.com/disabled-new"
	updated, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL})
	if err != nil {
		t.Fatalf("update URL while disabled: %v", err)
	}
	if updated.URL != newURL {
		t.Errorf("URL = %q, want %q", updated.URL, newURL)
	}
	if updated.DisabledAt == nil {
		t.Error("DisabledAt = nil, want disabled link to stay disabled")
	}

	// 启用后恢复跳转。
	enabled, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if enabled.DisabledAt != nil {
		t.Errorf("DisabledAt = %v, want nil", enabled.DisabledAt)
	}
	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode after enable: %v", err)
	}
	if got.URL != newURL {
		t.Errorf("GetByCode URL = %q, want %q", got.URL, newURL)
	}
}

func TestUpdateAndDeleteBanned(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	otherTenant := insertTenant(t, store, "other")
	l := createLink(t, store, tenantID, "https://example.com/banned-write")

	if _, err := store.Ban(ctx, l.Code, link.BanReasonMalware, link.BanSourceRisk); err != nil {
		t.Fatalf("Ban: %v", err)
	}

	newURL := "https://example.com/changed"
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); !errors.Is(err, link.ErrBanned) {
		t.Errorf("Update banned error = %v, want ErrBanned", err)
	}
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(true)}); !errors.Is(err, link.ErrBanned) {
		t.Errorf("Update banned disabled error = %v, want ErrBanned", err)
	}
	if err := store.Delete(ctx, tenantID, l.Code); !errors.Is(err, link.ErrBanned) {
		t.Errorf("Delete banned error = %v, want ErrBanned", err)
	}

	admin, err := store.GetForTenant(ctx, tenantID, l.Code)
	if err != nil {
		t.Fatalf("GetForTenant after rejected writes: %v", err)
	}
	if admin.URL != l.URL {
		t.Errorf("URL = %q, want unchanged %q", admin.URL, l.URL)
	}
	if admin.DisabledAt != nil {
		t.Errorf("DisabledAt = %v, want nil", admin.DisabledAt)
	}

	// 其他租户的链接不管封没封都还是 ErrNotFound。
	if _, err := store.Update(ctx, otherTenant, l.Code, link.Update{URL: &newURL}); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("other tenant update error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, otherTenant, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("other tenant delete error = %v, want ErrNotFound", err)
	}
}

func TestBanAndUnban(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	l, err := link.NewLink("https://example.com/ban", &future)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	l.TenantID = tenantID
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := store.Ban(ctx, l.Code, link.BanReasonPhishing, link.BanSourceOperator)
	if err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if first.Ban == nil {
		t.Fatal("Ban = nil after ban")
	}
	if first.Ban.Reason != link.BanReasonPhishing || first.Ban.Source != link.BanSourceOperator {
		t.Errorf("Ban = %+v, want phishing/operator", first.Ban)
	}
	if first.ExpiresAt == nil || !first.ExpiresAt.Equal(future) {
		t.Errorf("ExpiresAt = %v, want %v", first.ExpiresAt, future)
	}
	if _, err := store.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode banned error = %v, want ErrNotFound", err)
	}

	// 重复封禁保留第一次的时间、原因和来源。
	second, err := store.Ban(ctx, l.Code, link.BanReasonSpam, link.BanSourceRisk)
	if err != nil {
		t.Fatalf("Ban again: %v", err)
	}
	if second.Ban == nil {
		t.Fatal("Ban = nil after second ban")
	}
	if !second.Ban.At.Equal(first.Ban.At) || second.Ban.Reason != link.BanReasonPhishing || second.Ban.Source != link.BanSourceOperator {
		t.Errorf("Ban = %+v, want first values %+v", second.Ban, first.Ban)
	}

	// 停用状态直接写库（Update 对封禁链接只读），解封后链接应仍然不可跳转。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE links SET disabled_at = now() WHERE id = $1`, l.ID); err != nil {
		t.Fatalf("disable banned link: %v", err)
	}
	unbanned, err := store.Unban(ctx, l.Code)
	if err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if unbanned.Ban != nil {
		t.Errorf("Ban = %+v, want nil", unbanned.Ban)
	}
	if unbanned.DisabledAt == nil {
		t.Error("DisabledAt = nil, want disabled state preserved")
	}
	var cleared bool
	if err := store.db.QueryRowContext(ctx, `
SELECT banned_at IS NULL AND ban_reason IS NULL AND ban_source IS NULL
FROM links WHERE id = $1`, l.ID).Scan(&cleared); err != nil {
		t.Fatalf("check ban fields: %v", err)
	}
	if !cleared {
		t.Error("ban fields were not cleared")
	}
	if _, err := store.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode disabled after unban error = %v, want ErrNotFound", err)
	}

	// 没被封禁的链接解封是空操作，照样返回这一行。
	again, err := store.Unban(ctx, l.Code)
	if err != nil {
		t.Fatalf("Unban again: %v", err)
	}
	if again.Ban != nil || again.DisabledAt == nil {
		t.Errorf("Unban again = %+v, want empty ban and still disabled", again)
	}

	// 启用后恢复跳转。
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: boolPtr(false)}); err != nil {
		t.Fatalf("enable after unban: %v", err)
	}
	if _, err := store.GetByCode(ctx, l.Code); err != nil {
		t.Errorf("GetByCode after enable: %v", err)
	}

	// 短码不存在。
	if _, err := store.Ban(ctx, "missing1", link.BanReasonOther, link.BanSourceOperator); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("Ban missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.Unban(ctx, "missing1"); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("Unban missing error = %v, want ErrNotFound", err)
	}

	// 非法原因和来源在 Go 里就拒绝，不落库。
	if _, err := store.Ban(ctx, l.Code, link.BanReason("phishing2"), link.BanSourceOperator); err == nil {
		t.Error("Ban accepted an invalid reason")
	}
	if _, err := store.Ban(ctx, l.Code, link.BanReasonPhishing, link.BanSource("admin")); err == nil {
		t.Error("Ban accepted an invalid source")
	}
	admin, err := store.GetForTenant(ctx, tenantID, l.Code)
	if err != nil {
		t.Fatalf("GetForTenant: %v", err)
	}
	if admin.Ban != nil {
		t.Errorf("Ban = %+v after invalid calls, want nil", admin.Ban)
	}
}

/*
TestBanCheckConstraints 验证数据库 CHECK 兜底：
三个封禁字段必须同有同无，原因和来源只能是约定列表里的值。
*/
func TestBanCheckConstraints(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	l := createLink(t, store, tenantID, "https://example.com/constraints")
	now := time.Now()

	tests := []struct {
		name       string
		bannedAt   any
		reason     any
		source     any
		constraint string
	}{
		{"banned_at only", now, nil, nil, "ck_links_ban_fields"},
		{"reason without source", now, "phishing", nil, "ck_links_ban_fields"},
		{"reason and source without banned_at", nil, "phishing", "operator", "ck_links_ban_fields"},
		{"unknown reason", now, "phishing2", "operator", "ck_links_ban_reason"},
		{"empty reason", now, "", "operator", "ck_links_ban_reason"},
		{"unknown source", now, "phishing", "admin", "ck_links_ban_source"},
		{"empty source", now, "phishing", "", "ck_links_ban_source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, `
UPDATE links SET banned_at = $2, ban_reason = $3, ban_source = $4 WHERE id = $1`,
				l.ID, tt.bannedAt, tt.reason, tt.source)
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok {
				t.Fatalf("error = %v, want *pgconn.PgError", err)
			}
			if pgErr.Code != "23514" {
				t.Errorf("SQLSTATE = %q, want 23514", pgErr.Code)
			}
			if pgErr.ConstraintName != tt.constraint {
				t.Errorf("constraint = %q, want %q", pgErr.ConstraintName, tt.constraint)
			}

			admin, err := store.GetForTenant(ctx, tenantID, l.Code)
			if err != nil {
				t.Fatalf("GetForTenant after rejected update: %v", err)
			}
			if admin.Ban != nil {
				t.Errorf("Ban = %+v after rejected update, want nil", admin.Ban)
			}
		})
	}
}
