package cache

import (
	"context"
	"encoding/json"
	"errors"
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
必须用指针：singleflight.Group 里有锁，复制后就各记各的。
*/
type Store struct {
	next     LinkStore
	rdb      *redis.Client
	group    singleflight.Group // 零值就能用
	gets     *prometheus.CounterVec
	writes   *prometheus.CounterVec
	duration *prometheus.HistogramVec
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

singleflight 用领头请求的 ctx，它取消时等待者也拿到 context.Canceled。
每个请求按自己的读取结果计数，包括等在 singleflight 里的；计数和耗时都在 readCache 里记。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	key := keyPrefix + code

	cached, result := s.readCache(ctx, key)
	if result == resultHit {
		if cached == nil {
			return nil, link.ErrNotFound
		}
		return cached, nil
	}

	v, err, _ := s.group.Do(key, func() (any, error) {
		l, err := s.next.GetByCode(ctx, code)
		if err != nil && !errors.Is(err, link.ErrNotFound) {
			return nil, err // 真正的错误不写缓存，下次请求应该重试
		}
		s.writeCache(ctx, key, l)
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
坏数据删掉后按未命中，让回源重新写入。
*/
func (s *Store) readCache(ctx context.Context, key string) (cached *link.Link, result string) {
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
		if err := json.Unmarshal(data, &cached); err != nil {
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
writeCache 失败只影响下次命中。ctx 已取消时不记成写失败。

负缓存用 SET NX，避免后写的「不存在」盖掉刚创建的正缓存；
SET NX 没写进去说明正缓存已经在，记 skipped 而不是 success。
正缓存用 SET，创建可以盖掉旧的负缓存。
*/
func (s *Store) writeCache(ctx context.Context, key string, l *link.Link) {
	if ctx.Err() != nil {
		return
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
		written, setErr = s.rdb.SetNX(ctx, key, data, negativeTTL).Result()
	} else {
		setErr = s.rdb.Set(ctx, key, data, positiveTTL).Err()
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
