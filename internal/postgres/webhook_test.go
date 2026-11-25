package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/webhook"
)

func testWebhookCipher(t *testing.T) *webhook.SecretCipher {
	t.Helper()
	key, err := webhook.ParseMasterKeyHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseMasterKeyHex: %v", err)
	}
	c, err := webhook.NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	return c
}

func seedTestWebhook(t *testing.T, store *Store, tenantID int64, rawURL string, events ...webhook.EventType) webhook.Subscription {
	t.Helper()
	ctx := testAuditCtx(context.Background())
	c := testWebhookCipher(t)
	plain, prefix, err := webhook.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	ct, err := c.Encrypt(tenantID, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	sub, err := store.CreateWebhook(ctx, webhook.CreateParams{
		TenantID:         tenantID,
		URL:              rawURL,
		Description:      "test hook",
		Events:           events,
		SecretCiphertext: ct,
		SecretPrefix:     prefix,
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return sub
}

func claimAllPendingJobs(t *testing.T, store *Store) []job.Job {
	t.Helper()
	ctx := context.Background()
	var out []job.Job
	for {
		batch, err := store.Claim(ctx, job.ClaimParams{Limit: 100, Lease: time.Minute, LockedBy: "wh-test"})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch) == 0 {
			return out
		}
		for _, j := range batch {
			if err := store.Complete(ctx, j.ID, "wh-test", j.LockVersion); err != nil {
				t.Fatalf("Complete job %d: %v", j.ID, err)
			}
			out = append(out, j)
		}
	}
}

func TestWebhookCRUDRotationCapAndAudit(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	c := testWebhookCipher(t)

	// 1. 并发创建超过上限（MaxPerTenant = 5）时严格只允许 5 条成功。
	const workers = webhook.MaxPerTenant + 3
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			plain, prefix, err := webhook.GenerateSecret()
			if err != nil {
				errs[i] = err
				return
			}
			ct, err := c.Encrypt(tenantID, plain)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = store.CreateWebhook(ctx, webhook.CreateParams{
				TenantID:         tenantID,
				URL:              fmt.Sprintf("https://hooks.example.com/%d", i),
				Events:           []webhook.EventType{webhook.EventLinkCreated},
				SecretCiphertext: ct,
				SecretPrefix:     prefix,
			})
		})
	}
	wg.Wait()

	var okCount, limitCount int
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, webhook.ErrLimitExceeded):
			limitCount++
		default:
			t.Fatalf("unexpected CreateWebhook error: %v", err)
		}
	}
	if okCount != webhook.MaxPerTenant || limitCount != workers-webhook.MaxPerTenant {
		t.Fatalf("concurrent create ok=%d limit=%d, want (%d, %d)", okCount, limitCount, webhook.MaxPerTenant, workers-webhook.MaxPerTenant)
	}

	subs, err := store.ListWebhooks(ctx, tenantID)
	if err != nil || len(subs) != webhook.MaxPerTenant {
		t.Fatalf("ListWebhooks = (%d, %v), want %d", len(subs), err, webhook.MaxPerTenant)
	}
	target := subs[0]

	// 2. 更新、手动停用与重新启用（应清零失败计数）、密钥轮换、测试 ping 与删除。
	disabled := false
	newURL := "https://hooks.example.com/updated"
	upd, err := store.UpdateWebhook(ctx, webhook.UpdateParams{
		TenantID:  tenantID,
		WebhookID: target.ID,
		URL:       &newURL,
		Enabled:   &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateWebhook disable: %v", err)
	}
	if upd.Enabled || upd.DisabledReason != webhook.DisabledReasonManual || upd.URL != newURL {
		t.Fatalf("updated disabled = %+v", upd)
	}

	newPlain, newPrefix, _ := webhook.GenerateSecret()
	newCT, _ := c.Encrypt(tenantID, newPlain)
	rot, err := store.RotateWebhookSecret(ctx, tenantID, target.ID, newCT, newPrefix)
	if err != nil || rot.SecretPrefix != newPrefix {
		t.Fatalf("RotateWebhookSecret = (%+v, %v)", rot, err)
	}

	pingRes, err := store.EnqueueWebhookTest(ctx, tenantID, target.ID)
	if err != nil || pingRes.JobID <= 0 || !strings.HasPrefix(pingRes.DeliveryID, "dlv_") {
		t.Fatalf("EnqueueWebhookTest = (%+v, %v)", pingRes, err)
	}

	// 跨租户不可见与删除。
	otherTenant := tenantID + 999
	if err := store.DeleteWebhook(ctx, otherTenant, target.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Fatalf("cross-tenant DeleteWebhook = %v, want ErrNotFound", err)
	}
	if err := store.DeleteWebhook(ctx, tenantID, target.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if _, err := store.GetWebhookForDelivery(ctx, target.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Fatalf("GetWebhookForDelivery after delete = %v, want ErrNotFound", err)
	}

	// 校验相关审计动作完整落盘。
	auditEvents, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenantID, Limit: 50})
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	seenActions := map[audit.Action]bool{}
	for _, ev := range auditEvents {
		seenActions[ev.Action] = true
	}
	for _, want := range []audit.Action{
		audit.ActionWebhookCreate,
		audit.ActionWebhookUpdate,
		audit.ActionWebhookRotateSecret,
		audit.ActionWebhookTest,
		audit.ActionWebhookDelete,
	} {
		if !seenActions[want] {
			t.Errorf("missing audit action %s", want)
		}
	}
}

func TestWebhookOutboxFanoutAndIdempotencyAndInvitationEmail(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	subAll := seedTestWebhook(t, store, tenantID, "https://hooks.example.com/all", webhook.AllSubscribableEvents()...)
	subBanOnly := seedTestWebhook(t, store, tenantID, "https://hooks.example.com/ban", webhook.EventLinkBanned)

	// 1. Create 短码冲突回滚不会漏发或重发重复 webhook.deliver。
	codes := []string{"dupcode1", "dupcode1", "uniq0001"}
	store.generate = func() (string, error) {
		c := codes[0]
		codes = codes[1:]
		return c, nil
	}
	first := mustLink(t, "https://example.com/1", tenantID)
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second := mustLink(t, "https://example.com/2", tenantID)
	if err := store.Create(ctx, second); err != nil {
		t.Fatalf("Create second after collision: %v", err)
	}
	jobs := claimAllPendingJobs(t, store)
	var createdDeliveries []webhook.DeliverPayload
	for _, j := range jobs {
		if j.Kind == job.KindWebhookDeliver {
			var p webhook.DeliverPayload
			if err := json.Unmarshal(j.Payload, &p); err != nil {
				t.Fatalf("unmarshal webhook payload: %v", err)
			}
			createdDeliveries = append(createdDeliveries, p)
		}
	}
	if len(createdDeliveries) != 2 || createdDeliveries[0].Event != webhook.EventLinkCreated {
		t.Fatalf("created webhook deliveries = %+v, want 2 (1 per Create)", createdDeliveries)
	}

	// 2. Ban & 幂等重复 Ban：仅第一次入队且 subAll + subBanOnly 共享同一个 event_id。
	if _, err := store.Ban(ctx, second.Code, link.BanReasonPhishing, link.BanSourceRisk); err != nil {
		t.Fatalf("Ban 1st: %v", err)
	}
	if _, err := store.Ban(ctx, second.Code, link.BanReasonMalware, link.BanSourceOperator); err != nil {
		t.Fatalf("Ban 2nd idempotent: %v", err)
	}
	banJobs := claimAllPendingJobs(t, store)
	var banPayloads []webhook.DeliverPayload
	for _, j := range banJobs {
		if j.Kind == job.KindWebhookDeliver {
			var p webhook.DeliverPayload
			_ = json.Unmarshal(j.Payload, &p)
			banPayloads = append(banPayloads, p)
		}
	}
	if len(banPayloads) != 2 {
		t.Fatalf("ban webhook jobs = %d, want 2 (1 each for subAll & subBanOnly)", len(banPayloads))
	}
	if banPayloads[0].EventID != banPayloads[1].EventID || banPayloads[0].Event != webhook.EventLinkBanned {
		t.Fatalf("ban payloads = %+v, want shared event_id for link.banned", banPayloads)
	}
	if banPayloads[0].WebhookID == banPayloads[1].WebhookID || (banPayloads[0].WebhookID != subAll.ID && banPayloads[1].WebhookID != subAll.ID) || (banPayloads[0].WebhookID != subBanOnly.ID && banPayloads[1].WebhookID != subBanOnly.ID) {
		t.Fatalf("unexpected webhook IDs in ban fanout: %+v", banPayloads)
	}

	// 3. Unban & 幂等重复 Unban；Update；Delete 均触发对应单事件。
	if _, err := store.Unban(ctx, second.Code); err != nil {
		t.Fatalf("Unban 1st: %v", err)
	}
	if _, err := store.Unban(ctx, second.Code); err != nil {
		t.Fatalf("Unban 2nd idempotent: %v", err)
	}
	disabled := true
	if _, err := store.Update(ctx, tenantID, second.Code, link.Update{Disabled: &disabled}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := store.Delete(ctx, tenantID, second.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	restJobs := claimAllPendingJobs(t, store)
	eventCounts := map[webhook.EventType]int{}
	for _, j := range restJobs {
		if j.Kind == job.KindWebhookDeliver {
			var p webhook.DeliverPayload
			_ = json.Unmarshal(j.Payload, &p)
			eventCounts[p.Event]++
		}
	}
	if eventCounts[webhook.EventLinkUnbanned] != 1 || eventCounts[webhook.EventLinkUpdated] != 1 || eventCounts[webhook.EventLinkDeleted] != 1 {
		t.Fatalf("eventCounts = %+v, want unbanned=1 updated=1 deleted=1", eventCounts)
	}

	// 4. CreateInvitation 同事务入队去重 invitation.email + GetInvitationEmailTarget 查询实时信息。
	inv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID,
		Email:    "invitee@example.com",
		Role:     identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	invJobs := claimAllPendingJobs(t, store)
	if len(invJobs) != 1 || invJobs[0].Kind != job.KindInvitationEmail || invJobs[0].DedupKey != "invitation:"+strconv.FormatInt(inv.ID, 10) {
		t.Fatalf("invitation jobs = %+v, want 1 invitation.email with dedup key", invJobs)
	}
	target, err := store.GetInvitationEmailTarget(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetInvitationEmailTarget: %v", err)
	}
	if target.Email != "invitee@example.com" || target.TenantName != "test" || !target.Pending(time.Now()) {
		t.Fatalf("email target = %+v", target)
	}
}

func TestRecordWebhookDeliveryCircuitBreakerAndCleanup(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	sub := seedTestWebhook(t, store, tenantID, "https://hooks.example.com/cb", webhook.EventLinkCreated)
	code500 := 500
	code200 := 200

	// 2 次非 ping 终态失败（阈值 2）后应自动停用并写系统审计日志；ping 不受影响。
	if disabled, err := store.RecordWebhookDelivery(ctx, webhook.RecordDeliveryParams{
		TenantID:               tenantID,
		WebhookID:              sub.ID,
		JobID:                  101,
		DeliveryID:             "dlv_101",
		Event:                  webhook.EventPing,
		Attempt:                1,
		Status:                 webhook.StatusFailed,
		HTTPStatus:             &code500,
		DurationMs:             15,
		ErrorMessage:           "http 500",
		TerminalFailure:        true,
		IsPing:                 true,
		MaxConsecutiveFailures: 2,
	}); err != nil || disabled {
		t.Fatalf("ping failure = (%v, %v), want (false, nil)", disabled, err)
	}

	disabled1, err := store.RecordWebhookDelivery(ctx, webhook.RecordDeliveryParams{
		TenantID:               tenantID,
		WebhookID:              sub.ID,
		JobID:                  102,
		DeliveryID:             "dlv_102",
		Event:                  webhook.EventLinkCreated,
		Attempt:                6,
		Status:                 webhook.StatusFailed,
		HTTPStatus:             &code500,
		DurationMs:             20,
		ErrorMessage:           "http 500",
		TerminalFailure:        true,
		MaxConsecutiveFailures: 2,
	})
	if err != nil || disabled1 {
		t.Fatalf("1st terminal failure = (%v, %v), want (false, nil)", disabled1, err)
	}

	disabled2, err := store.RecordWebhookDelivery(ctx, webhook.RecordDeliveryParams{
		TenantID:               tenantID,
		WebhookID:              sub.ID,
		JobID:                  103,
		DeliveryID:             "dlv_103",
		Event:                  webhook.EventLinkCreated,
		Attempt:                6,
		Status:                 webhook.StatusFailed,
		HTTPStatus:             &code500,
		DurationMs:             25,
		ErrorMessage:           "http 500",
		TerminalFailure:        true,
		MaxConsecutiveFailures: 2,
	})
	if err != nil || !disabled2 {
		t.Fatalf("2nd terminal failure = (%v, %v), want (true, nil)", disabled2, err)
	}

	subs, err := store.ListWebhooks(ctx, tenantID)
	if err != nil || len(subs) != 1 || subs[0].Enabled || subs[0].DisabledReason != webhook.DisabledReasonConsecutiveFailures || subs[0].ConsecutiveFailures != 2 {
		t.Fatalf("sub after auto-disable = %+v, err=%v", subs, err)
	}
	autoDisabledAt := subs[0].DisabledAt

	// 对已自动熔断的订阅幂等更新 enabled=false 不得篡改 consecutive_failures disabled_reason/disabled_at。
	keepDisabled := false
	updatedDesc := "keep circuit breaker reason"
	kept, err := store.UpdateWebhook(ctx, webhook.UpdateParams{
		TenantID:    tenantID,
		WebhookID:   sub.ID,
		Description: &updatedDesc,
		Enabled:     &keepDisabled,
	})
	if err != nil || kept.Enabled || kept.DisabledReason != webhook.DisabledReasonConsecutiveFailures || kept.ConsecutiveFailures != 2 || kept.DisabledAt == nil || autoDisabledAt == nil || !kept.DisabledAt.Equal(*autoDisabledAt) {
		t.Fatalf("idempotent disabled UpdateWebhook = (%+v, %v), want preserved consecutive_failures & disabled_at", kept, err)
	}

	// 已停用期间晚到的在途终态失败只落投递记录，不再推进 consecutive_failures。
	if lateDisabled, err := store.RecordWebhookDelivery(ctx, webhook.RecordDeliveryParams{
		TenantID:               tenantID,
		WebhookID:              sub.ID,
		JobID:                  105,
		DeliveryID:             "dlv_105",
		Event:                  webhook.EventLinkCreated,
		Attempt:                6,
		Status:                 webhook.StatusFailed,
		HTTPStatus:             &code500,
		DurationMs:             20,
		ErrorMessage:           "http 500",
		TerminalFailure:        true,
		MaxConsecutiveFailures: 2,
	}); err != nil || lateDisabled {
		t.Fatalf("late terminal failure on disabled webhook = (%v, %v), want (false, nil)", lateDisabled, err)
	}
	subsAfterLate, err := store.ListWebhooks(ctx, tenantID)
	if err != nil || len(subsAfterLate) != 1 || subsAfterLate[0].ConsecutiveFailures != 2 || subsAfterLate[0].DisabledReason != webhook.DisabledReasonConsecutiveFailures {
		t.Fatalf("subs after late failure = %+v, err=%v", subsAfterLate, err)
	}

	// 重新启用后成功投递，确保计数归零；同时验证游标分页与保留期批删。
	reEnable := true
	reEnabled, err := store.UpdateWebhook(ctx, webhook.UpdateParams{TenantID: tenantID, WebhookID: sub.ID, Enabled: &reEnable})
	if err != nil || !reEnabled.Enabled || reEnabled.ConsecutiveFailures != 0 || reEnabled.DisabledReason != webhook.DisabledReasonNone {
		t.Fatalf("reEnabled = (%+v, %v)", reEnabled, err)
	}

	if _, err := store.RecordWebhookDelivery(ctx, webhook.RecordDeliveryParams{
		TenantID:   tenantID,
		WebhookID:  sub.ID,
		JobID:      104,
		DeliveryID: "dlv_104",
		Event:      webhook.EventLinkCreated,
		Attempt:    1,
		Status:     webhook.StatusSuccess,
		HTTPStatus: &code200,
		DurationMs: 12,
	}); err != nil {
		t.Fatalf("record success: %v", err)
	}

	page1, err := store.ListWebhookDeliveries(ctx, webhook.DeliveryListOptions{TenantID: tenantID, WebhookID: sub.ID, Limit: 2})
	if err != nil || len(page1) != 2 || page1[0].DeliveryID != "dlv_104" || page1[1].DeliveryID != "dlv_105" {
		t.Fatalf("page1 = (%+v, %v)", page1, err)
	}
	page2, err := store.ListWebhookDeliveries(ctx, webhook.DeliveryListOptions{
		TenantID:  tenantID,
		WebhookID: sub.ID,
		Limit:     10,
		Cursor:    &webhook.DeliveryListCursor{ID: page1[1].ID},
	})
	if err != nil || len(page2) != 3 || page2[0].DeliveryID != "dlv_103" || page2[2].DeliveryID != "dlv_101" {
		t.Fatalf("page2 = (%+v, %v), want 3 remaining items (dlv_103..dlv_101)", page2, err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE webhook_deliveries SET created_at = now() - interval '10 days' WHERE delivery_id IN ('dlv_101', 'dlv_102')`); err != nil {
		t.Fatalf("backdate deliveries: %v", err)
	}
	cleaned, err := store.CleanupWebhookDeliveries(ctx, 7*24*time.Hour, 100)
	if err != nil || cleaned != 2 {
		t.Fatalf("CleanupWebhookDeliveries = (%d, %v), want 2", cleaned, err)
	}
}

func TestWebhookExplainPlansOnSyntheticData(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	// LIFO：在 newTestStore 注册的 release/db.Close 之前清理合成数据，避免污染同一 TEST_DATABASE_URL 上后续运行的冒烟测试。
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `TRUNCATE webhooks, webhook_deliveries RESTART IDENTITY`)
	})

	// 构造多租户合成数据并 ANALYZE，确保优化器依据真实统计选择目标索引。
	if _, err := store.db.ExecContext(ctx, `
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

INSERT INTO webhook_deliveries (tenant_id, webhook_id, job_id, delivery_id, event, attempt, status, duration_ms, created_at)
SELECT (g % 200) + 1,
       (g % 500) + 1,
       g,
       'dlv_' || g,
       'link.created',
       1,
       'success',
       15,
       now() - ((g % 30) * interval '1 day')
FROM generate_series(1, 30000) AS g;

ANALYZE webhooks;
ANALYZE webhook_deliveries;
`); err != nil {
		t.Fatalf("seed synthetic webhook data: %v", err)
	}

	fanoutPlan := explainQuery(t, ctx, store, `EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT id
FROM webhooks
WHERE tenant_id = $1
  AND enabled = TRUE
  AND 'link.created' = ANY(events)`, tenantID+1)
	if !strings.Contains(fanoutPlan, "idx_webhooks_tenant_enabled") || strings.Contains(fanoutPlan, "Seq Scan on webhooks") {
		t.Errorf("fanout query missed idx_webhooks_tenant_enabled:\n%s", fanoutPlan)
	}

	listDeliveriesPlan := explainQuery(t, ctx, store, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)\n"+listWebhookDeliveriesFirstSQL, 7, 7, 20)
	if !strings.Contains(listDeliveriesPlan, "idx_webhook_deliveries_tenant_webhook_id") || strings.Contains(listDeliveriesPlan, "Seq Scan on webhook_deliveries") {
		t.Errorf("listWebhookDeliveriesFirstSQL missed idx_webhook_deliveries_tenant_webhook_id:\n%s", listDeliveriesPlan)
	}

	cleanupPlan := explainQuery(t, ctx, store, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)\n"+cleanupWebhookDeliveriesSQL, (7 * 24 * time.Hour).Microseconds(), 50)
	if !strings.Contains(cleanupPlan, "idx_webhook_deliveries_created_at_id") || strings.Contains(cleanupPlan, "Seq Scan on webhook_deliveries") {
		t.Errorf("cleanupWebhookDeliveriesSQL missed idx_webhook_deliveries_created_at_id:\n%s", cleanupPlan)
	}
}
