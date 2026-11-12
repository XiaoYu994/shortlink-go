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
newTestRedis 连 TEST_REDIS_ADDR 指向的真实 Redis；没有设置时跳过。
测试只读写自己生成的 test- 前缀短码，结束时删除，不使用 FLUSHDB。
*/
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rdb, err := Open(ctx, addr)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
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
	s := New(next, rdb)
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
}

func TestStoreGetByCodeNegativeCache(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return nil, link.ErrNotFound
	}}
	s := New(next, rdb)
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
	s := New(next, rdb)
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
	s := New(next, rdb)

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
	s := New(next, rdb)

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
	s := New(next, rdb)

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
	s := New(next, rdb)

	if err := s.Create(context.Background(), &link.Link{Code: "x", URL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := next.creates.Load(); n != 1 {
		t.Errorf("store Creates = %d, want 1", n)
	}
}
