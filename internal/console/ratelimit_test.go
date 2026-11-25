package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
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

func (k tenantKey) Authenticate(_ context.Context, token string) (identity.Principal, error) {
	tn, ok := k[token]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{
		Tenant:    tn,
		ActorKind: identity.ActorAPIKey,
		ActorID:   tn.ID,
		Scopes:    identity.AllAPIKeyScopes(),
	}, nil
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
同一租户的后续请求在虚拟时钟未补满 1 个令牌前仍被拒绝，补满后恢复放行。
*/
func TestRateLimitRejectsWithRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keys := tenantKey{testAPIKey: {ID: 1, RateLimit: 1}}
		srv := newLimitTestServer(keys)
		handler := srv.Routes()

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
		if got := testutil.ToFloat64(srv.rateLimited); got != 1 {
			t.Errorf("rate limited counter = %v, want 1", got)
		}

		// 公开端点和探针不受限流影响，它们不经过 authenticate 和 rateLimit。
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
		}

		// 虚拟时钟只过半秒，桶里只有 0.5 个令牌，仍被拒绝。
		time.Sleep(500 * time.Millisecond)
		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request after 500ms = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if got := testutil.ToFloat64(srv.rateLimited); got != 2 {
			t.Errorf("rate limited counter = %v, want 2", got)
		}

		// 再过半秒凑满 1 秒，x/time/rate 按气泡时钟补回 1 个令牌并恢复放行。
		time.Sleep(500 * time.Millisecond)
		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
			t.Fatalf("request after 1s = %d, want %d", rec.Code, http.StatusCreated)
		}
		if got := testutil.ToFloat64(srv.rateLimited); got != 2 {
			t.Errorf("rate limited counter after recovery = %v, want 2", got)
		}
	})
}

/*
TestRateLimitIsPerTenant 验证一个租户打满配额不会影响另一个租户。
放进 synctest 气泡里保证前后两次请求之间虚拟时钟不前进，不会因机器卡顿意外补回令牌。
*/
func TestRateLimitIsPerTenant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keys := tenantKey{
			testAPIKey:      {ID: 1, RateLimit: 1},
			testAPIKeyOther: {ID: 2, RateLimit: 1},
		}
		handler := newLimitTestServer(keys).Routes()

		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
			t.Fatalf("tenant 1 first request = %d, want %d", rec.Code, http.StatusCreated)
		}
		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("tenant 1 second request = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if rec := doRateLimitedRequest(t, handler, testAPIKeyOther); rec.Code != http.StatusCreated {
			t.Fatalf("tenant 2 first request = %d, want %d", rec.Code, http.StatusCreated)
		}
	})
}

/*
TestRateLimitChangeTakesEffect 验证改掉租户的速率后不用重启：
鉴权读到新值，限流器跟着调整。
*/
func TestRateLimitChangeTakesEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keys := tenantKey{testAPIKey: {ID: 1, RateLimit: 1}}
		srv := newLimitTestServer(keys)
		handler := srv.Routes()

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
	})
}

// TestRateLimitDisabled 验证 rate_limit_per_second 为 0 时不限流，也不占限流桶。
func TestRateLimitDisabled(t *testing.T) {
	srv := newLimitTestServer(tenantKey{testAPIKey: {ID: 1}})
	handler := srv.Routes()

	for i := range 20 {
		if rec := doRateLimitedRequest(t, handler, testAPIKey); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d, want %d", i, rec.Code, http.StatusCreated)
		}
	}
	if got := srv.limiter.Len(); got != 0 {
		t.Errorf("limiter buckets = %d, want 0 when the tenant is unlimited", got)
	}
}

type countingAuth struct {
	calls int
}

func (a *countingAuth) Authenticate(context.Context, string) (identity.Principal, error) {
	a.calls++
	return identity.Principal{}, identity.ErrUnauthenticated
}

func TestIPRateLimitBlocksForgedTokensBeforeAuth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := &countingAuth{}
		srv := newServer(serverDeps{
			logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			store:             memorystore.New(),
			requestTimeout:    5 * time.Second,
			registry:          newTestRegistry(),
			keys:              auth,
			authorizer:        identity.NewMapAuthorizer(),
			trustProxyHeaders: true,
			trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
			ipRateLimit:       1,
			ipRateBurst:       2,
		})
		handler := srv.Routes()

		send := func(ip, xff string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
			req.RemoteAddr = ip
			if xff != "" {
				req.Header.Set("X-Forwarded-For", xff)
			}
			// 格式合法但不存在的伪造 Bearer Token，若未被限流会直接穿透到 Authenticate。
			req.Header.Set("Authorization", "Bearer slk_"+strings.Repeat("12", 32))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}

		// 前两次消耗 burst=2，放行到鉴权返回 401；第 3 次直接 429，不触发 Authenticate。
		for i := range 2 {
			if rec := send("10.0.0.5:1234", "203.0.113.7, 10.0.0.9"); rec.Code != http.StatusUnauthorized {
				t.Fatalf("req %d = %d, want 401", i, rec.Code)
			}
		}
		rec := send("10.0.0.5:1234", "203.0.113.7, 10.0.0.9")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("3rd req = %d, want 429", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("missing Retry-After on 429 response")
		}
		if auth.calls != 2 {
			t.Errorf("Authenticate calls = %d, want 2 (blocked before DB lookup)", auth.calls)
		}
		if got := testutil.ToFloat64(srv.ipRateLimited); got != 1 {
			t.Errorf("ipRateLimited = %v, want 1", got)
		}
	})
}

func TestIPRateLimiterBoundedBuckets(t *testing.T) {
	l := newIPRateLimiter(10, 20, 4)
	for i := range 12 {
		l.allow(fmt.Sprintf("203.0.113.%d", i))
	}
	if got := l.limiter.Len(); got > 4 {
		t.Fatalf("buckets len = %d, want <= 4", got)
	}
}

func TestIPRateLimiterEmptyIPUsesUnknownBucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIPRateLimiter(1, 1, 4)
		if _, ok := l.allow(""); !ok {
			t.Fatal("first empty-ip request should be allowed")
		}
		if _, ok := l.allow(""); ok {
			t.Fatal("second immediate empty-ip request should share unknown bucket and be rejected")
		}
	})
}
