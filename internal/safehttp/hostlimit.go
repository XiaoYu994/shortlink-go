package safehttp

import (
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	hostLimiterIdleTTL   = 10 * time.Minute
	hostLimiterSweepStep = time.Minute
)

/*
HostLimiter 按目标站点 host 做进程内令牌桶限速，防止后台任务集中冲击同一第三方站点（ADR 0020、ADR 0027）。
空闲桶在请求路径惰性清理，不单开后台 goroutine。
*/
type HostLimiter struct {
	limit     int
	mu        sync.Mutex
	buckets   map[string]*hostBucket
	lastSweep time.Time
}

type hostBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewHostLimiter 构造按 host 限速器。perSecond <= 0 表示关闭限速。
func NewHostLimiter(perSecond int) *HostLimiter {
	return &HostLimiter{
		limit:     perSecond,
		buckets:   make(map[string]*hostBucket),
		lastSweep: time.Now(),
	}
}

// Allow 判断当前能否向 host 发起出站请求；被限速时返回建议延迟和 false。
func (l *HostLimiter) Allow(host string) (retryAfter time.Duration, allowed bool) {
	if l == nil || l.limit <= 0 {
		return 0, true
	}
	key := strings.ToLower(strings.TrimSpace(host))
	if key == "" {
		return 0, true
	}

	now := time.Now()
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = &hostBucket{limiter: rate.NewLimiter(rate.Limit(l.limit), l.limit)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	limiter := b.limiter
	l.sweepLocked(now)
	l.mu.Unlock()

	res := limiter.Reserve()
	if !res.OK() {
		return time.Second, false
	}
	if delay := res.Delay(); delay > 0 {
		res.Cancel()
		return delay, false
	}
	return 0, true
}

func (l *HostLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < hostLimiterSweepStep {
		return
	}
	l.lastSweep = now
	for host, b := range l.buckets {
		if now.Sub(b.lastSeen) > hostLimiterIdleTTL {
			delete(l.buckets, host)
		}
	}
}
