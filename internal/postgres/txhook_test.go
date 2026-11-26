package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/webhook"
)

func countJobsByKind(t *testing.T, s *Store, kind string) int {
	t.Helper()
	var n int
	var err error
	if kind == "" {
		err = s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n)
	} else {
		err = s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM jobs WHERE kind = $1`, kind).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count jobs(%q): %v", kind, err)
	}
	return n
}

func TestStoreNoHooksEnqueuesZeroJobs(t *testing.T) {
	store, tenantID := newTestStoreWithOptions(t)
	ctx := testAuditCtx(context.Background())

	// 即使表里存在启用中的 Webhook 订阅，未挂载 WebhookFanoutHook 时也绝不扇出任务。
	if _, err := store.CreateWebhook(ctx, webhook.CreateParams{
		TenantID: tenantID,
		URL:      "https://hooks.example.com/events",
		Events: []webhook.EventType{
			webhook.EventLinkCreated,
			webhook.EventLinkUpdated,
			webhook.EventLinkDeleted,
			webhook.EventLinkBanned,
			webhook.EventLinkUnbanned,
		},
		SecretCiphertext: bytes.Repeat([]byte{0xab}, 64),
		SecretPrefix:     "whsec_test",
	}); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	l := mustLink(t, "https://example.com/no-hooks", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	newURL := "https://example.com/no-hooks-v2"
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := store.Ban(ctx, l.Code, link.BanReasonSpam, link.BanSourceOperator); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if _, err := store.Unban(ctx, l.Code); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if err := store.Delete(ctx, tenantID, l.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID,
		Email:    "nohooks@example.com",
		Role:     identity.RoleMember,
	}); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	if got := countJobsByKind(t, store, ""); got != 0 {
		t.Fatalf("total jobs with no hooks = %d, want 0", got)
	}
	// 核心审计日志仍然必须完整落库。
	events, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenantID, Limit: 20})
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	if len(events) < 7 {
		t.Fatalf("audit events count = %d, want >= 7", len(events))
	}
}

func TestStoreIndividualTxHooks(t *testing.T) {
	t.Run("only risk scan hook", func(t *testing.T) {
		store, tenantID := newTestStoreWithOptions(t, WithTxHooks(RiskScanHook()))
		ctx := testAuditCtx(context.Background())

		l := mustLink(t, "https://example.com/risk-only", tenantID)
		if err := store.Create(ctx, l); err != nil {
			t.Fatalf("Create: %v", err)
		}
		disabled := true
		if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: &disabled}); err != nil {
			t.Fatalf("Update disabled: %v", err)
		}
		newURL := "https://example.com/risk-only-v2"
		if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
			t.Fatalf("Update url: %v", err)
		}
		if err := store.Delete(ctx, tenantID, l.Code); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if got := countJobsByKind(t, store, job.KindLinkRiskScan); got != 2 {
			t.Fatalf("risk_scan jobs = %d, want 2", got)
		}
		if got := countJobsByKind(t, store, ""); got != 2 {
			t.Fatalf("total jobs = %d, want 2 (no purge/webhook)", got)
		}
	})

	t.Run("only stats purge and email hooks", func(t *testing.T) {
		store, tenantID := newTestStoreWithOptions(t, WithTxHooks(StatsPurgeHook(), InvitationEmailHook()))
		ctx := testAuditCtx(context.Background())

		l := mustLink(t, "https://example.com/purge-only", tenantID)
		if err := store.Create(ctx, l); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Delete(ctx, tenantID, l.Code); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
			TenantID: tenantID,
			Email:    "email-only@example.com",
			Role:     identity.RoleViewer,
		}); err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}

		if got := countJobsByKind(t, store, job.KindLinkPurgeStats); got != 1 {
			t.Fatalf("link.purge_stats jobs = %d, want 1", got)
		}
		if got := countJobsByKind(t, store, job.KindInvitationEmail); got != 1 {
			t.Fatalf("invitation.email jobs = %d, want 1", got)
		}
		if got := countJobsByKind(t, store, ""); got != 2 {
			t.Fatalf("total jobs = %d, want 2", got)
		}
	})
}

func TestStoreFailingTxHookRollsBackTransaction(t *testing.T) {
	hookErr := errors.New("injected tx hook failure")
	failOnEvent := func(target func(TxEvent) bool) TxHook {
		return func(_ context.Context, _ *sql.Tx, ev TxEvent) error {
			if target(ev) {
				return hookErr
			}
			return nil
		}
	}

	t.Run("create rollback", func(t *testing.T) {
		store, tenantID := newTestStoreWithOptions(t, WithTxHooks(
			RiskScanHook(),
			failOnEvent(func(ev TxEvent) bool { _, ok := ev.(LinkCreatedEvent); return ok }),
		))
		ctx := testAuditCtx(context.Background())

		l := mustLink(t, "https://example.com/fail-create", tenantID)
		err := store.Create(ctx, l)
		if !errors.Is(err, hookErr) {
			t.Fatalf("Create err = %v, want %v", err, hookErr)
		}
		if got := countJobsByKind(t, store, ""); got != 0 {
			t.Fatalf("jobs after rolled back Create = %d, want 0", got)
		}
		limits, err := store.GetTenantLimits(ctx, tenantID)
		if err != nil {
			t.Fatalf("GetTenantLimits: %v", err)
		}
		if limits.LinkUsage != 0 {
			t.Fatalf("LinkUsage = %d after rolled back Create, want 0", limits.LinkUsage)
		}
		events, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenantID, Limit: 10})
		if err != nil {
			t.Fatalf("ListAuditLogs: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("audit events after rolled back Create = %d, want 0", len(events))
		}
	})

	t.Run("ban rollback", func(t *testing.T) {
		store, tenantID := newTestStoreWithOptions(t, WithTxHooks(
			WebhookFanoutHook(),
			failOnEvent(func(ev TxEvent) bool { _, ok := ev.(LinkBannedEvent); return ok }),
		))
		ctx := testAuditCtx(context.Background())

		if _, err := store.CreateWebhook(ctx, webhook.CreateParams{
			TenantID:         tenantID,
			URL:              "https://hooks.example.com/ban-rollback",
			Events:           []webhook.EventType{webhook.EventLinkBanned},
			SecretCiphertext: bytes.Repeat([]byte{0xcd}, 64),
			SecretPrefix:     "whsec_ban",
		}); err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		l := mustLink(t, "https://example.com/fail-ban", tenantID)
		if err := store.Create(ctx, l); err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err := store.Ban(ctx, l.Code, link.BanReasonPhishing, link.BanSourceOperator)
		if !errors.Is(err, hookErr) {
			t.Fatalf("Ban err = %v, want %v", err, hookErr)
		}
		got, err := store.GetForTenant(ctx, tenantID, l.Code)
		if err != nil {
			t.Fatalf("GetForTenant: %v", err)
		}
		if got.Ban != nil {
			t.Fatalf("Ban = %+v after rolled back Ban, want nil", got.Ban)
		}
		if gotJobs := countJobsByKind(t, store, job.KindWebhookDeliver); gotJobs != 0 {
			t.Fatalf("webhook.deliver jobs after rolled back Ban = %d, want 0", gotJobs)
		}
	})

	t.Run("invitation rollback", func(t *testing.T) {
		store, tenantID := newTestStoreWithOptions(t, WithTxHooks(
			InvitationEmailHook(),
			failOnEvent(func(ev TxEvent) bool { _, ok := ev.(InvitationCreatedEvent); return ok }),
		))
		ctx := testAuditCtx(context.Background())

		_, err := store.CreateInvitation(ctx, CreateInvitationParams{
			TenantID: tenantID,
			Email:    "rollback@example.com",
			Role:     identity.RoleMember,
		})
		if !errors.Is(err, hookErr) {
			t.Fatalf("CreateInvitation err = %v, want %v", err, hookErr)
		}
		invs, err := store.ListInvitations(ctx, tenantID)
		if err != nil {
			t.Fatalf("ListInvitations: %v", err)
		}
		if len(invs) != 0 {
			t.Fatalf("invitations after rollback = %d, want 0", len(invs))
		}
		if got := countJobsByKind(t, store, job.KindInvitationEmail); got != 0 {
			t.Fatalf("email jobs after rollback = %d, want 0", got)
		}
	})
}

func TestStoreWithAnalyticsDisabled(t *testing.T) {
	store, tenantID := newTestStoreWithOptions(t, WithAnalytics(false))
	ctx := testAuditCtx(context.Background())

	l := mustLink(t, "https://example.com/no-analytics", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 故意往 link_clicks_daily 写一行今日点击，验证 WithAnalytics(false) 确实跳过该表读取。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO link_clicks_daily (tenant_id, link_id, day, clicks) VALUES ($1, $2, (now() AT TIME ZONE 'UTC')::date, 42)`,
		tenantID, l.ID); err != nil {
		t.Fatalf("seed clicks_daily: %v", err)
	}

	got, err := store.GetForTenant(ctx, tenantID, l.Code)
	if err != nil {
		t.Fatalf("GetForTenant: %v", err)
	}
	if got.ClicksToday != 0 {
		t.Errorf("GetForTenant ClicksToday = %d, want 0 (must skip link_clicks_daily)", got.ClicksToday)
	}

	items, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 10, Sort: link.SortID})
	if err != nil {
		t.Fatalf("List(SortID): %v", err)
	}
	if len(items) != 1 || items[0].ClicksToday != 0 {
		t.Errorf("List(SortID) = %+v, want 1 item with ClicksToday=0", items)
	}

	for _, s := range []link.Sort{link.SortClicks, link.SortClicksToday} {
		if _, err := store.List(ctx, link.ListOptions{TenantID: tenantID, Limit: 10, Sort: s}); !errors.Is(err, link.ErrAnalyticsDisabled) {
			t.Errorf("List(%s) err = %v, want link.ErrAnalyticsDisabled", s, err)
		}
	}

	newURL := "https://example.com/no-analytics-v2"
	updated, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ClicksToday != 0 {
		t.Errorf("Update ClicksToday = %d, want 0 (must skip link_clicks_daily)", updated.ClicksToday)
	}
}
