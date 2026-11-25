/*
Package ratelimit 提供零业务依赖的进程内按 Key 令牌桶限流器。

基于 golang.org/x/time/rate 与标准库 container/list 维护按最近访问时间有序的精确 LRU 桶表：
支持固定速率与动态速率（参数变更时重置满桶），通过 MaxBuckets 与 IdleTTL 控制内存上界，
满容驱逐与过期回收均从队尾 O(1) 起步，避免高并发随机 Key 洪水触发全表持锁扫描。
本包不依赖 HTTP、Prometheus 或任何业务领域包，由 console 与 safehttp 复用。
*/
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// DefaultIdleTTL 是令牌桶空闲超过该时长后可被回收的默认门槛。
	DefaultIdleTTL = 10 * time.Minute
	// DefaultSweepInterval 是两次惰性过期回收之间的默认最短间隔。
	DefaultSweepInterval = time.Minute
	// DefaultMaxBuckets 是有界限流器未显式指定上限时的默认最大桶数。
	DefaultMaxBuckets = 65536
	// defaultMaxSweepBatch 限制单次惰性回收最多清理的过期节点数，避免大批条目同时过期时长时间持锁。
	defaultMaxSweepBatch = 64
)

// Options 配置按 Key 令牌桶限流器的容量上限与惰性回收策略；零值字段使用默认值。
type Options struct {
	/*
		MaxBuckets 限制内存中保留的最大桶数，防无界 Key（来源 IP、外部 host）撑爆内存。
		0 表示不设硬上限（仅适用于租户数受控等有界场景）；负数按 0 处理。
	*/
	MaxBuckets int
	// IdleTTL 是桶空闲超过该时长后可被回收的门槛；<=0 时使用 DefaultIdleTTL。
	IdleTTL time.Duration
	// SweepInterval 是两次惰性过期回收之间的最短间隔；<=0 时使用 DefaultSweepInterval。
	SweepInterval time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxBuckets < 0 {
		o.MaxBuckets = 0
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = DefaultIdleTTL
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = DefaultSweepInterval
	}
	return o
}

/*
Limiter 维护按 Key 隔离的进程内令牌桶。
结构体带锁，必须使用指针接收者；惰性清理随请求触发，不单开后台 goroutine。
*/
type Limiter[K comparable] struct {
	maxBuckets    int
	idleTTL       time.Duration
	sweepInterval time.Duration

	mu        sync.Mutex
	buckets   map[K]*list.Element
	lru       *list.List // 队头最新、队尾最旧，节点值为 *bucketEntry[K]
	lastSweep time.Time
}

type bucketEntry[K comparable] struct {
	key      K
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New 构造按 Key 隔离的进程内令牌桶限流器。
func New[K comparable](opts Options) *Limiter[K] {
	opts = opts.withDefaults()
	return &Limiter[K]{
		maxBuckets:    opts.MaxBuckets,
		idleTTL:       opts.IdleTTL,
		sweepInterval: opts.SweepInterval,
		buckets:       make(map[K]*list.Element),
		lru:           list.New(),
		lastSweep:     time.Now(),
	}
}

/*
Allow 判断 key 对应的请求能否在给定速率（rps）与突发容量（burst）下放行。
rps <= 0 表示不限制，直接放行且不占桶位；burst <= 0 时默认等于 rps。
同一 key 的 rps 或 burst 变更时会重建满桶限流器，让新配置立即生效。
返回 false 时 retryAfter 是建议的重试等待时长。
*/
func (l *Limiter[K]) Allow(key K, rps, burst int) (retryAfter time.Duration, allowed bool) {
	if l == nil || rps <= 0 {
		return 0, true
	}
	if burst <= 0 {
		burst = rps
	}

	now := time.Now()
	limit := rate.Limit(rps)

	l.mu.Lock()
	elem, ok := l.buckets[key]
	var entry *bucketEntry[K]
	if !ok {
		for l.maxBuckets > 0 && len(l.buckets) >= l.maxBuckets {
			l.evictOldestLocked()
		}
		entry = &bucketEntry[K]{
			key:      key,
			limiter:  rate.NewLimiter(limit, burst),
			lastSeen: now,
		}
		elem = l.lru.PushFront(entry)
		l.buckets[key] = elem
	} else {
		entry = elem.Value.(*bucketEntry[K])
		if entry.limiter.Limit() != limit || entry.limiter.Burst() != burst {
			/*
				速率或突发容量变更：换一个满桶新限流器，避免只调 SetLimit/SetBurst 时
				桶内余量不足 1 枚令牌导致新配置看起来未生效。
			*/
			entry.limiter = rate.NewLimiter(limit, burst)
		}
		entry.lastSeen = now
		l.lru.MoveToFront(elem)
	}
	/*
		桶指针必须在锁内取出：解锁后别的协程可能因速率变更替换 entry.limiter，
		锁外直接读该字段会构成数据竞争。*rate.Limiter 自身并发安全，锁外调用可缩短持锁时间。
	*/
	limiter := entry.limiter
	l.sweepLocked(now)
	l.mu.Unlock()

	return reserve(limiter)
}

// Len 返回当前驻留在内存中的活跃桶数量。
func (l *Limiter[K]) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// evictOldestLocked O(1) 驱逐最久未活跃的队尾桶。调用方必须持有 l.mu。
func (l *Limiter[K]) evictOldestLocked() {
	oldest := l.lru.Back()
	if oldest == nil {
		return
	}
	entry := oldest.Value.(*bucketEntry[K])
	l.lru.Remove(oldest)
	delete(l.buckets, entry.key)
}

/*
sweepLocked 从 LRU 队尾按空闲时长惰性回收过期桶，调用方必须持有 l.mu。
由于链表按 lastSeen 有序，遇到第一个未过期节点即可立刻停止，满容全活跃时仅 O(1)；
单次最多清理 defaultMaxSweepBatch 个节点，若队尾仍有过期项则不推进 lastSweep，
把大批同时过期的清理成本分摊到后续请求中。
*/
func (l *Limiter[K]) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.sweepInterval {
		return
	}
	for range defaultMaxSweepBatch {
		oldest := l.lru.Back()
		if oldest == nil {
			l.lastSweep = now
			return
		}
		entry := oldest.Value.(*bucketEntry[K])
		if now.Sub(entry.lastSeen) <= l.idleTTL {
			l.lastSweep = now
			return
		}
		l.lru.Remove(oldest)
		delete(l.buckets, entry.key)
	}
	if oldest := l.lru.Back(); oldest == nil || now.Sub(oldest.Value.(*bucketEntry[K]).lastSeen) <= l.idleTTL {
		l.lastSweep = now
	}
}

func reserve(limiter *rate.Limiter) (time.Duration, bool) {
	/*
		Reserve 在无令牌时会预支未来时刻的令牌，Delay 是需要等待的时间。
		超限拒收时必须 Cancel 退回预约令牌，否则高频重试会持续透支未来配额导致长期锁死。
	*/
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
