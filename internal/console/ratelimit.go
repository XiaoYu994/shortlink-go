package console

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/httpx"
	"shortlink/internal/logx"
	"shortlink/internal/ratelimit"
)

const (
	rateLimitedMetricName   = "shortlink_rate_limited_total"
	ipRateLimitedMetricName = "shortlink_ip_rate_limited_total"
	defaultMaxIPBuckets     = ratelimit.DefaultMaxBuckets
)

func newRateLimitedCounter(registry *prometheus.Registry) prometheus.Counter {
	// 租户 ID 不能做 label：租户数量无上界，会把时间序列撑开。这里只记总数。
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: rateLimitedMetricName,
		Help: "Total number of management API requests rejected by the per-tenant rate limiter.",
	})
	registry.MustRegister(c)
	return c
}

func newIPRateLimitedCounter(registry *prometheus.Registry) prometheus.Counter {
	// 来源 IP 基数无界，不放入 label，防止 Prometheus 时序膨胀。
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: ipRateLimitedMetricName,
		Help: "Total number of management API requests rejected by the pre-auth per-IP rate limiter.",
	})
	registry.MustRegister(c)
	return c
}

/*
ipRateLimiter 是鉴权前的有界源 IP 令牌桶限流器，阻断伪造凭证或未授权洪水穿透查库（ADR 0024）。
底层复用 internal/ratelimit 的精确 LRU 有界桶表，满容时 O(1) 淘汰最久未活跃条目。
*/
type ipRateLimiter struct {
	limiter *ratelimit.Limiter[string]
	rps     int
	burst   int
}

func newIPRateLimiter(rps, burst, maxBuckets int) *ipRateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = rps
	}
	if maxBuckets <= 0 {
		maxBuckets = defaultMaxIPBuckets
	}
	return &ipRateLimiter{
		limiter: ratelimit.New[string](ratelimit.Options{MaxBuckets: maxBuckets}),
		rps:     rps,
		burst:   burst,
	}
}

func (l *ipRateLimiter) allow(ip string) (retryAfter time.Duration, allowed bool) {
	if l == nil || l.rps <= 0 {
		return 0, true
	}
	if ip == "" {
		ip = "unknown"
	}
	return l.limiter.Allow(ip, l.rps, l.burst)
}

/*
rateLimit 按租户限制管理 API 的请求速率。
它必须套在 authenticate 里面：要等鉴权把租户写进 context 才能按租户取桶。
超限返回 429 和 Retry-After（整数秒，至少 1），错误体和其他接口一样是 {"error":"..."}。
*/
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsClientGone(s.logger, r) {
			return
		}
		p, ok := principalOrInternalError(s.logger, w, r)
		if !ok {
			return
		}
		retryAfter, allowed := s.limiter.Allow(p.Tenant.ID, p.Tenant.RateLimit, p.Tenant.RateLimit)
		if !allowed {
			s.writeRateLimited(w, r, s.rateLimited, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ipRateLimit(next http.Handler) http.Handler {
	if s.ipLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsClientGone(s.logger, r) {
			return
		}
		ip := httpx.ClientIPFrom(r.Context())
		retryAfter, allowed := s.ipLimiter.allow(ip)
		if !allowed {
			s.writeRateLimited(w, r, s.ipRateLimited, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeRateLimited(w http.ResponseWriter, r *http.Request, counter prometheus.Counter, retryAfter time.Duration) {
	counter.Inc()
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
	logx.SetRateLimited(r.Context(), seconds)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	httpx.WriteError(s.logger, r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
}
