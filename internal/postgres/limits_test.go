package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"shortlink/internal/link"
	"shortlink/internal/tenant"
)

// setTenantQuota 直接改测试租户的配额，模拟运维用 console tenant set 之后的库状态。
func setTenantQuota(t *testing.T, store *Store, tenantID, quota int64) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE tenants SET link_quota = $2 WHERE id = $1`, tenantID, quota); err != nil {
		t.Fatalf("set link quota: %v", err)
	}
}

func countTenantLinks(t *testing.T, store *Store, tenantID int64) (links, counted int64) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT count(*) FROM links WHERE tenant_id = $1`, tenantID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if err := store.db.QueryRow(`SELECT link_count FROM tenants WHERE id = $1`, tenantID).Scan(&counted); err != nil {
		t.Fatalf("read link_count: %v", err)
	}
	return links, counted
}

/*
TestCreateEnforcesQuotaAndDeleteReleases 覆盖配额的完整生命周期：
满额后创建返回 ErrQuotaExceeded，过期链接仍然占配额，删除释放配额。
*/
func TestCreateEnforcesQuotaAndDeleteReleases(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	setTenantQuota(t, store, tenantID, 2)

	first := createLink(t, store, tenantID, "https://example.com/first")
	createLink(t, store, tenantID, "https://example.com/second")

	err := store.Create(ctx, mustLink(t, "https://example.com/third", tenantID))
	if !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("Create over quota error = %v, want ErrQuotaExceeded", err)
	}

	// 过期链接仍然存在、仍然可管理，也仍然占配额（ADR 0011）。
	if _, err := store.db.Exec(`UPDATE links SET expires_at = now() - interval '1 minute' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
	if err := store.Create(ctx, mustLink(t, "https://example.com/still-full", tenantID)); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("Create with an expired link error = %v, want ErrQuotaExceeded", err)
	}

	if links, counted := countTenantLinks(t, store, tenantID); links != 2 || counted != 2 {
		t.Fatalf("links = %d, link_count = %d, want 2 and 2", links, counted)
	}

	if err := store.Delete(ctx, tenantID, first.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if links, counted := countTenantLinks(t, store, tenantID); links != 1 || counted != 1 {
		t.Fatalf("after delete links = %d, link_count = %d, want 1 and 1", links, counted)
	}
	createLink(t, store, tenantID, "https://example.com/after-delete")
	if links, counted := countTenantLinks(t, store, tenantID); links != 2 || counted != 2 {
		t.Fatalf("after refill links = %d, link_count = %d, want 2 and 2", links, counted)
	}
}

/*
TestCreateConcurrentRespectsQuota 用 20 个并发创建撞 5 个配额。
计数和插入在同一条语句里，成功的次数必须正好等于配额，不能多写一条。
*/
func TestCreateConcurrentRespectsQuota(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	const quota = 5
	setTenantQuota(t, store, tenantID, quota)

	const workers = 20
	links := make([]*link.Link, workers)
	for i := range workers {
		links[i] = mustLink(t, fmt.Sprintf("https://example.com/concurrent/%d", i), tenantID)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		denied  int
		failed  []error
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.Create(ctx, links[i])
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, link.ErrQuotaExceeded):
				denied++
			default:
				failed = append(failed, err)
			}
		}()
	}
	wg.Wait()

	if len(failed) != 0 {
		t.Fatalf("unexpected create errors: %v", failed)
	}
	if created != quota || denied != workers-quota {
		t.Fatalf("created = %d, denied = %d, want %d and %d", created, denied, quota, workers-quota)
	}
	if links, counted := countTenantLinks(t, store, tenantID); links != quota || counted != quota {
		t.Fatalf("links = %d, link_count = %d, want %d and %d", links, counted, quota, quota)
	}
}

// TestDeleteDoesNotGoBelowZero 验证计数被外部改小后，删除不会把它减成负数。
func TestDeleteDoesNotGoBelowZero(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	l := createLink(t, store, tenantID, "https://example.com/floor")
	if _, err := store.db.Exec(`UPDATE tenants SET link_count = 0 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("reset link_count: %v", err)
	}
	if err := store.Delete(ctx, tenantID, l.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, counted := countTenantLinks(t, store, tenantID); counted != 0 {
		t.Fatalf("link_count = %d, want 0", counted)
	}
}

func TestAuthenticateReturnsRateLimit(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	if _, err := store.db.Exec(`UPDATE tenants SET rate_limit_per_second = 42 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("set rate limit: %v", err)
	}

	plaintext, hash, err := tenant.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO api_keys (tenant_id, key_hash) VALUES ($1, $2)`, tenantID, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tenantID || got.RateLimit != 42 {
		t.Fatalf("Authenticate = %+v, want tenant %d rate limit 42", got, tenantID)
	}
}

/*
TestTenantLimits 验证运维命令背后的读写：默认值、只改一个字段、非法参数和租户不存在。
*/
func TestTenantLimits(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetTenantLimits(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantLimits: %v", err)
	}
	want := TenantLimits{ID: tenantID, Name: "test", LinkCount: 0, LinkQuota: 1000, RateLimit: 10}
	if got != want {
		t.Fatalf("GetTenantLimits = %+v, want %+v", got, want)
	}

	createLink(t, store, tenantID, "https://example.com/limits")
	rate := 0
	got, err = store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{RateLimit: &rate})
	if err != nil {
		t.Fatalf("SetTenantLimits rate: %v", err)
	}
	if got.LinkQuota != 1000 || got.RateLimit != 0 || got.LinkCount != 1 {
		t.Fatalf("after rate update = %+v, want quota 1000, rate 0, count 1", got)
	}

	quota := int64(5)
	got, err = store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &quota})
	if err != nil {
		t.Fatalf("SetTenantLimits quota: %v", err)
	}
	if got.LinkQuota != 5 || got.RateLimit != 0 {
		t.Fatalf("after quota update = %+v, want quota 5, rate 0", got)
	}

	if _, err := store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{}); !errors.Is(err, ErrNoLimitChange) {
		t.Errorf("empty params error = %v, want ErrNoLimitChange", err)
	}
	negative := int64(-1)
	if _, err := store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &negative}); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("negative quota error = %v, want ErrNegativeLimit", err)
	}

	missing := tenantID + 1_000_000
	if _, err := store.GetTenantLimits(ctx, missing); !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("Get missing tenant error = %v, want ErrTenantNotFound", err)
	}
	if _, err := store.SetTenantLimits(ctx, missing, SetTenantLimitsParams{LinkQuota: &quota}); !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("Set missing tenant error = %v, want ErrTenantNotFound", err)
	}
}
