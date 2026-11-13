package cache

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"shortlink/internal/link"
)

const (
	// keyPrefix 把缓存 key 和 Redis 里其他应用的 key 隔开。
	keyPrefix   = "shortlink:link:"
	positiveTTL = time.Hour
	// negativeTTL 更短：不存在的短码不该久占内存，又要挡住随机短码打数据库。
	negativeTTL = time.Minute

	// fallbackConcurrency 是 Redis 读失败时同时回源的最大请求数。
	// 比默认连接池上限 10 小，给创建等其他查询留余量；超出就快速失败，不排队。
	fallbackConcurrency = 8

	// defaultFetchTimeout 只在请求 ctx 没有 deadline 时兜底。正常请求由 timeout 中间件设置。
	defaultFetchTimeout = 5 * time.Second

	getsMetricName     = "shortlink_cache_gets_total"
	writesMetricName   = "shortlink_cache_writes_total"
	durationMetricName = "shortlink_cache_operation_duration_seconds"

	operationGet = "get"
	operationSet = "set"

	resultHit      = "hit"
	resultMiss     = "miss"
	resultFallback = "fallback"
	// resultSkipped 不计数，也不记耗时。调用方 ctx 已结束，不是 Redis 故障。
	resultSkipped = ""

	writeSuccess = "success"
	// writeSkipped 表示负缓存的 SET NX 没写进去：key 已被并发的创建写好。
	writeSkipped = "skipped"
	writeError   = "error"
)

/*
和 HTTP、存储指标用同一组桶：Redis 命中通常在 1ms 内，默认的 5ms 起步太粗。
从 0.1ms 开始按 2 倍增长，覆盖到约 13s。
*/
var durationBuckets = prometheus.ExponentialBuckets(0.0001, 2, 18)

/*
LinkStore 是装饰器需要的最小能力。接口放在使用方，cache 就不依赖 httpapi 和 postgres。
*/
type LinkStore interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

/*
Store 是带 Redis 读缓存的 LinkStore。

Create 成功后写穿，盖掉负缓存，否则新短码会在 TTL 内继续 404。
必须用指针：singleflight.Group 和 fallbackSlots 都是共享状态，复制之后就各记各的。
*/
type Store struct {
	next     LinkStore
	rdb      *redis.Client
	group    singleflight.Group // 零值就能用
	gets     *prometheus.CounterVec
	writes   *prometheus.CounterVec
	duration *prometheus.HistogramVec

	// fallbackSlots 是 Redis 读失败时的回源许可。满了直接拒绝，排队只会一起超时。
	fallbackSlots chan struct{}
}

// New 把指标注册到 registry。测试每次传新的，避免重复注册 panic。
func New(next LinkStore, rdb *redis.Client, registry *prometheus.Registry) *Store {
	s := &Store{
		next: next,
		rdb:  rdb,
		gets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: getsMetricName,
			Help: "Total number of cache lookups by result.",
		}, []string{"result"}),
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: writesMetricName,
			Help: "Total number of cache writes by result.",
		}, []string{"result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    durationMetricName,
			Help:    "Cache operation duration in seconds by operation and result.",
			Buckets: durationBuckets,
		}, []string{"operation", "result"}),
		fallbackSlots: make(chan struct{}, fallbackConcurrency),
	}
	registry.MustRegister(s.gets, s.writes, s.duration)
	return s
}

// Create 成功后写穿正缓存，盖掉负缓存。写失败不影响创建结果。
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if err := s.next.Create(ctx, l); err != nil {
		return err
	}
	s.writeCache(ctx, keyPrefix+l.Code, l)
	return nil
}

/*
GetByCode 未命中或 Redis 故障时回源。负缓存的 value 是 JSON null。

singleflight 的共享回源用独立 ctx：领头请求取消时，等待者不再跟着拿到
context.Canceled。回源保留原来的 deadline，仍受 REQUEST_TIMEOUT 约束。
Redis 读失败时限制回源并发，超过上限快速返回临时过载错误，由 HTTP 层映射成 503。
每个请求按自己的读取结果计数，包括等在 singleflight 里的；计数和耗时都在 readCache 里记。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	key := keyPrefix + code

	cached, result := s.readCache(ctx, key, code)
	if result == resultHit {
		// TTL 到点后 GET 读不到这个 key。这里再看一次 ExpiresAt：
		// 时钟前跳，或 key 的 TTL 比链接寿命更长时，仍然不跳过期链接。
		if cached == nil || cached.Expired(time.Now()) {
			if cached != nil {
				_ = s.rdb.Del(ctx, key).Err()
			}
			return nil, link.ErrNotFound
		}
		return cached, nil
	}

	v, err, _ := s.group.Do(key, func() (any, error) {
		fetchCtx, cancel := fetchContext(ctx)
		defer cancel()

		l, err := s.readStore(fetchCtx, code, result == resultFallback)
		if err != nil && !errors.Is(err, link.ErrNotFound) {
			return nil, err // 真正的错误不写缓存，下次请求应该重试
		}
		s.writeCache(fetchCtx, key, l)
		return l, err
	})

	l, _ := v.(*link.Link)
	return l, err
}

/*
readCache 在调用方 ctx 已结束时返回 resultSkipped，不计数也不记耗时，
不把断开记成降级。

看 ctx.Err()，不看错误里的 DeadlineExceeded：go-redis 拨号用子 context，
超时也是这个错误，但调用方的 ctx 还活着，那是 Redis 没连上。
坏数据删掉后按未命中，让回源重新写入。合法 JSON 缺字段不会报错，
字段名也不区分大小写，所以 URL 为空或 Code 对不上也算坏数据。
*/
func (s *Store) readCache(ctx context.Context, key, code string) (cached *link.Link, result string) {
	start := time.Now()
	defer func() {
		if result == resultSkipped {
			return
		}
		s.gets.WithLabelValues(result).Inc()
		s.duration.WithLabelValues(operationGet, result).Observe(time.Since(start).Seconds())
	}()

	data, err := s.rdb.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cached); err != nil || !usableCachedLink(cached, code) {
			_ = s.rdb.Del(ctx, key).Err()
			return nil, resultMiss
		}
		return cached, resultHit
	case errors.Is(err, redis.Nil):
		return nil, resultMiss
	case ctx.Err() != nil:
		return nil, resultSkipped
	default:
		return nil, resultFallback
	}
}

/*
usableCachedLink 判断命中的值能不能用。
JSON null 是负缓存，解出来是 nil。
零值 Link 会让跳转 302 到空的 Location，短码对不上则会把别的链接拿去跳。
*/
func usableCachedLink(cached *link.Link, code string) bool {
	if cached == nil {
		return true
	}
	return cached.URL != "" && cached.Code == code
}

/*
writeCache 失败只影响下次命中。ctx 已取消时不记成写失败。

负缓存用 SET NX，避免后写的「不存在」盖掉刚创建的正缓存；
SET NX 没写进去说明正缓存已经在，记 skipped 而不是 success。
正缓存用 SET，创建可以盖掉旧的负缓存。

TTL 向下随机抖动，避免同一批写入的 key 集中过期。
*/
func (s *Store) writeCache(ctx context.Context, key string, l *link.Link) {
	if ctx.Err() != nil {
		return
	}

	ttl := negativeTTL
	if l != nil {
		ttl = cacheTTL(l)
		// go-redis 的 Set 在 duration <= 0 时不带过期时间，key 会一直留着。
		if ttl <= 0 {
			return
		}
	}

	data, marshalErr := json.Marshal(l)
	if marshalErr != nil {
		s.writes.WithLabelValues(writeError).Inc()
		return
	}

	start := time.Now()
	var (
		written bool
		setErr  error
	)
	if l == nil {
		written, setErr = s.rdb.SetNX(ctx, key, data, jitteredTTL(ttl)).Result()
	} else {
		setErr = s.rdb.Set(ctx, key, data, jitteredTTL(ttl)).Err()
		written = setErr == nil
	}
	// 调用中取消同样不记。拨号超时不会让 ctx.Err() 非空，仍记成 error。
	if setErr != nil && ctx.Err() != nil {
		return
	}
	result := writeSuccess
	switch {
	case setErr != nil:
		result = writeError
	case !written:
		result = writeSkipped
	}
	s.writes.WithLabelValues(result).Inc()
	s.duration.WithLabelValues(operationSet, result).Observe(time.Since(start).Seconds())
}

/*
fetchContext 给 singleflight 的共享回源派生独立 ctx。
WithoutCancel 去掉取消信号、保留 value；deadline 单独加回来，
回源仍然不能超过请求的处理时限。正常请求由 timeout 中间件设置 deadline，
没有 deadline 时（测试、直接调用）用 defaultFetchTimeout 兜底。
*/
func fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return context.WithTimeout(detached, defaultFetchTimeout)
}

/*
readStore 回源。limited 为 true（Redis 读失败）时先抢一个回源许可，
抢不到立即返回临时过载错误，不排队：Redis 故障时让请求快速失败，
比把数据库连接池排满后一起超时好。许可只包住数据库调用，写缓存不占着它。
*/
func (s *Store) readStore(ctx context.Context, code string, limited bool) (*link.Link, error) {
	if limited {
		select {
		case s.fallbackSlots <- struct{}{}:
			defer func() { <-s.fallbackSlots }()
		default:
			return nil, fallbackBusyError{}
		}
	}
	return s.next.GetByCode(ctx, code)
}

/*
cacheTTL 是这条正缓存能放多久。
没有到期时间时用 1 小时；有的话不能比链接本身活得更久。
*/
func cacheTTL(l *link.Link) time.Duration {
	if l.ExpiresAt == nil {
		return positiveTTL
	}
	remaining := time.Until(*l.ExpiresAt)
	if remaining < positiveTTL {
		return remaining
	}
	return positiveTTL
}

/*
jitteredTTL 把 TTL 向下随机缩短最多 10%。
同一批写入的 key 不会在同一秒集中失效，TTL 又不会超过调用方给的上限。
*/
func jitteredTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	step := ttl / 10
	if step <= 0 {
		return ttl
	}
	return ttl - rand.N(step)
}

// fallbackBusyError 表示 Redis 读失败，同时回源的请求数已达上限。
// Temporary 让 HTTP 层返回 503 而不是 500：这是临时过载，不是程序错误。
type fallbackBusyError struct{}

func (fallbackBusyError) Error() string   { return "cache fallback concurrency limit reached" }
func (fallbackBusyError) Temporary() bool { return true }
