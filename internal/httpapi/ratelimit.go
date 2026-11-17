package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"shortlink/internal/tenant"
)

const (
	/*
		limiterIdleTTL 是令牌桶在内存里保留多久。
		超过这个时间没有请求的租户会被清掉；下次请求重新从满桶开始，
		代价是丢掉一点限流状态，换来 map 不会随着租户数量一直长。
	*/
	limiterIdleTTL = 10 * time.Minute
	// limiterSweepPeriod 是两次清理之间的最短间隔，避免每个请求都遍历整张 map。
	limiterSweepPeriod = time.Minute
)

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
	// now 和下面两个时长可以在测试里替换，生产用默认值。
	now        func() time.Time
	idleTTL    time.Duration
	sweepEvery time.Duration
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newTenantRateLimiter() *tenantRateLimiter {
	now := time.Now
	return &tenantRateLimiter{
		buckets:    make(map[int64]*limiterEntry),
		lastSweep:  now(),
		now:        now,
		idleTTL:    limiterIdleTTL,
		sweepEvery: limiterSweepPeriod,
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

	now := l.now()
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
	l.sweepLocked(now)
	l.mu.Unlock()

	/*
		Reserve 在没有令牌时也会答应一个将来的时刻，Delay 就是需要等的时间。
		超限时取消这次预约，把令牌还回去，否则每次被拒的请求都会往后推。
	*/
	reservation := entry.limiter.Reserve()
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
	if now.Sub(l.lastSweep) < l.sweepEvery {
		return
	}
	l.lastSweep = now
	for id, entry := range l.buckets {
		if now.Sub(entry.lastSeen) > l.idleTTL {
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
		if s.clientGone(r) {
			return
		}
		tn, ok := s.principal(w, r)
		if !ok {
			return
		}
		retryAfter, allowed := s.limiter.allow(tn)
		if !allowed {
			s.metrics.rateLimited.Inc()
			seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			s.writeError(r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
