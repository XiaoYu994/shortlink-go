package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"shortlink/internal/identity"
	"shortlink/internal/memorystore"
	"shortlink/internal/tenant"
)

// testAPIKeyOther 是第二个租户的固定密钥，格式和 testAPIKey 一样。
var testAPIKeyOther = "slk_" + strings.Repeat("cd", 32)

// tenantKey 按密钥返回不同租户，用来验证按租户限流和改值生效。
type tenantKey map[string]tenant.Tenant

func (k tenantKey) Authenticate(_ context.Context, token string) (tenant.Tenant, error) {
	tn, ok := k[token]
	if !ok {
		return tenant.Tenant{}, tenant.ErrUnauthenticated
	}
	return tn, nil
}

func newLimitTestServer(keys APIKeyAuthenticator) *Server {
	return newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           keys,
		authorizer:     identity.NewMapAuthorizer(),
	})
}

// doRateLimitedRequest 用指定密钥创建一条链接，返回响应。
func doRateLimitedRequest(t *testing.T, handler http.Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/limited"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

/*
TestRateLimitRejectsWithRetryAfter 验证超限返回 429、Retry-After 和 JSON 错误体，
并且同一个租户的下一次请求仍被拒绝。
*/
func TestRateLimitRejectsWithRetryAfter(t *testing.T) {
	keys := tenantKey{testAPIKey: {ID: 1, RateLimit: 1}}
	srv := newLimitTestServer(keys)
	handler := srv.ConsoleRoutes()

	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
		t.Fatalf("first request = %d, want %d", rec.Code, http.StatusCreated)
	}

	rec := doRateLimitedRequest(t, handler, testAPIKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got, want := rec.Body.String(), `{"error":"rate limit exceeded"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Errorf("Retry-After = %q, want an integer of at least 1 (err %v)", rec.Header().Get("Retry-After"), err)
	}
	if got := testutil.ToFloat64(srv.metrics.rateLimited); got != 1 {
		t.Errorf("rate limited counter = %v, want 1", got)
	}

	// 公开端点和探针不受限流影响，它们不经过 authenticate 和 rateLimit。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

/*
TestRateLimitIsPerTenant 验证一个租户打满配额不会影响另一个租户。
*/
func TestRateLimitIsPerTenant(t *testing.T) {
	keys := tenantKey{
		testAPIKey:      {ID: 1, RateLimit: 1},
		testAPIKeyOther: {ID: 2, RateLimit: 1},
	}
	handler := newLimitTestServer(keys).ConsoleRoutes()

	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
		t.Fatalf("tenant 1 first request = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant 1 second request = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if rec := doRateLimitedRequest(t, handler, testAPIKeyOther); rec.Code != http.StatusCreated {
		t.Fatalf("tenant 2 first request = %d, want %d", rec.Code, http.StatusCreated)
	}
}

/*
TestRateLimitChangeTakesEffect 验证改掉租户的速率后不用重启：
鉴权读到新值，限流器跟着调整。
*/
func TestRateLimitChangeTakesEffect(t *testing.T) {
	keys := tenantKey{testAPIKey: {ID: 1, RateLimit: 1}}
	srv := newLimitTestServer(keys)
	handler := srv.ConsoleRoutes()

	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
		t.Fatalf("first request = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	// 和 SetTenantLimits 之后下一次鉴权返回的效果一样：同一个租户、新的速率。
	keys[testAPIKey] = tenant.Tenant{ID: 1, RateLimit: 1000}
	if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
		t.Fatalf("request after limit increase = %d, want %d", rec.Code, http.StatusCreated)
	}
}

/*
TestRateLimiterConcurrentLimitChange 让同一个租户的并发请求交替带两种速率，
每次都会换桶。换桶在锁里写 entry.limiter，取令牌必须用锁里拿到的那个桶，
否则 -race 会报出锁外读和锁内写的竞争。这里不断言放行结果，只靠 -race 发现问题。
*/
func TestRateLimiterConcurrentLimitChange(t *testing.T) {
	l := newTenantRateLimiter()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			tn := tenant.Tenant{ID: 1, RateLimit: 10 + i%2}
			for range 200 {
				l.allow(tn)
			}
		})
	}
	wg.Wait()
}

// TestRateLimitDisabled 验证 rate_limit_per_second 为 0 时不限流，也不占限流 map。
func TestRateLimitDisabled(t *testing.T) {
	srv := newLimitTestServer(tenantKey{testAPIKey: {ID: 1}})
	handler := srv.ConsoleRoutes()

	for i := range 20 {
		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d, want %d", i, rec.Code, http.StatusCreated)
		}
	}
	if got := len(srv.limiter.buckets); got != 0 {
		t.Errorf("limiter buckets = %d, want 0 when the tenant is unlimited", got)
	}
}

/*
TestRateLimiterEvictsIdle 用假时钟验证没有请求的租户会被清掉。
清理在每个请求里顺带触发，不需要后台 goroutine。
*/
func TestRateLimiterEvictsIdle(t *testing.T) {
	l := newTenantRateLimiter()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	now := base
	l.now = func() time.Time { return now }
	l.lastSweep = base
	l.idleTTL = time.Minute
	l.sweepEvery = 10 * time.Second

	l.allow(tenant.Tenant{ID: 1, RateLimit: 10})
	l.allow(tenant.Tenant{ID: 2, RateLimit: 10})

	// 越过空闲时限后再来一个租户，触发清理。
	now = base.Add(2 * time.Minute)
	l.allow(tenant.Tenant{ID: 3, RateLimit: 10})

	for _, id := range []int64{1, 2} {
		if _, ok := l.buckets[id]; ok {
			t.Errorf("idle tenant %d was not evicted", id)
		}
	}
	if _, ok := l.buckets[3]; !ok {
		t.Error("active tenant 3 is missing from the limiter")
	}
}
