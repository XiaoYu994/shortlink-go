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
	ctx := testAuditCtx(context.Background())

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
	if err := store.Complete(ctx, claimed[0].ID, "test-worker", claimed[0].LockVersion); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 只修改 disabled 或 expires_at 不应入队任何后台任务（ADR 0031）。
	disabled := true
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{Disabled: &disabled}); err != nil {
		t.Fatalf("Update disabled: %v", err)
	}
	claimed, err = store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "test-worker"})
	if err != nil {
		t.Fatalf("Claim after Update disabled: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed after disabled-only Update = %+v, want 0", claimed)
	}

	// 修改 URL 应同事务入队一条 link.risk_scan。
	newURL := "https://example.com/updated-url"
	if _, err := store.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update url: %v", err)
	}
	claimed, err = store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "test-worker"})
	if err != nil {
		t.Fatalf("Claim after Update url: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Kind != job.KindLinkRiskScan {
		t.Fatalf("claimed after url Update = %+v, want 1 link.risk_scan", claimed)
	}
	if err := json.Unmarshal(claimed[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal updated payload: %v", err)
	}
	if payload.URL != newURL {
		t.Errorf("payload.URL = %q, want %q", payload.URL, newURL)
	}
	if err := store.Complete(ctx, claimed[0].ID, "test-worker", claimed[0].LockVersion); err != nil {
		t.Fatalf("Complete url job %s: %v", claimed[0].Kind, err)
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
	ctx := testAuditCtx(context.Background())

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
		if err := store.Complete(ctx, j.ID, "fair-worker", j.LockVersion); err != nil {
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
	ctx := testAuditCtx(context.Background())

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
	ctx := testAuditCtx(context.Background())

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

	reaped, err := store.ReapExpired(ctx, 0, 10)
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

	// 2. OldestPendingAges 反映到期 pending 任务等待时长，且忽略未到期的未来任务。
	if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: "future.only", TenantID: &tenantID, Delay: time.Hour}); err != nil {
		t.Fatalf("EnqueueJob future.only: %v", err)
	}
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
	if _, ok := ages["future.only"]; ok {
		t.Errorf("future.only should not appear in OldestPendingAges: %+v", ages)
	}

	// 3. CleanupDoneJobs 只删超保留期的 done 任务，保留 dead 和近期 done。
	claimedRetry, err := store.Claim(ctx, job.ClaimParams{Limit: 1, Lease: time.Minute, LockedBy: "w"})
	if err != nil || len(claimedRetry) != 1 {
		t.Fatalf("Claim idRetry: got %d err=%v", len(claimedRetry), err)
	}
	if err := store.Complete(ctx, claimedRetry[0].ID, "w", claimedRetry[0].LockVersion); err != nil {
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

	// 4. CleanupExpiredSessions 拆分清理过期会话与已吊销超保留期会话，并遵守 limit 上限。
	activeSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-active", Email: "a@example.com", EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity active: %v", err)
	}
	oldExpiredSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-old", Email: "b@example.com", EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity old: %v", err)
	}
	oldRevokedSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-revoked-old", Email: "c@example.com", EmailVerified: true,
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity old revoked: %v", err)
	}
	recentRevokedSess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer: "https://dex.example", Subject: "sub-revoked-recent", Email: "d@example.com", EmailVerified: true,
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity recent revoked: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET created_at = now() - interval '12 days', expires_at = now() - interval '10 days' WHERE id = $1`,
		oldExpiredSess.Session.ID); err != nil {
		t.Fatalf("backdate expired session: %v", err)
	}
	// 已吊销但 expires_at 仍在将来：验证拆分后的第二条按 revoked_at 清理的索引路径生效。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() - interval '10 days' WHERE id = $1`,
		oldRevokedSess.Session.ID); err != nil {
		t.Fatalf("backdate old revoked session: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() - interval '1 hour' WHERE id = $1`,
		recentRevokedSess.Session.ID); err != nil {
		t.Fatalf("set recent revoked session: %v", err)
	}

	deletedSess, err := store.CleanupExpiredSessions(ctx, 7*24*time.Hour, 1)
	if err != nil {
		t.Fatalf("CleanupExpiredSessions limit=1: %v", err)
	}
	if deletedSess != 1 {
		t.Errorf("deletedSess (limit=1) = %d, want 1", deletedSess)
	}
	deletedSess2, err := store.CleanupExpiredSessions(ctx, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("CleanupExpiredSessions remaining: %v", err)
	}
	if deletedSess2 != 1 {
		t.Errorf("deletedSess (remaining) = %d, want 1 (old revoked session)", deletedSess2)
	}
	var keptSessions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&keptSessions); err != nil {
		t.Fatalf("count remaining sessions: %v", err)
	}
	if keptSessions != 2 {
		t.Errorf("remaining sessions = %d, want 2 (active + recent revoked)", keptSessions)
	}
	if _, err := store.GetSessionInfo(ctx, activeSess.Token); err != nil {
		t.Errorf("active session should remain valid: %v", err)
	}
}

func TestJobLeaseFencingPreventsSameWorkerABAAndReapBatchLimit(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	id, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 5})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	// 1. 同一 worker 首次抢占，拿到 lock_version = 1。
	first, err := store.Claim(ctx, job.ClaimParams{Limit: 1, Lease: time.Minute, LockedBy: "same-worker"})
	if err != nil || len(first) != 1 || first[0].LockVersion != 1 {
		t.Fatalf("first Claim = %+v err=%v, want 1 job lockVersion=1", first, err)
	}

	// 2. 模拟租约到期被 Reaper 回收，随后被同一 WorkerID 再次抢占，拿到 lock_version = 2。
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET locked_until = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reaped, err := store.ReapExpired(ctx, 0, 10)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("ReapExpired = %+v err=%v, want 1", reaped, err)
	}
	second, err := store.Claim(ctx, job.ClaimParams{Limit: 1, Lease: time.Minute, LockedBy: "same-worker"})
	if err != nil || len(second) != 1 || second[0].LockVersion != 2 {
		t.Fatalf("second Claim = %+v err=%v, want 1 job lockVersion=2", second, err)
	}

	// 3. 持有旧 lock_version=1 的陈旧协程调 Complete / Retry / Dead 全部被拒，不能覆盖新租约。
	if err := store.Complete(ctx, id, "same-worker", 1); !errors.Is(err, job.ErrLeaseLost) {
		t.Errorf("stale Complete err = %v, want ErrLeaseLost", err)
	}
	if err := store.Retry(ctx, job.RetryParams{ID: id, LockedBy: "same-worker", LockVersion: 1, LastError: "stale"}); !errors.Is(err, job.ErrLeaseLost) {
		t.Errorf("stale Retry err = %v, want ErrLeaseLost", err)
	}
	if err := store.Dead(ctx, id, "same-worker", 1, "stale"); !errors.Is(err, job.ErrLeaseLost) {
		t.Errorf("stale Dead err = %v, want ErrLeaseLost", err)
	}

	var status string
	var lockVersion int64
	if err := store.db.QueryRowContext(ctx, `SELECT status, lock_version FROM jobs WHERE id = $1`, id).Scan(&status, &lockVersion); err != nil {
		t.Fatalf("select job: %v", err)
	}
	if status != job.StatusRunning || lockVersion != 2 {
		t.Fatalf("job after stale writes = (%q, v%d), want (running, v2)", status, lockVersion)
	}

	// 4. 持有当前 lock_version=2 的协程正常提交成功。
	if err := store.Complete(ctx, id, "same-worker", 2); err != nil {
		t.Fatalf("current Complete: %v", err)
	}

	// 5. ReapExpired 遵守 limit 分批回收上限。
	for range 5 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID}); err != nil {
			t.Fatalf("EnqueueJob batch: %v", err)
		}
	}
	if _, err := store.Claim(ctx, job.ClaimParams{Limit: 5, Lease: time.Minute, LockedBy: "batch-w"}); err != nil {
		t.Fatalf("Claim batch: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET locked_until = now() - interval '1 second' WHERE status = 'running'`); err != nil {
		t.Fatalf("expire batch leases: %v", err)
	}
	for _, want := range []int{2, 2, 1, 0} {
		got, err := store.ReapExpired(ctx, 0, 2)
		if err != nil {
			t.Fatalf("ReapExpired limit=2: %v", err)
		}
		if len(got) != want {
			t.Fatalf("ReapExpired len = %d, want %d", len(got), want)
		}
	}
}

func TestJobClaimFairnessIncludesPlatformJobsAndSkipsFuture(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	issuedB, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "tenant-fair-b", Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey tenantB: %v", err)
	}
	tenantB := issuedB.Tenant.ID

	// 租户 A 放 4 条到期 + 2 条未来任务；租户 B 放 1 条到期；平台级（tenant_id=NULL）放 2 条到期。
	for range 4 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantA}); err != nil {
			t.Fatalf("enqueue A due: %v", err)
		}
	}
	for range 2 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantA, Delay: time.Hour}); err != nil {
			t.Fatalf("enqueue A future: %v", err)
		}
	}
	if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantB}); err != nil {
		t.Fatalf("enqueue B due: %v", err)
	}
	for range 2 {
		if _, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: "platform.sync", TenantID: nil}); err != nil {
			t.Fatalf("enqueue platform: %v", err)
		}
	}

	batch, err := store.Claim(ctx, job.ClaimParams{Limit: 10, MaxPerTenant: 2, Lease: time.Minute, LockedBy: "w-fair"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// 期望：A 最多 2 条 + B 1 条 + 平台 2 条 = 5 条（不含未来任务，也不超 A 的 2 条上限）。
	if len(batch) != 5 {
		t.Fatalf("claimed len = %d, want 5", len(batch))
	}
	var countA, countB, countPlatform int
	for _, j := range batch {
		switch {
		case j.TenantID == nil:
			countPlatform++
		case *j.TenantID == tenantA:
			countA++
		case *j.TenantID == tenantB:
			countB++
		}
	}
	if countA != 2 || countB != 1 || countPlatform != 2 {
		t.Errorf("counts (A=%d, B=%d, platform=%d), want (2, 1, 2)", countA, countB, countPlatform)
	}
}

func TestCleanupDeadJobsAndInvitations(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	// 1. dead job 清理：仅删除早于保留期的 dead 任务，保留近期 dead 任务。
	oldDeadID, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("EnqueueJob oldDead: %v", err)
	}
	recentDeadID, err := store.EnqueueJob(ctx, EnqueueJobParams{Kind: job.KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("EnqueueJob recentDead: %v", err)
	}
	claimed, err := store.Claim(ctx, job.ClaimParams{Limit: 10, Lease: time.Minute, LockedBy: "cleanup-w"})
	if err != nil || len(claimed) != 2 {
		t.Fatalf("Claim = %d err=%v, want 2", len(claimed), err)
	}
	for _, j := range claimed {
		if err := store.Dead(ctx, j.ID, "cleanup-w", j.LockVersion, "fatal"); err != nil {
			t.Fatalf("Dead %d: %v", j.ID, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET finished_at = now() - interval '40 days' WHERE id = $1`, oldDeadID); err != nil {
		t.Fatalf("backdate old dead job: %v", err)
	}
	deletedDead, err := store.CleanupDeadJobs(ctx, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("CleanupDeadJobs: %v", err)
	}
	if deletedDead != 1 {
		t.Errorf("deletedDead = %d, want 1", deletedDead)
	}
	var remainID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE status = 'dead'`).Scan(&remainID); err != nil || remainID != recentDeadID {
		t.Errorf("remaining dead job = (%d, %v), want %d", remainID, err, recentDeadID)
	}

	// 2. invitations 清理：分别清理超期未接受、已撤销、被取代的老记录，保留活跃待兑现邀请。
	activeInv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID, Email: "active-inv@example.com", Role: identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("CreateInvitation active: %v", err)
	}
	future := time.Now().Add(time.Hour)
	expiredInv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID, Email: "expired-inv@example.com", Role: identity.RoleMember, ExpiresAt: &future,
	})
	if err != nil {
		t.Fatalf("CreateInvitation expired: %v", err)
	}
	revokedInv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID, Email: "revoked-inv@example.com", Role: identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("CreateInvitation revoked: %v", err)
	}
	supersededInv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenantID, Email: "superseded-inv@example.com", Role: identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("CreateInvitation superseded: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET created_at = now() - interval '45 days', expires_at = now() - interval '40 days' WHERE id = $1`,
		expiredInv.ID); err != nil {
		t.Fatalf("backdate expired invitation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET created_at = now() - interval '45 days', revoked_at = now() - interval '40 days' WHERE id = $1`,
		revokedInv.ID); err != nil {
		t.Fatalf("backdate revoked invitation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET created_at = now() - interval '45 days', superseded_at = now() - interval '40 days' WHERE id = $1`,
		supersededInv.ID); err != nil {
		t.Fatalf("backdate superseded invitation: %v", err)
	}

	retention := 30 * 24 * time.Hour
	nExp, err := store.CleanupExpiredInvitationsByExpiry(ctx, retention, 100)
	if err != nil || nExp != 1 {
		t.Fatalf("CleanupExpiredInvitationsByExpiry = (%d, %v), want (1, nil)", nExp, err)
	}
	nRev, err := store.CleanupRevokedInvitations(ctx, retention, 100)
	if err != nil || nRev != 1 {
		t.Fatalf("CleanupRevokedInvitations = (%d, %v), want (1, nil)", nRev, err)
	}
	nSup, err := store.CleanupSupersededInvitations(ctx, retention, 100)
	if err != nil || nSup != 1 {
		t.Fatalf("CleanupSupersededInvitations = (%d, %v), want (1, nil)", nSup, err)
	}
	var keptInvID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM invitations`).Scan(&keptInvID); err != nil || keptInvID != activeInv.ID {
		t.Errorf("remaining invitation = (%d, %v), want %d", keptInvID, err, activeInv.ID)
	}
}
