package console

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"shortlink/internal/httpx"
	"shortlink/internal/logx"
	"shortlink/internal/tenant"
)

const (
	rateLimitedMetricName   = "shortlink_rate_limited_total"
	ipRateLimitedMetricName = "shortlink_ip_rate_limited_total"
	defaultMaxIPBuckets     = 65536
	/*
		limiterIdleTTL 是令牌桶在内存里保留多久。
		超过这个时间没有请求的租户会被清掉；下次请求重新从满桶开始，
		代价是丢掉一点限流状态，换来 map 不会随着租户数量一直长。
	*/
	limiterIdleTTL = 10 * time.Minute
	// limiterSweepPeriod 是两次清理之间的最短间隔，避免每个请求都遍历整张 map。
	limiterSweepPeriod = time.Minute
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
tenantRateLimiter 是管理 API 的进程内限流状态，每个租户一个令牌桶。
速率和突发容量都取租户的 rate_limit_per_second：速率 N 表示长期每秒 N 个请求，
容量 N 允许一次突发 N 个。0 表示不限制，连桶都不建。

桶会和数据库里的配置对齐：鉴权每次从库里读出当前值，值变了就换一个新桶从满桶开始，
改完库下一次请求就生效，不用重启。多副本时每个副本各限各的，总量是单副本上限乘以副本数。

map 的清理在每个请求里顺带做，不为它单开 goroutine，也就不需要额外的退出点。
*/
type tenantRateLimiter struct {
	mu        sync.Mutex
	buckets   map[int64]*limiterEntry
	lastSweep time.Time
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newTenantRateLimiter() *tenantRateLimiter {
	return &tenantRateLimiter{
		buckets:   make(map[int64]*limiterEntry),
		lastSweep: time.Now(),
	}
}

/*
allow 判断一次请求能不能放行。
返回 false 时 retryAfter 是建议的重试等待，调用方把它写进 Retry-After。
rate_limit_per_second 为 0 的租户直接放行，不占 map 条目。
*/
func (l *tenantRateLimiter) allow(tn tenant.Tenant) (retryAfter time.Duration, allowed bool) {
	if tn.RateLimit <= 0 {
		return 0, true
	}

	now := time.Now()
	l.mu.Lock()
	entry, ok := l.buckets[tn.ID]
	if !ok {
		entry = &limiterEntry{limiter: rate.NewLimiter(rate.Limit(tn.RateLimit), tn.RateLimit)}
		l.buckets[tn.ID] = entry
	} else if entry.limiter.Limit() != rate.Limit(tn.RateLimit) || entry.limiter.Burst() != tn.RateLimit {
		/*
			数据库里的限制改过：换一个新桶，从满桶开始。
			只调 SetLimit / SetBurst 的话，桶里不足一个令牌时新请求仍会被拒，
			看起来像改了没生效。重启一个桶的代价只是丢掉几秒的令牌状态。
		*/
		entry.limiter = rate.NewLimiter(rate.Limit(tn.RateLimit), tn.RateLimit)
	}
	entry.lastSeen = now
	/*
		桶指针在锁里取出。解锁后别的请求可能因为速率变了给 entry.limiter 换新桶，
		锁外再读这个字段就是数据竞争。*rate.Limiter 自己是并发安全的，锁外用它没问题；
		赶上换桶时这次请求按旧桶算，只差一次。
	*/
	limiter := entry.limiter
	l.sweepLocked(now)
	l.mu.Unlock()

	/*
		Reserve 在没有令牌时也会答应一个将来的时刻，Delay 就是需要等的时间。
		超限时取消这次预约，把令牌还回去，否则每次被拒的请求都会往后推。
	*/
	reservation := limiter.Reserve()
	if !reservation.OK() {
		return time.Second, false
	}
	if delay := reservation.Delay(); delay > 0 {
		reservation.Cancel()
		return delay, false
	}
	return 0, true
}

// sweepLocked 清掉长时间没有请求的租户。调用方必须持有 l.mu。
func (l *tenantRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < limiterSweepPeriod {
		return
	}
	l.lastSweep = now
	for id, entry := range l.buckets {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(l.buckets, id)
		}
	}
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
		retryAfter, allowed := s.limiter.allow(p.Tenant)
		if !allowed {
			s.rateLimited.Inc()
			seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
			logx.SetRateLimited(r.Context(), seconds)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			httpx.WriteError(s.logger, r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

/*
ipRateLimiter 是鉴权前的有界源 IP 令牌桶限流器，阻断伪造凭证或未授权洪水穿透查库（ADR 0024）。
map 设有 maxBuckets 硬上限：超容时先立即清理空闲条目；若仍满则 O(1) 驱逐一个已有条目，
避免 O(N) 扫描持锁阻塞，确保内存占用严格有界。
*/
type ipRateLimiter struct {
	rps        int
	burst      int
	maxBuckets int

	mu        sync.Mutex
	buckets   map[string]*limiterEntry
	lastSweep time.Time
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
		rps:        rps,
		burst:      burst,
		maxBuckets: maxBuckets,
		buckets:    make(map[string]*limiterEntry),
		lastSweep:  time.Now(),
	}
}

func (l *ipRateLimiter) allow(ip string) (retryAfter time.Duration, allowed bool) {
	if l == nil || l.rps <= 0 {
		return 0, true
	}
	if ip == "" {
		ip = "unknown"
	}

	now := time.Now()
	l.mu.Lock()
	entry, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.sweepLocked(now, true)
			for len(l.buckets) >= l.maxBuckets {
				for k := range l.buckets {
					delete(l.buckets, k)
					break
				}
			}
		}
		entry = &limiterEntry{
			limiter: rate.NewLimiter(rate.Limit(l.rps), l.burst),
		}
		l.buckets[ip] = entry
	}
	entry.lastSeen = now
	limiter := entry.limiter
	l.sweepLocked(now, false)
	l.mu.Unlock()

	reservation := limiter.Reserve()
	if !reservation.OK() {
		return time.Second, false
	}
	if delay := reservation.Delay(); delay > 0 {
		reservation.Cancel()
		return delay, false
	}
	return 0, true
}

func (l *ipRateLimiter) sweepLocked(now time.Time, force bool) {
	if !force && now.Sub(l.lastSweep) < limiterSweepPeriod {
		return
	}
	l.lastSweep = now
	for ip, entry := range l.buckets {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(l.buckets, ip)
		}
	}
}

func (s *Server) ipRateLimit(next http.Handler) http.Handler {
	if s.ipLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsClientGone(s.logger, r) {
			return
		}
		ip := httpx.ClientIPWithTrustedProxies(r, s.trustProxy, s.trustedProxies)
		retryAfter, allowed := s.ipLimiter.allow(ip)
		if !allowed {
			s.ipRateLimited.Inc()
			seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
			logx.SetRateLimited(r.Context(), seconds)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			httpx.WriteError(s.logger, r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
