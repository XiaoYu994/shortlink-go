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

	/*
		DefaultMissConcurrency 是普通未命中回源的默认并发上限。
		多 key 压测确认 8 能让连接池留出余量、不产生等待；见 ADR 0008。
	*/
	DefaultMissConcurrency = 8

	/*
		DefaultFetchTimeout 是单次回源超时的默认值。
		数据库正常 p99 在 20ms 量级，1s 足够挡掉排队；
		它独立于 REQUEST_TIMEOUT，见 ADR 0008。
	*/
	DefaultFetchTimeout = time.Second

	getsMetricName     = "shortlink_cache_gets_total"
	writesMetricName   = "shortlink_cache_writes_total"
	durationMetricName = "shortlink_cache_operation_duration_seconds"
	shedMetricName     = "shortlink_cache_shed_total"

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
Options 控制回源保护。零值换成默认值，测试可以只改关心的一项。
MissConcurrency 是普通未命中的回源许可数；FetchTimeout 是单次回源的超时。
*/
type Options struct {
	MissConcurrency int
	FetchTimeout    time.Duration
}

func (o Options) withDefaults() Options {
	if o.MissConcurrency <= 0 {
		o.MissConcurrency = DefaultMissConcurrency
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	return o
}

/*
Store 是带 Redis 读缓存的 LinkStore。

Create 成功后写穿，盖掉负缓存，否则新短码会在 TTL 内继续 404。
必须用指针：singleflight.Group、missSlots 和 fallbackSlots 都是共享状态，
复制之后就各记各的。
*/
type Store struct {
	next     LinkStore
	rdb      *redis.Client
	group    singleflight.Group // 零值就能用
	gets     *prometheus.CounterVec
	writes   *prometheus.CounterVec
	duration *prometheus.HistogramVec
	shed     *prometheus.CounterVec

	// missSlots 是普通未命中的回源许可。和 fallbackSlots 分开：
	// 长尾短码和 Redis 故障是两种压力，不能互相占满配额。
	missSlots chan struct{}
	// fallbackSlots 是 Redis 读失败时的回源许可。满了直接拒绝，排队只会一起超时。
	fallbackSlots chan struct{}

	// fetchTimeout 是单次回源的上限，独立于请求时限。
	fetchTimeout time.Duration
}

// New 把指标注册到 registry。测试每次传新的，避免重复注册 panic。
func New(next LinkStore, rdb *redis.Client, registry *prometheus.Registry, opts Options) *Store {
	opts = opts.withDefaults()
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
		shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: shedMetricName,
			Help: "Total number of cache fetches rejected by the concurrency limit, by path.",
		}, []string{"path"}),
		missSlots:     make(chan struct{}, opts.MissConcurrency),
		fallbackSlots: make(chan struct{}, fallbackConcurrency),
		fetchTimeout:  opts.FetchTimeout,
	}
	registry.MustRegister(s.gets, s.writes, s.duration, s.shed)
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

普通未命中和 Redis 读失败（fallback）各有回源并发上限，超过上限快速返回
临时过载错误，由 HTTP 层映射成 503。回源用独立 ctx 和独立超时：
领头请求取消不再连累等待者，排队上限也不再是 REQUEST_TIMEOUT。
每个请求按自己的读取结果计数，包括等在 singleflight 里的；计数和耗时都在 readCache 里记。
被限流的请求按请求计一次 shed，等待者也会收到同一个错误，见 ADR 0008。
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

	// shared 丢掉：每个请求在进 singleflight 之前已经按自己的读取结果记过数。
	l, err, _ := doTyped[*link.Link](&s.group, key, func() (*link.Link, error) {
		fetchCtx, cancel := fetchContext(ctx, s.fetchTimeout)
		defer cancel()

		found, err := s.readStore(fetchCtx, code, result)
		if err != nil && !errors.Is(err, link.ErrNotFound) {
			return nil, err // 真正的错误不写缓存，下次请求应该重试
		}
		s.writeCache(fetchCtx, key, found)
		return found, err
	})
	if ctx.Err() == nil {
		var shedErr fetchShedError
		if errors.As(err, &shedErr) {
			s.shed.WithLabelValues(shedErr.path).Inc()
		}
	}
	return l, err
}

/*
doTyped 把 singleflight.Do 的 any 收成 T。

Group.Do 的结果类型是 any。回调返回 T，断言只留在这里；
失败说明包装写错了，直接 panic，避免把类型错误变成静默的 nil。

回调先返回 T，再装进 any。T 是 nil 指针时，any 里仍带着 T 的类型，
断言得到 T 的零值。
*/
func doTyped[T any](g *singleflight.Group, key string, fn func() (T, error)) (T, error, bool) {
	v, err, shared := g.Do(key, func() (any, error) {
		return fn()
	})
	return v.(T), err, shared
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
fetchContext 给 singleflight 的共享回源派生独立 ctx 和独立超时。
WithoutCancel 去掉取消信号、保留 value；deadline 用回源超时，
不再借用请求的 REQUEST_TIMEOUT。领头请求取消时，等待者仍能拿到结果；
排队等待数据库的时间也不会再挂满整个请求时限。
*/
func fetchContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

/*
readStore 回源。path 决定走哪条限流：普通 miss 用 missSlots，
Redis 读失败（fallback）用 fallbackSlots。两条路径分开，
Redis 故障不会把正常回源的配额占满。

抢不到许可立即返回临时过载错误，不排队：长尾短码或 Redis 故障时
让请求快速失败，比把数据库连接池排满后一起超时好。
许可只包住数据库调用，写缓存不占着它。
*/
func (s *Store) readStore(ctx context.Context, code, path string) (*link.Link, error) {
	slots := s.missSlots
	if path == resultFallback {
		slots = s.fallbackSlots
	} else {
		// resultSkipped 是 ctx 已取消，仍然按普通回源处理。
		path = resultMiss
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return nil, fetchShedError{path: path}
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

/*
fetchShedError 表示回源并发已达上限。path 区分普通未命中和 Redis 故障降级。
Temporary 让 HTTP 层返回 503 而不是 500：这是临时过载，不是程序错误。
*/
type fetchShedError struct {
	path string
}

func (e fetchShedError) Error() string {
	return "cache " + e.path + " fetch concurrency limit reached"
}

func (fetchShedError) Temporary() bool { return true }
