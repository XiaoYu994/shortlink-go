package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/link"
)

func TestGetByCodeCountsMissThenHit(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	reg := prometheus.NewRegistry()
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{ID: 1, Code: code, URL: "https://example.com/1"}, nil
	}}
	s := New(next, rdb, reg, Options{})
	ctx := context.Background()

	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("first GetByCode: %v", err)
	}
	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("second GetByCode: %v", err)
	}

	assertCacheResult(t, s, resultMiss, 1)
	assertCacheResult(t, s, resultHit, 1)
	assertCacheResult(t, s, resultFallback, 0)
	// get/miss、get/hit，加上回源后写正缓存的 set/success。
	if n := testutil.CollectAndCount(s.duration); n != 3 {
		t.Errorf("duration series = %d, want 3", n)
	}
}

func TestGetByCodeCountsNegativeAsHit(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	reg := prometheus.NewRegistry()
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return nil, link.ErrNotFound
	}}
	s := New(next, rdb, reg, Options{})
	ctx := context.Background()

	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("first GetByCode error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("second GetByCode error = %v, want ErrNotFound", err)
	}

	assertCacheResult(t, s, resultMiss, 1)
	assertCacheResult(t, s, resultHit, 1)
	assertCacheResult(t, s, resultFallback, 0)
}

func TestGetByCodeCountsCorruptAsMiss(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	if err := rdb.Set(ctx, keyPrefix+code, "not json", time.Hour).Err(); err != nil {
		t.Fatalf("seed corrupted value: %v", err)
	}

	reg := prometheus.NewRegistry()
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{ID: 9, Code: code, URL: "https://example.com/9"}, nil
	}}
	s := New(next, rdb, reg, Options{})

	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	assertCacheResult(t, s, resultMiss, 1)
	assertCacheResult(t, s, resultHit, 0)
	assertCacheResult(t, s, resultFallback, 0)
}

func TestGetByCodeCountsFallback(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	reg := prometheus.NewRegistry()
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{URL: "https://example.com/down"}, nil
	}}
	s := New(next, rdb, reg, Options{})

	if _, err := s.GetByCode(context.Background(), "down"); err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	assertCacheResult(t, s, resultFallback, 1)
	assertCacheResult(t, s, resultMiss, 0)
	assertCacheResult(t, s, resultHit, 0)
}

func TestGetByCodeCanceledDoesNotCount(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	reg := prometheus.NewRegistry()
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		return &link.Link{URL: "https://example.com/canceled"}, nil
	}}
	s := New(next, rdb, reg, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetByCode(ctx, code); err != nil {
		t.Fatalf("GetByCode: %v", err)
	}

	assertCacheResult(t, s, resultHit, 0)
	assertCacheResult(t, s, resultMiss, 0)
	assertCacheResult(t, s, resultFallback, 0)
}

func TestGetByCodeSingleflightCountsEachMiss(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	const goroutines = 20
	// 等全部 GET 返回再放行。用 sleep 对齐时，晚到的请求会读到刚写上的正缓存。
	reads := &countGetsHook{want: goroutines, done: make(chan struct{})}
	rdb.AddHook(reads)

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var enteredOnce sync.Once
	next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return &link.Link{ID: 7, Code: code, URL: "https://example.com/7"}, nil
	}}
	reg := prometheus.NewRegistry()
	s := New(next, rdb, reg, Options{})

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Go(func() {
			<-start
			if _, err := s.GetByCode(context.Background(), code); err != nil {
				errs <- err
			}
		})
	}

	close(start)
	<-entered
	select {
	case <-reads.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %d cache reads, got %d; synchronization failed before checking cache results", goroutines, reads.n.Load())
	}
	unblock()
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("store calls = %d, want 1", n)
	}
	assertCacheResult(t, s, resultMiss, goroutines)
	assertCacheResult(t, s, resultHit, 0)
	assertCacheResult(t, s, resultFallback, 0)
}

func TestCreateDoesNotCount(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	reg := prometheus.NewRegistry()
	s := New(&fakeStore{}, rdb, reg, Options{})
	if err := s.Create(context.Background(), &link.Link{Code: "x", URL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() == getsMetricName {
			t.Fatalf("Create recorded %d cache series", len(fam.GetMetric()))
		}
	}
}

func TestWriteCanceledDuringSetDoesNotCount(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })

	started := make(chan struct{})
	rdb.AddHook(&blockSetHook{started: started})

	reg := prometheus.NewRegistry()
	s := New(&fakeStore{}, rdb, reg, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Create(ctx, &link.Link{Code: "x", URL: "https://example.com"})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SET to start")
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Create: %v", err)
	}

	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeError)); n != 0 {
		t.Errorf("writes{result=%q} = %v, want 0", writeError, n)
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); n != 0 {
		t.Errorf("writes{result=%q} = %v, want 0", writeSuccess, n)
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSkipped)); n != 0 {
		t.Errorf("writes{result=%q} = %v, want 0", writeSkipped, n)
	}
}

func assertCacheResult(t *testing.T, s *Store, result string, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(s.gets.WithLabelValues(result)); got != want {
		t.Errorf("gets{result=%q} = %v, want %v", result, got, want)
	}
}

// countGetsHook 在 GET 返回后计数，用来等所有请求都读完缓存。
type countGetsHook struct {
	n    atomic.Int32
	want int32
	done chan struct{}
	once sync.Once
}

func (h *countGetsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *countGetsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *countGetsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "get" && h.n.Add(1) == h.want {
			h.once.Do(func() { close(h.done) })
		}
		return err
	}
}

// blockSetHook 把 SET 停住直到 ctx 取消，覆盖调用过程中取消的情况。
type blockSetHook struct {
	started chan struct{}
	once    sync.Once
}

func (h *blockSetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *blockSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *blockSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "set" {
			return next(ctx, cmd)
		}
		h.once.Do(func() { close(h.started) })
		<-ctx.Done()
		return ctx.Err()
	}
}
