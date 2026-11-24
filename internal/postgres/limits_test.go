package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/link"
)

// setTenantQuota 直接改测试租户的配额，模拟运维用 console tenant set 之后的库状态。
func setTenantQuota(t *testing.T, store *Store, tenantID, quota int64) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE tenants SET link_quota = $2 WHERE id = $1`, tenantID, quota); err != nil {
		t.Fatalf("set link quota: %v", err)
	}
}

/*
setTenantUsage 把已用量和它所属的月份写成指定值，monthOffset 是相对数据库当前 UTC 月份的月数：
-1 是上个月，0 是本月，1 是下个月。测试改不了数据库时钟，用月份偏移造跨月和时钟偏差；
月份从数据库算，不写死某一天。
*/
func setTenantUsage(t *testing.T, store *Store, tenantID, usage int64, monthOffset int) {
	t.Helper()
	query := `UPDATE tenants
SET link_usage = $2,
    link_usage_month = (` + currentMonthSQL + ` + make_interval(months => $3::int))::date
WHERE id = $1`
	if _, err := store.db.Exec(query, tenantID, usage, monthOffset); err != nil {
		t.Fatalf("set tenant usage: %v", err)
	}
}

// readTenantUsage 读原始的用量列，不做有效已用量的判断：测试要同时断言用量和月份。
func readTenantUsage(t *testing.T, store *Store, tenantID int64) (usage int64, month string) {
	t.Helper()
	var got time.Time
	if err := store.db.QueryRow(
		`SELECT link_usage, link_usage_month FROM tenants WHERE id = $1`, tenantID).Scan(&usage, &got); err != nil {
		t.Fatalf("read tenant usage: %v", err)
	}
	return usage, got.UTC().Format(time.DateOnly)
}

// monthAtOffset 返回相对数据库当前 UTC 月份偏移的月份，用于断言月份写到了哪一个月。
func monthAtOffset(t *testing.T, store *Store, offset int) string {
	t.Helper()
	var got time.Time
	if err := store.db.QueryRow(
		`SELECT (`+currentMonthSQL+` + make_interval(months => $1::int))::date`, offset).Scan(&got); err != nil {
		t.Fatalf("query month: %v", err)
	}
	return got.UTC().Format(time.DateOnly)
}

func countTenantLinks(t *testing.T, store *Store, tenantID int64) (links, usage int64) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT count(*) FROM links WHERE tenant_id = $1`, tenantID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if err := store.db.QueryRow(`SELECT link_usage FROM tenants WHERE id = $1`, tenantID).Scan(&usage); err != nil {
		t.Fatalf("read link_usage: %v", err)
	}
	return links, usage
}

/*
TestCreateResetsStaleMonth 验证跨月懒重置：上个月已经用满配额，本月第一次创建成功，
用量从 1 重新计，月份写成当前月。
*/
func TestCreateResetsStaleMonth(t *testing.T) {
	store, tenantID := newTestStore(t)
	setTenantQuota(t, store, tenantID, 2)
	setTenantUsage(t, store, tenantID, 2, -1)

	if err := store.Create(testAuditCtx(context.Background()), mustLink(t, "https://example.com/new-month", tenantID)); err != nil {
		t.Fatalf("Create after month rollover: %v", err)
	}
	usage, month := readTenantUsage(t, store, tenantID)
	if usage != 1 {
		t.Fatalf("link_usage = %d, want 1", usage)
	}
	if want := monthAtOffset(t, store, 0); month != want {
		t.Fatalf("link_usage_month = %s, want current month %s", month, want)
	}
}

// TestCreateDoesNotResetFutureMonth 验证月份只前进不后退：记录晚于当前月时照常加一，月份不变。
func TestCreateDoesNotResetFutureMonth(t *testing.T) {
	store, tenantID := newTestStore(t)
	setTenantQuota(t, store, tenantID, 10)
	setTenantUsage(t, store, tenantID, 3, 1)

	if err := store.Create(testAuditCtx(context.Background()), mustLink(t, "https://example.com/future-month", tenantID)); err != nil {
		t.Fatalf("Create with future month: %v", err)
	}
	usage, month := readTenantUsage(t, store, tenantID)
	if usage != 4 {
		t.Fatalf("link_usage = %d, want 4", usage)
	}
	if want := monthAtOffset(t, store, 1); month != want {
		t.Fatalf("link_usage_month = %s, want unchanged future month %s", month, want)
	}
}

/*
TestCreateRejectsExhaustedQuota 验证三种拒绝：本月用满、配额为 0、配额为 0 且月份过期。
拒绝时不写链接，也不改写租户行；最后一种同时确认月份不会被 UPDATE 提前推进。
*/
func TestCreateRejectsExhaustedQuota(t *testing.T) {
	tests := []struct {
		name        string
		quota       int64
		usage       int64
		monthOffset int
	}{
		{name: "month full", quota: 2, usage: 2, monthOffset: 0},
		{name: "zero quota", quota: 0, usage: 0, monthOffset: 0},
		{name: "zero quota with stale month", quota: 0, usage: 0, monthOffset: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, tenantID := newTestStore(t)
			setTenantQuota(t, store, tenantID, tt.quota)
			setTenantUsage(t, store, tenantID, tt.usage, tt.monthOffset)
			beforeUsage, beforeMonth := readTenantUsage(t, store, tenantID)

			err := store.Create(testAuditCtx(context.Background()), mustLink(t, "https://example.com/over-quota", tenantID))
			if !errors.Is(err, link.ErrQuotaExceeded) {
				t.Fatalf("Create error = %v, want ErrQuotaExceeded", err)
			}

			afterUsage, afterMonth := readTenantUsage(t, store, tenantID)
			if afterUsage != beforeUsage || afterMonth != beforeMonth {
				t.Fatalf("tenant row changed: usage %d -> %d, month %s -> %s",
					beforeUsage, afterUsage, beforeMonth, afterMonth)
			}
			if links, _ := countTenantLinks(t, store, tenantID); links != 0 {
				t.Fatalf("links = %d, want 0", links)
			}
		})
	}
}

/*
TestDeleteDoesNotReleaseQuota 验证删除不返还配额：
用满后过期一条链接仍然不能创建；删掉它之后用量不变，也仍然不能创建。
*/
func TestDeleteDoesNotReleaseQuota(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	setTenantQuota(t, store, tenantID, 2)

	first := createLink(t, store, tenantID, "https://example.com/first")
	createLink(t, store, tenantID, "https://example.com/second")

	// 过期不改变用量：配额数的是创建动作，不是当前有效链接数。
	if _, err := store.db.Exec(
		`UPDATE links SET expires_at = now() - interval '1 minute' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
	if err := store.Create(ctx, mustLink(t, "https://example.com/still-full", tenantID)); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("Create with an expired link error = %v, want ErrQuotaExceeded", err)
	}

	if err := store.Delete(ctx, tenantID, first.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if links, usage := countTenantLinks(t, store, tenantID); links != 1 || usage != 2 {
		t.Fatalf("after delete links = %d, link_usage = %d, want 1 and 2", links, usage)
	}
	if err := store.Create(ctx, mustLink(t, "https://example.com/after-delete", tenantID)); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("Create after delete error = %v, want ErrQuotaExceeded", err)
	}
}

// TestDeleteDoesNotTouchUsage 验证删除事务不再改写租户行：外部把用量改小后，删除也保持原值。
func TestDeleteDoesNotTouchUsage(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	l := createLink(t, store, tenantID, "https://example.com/floor")
	setTenantUsage(t, store, tenantID, 0, -1)
	beforeUsage, beforeMonth := readTenantUsage(t, store, tenantID)

	if err := store.Delete(ctx, tenantID, l.Code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	afterUsage, afterMonth := readTenantUsage(t, store, tenantID)
	if afterUsage != beforeUsage || afterMonth != beforeMonth {
		t.Fatalf("tenant row changed: usage %d -> %d, month %s -> %s",
			beforeUsage, afterUsage, beforeMonth, afterMonth)
	}
}

/*
TestCreateConcurrentRespectsMonthlyQuota 用 20 个并发创建撞一个上个月用满、配额为 5 的租户。
重置和加一在同一条 UPDATE 里，成功的次数必须正好等于配额，不能多写一条。
*/
func TestCreateConcurrentRespectsMonthlyQuota(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	const quota = 5
	setTenantQuota(t, store, tenantID, quota)
	setTenantUsage(t, store, tenantID, quota, -1)

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
	if links, usage := countTenantLinks(t, store, tenantID); links != quota || usage != quota {
		t.Fatalf("links = %d, link_usage = %d, want %d and %d", links, usage, quota, quota)
	}
	if _, month := readTenantUsage(t, store, tenantID); month != monthAtOffset(t, store, 0) {
		t.Fatalf("link_usage_month = %s, want current month %s", month, monthAtOffset(t, store, 0))
	}
}

func TestAuthenticateReturnsRateLimit(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	if _, err := store.db.Exec(`UPDATE tenants SET rate_limit_per_second = 42 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("set rate limit: %v", err)
	}

	plaintext, hash, err := identity.APIKeyToken.Generate()
	if err != nil {
		t.Fatalf("APIKeyToken.Generate: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, $2, $3::text[])`,
		tenantID, hash, permsToStrings(identity.AllAPIKeyScopes())); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Tenant.ID != tenantID || got.Tenant.RateLimit != 42 {
		t.Fatalf("Authenticate = %+v, want tenant %d rate limit 42", got, tenantID)
	}
}

/*
TestTenantLimits 验证运维命令背后的读写：默认值、只改一个字段、非法参数和租户不存在。
Get 和 Set 的回显都用有效已用量，月份过期时返回 0，而不是上个月的数。
*/
func TestTenantLimits(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	got, err := store.GetTenantLimits(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantLimits: %v", err)
	}
	want := TenantLimits{ID: tenantID, Name: "test", LinkUsage: 0, LinkQuota: 1000, RateLimit: 10}
	if got != want {
		t.Fatalf("GetTenantLimits = %+v, want %+v", got, want)
	}

	createLink(t, store, tenantID, "https://example.com/limits")
	rate := 0
	got, err = store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{RateLimit: &rate})
	if err != nil {
		t.Fatalf("SetTenantLimits rate: %v", err)
	}
	if got.LinkQuota != 1000 || got.RateLimit != 0 || got.LinkUsage != 1 {
		t.Fatalf("after rate update = %+v, want quota 1000, rate 0, usage 1", got)
	}

	quota := int64(5)
	got, err = store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &quota})
	if err != nil {
		t.Fatalf("SetTenantLimits quota: %v", err)
	}
	if got.LinkQuota != 5 || got.RateLimit != 0 {
		t.Fatalf("after quota update = %+v, want quota 5, rate 0", got)
	}

	// 月份过期时 Get 和 Set 的回显都按 0 算，不暴露上个月的用量。
	setTenantUsage(t, store, tenantID, 9, -1)
	got, err = store.GetTenantLimits(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantLimits stale month: %v", err)
	}
	if got.LinkUsage != 0 {
		t.Fatalf("GetTenantLimits stale month usage = %d, want 0", got.LinkUsage)
	}
	rate = 5
	got, err = store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{RateLimit: &rate})
	if err != nil {
		t.Fatalf("SetTenantLimits stale month: %v", err)
	}
	if got.LinkUsage != 0 {
		t.Fatalf("SetTenantLimits stale month usage = %d, want 0", got.LinkUsage)
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
