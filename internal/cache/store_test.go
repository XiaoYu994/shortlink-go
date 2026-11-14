package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"shortlink/internal/link"
)

/*
fakeStore 是下一层存储的替身。get 每次调用都计数，
测试用它断言请求有没有被缓存挡住。
*/
type fakeStore struct {
	calls   atomic.Int32
	creates atomic.Int32
	get     func(ctx context.Context, code string) (*link.Link, error)
	create  func(l *link.Link) error
}

func (f *fakeStore) Create(ctx context.Context, l *link.Link) error {
	f.creates.Add(1)
	if f.create != nil {
		return f.create(l)
	}
	return nil
}

func (f *fakeStore) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	f.calls.Add(1)
	return f.get(ctx, code)
}

/*
newTestRedis 连 TEST_REDIS_URL 指向的真实 Redis；没有设置时跳过。
测试只读写自己生成的 test- 前缀短码，结束时删除，不使用 FLUSHDB。
*/
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rdb, err := Open(ctx, redisURL)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newTestStore 给不关心指标的测试一个用完即弃的 Registry。
func newTestStore(t *testing.T, next LinkStore, rdb *redis.Client) *Store {
	t.Helper()
	return New(next, rdb, prometheus.NewRegistry(), Options{})
}

// newTestCode 生成每个测试独有的短码，避免测试之间和残留 key 互相影响。
func newTestCode(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	return "test-" + name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// cleanupTestKey 只删测试自己写入的缓存 key，不碰 Redis 里的其他数据。
func cleanupTestKey(t *testing.T, rdb *redis.Client, code string) {
	t.Helper()
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), keyPrefix+code).Err()
	})
}

func TestStoreGetByCodeCacheHit(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	want := &link.Link{
		ID:        42,
		Code:      code,
		URL:       "https://example.com/articles/1",
		CreatedAt: time.Now().Truncate(time.Second),
	}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("first GetByCode: %v", err)
	}
	if got.ID != want.ID || got.Code != want.Code || got.URL != want.URL || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("first GetByCode = %+v, want %+v", got, want)
	}

	got, err = s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("second GetByCode: %v", err)
	}
	if got.ID != want.ID || got.URL != want.URL {
		t.Errorf("second GetByCode = %+v, want %+v", got, want)
	}

	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
	if ttl := rdb.PTTL(ctx, keyPrefix+code).Val(); ttl <= 0 || ttl > positiveTTL {
		t.Errorf("cache TTL = %v, want in (0, %v]", ttl, positiveTTL)
	}
	if got := testutil.ToFloat64(s.gets.WithLabelValues(resultMiss)); got != 1 {
		t.Errorf("gets{result=miss} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.gets.WithLabelValues(resultHit)); got != 1 {
		t.Errorf("gets{result=hit} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); got != 1 {
		t.Errorf("writes{result=success} = %v, want 1", got)
	}
}

func TestStoreGetByCodeNegativeCache(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return nil, link.ErrNotFound
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("first GetByCode error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("second GetByCode error = %v, want ErrNotFound", err)
	}

	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
	data, err := rdb.Get(ctx, keyPrefix+code).Bytes()
	if err != nil {
		t.Fatalf("get cached value: %v", err)
	}
	if string(data) != "null" {
		t.Errorf("cached value = %q, want %q", data, "null")
	}
	if ttl := rdb.PTTL(ctx, keyPrefix+code).Val(); ttl <= 0 || ttl > negativeTTL {
		t.Errorf("negative TTL = %v, want in (0, %v]", ttl, negativeTTL)
	}
	if got := testutil.ToFloat64(s.gets.WithLabelValues(resultMiss)); got != 1 {
		t.Errorf("gets{result=miss} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.gets.WithLabelValues(resultHit)); got != 1 {
		t.Errorf("gets{result=hit} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); got != 1 {
		t.Errorf("writes{result=success} = %v, want 1", got)
	}
}

func TestStoreGetByCodeSingleflight(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	want := &link.Link{ID: 7, Code: code, URL: "https://example.com/7"}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return want, nil
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for range goroutines {
		wg.Go(func() {
			<-start
			got, err := s.GetByCode(ctx, code)
			if err != nil {
				errs <- err
				return
			}
			if got.ID != want.ID {
				errs <- fmt.Errorf("ID = %d, want %d", got.ID, want.ID)
			}
		})
	}

	close(start)
	<-entered
	// 领头的请求卡在回源里，等其余 goroutine 走到 singleflight 再放行，
	// 让它们和领头请求重叠，验证只回源一次。
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
}

func TestDoTyped(t *testing.T) {
	errNotFound := errors.New("not found")
	errDB := errors.New("db down")
	found := &link.Link{Code: "abc", URL: "https://example.com"}

	tests := []struct {
		name    string
		fn      func() (*link.Link, error)
		want    *link.Link
		wantErr error
	}{
		{
			name: "hit",
			fn: func() (*link.Link, error) {
				return found, nil
			},
			want: found,
		},
		{
			name: "nil pointer with sentinel",
			fn: func() (*link.Link, error) {
				return nil, errNotFound
			},
			wantErr: errNotFound,
		},
		{
			name: "nil pointer with error",
			fn: func() (*link.Link, error) {
				return nil, errDB
			},
			wantErr: errDB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g singleflight.Group
			got, err, shared := doTyped[*link.Link](&g, tt.name, tt.fn)
			if shared {
				t.Fatalf("shared = true, want false")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("link = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDoTypedShared(t *testing.T) {
	var g singleflight.Group
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	fn := func() (*link.Link, error) {
		once.Do(func() { close(started) })
		<-release
		return &link.Link{Code: "abc"}, nil
	}

	const goroutines = 8
	type result struct {
		shared bool
		err    error
	}
	results := make(chan result, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Go(func() {
			<-start
			got, err, shared := doTyped[*link.Link](&g, "k", fn)
			if err != nil {
				results <- result{err: err}
				return
			}
			if got == nil || got.Code != "abc" {
				results <- result{err: fmt.Errorf("link = %+v", got)}
				return
			}
			results <- result{shared: shared}
		})
	}

	close(start)
	<-started
	// 领头请求卡在 fn 里，等其余 goroutine 进入 Do 再放行。
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	var sharedN int
	for r := range results {
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		if r.shared {
			sharedN++
		}
	}
	if sharedN == 0 {
		t.Fatal("shared was never true")
	}
}

func TestStoreGetByCodeFallsBackWhenRedisFails(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	want := &link.Link{ID: 1, Code: "down", URL: "https://example.com/down"}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	s := newTestStore(t, next, rdb)

	got, err := s.GetByCode(context.Background(), "down")
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.URL != want.URL {
		t.Errorf("URL = %q, want %q", got.URL, want.URL)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
	if got := testutil.ToFloat64(s.gets.WithLabelValues(resultFallback)); got != 1 {
		t.Errorf("gets{result=fallback} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeError)); got != 1 {
		t.Errorf("writes{result=error} = %v, want 1", got)
	}
}

func TestStoreGetByCodeRepairsUnusableValue(t *testing.T) {
	tests := []struct {
		name    string
		value   func(code string) string
		wantHit bool
	}{
		{name: "not json", value: func(string) string { return "not json" }},
		{name: "empty object", value: func(string) string { return "{}" }},
		{name: "empty url", value: func(code string) string {
			return fmt.Sprintf(`{"Code":%q,"URL":""}`, code)
		}},
		{name: "wrong code", value: func(string) string {
			return `{"Code":"xxxxxxxx","URL":"https://example.com/evil"}`
		}},
		{name: "lower case fields", wantHit: true, value: func(code string) string {
			return fmt.Sprintf(`{"id":3,"code":%q,"url":"https://example.com/ok"}`, code)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := newTestRedis(t)
			code := newTestCode(t)
			cleanupTestKey(t, rdb, code)
			ctx := context.Background()

			key := keyPrefix + code
			if err := rdb.Set(ctx, key, tt.value(code), time.Hour).Err(); err != nil {
				t.Fatalf("seed value: %v", err)
			}

			want := &link.Link{ID: 9, Code: code, URL: "https://example.com/9"}
			next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
				return want, nil
			}}
			s := newTestStore(t, next, rdb)

			got, err := s.GetByCode(ctx, code)
			if err != nil {
				t.Fatalf("GetByCode: %v", err)
			}
			if tt.wantHit {
				if got.URL != "https://example.com/ok" || got.Code != code {
					t.Fatalf("GetByCode = %+v, want cached link", got)
				}
				if n := next.calls.Load(); n != 0 {
					t.Errorf("store calls = %d, want 0", n)
				}
				if n := testutil.ToFloat64(s.gets.WithLabelValues(resultHit)); n != 1 {
					t.Errorf("gets{result=hit} = %v, want 1", n)
				}
				return
			}
			if got.ID != want.ID {
				t.Errorf("ID = %d, want %d", got.ID, want.ID)
			}
			if n := next.calls.Load(); n != 1 {
				t.Errorf("store calls = %d, want 1", n)
			}
			if n := testutil.ToFloat64(s.gets.WithLabelValues(resultMiss)); n != 1 {
				t.Errorf("gets{result=miss} = %v, want 1", n)
			}
			if n := testutil.ToFloat64(s.gets.WithLabelValues(resultHit)); n != 0 {
				t.Errorf("gets{result=hit} = %v, want 0", n)
			}

			data, err := rdb.Get(ctx, key).Bytes()
			if err != nil {
				t.Fatalf("get repaired value: %v", err)
			}
			var cached *link.Link
			if err := json.Unmarshal(data, &cached); err != nil {
				t.Fatalf("cached value was not repaired: %v", err)
			}
			if cached == nil || cached.ID != want.ID {
				t.Errorf("cached value = %+v, want ID %d", cached, want.ID)
			}
		})
	}
}

func TestStoreGetByCodeDoesNotCacheErrors(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	boom := errors.New("database down")
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return nil, boom
	}}
	s := newTestStore(t, next, rdb)

	if _, err := s.GetByCode(ctx, code); !errors.Is(err, boom) {
		t.Fatalf("first GetByCode error = %v, want boom", err)
	}
	if n := rdb.Exists(ctx, keyPrefix+code).Val(); n != 0 {
		t.Errorf("error result was cached, key exists")
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, boom) {
		t.Fatalf("second GetByCode error = %v, want boom", err)
	}
	if n := next.calls.Load(); n != 2 {
		t.Errorf("store calls = %d, want 2", n)
	}
}

func TestStoreCreatePassesThrough(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	next := &fakeStore{}
	s := newTestStore(t, next, rdb)

	if err := s.Create(context.Background(), &link.Link{Code: "x", URL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := next.creates.Load(); n != 1 {
		t.Errorf("store Creates = %d, want 1", n)
	}
}

func TestStoreCreateWritesThrough(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{create: func(l *link.Link) error {
		l.ID = 5
		l.Code = code
		l.CreatedAt = time.Now().Truncate(time.Second)
		return nil
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	l := &link.Link{URL: "https://example.com/5"}
	if err := s.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != l.ID || got.URL != l.URL {
		t.Errorf("GetByCode = %+v, want ID %d URL %q", got, l.ID, l.URL)
	}
	if n := next.calls.Load(); n != 0 {
		t.Errorf("store GetByCode calls = %d, want 0 (cache hit)", n)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); got != 1 {
		t.Errorf("writes{result=success} = %v, want 1", got)
	}
}

/*
TestStoreNegativeWriteDoesNotOverwritePositive 模拟查找回源和创建并发：
后到的负缓存写入不能覆盖已经写好的正缓存，否则刚创建的短码会继续 404。
*/
func TestStoreNegativeWriteDoesNotOverwritePositive(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()
	key := keyPrefix + code

	want := &link.Link{ID: 11, Code: code, URL: "https://example.com/11"}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	s := newTestStore(t, next, rdb)

	s.writeCache(ctx, key, want)
	s.writeCache(ctx, key, nil)

	data, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("get cached value: %v", err)
	}
	var cached *link.Link
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decode cached value: %v", err)
	}
	if cached == nil || cached.ID != want.ID {
		t.Errorf("cached value = %+v, want ID %d", cached, want.ID)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); got != 1 {
		t.Errorf("writes{result=%q} = %v, want 1", writeSuccess, got)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSkipped)); got != 1 {
		t.Errorf("writes{result=%q} = %v, want 1", writeSkipped, got)
	}
	if n := testutil.CollectAndCount(s.duration); n != 2 {
		t.Errorf("duration series = %d, want 2", n)
	}
}

/*
TestStoreCreateOverwritesNegativeCache 覆盖完整时序：
先访问不存在的短码留下负缓存，再创建同码短链，
写穿必须盖掉负缓存，之后的访问直接命中正缓存。
*/
func TestStoreCreateOverwritesNegativeCache(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	want := &link.Link{ID: 7, Code: code, URL: "https://example.com/7"}
	exists := false
	next := &fakeStore{
		get: func(context.Context, string) (*link.Link, error) {
			if !exists {
				return nil, link.ErrNotFound
			}
			return want, nil
		},
		create: func(*link.Link) error {
			exists = true
			return nil
		},
	}
	s := newTestStore(t, next, rdb)

	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("GetByCode before create = %v, want ErrNotFound", err)
	}
	if got := rdb.Get(ctx, keyPrefix+code).Val(); got != "null" {
		t.Fatalf("negative cache = %q, want null", got)
	}

	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	callsBefore := next.calls.Load()
	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode after create: %v", err)
	}
	if got.ID != want.ID || got.URL != want.URL {
		t.Errorf("GetByCode after create = %+v, want ID %d URL %q", got, want.ID, want.URL)
	}
	if n := next.calls.Load() - callsBefore; n != 0 {
		t.Errorf("store GetByCode calls after create = %d, want 0 (cache hit)", n)
	}
}

// TestStoreGetByCodeSkipsMetricOnCanceledContext 验证客户端断开不计进命中率，也不冒充 Redis 降级。
func TestStoreGetByCodeSkipsMetricOnCanceledContext(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	want := &link.Link{ID: 3, Code: code, URL: "https://example.com/3"}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	s := newTestStore(t, next, rdb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %d, want %d", got.ID, want.ID)
	}
	for _, result := range []string{resultHit, resultMiss, resultFallback} {
		if n := testutil.ToFloat64(s.gets.WithLabelValues(result)); n != 0 {
			t.Errorf("gets{result=%q} = %v, want 0", result, n)
		}
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeError)); n != 0 {
		t.Errorf("writes{result=error} = %v, want 0", n)
	}
}

func TestCacheTTL(t *testing.T) {
	soon := time.Now().Add(30 * time.Minute)
	past := time.Now().Add(-time.Second)
	long := time.Now().Add(2 * time.Hour)
	tests := []struct {
		name       string
		at         *time.Time
		wantExact  time.Duration
		exact      bool
		upperBound time.Duration
	}{
		{name: "no expiry", wantExact: positiveTTL, exact: true},
		{name: "longer than an hour", at: &long, wantExact: positiveTTL, exact: true},
		{name: "thirty minutes", at: &soon, upperBound: 30 * time.Minute},
		{name: "past", at: &past},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheTTL(&link.Link{ExpiresAt: tt.at})
			switch {
			case tt.exact:
				if got != tt.wantExact {
					t.Errorf("cacheTTL = %v, want %v", got, tt.wantExact)
				}
			case tt.upperBound > 0:
				if got <= 0 || got > tt.upperBound {
					t.Errorf("cacheTTL = %v, want in (0, %v]", got, tt.upperBound)
				}
			default:
				if got > 0 {
					t.Errorf("cacheTTL = %v, want <= 0", got)
				}
			}
		})
	}
}

func TestStoreGetByCodeCacheTTLFollowsExpiry(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	expires := time.Now().Add(30 * time.Minute)
	want := &link.Link{
		ID:        7,
		Code:      code,
		URL:       "https://example.com/exp",
		ExpiresAt: &expires,
		CreatedAt: time.Now().Truncate(time.Second),
	}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	ttl := rdb.PTTL(ctx, keyPrefix+code).Val()
	if ttl <= 0 || ttl > 30*time.Minute {
		t.Errorf("cache TTL = %v, want in (0, 30m]", ttl)
	}
}

func TestStoreGetByCodeExpiredCacheEntry(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	past := time.Now().Add(-time.Minute).UTC()
	body, err := json.Marshal(&link.Link{
		ID:        7,
		Code:      code,
		URL:       "https://example.com/old",
		ExpiresAt: &past,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, keyPrefix+code, body, time.Hour).Err(); err != nil {
		t.Fatalf("set cache: %v", err)
	}

	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		t.Error("expired cache entry fell through to the store")
		return nil, link.ErrNotFound
	}}
	s := newTestStore(t, next, rdb)
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("GetByCode error = %v, want ErrNotFound", err)
	}
	if n := rdb.Exists(ctx, keyPrefix+code).Val(); n != 0 {
		t.Errorf("cached key still exists")
	}
}

func TestStoreSkipsCacheWhenLinkAlreadyExpired(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	past := time.Now().Add(-time.Minute)
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{
			ID:        7,
			Code:      code,
			URL:       "https://example.com/past",
			ExpiresAt: &past,
		}, nil
	}}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()
	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if n := rdb.Exists(ctx, keyPrefix+code).Val(); n != 0 {
		t.Errorf("expired link was cached")
	}
}

func TestJitteredTTL(t *testing.T) {
	const ttl = time.Hour
	seen := make(map[time.Duration]struct{})
	for range 100 {
		got := jitteredTTL(ttl)
		if got < ttl*9/10 || got > ttl {
			t.Fatalf("jitteredTTL(%v) = %v, want in [%v, %v]", ttl, got, ttl*9/10, ttl)
		}
		seen[got] = struct{}{}
	}
	if len(seen) == 1 {
		t.Error("jitteredTTL returned the same value 100 times; jitter is not applied")
	}
}

/*
TestStoreGetByCodeSingleflightSurvivesLeaderCancel 固定共享回源不跟随领头取消：
领头请求断开后，还在等待的请求应该拿到链接，而不是 context.Canceled。
*/
func TestStoreGetByCodeSingleflightSurvivesLeaderCancel(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	want := &link.Link{ID: 8, Code: code, URL: "https://example.com/8"}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	next := &fakeStore{get: func(ctx context.Context, _ string) (*link.Link, error) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	s := newTestStore(t, next, rdb)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	type result struct {
		l   *link.Link
		err error
	}
	leaderCh := make(chan result, 1)
	go func() {
		l, err := s.GetByCode(leaderCtx, code)
		leaderCh <- result{l: l, err: err}
	}()
	<-entered

	waiterCh := make(chan result, 1)
	go func() {
		l, err := s.GetByCode(context.Background(), code)
		waiterCh <- result{l: l, err: err}
	}()
	// 让等待者进入 singleflight，再模拟领头请求断开。
	time.Sleep(100 * time.Millisecond)
	cancelLeader()
	// 旧实现里回源会在这里被取消；新实现继续等 release。
	time.Sleep(100 * time.Millisecond)
	close(release)

	if got := <-leaderCh; got.err != nil {
		t.Fatalf("leader GetByCode: %v", got.err)
	}
	waiter := <-waiterCh
	if waiter.err != nil {
		t.Fatalf("waiter GetByCode: %v", waiter.err)
	}
	if waiter.l.ID != want.ID {
		t.Errorf("waiter ID = %d, want %d", waiter.l.ID, want.ID)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
}

/*
TestStoreFallbackShedsWhenConcurrencyFull 验证 Redis 读失败时的回源上限：
最多 fallbackConcurrency 个请求进入下一层存储，其余立即收到临时过载错误。
*/
func TestStoreFallbackShedsWhenConcurrencyFull(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	var current, peak atomic.Int32
	release := make(chan struct{})
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		n := current.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		current.Add(-1)
		return &link.Link{ID: 1, URL: "https://example.com"}, nil
	}}
	s := newTestStore(t, next, rdb)

	const shed = 4
	const total = fallbackConcurrency + shed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var busy atomic.Int32
	errs := make(chan error, total)
	for i := range total {
		wg.Go(func() {
			_, err := s.GetByCode(ctx, fmt.Sprintf("busy-%d", i))
			if err == nil {
				return
			}
			var temp interface{ Temporary() bool }
			if errors.As(err, &temp) && temp.Temporary() {
				busy.Add(1)
				return
			}
			errs <- err
		})
	}

	deadline := time.Now().Add(2 * time.Second)
	for int(current.Load()+busy.Load()) < total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := peak.Load(); got != fallbackConcurrency {
		close(release)
		wg.Wait()
		t.Fatalf("peak concurrent store calls = %d, want %d", got, fallbackConcurrency)
	}
	if got := busy.Load(); got != shed {
		close(release)
		wg.Wait()
		t.Fatalf("shed requests = %d, want %d", got, shed)
	}
	if got := testutil.ToFloat64(s.shed.WithLabelValues(resultFallback)); got != shed {
		close(release)
		wg.Wait()
		t.Fatalf("shed{path=%q} = %v, want %d", resultFallback, got, shed)
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

/*
TestStoreMissShedsWhenConcurrencyFull 验证普通未命中的回源上限：
最多 MissConcurrency 个请求进入下一层存储，其余立即收到临时过载错误。
Redis 是真实的，所有短码都不同，每个请求都会 miss 并各走一次 singleflight。
*/
func TestStoreMissShedsWhenConcurrencyFull(t *testing.T) {
	rdb := newTestRedis(t)

	const concurrency = 2
	const shed = 4
	const total = concurrency + shed

	var current, peak atomic.Int32
	release := make(chan struct{})
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		n := current.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		current.Add(-1)
		return &link.Link{ID: 1, URL: "https://example.com"}, nil
	}}
	reg := prometheus.NewRegistry()
	s := New(next, rdb, reg, Options{MissConcurrency: concurrency})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	codePrefix := newTestCode(t)
	var cleanup []string
	t.Cleanup(func() {
		for _, code := range cleanup {
			_ = rdb.Del(context.Background(), keyPrefix+code).Err()
		}
	})

	var wg sync.WaitGroup
	var busy atomic.Int32
	errs := make(chan error, total)
	for i := range total {
		code := fmt.Sprintf("%s-%d", codePrefix, i)
		cleanup = append(cleanup, code)
		wg.Go(func() {
			_, err := s.GetByCode(ctx, code)
			if err == nil {
				return
			}
			var temp interface{ Temporary() bool }
			if errors.As(err, &temp) && temp.Temporary() {
				busy.Add(1)
				return
			}
			errs <- err
		})
	}

	deadline := time.Now().Add(2 * time.Second)
	for int(current.Load()+busy.Load()) < total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := peak.Load(); got != concurrency {
		close(release)
		wg.Wait()
		t.Fatalf("peak concurrent store calls = %d, want %d", got, concurrency)
	}
	if got := busy.Load(); got != shed {
		close(release)
		wg.Wait()
		t.Fatalf("shed requests = %d, want %d", got, shed)
	}
	if got := testutil.ToFloat64(s.shed.WithLabelValues(resultMiss)); got != shed {
		close(release)
		wg.Wait()
		t.Fatalf("shed{path=%q} = %v, want %d", resultMiss, got, shed)
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

/*
TestStoreGetByCodeUsesFetchTimeout 验证回源超时独立于请求时限：
请求 ctx 没有 deadline，回源仍然在 FetchTimeout 到点后取消，而不是等 REQUEST_TIMEOUT。
*/
func TestStoreGetByCodeUsesFetchTimeout(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{get: func(ctx context.Context, _ string) (*link.Link, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	const fetchTimeout = 100 * time.Millisecond
	s := New(next, rdb, prometheus.NewRegistry(), Options{FetchTimeout: fetchTimeout})

	start := time.Now()
	_, err := s.GetByCode(context.Background(), code)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetByCode error = %v, want DeadlineExceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("GetByCode took %v, fetch timeout %v was not applied", elapsed, fetchTimeout)
	}
}
