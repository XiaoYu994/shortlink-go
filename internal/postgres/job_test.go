package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
)

type riskScanPayload struct {
	LinkID int64  `json:"link_id"`
	Code   string `json:"code"`
	URL    string `json:"url"`
}

func TestLinkCreateAndUpdateEnqueueRiskScanAtomically(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	// Create 成功时同语句入队一条 link.risk_scan。
	l := mustLink(t, "https://example.com/initial", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, err := store.Claim(ctx, job.ClaimParams{
		Limit:    10,
		Lease:    time.Minute,
		LockedBy: "test-worker",
	})
	if err != nil {
		t.Fatalf("Claim after Create: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d, want 1", len(claimed))
	}
	if claimed[0].Kind != job.KindLinkRiskScan {
		t.Errorf("kind = %q, want %q", claimed[0].Kind, job.KindLinkRiskScan)
	}
	if claimed[0].TenantID == nil || *claimed[0].TenantID != tenantID {
		t.Errorf("tenant_id = %v, want %d", claimed[0].TenantID, tenantID)
	}
	var payload riskScanPayload
	if err := json.Unmarshal(claimed[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.LinkID != l.ID || payload.Code != l.Code || payload.URL != l.URL {
		t.Errorf("payload = %+v, want link (%d, %q, %q)", payload, l.ID, l.Code, l.URL)
	}
	if err := store.Complete(ctx, claimed[0].ID, "test-worker"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 只修改 disabled 或 expires_at 不应入队。
	disabled := true
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: &disabled}); err != nil {
		t.Fatalf("Update disabled: %v", err)
	}
	claimed, err = store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "test-worker"})
	if err != nil {
		t.Fatalf("Claim after Update disabled: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed after disabled-only Update = %d, want 0", len(claimed))
	}

	// 修改 URL 应同语句入队一条新任务。
	newURL := "https://example.com/updated-url"
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update url: %v", err)
	}
	claimed, err = store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "test-worker"})
	if err != nil {
		t.Fatalf("Claim after Update url: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed after url Update = %d, want 1", len(claimed))
	}
	if err := json.Unmarshal(claimed[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal updated payload: %v", err)
	}
	if payload.URL != newURL {
		t.Errorf("payload.URL = %q, want %q", payload.URL, newURL)
	}

	// 配额不足时回滚，不留孤儿任务。
	zero := int64(0)
	if _, err := store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &zero}); err != nil {
		t.Fatalf("SetTenantLimits: %v", err)
	}
	overQuota := mustLink(t, "https://example.com/over-quota", tenantID)
	if err := store.Create(ctx, overQuota); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("Create over quota = %v, want ErrQuotaExceeded", err)
	}
	claimed, err = store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "test-worker"})
	if err != nil {
		t.Fatalf("Claim after quota failure: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed after failed Create = %d, want 0", len(claimed))
	}
}

func TestJobClaimSkipLockedConcurrentAndFairness(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := context.Background()

	issuedB, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "tenant-b", Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey tenant-b: %v", err)
	}
	tenantB := issuedB.Tenant.ID

	// 先给租户 A 灌 6 条，再给租户 B 灌 2 条。
	for range 6 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantA}); err != nil {
			t.Fatalf("EnqueueJob A: %v", err)
		}
	}
	for range 2 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantB}); err != nil {
			t.Fatalf("EnqueueJob B: %v", err)
		}
	}

	// 每租户软并发上限 2，一次抢 4 条：应拿到 A 2 条 + B 2 条，不能全被 A 占满。
	fairBatch, err := store.Claim(ctx, job.ClaimParams{
		Limit:        4,
		MaxPerTenant: 2,
		Lease:        time.Minute,
		LockedBy:     "fair-worker",
	})
	if err != nil {
		t.Fatalf("Claim fair batch: %v", err)
	}
	if len(fairBatch) != 4 {
		t.Fatalf("fairBatch len = %d, want 4", len(fairBatch))
	}
	counts := map[int64]int{}
	for _, j := range fairBatch {
		counts[*j.TenantID]++
		if err := store.Complete(ctx, j.ID, "fair-worker"); err != nil {
			t.Fatalf("Complete %d: %v", j.ID, err)
		}
	}
	if counts[tenantA] != 2 || counts[tenantB] != 2 {
		t.Errorf("fairness counts = %v, want A=2 B=2", counts)
	}

	// 多 goroutine 并发 SKIP LOCKED 抢占剩余 4 条，每条只能被抢到一次。
	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for i := range 8 {
		wg.Go(func() {
			workerID := "w-" + string(rune('a'+i))
			for {
				batch, err := store.Claim(ctx, job.ClaimParams{
					Limit:    1,
					Lease:    time.Minute,
					LockedBy: workerID,
				})
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, j := range batch {
					seen[j.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(seen) != 4 {
		t.Fatalf("concurrent claimed unique = %d, want 4", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %d claimed %d times, want 1", id, n)
		}
	}
}

func TestBanIfURLPreventsStaleRiskBan(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	badURL := "https://evil.example/phish"
	goodURL := "https://safe.example/home"
	l := mustLink(t, badURL, tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 租户在扫描完成前把 URL 改成合规地址。
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &goodURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// 旧扫描携带 badURL 调 BanIfURL，必须返回 ErrURLChanged 且不封禁新链接。
	if _, err := store.BanIfURL(ctx, l.Code, badURL, link.BanReasonPhishing, link.BanSourceRisk); !errors.Is(err, link.ErrURLChanged) {
		t.Fatalf("BanIfURL stale URL error = %v, want ErrURLChanged", err)
	}
	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode after skipped stale ban: %v", err)
	}
	if got.URL != goodURL {
		t.Errorf("URL = %q, want %q", got.URL, goodURL)
	}

	// 匹配当前 URL 时正常封禁。
	banned, err := store.BanIfURL(ctx, l.Code, goodURL, link.BanReasonSpam, link.BanSourceRisk)
	if err != nil {
		t.Fatalf("BanIfURL matching URL: %v", err)
	}
	if banned.Ban == nil || banned.Ban.Reason != link.BanReasonSpam || banned.Ban.Source != link.BanSourceRisk {
		t.Errorf("ban = %+v, want spam/risk", banned.Ban)
	}

	// 不存在的短码返回 ErrNotFound。
	if _, err := store.BanIfURL(ctx, "missing0", badURL, link.BanReasonPhishing, link.BanSourceRisk); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("BanIfURL missing error = %v, want ErrNotFound", err)
	}
}

func TestJobReapAndCleanup(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	// 1. 租约过期回收：未达上限回 pending，达到上限进 dead。
	idRetry, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("EnqueueJob retry: %v", err)
	}
	idDead, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("EnqueueJob dead: %v", err)
	}
	if _, err := store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Millisecond, LockedBy: "dead-worker"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET locked_until = now() - interval '1 minute' WHERE id IN ($1, $2)`, idRetry, idDead); err != nil {
		t.Fatalf("expire leases: %v", err)
	}

	reaped, err := store.ReapExpired(ctx, 0)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if len(reaped) != 2 {
		t.Fatalf("reaped = %+v, want 2 records", reaped)
	}

	var statusRetry, statusDead string
	_ = store.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = $1`, idRetry).Scan(&statusRetry)
	_ = store.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = $1`, idDead).Scan(&statusDead)
	if statusRetry != job.StatusPending || statusDead != job.StatusDead {
		t.Errorf("statuses = (%q, %q), want (pending, dead)", statusRetry, statusDead)
	}

	// 2. OldestPendingAges 反映到期 pending 任务等待时长。
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET run_at = now() - interval '30 seconds' WHERE id = $1`, idRetry); err != nil {
		t.Fatalf("backdate run_at: %v", err)
	}
	ages, err := store.OldestPendingAges(ctx)
	if err != nil {
		t.Fatalf("OldestPendingAges: %v", err)
	}
	if ages[job.KindLinkRiskScan] < 25*time.Second {
		t.Errorf("oldest pending age = %v, want >= 25s", ages[job.KindLinkRiskScan])
	}

	// 3. CleanupDoneJobs 只删超保留期的 done 任务，保留 dead 和近期 done。
	if _, err := store.Claim(ctx, job.ClaimParams{Limit: 1, Lease: time.Minute, LockedBy: "w"}); err != nil {
		t.Fatalf("Claim idRetry: %v", err)
	}
	if err := store.Complete(ctx, idRetry, "w"); err != nil {
		t.Fatalf("Complete idRetry: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET finished_at = now() - interval '10 days' WHERE id = $1`, idRetry); err != nil {
		t.Fatalf("backdate finished_at: %v", err)
	}
	deletedJobs, err := store.CleanupDoneJobs(ctx, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("CleanupDoneJobs: %v", err)
	}
	if deletedJobs != 1 {
		t.Errorf("deletedJobs = %d, want 1", deletedJobs)
	}
	var deadCount int
	_ = store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, idDead).Scan(&deadCount)
	if deadCount != 1 {
		t.Errorf("dead job should be preserved, count = %d", deadCount)
	}

	// 4. CleanupExpiredSessions 只删过期/吊销超保留期的会话，保留有效会话。
	activeSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-active", Email: "a@example.com", EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity active: %v", err)
	}
	oldSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-old", Email: "b@example.com", EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity old: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET created_at = now() - interval '12 days', expires_at = now() - interval '10 days' WHERE id = $1`,
		oldSess.Session.ID); err != nil {
		t.Fatalf("backdate session: %v", err)
	}
	deletedSess, err := store.CleanupExpiredSessions(ctx, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("CleanupExpiredSessions: %v", err)
	}
	if deletedSess != 1 {
		t.Errorf("deletedSess = %d, want 1", deletedSess)
	}
	if _, err := store.GetSessionInfo(ctx, activeSess.Token); err != nil {
		t.Errorf("active session should remain valid: %v", err)
	}
}
