package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"shortlink/internal/link"
	"shortlink/internal/logx"
)

const (
	// keyPrefix 把缓存 key 和 Redis 里其他应用的 key 隔开。
	keyPrefix   = "shortlink:link:"
	positiveTTL = time.Hour
	// negativeTTL 更短：不存在的短码不该久占内存，又要挡住随机短码打数据库。
	negativeTTL = time.Minute

	/*
		fallbackConcurrency 是 Redis 读失败时同时回源的最大请求数。
		比默认连接池上限 10 小，给创建等其他查询留余量；超出就快速失败，不排队。
	*/
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

	/*
		committedWriteTimeout 是管理写提交之后更新缓存的上限。
		这段不跟请求取消走：客户端断开或请求超时的时候，数据库已经提交。
		若这时跳过 SET，删除的链接会继续跳转，直到正缓存 TTL（最长 1 小时）。
	*/
	committedWriteTimeout = time.Second

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
	/*
		writeSkipped 表示回源的 SET NX 没写进去：key 已经有一个更新的值
		（并发的创建或管理写）。管理写的无条件 SET 不会产生这个结果。
	*/
	writeSkipped = "skipped"
	writeError   = "error"
)

/*
writeMode 区分回源和管理写。

回源用 writeNX：请求读到的是数据库的旧快照，只有 key 不存在时才写，
避免修改或删除提交之后，在途请求把旧值盖回来。

管理写（创建、修改、删除）用 writeOverwrite：数据库已经提交，
无条件覆盖缓存；删除写负缓存，修改后的链接已过期也写负缓存。
*/
type writeMode int

const (
	writeNX writeMode = iota
	writeOverwrite
)

/*
和 HTTP、存储指标用同一组桶：Redis 命中通常在 1ms 内，默认的 5ms 起步太粗。
从 0.1ms 开始按 2 倍增长，覆盖到约 13s。
*/
var durationBuckets = prometheus.ExponentialBuckets(0.0001, 2, 18)

/*
ErrNotUpdated 表示数据库已经提交，但缓存没有落到正确的值上。
Ban、Unban 返回这个哨兵时，*link.Link 是数据库里的结果；
调用方用 errors.Is 区分「库都没写成」（直接返回下游错误）和
「库写了、缓存没更新」（可以重跑同一条命令补写缓存）。
*/
var ErrNotUpdated = errors.New("cache not updated after successful write")

/*
LinkStore 是装饰器需要的完整能力，按基础、管理、统计和平台下架分组。
接口放在使用方，cache 就不依赖 redirector、console 和 postgres。
Create、Update、Delete、Ban、Unban 除了写库还要维护缓存，其余方法直接透传。
*/
type LinkStore interface {
	LinkBasics
	LinkAdmin
	LinkStats
	LinkTakedown
}

// LinkBasics 是创建和跳转查询。
type LinkBasics interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

// LinkAdmin 是管理接口的列表、详情、修改和删除。
type LinkAdmin interface {
	List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error)
	GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error)
	Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error)
	Delete(ctx context.Context, tenantID int64, code string) error
}

// LinkStats 是点击统计查询。
type LinkStats interface {
	Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error)
}

// LinkTakedown 是平台封禁和解封，按短码操作，不需要租户 ID。
type LinkTakedown interface {
	Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	Unban(ctx context.Context, code string) (*link.Link, error)
}

/*
Options 控制回源保护和日志。零值换成默认值，测试可以只改关心的一项。
MissConcurrency 是普通未命中的回源许可数；FetchTimeout 是单次回源的超时。
Logger 只用来记录写缓存失败；为 nil 时丢弃日志，生产由 main 注入。
*/
type Options struct {
	MissConcurrency int
	FetchTimeout    time.Duration
	Logger          *slog.Logger
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

	/*
		missSlots 是普通未命中的回源许可。和 fallbackSlots 分开：
		长尾短码和 Redis 故障是两种压力，不能互相占满配额。
	*/
	missSlots chan struct{}
	// fallbackSlots 是 Redis 读失败时的回源许可。满了直接拒绝，排队只会一起超时。
	fallbackSlots chan struct{}

	// fetchTimeout 是单次回源的上限，独立于请求时限。
	fetchTimeout time.Duration
	logger       *slog.Logger
}

// New 把指标注册到 registry。测试每次传新的，避免重复注册 panic。
func New(next LinkStore, rdb *redis.Client, registry *prometheus.Registry, opts Options) *Store {
	opts = opts.withDefaults()
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Store{
		next:   next,
		rdb:    rdb,
		logger: logger,
		gets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: getsMetricName,
			Help: "Total number of cache lookups by result.",
		}, []string{"result"}),
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: writesMetricName,
			Help: "Total number of cache writes by result; skipped means an origin SET NX found a newer value.",
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

/*
Create 成功后写穿正缓存，盖掉负缓存；写完后再按跳转查询复查一次数据库。
异步风控上线后（ADR 0016、ADR 0020），创建提交后 worker 可能立刻封禁并写负缓存，
复查确保晚到的正缓存不会把风控负缓存盖回去。写失败不影响创建结果。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if err := s.next.Create(ctx, l); err != nil {
		return err
	}
	key := keyPrefix + l.Code
	if !l.Servable(time.Now()) {
		s.writeCommitted(ctx, key, nil)
		return nil
	}
	s.writeCommitted(ctx, key, l)
	if _, err := s.recheckAfterPositiveWrite(ctx, key, l.Code); err != nil {
		s.logger.WarnContext(ctx, "write negative cache after recheck", "code", l.Code, "err", err)
	}
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
		/*
			TTL 到点后 GET 读不到这个 key。这里再按 Servable 判断一次：
			时钟前跳，或 key 的 TTL 比链接寿命更长时，仍然不跳过期链接；
			带着停用或封禁信息的正缓存也按找不到处理，防写缓存的地方漏判。
		*/
		if cached == nil || !cached.Servable(time.Now()) {
			logx.SetCacheResult(ctx, "neg_hit")
			if cached != nil {
				_ = s.rdb.Del(ctx, key).Err()
			}
			return nil, link.ErrNotFound
		}
		logx.SetCacheResult(ctx, resultHit)
		return cached, nil
	}
	if result != resultSkipped {
		logx.SetCacheResult(ctx, result)
	}

	// shared 丢掉：每个请求在进 singleflight 之前已经按自己的读取结果记过数。
	l, err, _ := doTyped[*link.Link](&s.group, key, func() (*link.Link, error) {
		fetchCtx, cancel := fetchContext(ctx, s.fetchTimeout)
		defer cancel()

		found, err := s.readStore(fetchCtx, code, result)
		if err != nil && !errors.Is(err, link.ErrNotFound) {
			return nil, err // 真正的错误不写缓存，下次请求应该重试
		}
		/*
			回源写缓存必须用 NX：管理写可能已经在数据库提交并覆盖了缓存，
			这个请求读到的旧快照不能把它盖回去。见 ADR 0011。
		*/
		s.writeCacheBestEffort(fetchCtx, key, found, writeNX)
		return found, err
	})
	if ctx.Err() == nil {
		if shedErr, ok := errors.AsType[fetchShedError](err); ok {
			s.shed.WithLabelValues(shedErr.path).Inc()
		}
	}
	return l, err
}

/*
List 和 GetForTenant 直接透传，不读也不写缓存。
管理视角包含过期链接和实时计数，缓存里只有跳转可见的 Link。
*/
func (s *Store) List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	return s.next.List(ctx, opts)
}

func (s *Store) GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error) {
	return s.next.GetForTenant(ctx, tenantID, code)
}

/*
Update 先提交数据库，再无条件覆盖缓存。

修改后的链接如果不可跳转（已过期、已停用或已封禁），写负缓存，
让 redirector 直接 404；否则写新的正缓存。停用因而写负缓存，启用写正缓存。
无条件 SET 会覆盖在途回源；同时回源改用 SET NX，旧快照不会反过来盖掉这里的新值。
Redis 写失败只影响一致性，旧值最多保留到 TTL，见 ADR 0011。

写了正缓存之后还要复查一次数据库：删除、停用、封禁可能和这次修改并发，
晚到的正缓存会盖掉它们的负缓存。复查偏向不跳转，见 ADR 0016、ADR 0020。
*/
func (s *Store) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	updated, err := s.next.Update(ctx, tenantID, code, u)
	if err != nil {
		return nil, err
	}
	key := keyPrefix + code
	if !updated.Servable(time.Now()) {
		s.writeCommitted(ctx, key, nil)
		return updated, nil
	}
	s.writeCommitted(ctx, key, &updated.Link)
	if _, err := s.recheckAfterPositiveWrite(ctx, key, code); err != nil {
		s.logger.WarnContext(ctx, "write negative cache after recheck", "code", code, "err", err)
	}
	return updated, nil
}

/*
Delete 先提交数据库，再无条件写负缓存，盖掉旧的正缓存。
删除不是强撤销：已经在途的跳转仍可能返回 302，之后的新请求才会 404。
*/
func (s *Store) Delete(ctx context.Context, tenantID int64, code string) error {
	if err := s.next.Delete(ctx, tenantID, code); err != nil {
		return err
	}
	s.writeCommitted(ctx, keyPrefix+code, nil)
	return nil
}

/*
Ban 先封禁数据库，再无条件写负缓存，盖掉旧的正缓存。

下游错误原样返回（link.ErrNotFound 可以被 errors.Is 认出），这时缓存不动。
数据库写成功、负缓存没写上时，返回数据库里的 Link 和一个包装 ErrNotUpdated 的错误；
调用方重跑同一条命令即可，封禁幂等。缓存包不在这里记日志：错误交给调用方处理。
*/
func (s *Store) Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	banned, err := s.next.Ban(ctx, code, reason, source)
	if err != nil {
		return nil, err
	}
	if err := s.writeCommittedResult(ctx, keyPrefix+code, nil); err != nil {
		return banned, notUpdated("ban link", err)
	}
	return banned, nil
}

/*
BanIfURL 供异步风控按期望 URL 条件封禁，并写负缓存（ADR 0020）。
链接已删除（link.ErrNotFound）或 URL 已变更（link.ErrURLChanged）时缓存不动；
数据库已封禁但负缓存写失败时返回包装 ErrNotUpdated 的错误，由 worker 自动退避重试自愈。
*/
func (s *Store) BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	banned, err := s.next.BanIfURL(ctx, code, expectedURL, reason, source)
	if err != nil {
		return nil, err
	}
	if err := s.writeCommittedResult(ctx, keyPrefix+code, nil); err != nil {
		return banned, notUpdated("ban link", err)
	}
	return banned, nil
}

/*
Unban 先解封数据库，再按返回链接的 Servable 决定写正缓存还是负缓存：
解封了但仍然停用或已过期时写负缓存。

写正缓存的分支和 Update 一样要复查：解封可能和停用、删除、封禁交错。
下游错误原样返回，缓存不动；数据库写成功但缓存没有落到正确的值上时，
返回数据库里的 Link 和一个包装 ErrNotUpdated 的错误。
如果复查确认链接已经找不到，补写的负缓存就是当时正确的值，不算失败。
*/
func (s *Store) Unban(ctx context.Context, code string) (*link.Link, error) {
	unbanned, err := s.next.Unban(ctx, code)
	if err != nil {
		return nil, err
	}
	key := keyPrefix + code
	if !unbanned.Servable(time.Now()) {
		if err := s.writeCommittedResult(ctx, key, nil); err != nil {
			return unbanned, notUpdated("unban link", err)
		}
		return unbanned, nil
	}

	writeErr := s.writeCommittedResult(ctx, key, unbanned)
	wroteNegative, recheckErr := s.recheckAfterPositiveWrite(ctx, key, code)
	switch {
	case recheckErr != nil:
		return unbanned, notUpdated("unban link", errors.Join(writeErr, recheckErr))
	case !wroteNegative && writeErr != nil:
		// 复查读到了链接，正缓存那次写失败就没被负缓存盖掉，缓存状态不确定。
		return unbanned, notUpdated("unban link", writeErr)
	default:
		// 复查补写了负缓存：它排在正缓存之后，最终值符合复查结论。
		return unbanned, nil
	}
}

// Stats 直接查 PostgreSQL，不经过缓存；管理接口流量低，先不做结果缓存。
func (s *Store) Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error) {
	return s.next.Stats(ctx, tenantID, code, from, to)
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
writeCommitted 在数据库提交之后尽力更新缓存，失败只记日志和指标。
用于创建、修改、删除和回源：ADR 0011 接受的最长到 TTL 的最终一致。
请求 ctx 可能已经取消：客户端断开，或者请求超时发生在提交之后。
WithoutCancel 丢掉取消和 deadline，只留日志要用的 value；
再套一段独立超时，避免 Redis 卡住时这个 handler 一直不返回。
*/
func (s *Store) writeCommitted(ctx context.Context, key string, l *link.Link) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedWriteTimeout)
	defer cancel()
	s.writeCacheBestEffort(writeCtx, key, l, writeOverwrite)
}

/*
writeCommittedResult 和 writeCommitted 一样脱离请求取消，但把失败返回给调用方。
Ban、Unban 用它：封禁的对象往往不能再多跳一小时，缓存没写上必须让操作的人知道。
这条路径不在这里记日志，由调用方处理一次。
*/
func (s *Store) writeCommittedResult(ctx context.Context, key string, l *link.Link) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedWriteTimeout)
	defer cancel()
	return s.writeCache(writeCtx, key, l, writeOverwrite)
}

// writeCacheBestEffort 写失败只记日志和指标，调用方不受影响。
func (s *Store) writeCacheBestEffort(ctx context.Context, key string, l *link.Link, mode writeMode) {
	if err := s.writeCache(ctx, key, l, mode); err != nil {
		s.logger.WarnContext(ctx, "write cache", "key", key, "err", err)
	}
}

/*
writeCache 写一次缓存，返回这次写入算不算失败。

管理写（无条件 SET）没写上返回非 nil：ctx 已结束、序列化失败或 Redis 报错。
回源填充（SET NX）被跳过、或调用方已取消，都不算失败，返回 nil。
调用方决定怎么处理：Create、Update、Delete 和回源只记日志和指标（ADR 0011），
Ban、Unban 返回给上层（ADR 0016）。

mode 决定写入方式，不能只按 l 是否为 nil 判断：
回源的正、负缓存都用 SET NX，管理写（包括删除的负缓存）都用无条件 SET。
SET NX 没写进去说明已经有更新的值，记 skipped 而不是 success。

TTL 向下随机抖动，避免同一批写入的 key 集中过期。
*/
func (s *Store) writeCache(ctx context.Context, key string, l *link.Link, mode writeMode) error {
	if ctx.Err() != nil {
		if mode == writeNX {
			return nil
		}
		s.writes.WithLabelValues(writeError).Inc()
		return ctx.Err()
	}

	ttl := negativeTTL
	if l != nil {
		ttl = cacheTTL(l)
		// go-redis 的 Set 在 duration <= 0 时不带过期时间，key 会一直留着。
		if ttl <= 0 {
			return nil
		}
	}

	data, marshalErr := json.Marshal(l)
	if marshalErr != nil {
		s.writes.WithLabelValues(writeError).Inc()
		return fmt.Errorf("marshal cache value: %w", marshalErr)
	}

	start := time.Now()
	var (
		written bool
		setErr  error
	)
	if mode == writeNX {
		written, setErr = s.rdb.SetNX(ctx, key, data, jitteredTTL(ttl)).Result()
	} else {
		setErr = s.rdb.Set(ctx, key, data, jitteredTTL(ttl)).Err()
		written = setErr == nil
	}
	/*
		回源填充被调用方取消时不记。拨号超时不会让 ctx.Err() 非空，仍记成 error。
		管理写的 ctx 是独立超时；这段超时也算没写上缓存，要记下来。
	*/
	if setErr != nil && ctx.Err() != nil && mode == writeNX {
		return nil
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
	if setErr != nil {
		return fmt.Errorf("set cache: %w", setErr)
	}
	return nil
}

/*
recheckAfterPositiveWrite 在写完正缓存之后，按跳转查询复查一次数据库。

原因见 ADR 0016「下架和管理写交错」：删除、停用、封禁可能和这次管理写并发，
它们的负缓存先写，晚到的正缓存又盖回去，不可跳转的链接会继续跳最长 1 小时。
读完再补写负缓存，负缓存就排在自己的正缓存之后；如果复查时已经读不到封禁，
说明封禁在复查之后才提交，封禁自己的负缓存写入更晚，同样排在正缓存之后。

读到链接就结束；读到 link.ErrNotFound 就补写负缓存。
读库失败（超时、库故障）同样补写负缓存并记日志：ADR 0016 的复查偏向不跳转，
宁可让这条链接最多多 404 一分钟（负缓存过期后回源自愈），
也不让可能已经被下架或删除的链接继续跳。这是有意的取舍。

直接调 s.next.GetByCode，不走 readStore 的 missSlots/fallbackSlots：
那是跳转路径的回源保护，管理写频率低，用不上也不该占它的配额。
读库、正缓存写入和负缓存补写各自用独立的超时，不共用会被前一步耗尽的 deadline。

返回 wroteNegative 表示走了补写负缓存的分支；err 只在补写失败时非 nil。
读库失败本身只记日志，不算补写失败：负缓存已经写上就是复查选择的最终值。
*/
func (s *Store) recheckAfterPositiveWrite(ctx context.Context, key, code string) (wroteNegative bool, err error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.fetchTimeout)
	defer cancel()
	_, readErr := s.next.GetByCode(readCtx, code)
	switch {
	case readErr == nil:
		return false, nil
	case errors.Is(readErr, link.ErrNotFound):
		return true, s.writeCommittedResult(ctx, key, nil)
	default:
		s.logger.WarnContext(ctx, "recheck link after cache write", "code", code, "err", readErr)
		return true, s.writeCommittedResult(ctx, key, nil)
	}
}

// notUpdated 给「库写成功、缓存没更新」的错误加上操作名和哨兵。
func notUpdated(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, ErrNotUpdated, err)
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
