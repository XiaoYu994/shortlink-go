package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/link"
)

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
			if temp, ok := errors.AsType[interface {
				error
				Temporary() bool
			}](err); ok && temp.Temporary() {
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
			if temp, ok := errors.AsType[interface {
				error
				Temporary() bool
			}](err); ok && temp.Temporary() {
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

/*
TestStoreUpdateWritesThrough 验证修改提交后无条件覆盖缓存：
之后的跳转直接命中新值，不再回源。
*/
