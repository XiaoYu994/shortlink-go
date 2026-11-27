package safehttp

import (
	"strings"
	"time"

	"shortlink/internal/ratelimit"
)

/*
HostLimiter 按目标站点 host 做进程内有界令牌桶限速，防止后台任务集中冲击同一第三方站点（ADR 0020、ADR 0027）。
底层复用 internal/ratelimit 的精确 LRU 有界桶表，惰性清理空闲桶并限制最大桶数，避免随机域名撑爆内存。
*/
type HostLimiter struct {
	limit   int
	limiter *ratelimit.Limiter[string]
}

// NewHostLimiter 构造按 host 限速器。perSecond <= 0 表示关闭限速。
func NewHostLimiter(perSecond int) *HostLimiter {
	if perSecond <= 0 {
		return &HostLimiter{}
	}
	return &HostLimiter{
		limit:   perSecond,
		limiter: ratelimit.New[string](ratelimit.Options{MaxBuckets: ratelimit.DefaultMaxBuckets}),
	}
}

// Allow 判断当前能否向 host 发起出站请求；被限速时返回建议延迟和 false。
func (l *HostLimiter) Allow(host string) (retryAfter time.Duration, allowed bool) {
	if l == nil || l.limit <= 0 {
		return 0, true
	}
	key := strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	if key == "" {
		return 0, true
	}
	return l.limiter.Allow(key, l.limit, l.limit)
}
