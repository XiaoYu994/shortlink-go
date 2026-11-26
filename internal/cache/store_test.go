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
	"shortlink/internal/logx"
)

/*
fakeStore 是下一层存储的替身。get 每次调用都计数，
测试用它断言请求有没有被缓存挡住。
*/
type fakeStore struct {
	calls    atomic.Int32
	creates  atomic.Int32
	updates  atomic.Int32
	deletes  atomic.Int32
	bans     atomic.Int32
	unbans   atomic.Int32
	get      func(ctx context.Context, code string) (*link.Link, error)
	create   func(l *link.Link) error
	update   func(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error)
	deleteFn func(ctx context.Context, tenantID int64, code string) error
	ban      func(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	banIfURL func(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	unban    func(ctx context.Context, code string) (*link.Link, error)
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
	if f.get != nil {
		return f.get(ctx, code)
	}
	return nil, link.ErrNotFound
}

func (f *fakeStore) List(context.Context, link.ListOptions) ([]link.AdminLink, error) {
	return nil, nil
}

func (f *fakeStore) GetForTenant(context.Context, int64, string) (*link.AdminLink, error) {
	return nil, link.ErrNotFound
}

func (f *fakeStore) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	f.updates.Add(1)
	if f.update != nil {
		return f.update(ctx, tenantID, code, u)
	}
	return nil, link.ErrNotFound
}

func (f *fakeStore) Delete(ctx context.Context, tenantID int64, code string) error {
	f.deletes.Add(1)
	if f.deleteFn != nil {
		return f.deleteFn(ctx, tenantID, code)
	}
	return nil
}

func (f *fakeStore) Stats(context.Context, int64, string, time.Time, time.Time) (*link.Stats, error) {
	return nil, link.ErrNotFound
}

func (f *fakeStore) Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	f.bans.Add(1)
	return f.ban(ctx, code, reason, source)
}

func (f *fakeStore) BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	f.bans.Add(1)
	if f.banIfURL != nil {
		return f.banIfURL(ctx, code, expectedURL, reason, source)
	}
	return f.ban(ctx, code, reason, source)
}

func (f *fakeStore) Unban(ctx context.Context, code string) (*link.Link, error) {
	f.unbans.Add(1)
	return f.unban(ctx, code)
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
	// 降级回源不触发 SET NX 回写：Redis 读已失败时不白等一次写超时。
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeError)); got != 0 {
		t.Errorf("writes{result=error} = %v, want 0", got)
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
			return fmt.Sprintf(`{"ID":3,"Code":%q,"URL":""}`, code)
		}},
		{name: "missing id", value: func(code string) string {
			return fmt.Sprintf(`{"Code":%q,"URL":"https://example.com/no-id"}`, code)
		}},
		{name: "wrong code", value: func(string) string {
			return `{"ID":3,"Code":"xxxxxxxx","URL":"https://example.com/evil"}`
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

	var created *link.Link
	next := &fakeStore{
		create: func(l *link.Link) error {
			l.ID = 5
			l.Code = code
			l.CreatedAt = time.Now().Truncate(time.Second)
			created = l
			return nil
		},
		get: func(context.Context, string) (*link.Link, error) {
			return created, nil
		},
	}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	l := &link.Link{URL: "https://example.com/5"}
	if err := s.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	callsBefore := next.calls.Load()
	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != l.ID || got.URL != l.URL {
		t.Errorf("GetByCode = %+v, want ID %d URL %q", got, l.ID, l.URL)
	}
	if n := next.calls.Load() - callsBefore; n != 0 {
		t.Errorf("store GetByCode calls after create = %d, want 0 (cache hit)", n)
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

	// 正缓存来自创建（无条件写），负缓存来自回源（SET NX 不能盖掉它）。
	_ = s.writeCache(ctx, key, want, writeOverwrite)
	_ = s.writeCache(ctx, key, nil, writeNX)

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

// TestStoreGetByCodeSkipsMetricOnCanceledContext 验证客户端断开直接返回 ctx 错误，不回源也不计入缓存指标。
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetByCode error = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("GetByCode = %+v, want nil", got)
	}
	if n := next.calls.Load(); n != 0 {
		t.Errorf("store calls = %d, want 0", n)
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

type cancelOnGetHook struct {
	cancel context.CancelFunc
	setN   atomic.Int32
}

func (h *cancelOnGetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *cancelOnGetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *cancelOnGetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "get":
			h.cancel()
			return context.Canceled
		case "set", "setnx":
			h.setN.Add(1)
		}
		return next(ctx, cmd)
	}
}

/*
TestStoreGetByCodeCanceledDuringCacheReadDoesNotFetchOrigin 验证读缓存途中 ctx 取消（resultSkipped）时，
不会通过 WithoutCancel 穿透进 singleflight、占满 missSlots 槽位、回源数据库或写回缓存。
*/
func TestStoreGetByCodeCanceledDuringCacheReadDoesNotFetchOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	hook := &cancelOnGetHook{cancel: cancel}
	rdb.AddHook(hook)

	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{ID: 99, Code: "abc12345", URL: "https://example.com/99"}, nil
	}}
	s := New(next, rdb, prometheus.NewRegistry(), Options{MissConcurrency: 1})
	// 预先占满 missSlots：若误进入 singleflight 回源，会返回 503 shed 错误而不是 context.Canceled。
	s.missSlots <- struct{}{}
	defer func() { <-s.missSlots }()

	got, err := s.GetByCode(ctx, "abc12345")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetByCode error = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("GetByCode = %+v, want nil", got)
	}
	if n := next.calls.Load(); n != 0 {
		t.Errorf("store calls = %d, want 0", n)
	}
	if n := hook.setN.Load(); n != 0 {
		t.Errorf("redis set calls = %d, want 0", n)
	}
	for _, result := range []string{resultHit, resultMiss, resultFallback} {
		if n := testutil.ToFloat64(s.gets.WithLabelValues(result)); n != 0 {
			t.Errorf("gets{result=%q} = %v, want 0", result, n)
		}
	}
	for _, path := range []string{resultMiss, resultFallback} {
		if n := testutil.ToFloat64(s.shed.WithLabelValues(path)); n != 0 {
			t.Errorf("shed{path=%q} = %v, want 0", path, n)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	soon := time.Now().Add(2 * time.Minute)
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
		{name: "longer than positiveTTL", at: &long, wantExact: positiveTTL, exact: true},
		{name: "two minutes", at: &soon, upperBound: 2 * time.Minute},
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

	expires := time.Now().Add(2 * time.Minute)
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
	if ttl <= 0 || ttl > 2*time.Minute {
		t.Errorf("cache TTL = %v, want in (0, 2m]", ttl)
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

func TestStoreGetByCodePopulatesRequestEvent(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{get: func(_ context.Context, c string) (*link.Link, error) {
		return &link.Link{ID: 7, Code: c, URL: "https://example.com"}, nil
	}}
	s := newTestStore(t, next, rdb)

	// 1. miss
	ctx, ev := logx.WithRequestEvent(context.Background())
	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode miss: %v", err)
	}
	if got := ev.Snapshot().Cache; got != resultMiss {
		t.Errorf("first GetByCode cache = %q, want %q", got, resultMiss)
	}

	// 2. hit
	ctx, ev = logx.WithRequestEvent(context.Background())
	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode hit: %v", err)
	}
	if got := ev.Snapshot().Cache; got != resultHit {
		t.Errorf("second GetByCode cache = %q, want %q", got, resultHit)
	}

	// 3. neg_hit
	if err := rdb.Set(context.Background(), keyPrefix+code, "null", time.Minute).Err(); err != nil {
		t.Fatalf("set negative cache: %v", err)
	}
	ctx, ev = logx.WithRequestEvent(context.Background())
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("GetByCode neg_hit err = %v, want ErrNotFound", err)
	}
	if got := ev.Snapshot().Cache; got != "neg_hit" {
		t.Errorf("neg_hit cache = %q, want neg_hit", got)
	}

	// 4. fallback (unavailable Redis)
	badRDB := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = badRDB.Close() })
	fallbackStore := newTestStore(t, next, badRDB)
	ctx, ev = logx.WithRequestEvent(context.Background())
	if _, err := fallbackStore.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode fallback: %v", err)
	}
	if got := ev.Snapshot().Cache; got != resultFallback {
		t.Errorf("fallback cache = %q, want %q", got, resultFallback)
	}
}

func TestNewReaderGetsAndCachesLink(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	want := &link.Link{ID: 11, Code: code, URL: "https://example.com/reader"}
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return want, nil
	}}
	reg := prometheus.NewRegistry()
	r := NewReader(next, rdb, reg, Options{})

	got, err := r.GetByCode(context.Background(), code)
	if err != nil || got.URL != want.URL {
		t.Fatalf("first GetByCode = (%+v, %v), want URL %q", got, err, want.URL)
	}
	got, err = r.GetByCode(context.Background(), code)
	if err != nil || got.URL != want.URL {
		t.Fatalf("second GetByCode = (%+v, %v), want URL %q", got, err, want.URL)
	}
	if calls := next.calls.Load(); calls != 1 {
		t.Errorf("origin calls = %d, want 1", calls)
	}
}
