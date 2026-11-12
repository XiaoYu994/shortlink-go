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
	return New(next, rdb, prometheus.NewRegistry())
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

func TestStoreGetByCodeRepairsCorruptedValue(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	key := keyPrefix + code
	if err := rdb.Set(ctx, key, "not json", time.Hour).Err(); err != nil {
		t.Fatalf("seed corrupted value: %v", err)
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
	if got.ID != want.ID {
		t.Errorf("ID = %d, want %d", got.ID, want.ID)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
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
