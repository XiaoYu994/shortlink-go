package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/cache"
	"shortlink/internal/link"
)

/*
TestCacheFallsBackToPostgres 在 Redis 已经不可用时回源真实数据库。
缓存单测用的是假存储；这里确认降级读到的是表里的那一行。
*/
func TestCacheFallsBackToPostgres(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	l := mustLink(t, "https://example.com/fallback", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()
	cached := cache.New(store, rdb, prometheus.NewRegistry(), cache.Options{})

	got, err := cached.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != l.ID || got.URL != l.URL || got.Code != l.Code {
		t.Errorf("GetByCode = %+v, want id %d url %q", got, l.ID, l.URL)
	}
}

/*
TestCacheOriginWriteKeepsPostgresUpdate 重放「回源读到旧行，管理写已经提交」的交错。
在途请求返回的是它读到的旧 URL；写回缓存用 SET NX，不能把管理写的新 URL 盖掉。
缓存单测直接调用 writeCache，这里两边都是真实的 PostgreSQL 和 Redis。
*/
func TestCacheOriginWriteKeepsPostgresUpdate(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	oldURL := "https://example.com/origin-old"
	newURL := "https://example.com/origin-new"
	l := mustLink(t, oldURL, tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rdb, err := cache.Open(ctx, redisURL)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	// key 前缀和 cache.keyPrefix 一致。创建没经过缓存，删一次是为了清掉上次中断留下的同名 key。
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), "shortlink:link:"+l.Code).Err()
	})

	gate := &gateStore{Store: store, started: make(chan struct{}), release: make(chan struct{})}
	cached := cache.New(gate, rdb, prometheus.NewRegistry(), cache.Options{
		FetchTimeout: 5 * time.Second,
	})

	type fetchResult struct {
		url string
		err error
	}
	fetched := make(chan fetchResult, 1)
	go func() {
		got, err := cached.GetByCode(ctx, l.Code)
		if err != nil {
			fetched <- fetchResult{err: err}
			return
		}
		fetched <- fetchResult{url: got.URL}
	}()

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("GetByCode did not reach the database")
	}

	if _, err := cached.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(gate.release)

	select {
	case res := <-fetched:
		if res.err != nil {
			t.Fatalf("in-flight GetByCode: %v", res.err)
		}
		if res.url != oldURL {
			t.Errorf("in-flight URL = %q, want the snapshot %q", res.url, oldURL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight GetByCode did not return")
	}

	got, err := cached.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("cached GetByCode: %v", err)
	}
	if got.URL != newURL {
		t.Errorf("cached URL = %q, want %q", got.URL, newURL)
	}
	// 第一次是在途回源，第二次是 Update 写完正缓存后的复查；最后一次命中缓存不该再读库。
	if calls := gate.calls.Load(); calls != 2 {
		t.Errorf("store GetByCode calls = %d, want 2", calls)
	}
}

/*
TestCacheRecheckClosesTakedownInterleaving 重放 ADR 0016「下架和管理写交错」：
租户 PATCH 提交之后、写正缓存之前，平台封禁提交并写了负缓存；
晚到的正缓存会盖掉负缓存，但 Update 的复查会再读一次库并补写负缓存。
*/
func TestCacheRecheckClosesTakedownInterleaving(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	l := mustLink(t, "https://example.com/interleaved", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rdb, err := cache.Open(ctx, redisURL)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	key := "shortlink:link:" + l.Code
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	/*
		inner 是平台封禁用的一份缓存存储：它的 Ban 提交数据库并写负缓存。
		outer 的 Update 在数据库提交之后、写正缓存之前调用 inner.Ban，
		复现「平台 SET 负缓存 -> 租户 SET 正缓存」的顺序。
		只看最终值：外层复查补写负缓存后，Redis 里必须是 null。
	*/
	inner := cache.New(store, rdb, prometheus.NewRegistry(), cache.Options{})
	outerStore := &interleaveStore{Store: store, inner: inner, code: l.Code}
	outer := cache.New(outerStore, rdb, prometheus.NewRegistry(), cache.Options{})

	newURL := "https://example.com/interleaved-new"
	if _, err := outer.Update(ctx, tenantID, l.Code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if outerStore.banErr != nil {
		t.Fatalf("interleaved Ban: %v", outerStore.banErr)
	}

	data, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("get cached value: %v", err)
	}
	if string(data) != "null" {
		t.Fatalf("cached value = %q, want null after recheck", data)
	}
	if _, err := outer.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("GetByCode error = %v, want ErrNotFound", err)
	}
}

/*
interleaveStore 在真实 postgres.Store 的 Update 提交之后、cache.Store 写正缓存之前，
用另一个 cache.Store 完成一次 Ban。只触发一次，避免复查的读也触发。
*/
type interleaveStore struct {
	*Store
	inner  *cache.Store
	code   string
	once   sync.Once
	banErr error
}

func (s *interleaveStore) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	got, err := s.Store.Update(ctx, tenantID, code, u)
	if err != nil {
		return nil, err
	}
	s.once.Do(func() {
		_, s.banErr = s.inner.Ban(ctx, s.code, link.BanReasonPhishing, link.BanSourceOperator)
	})
	return got, nil
}

/*
gateStore 在读完数据库之后停住，让管理写插到回源写缓存之前。
停在查询完成之后：连接已经还回池，Update 不会被这次读取堵住。
只有第一次读库停住；Update 写完正缓存后的复查要直接放行，
否则复查会等在这里，和测试主流程互相等待。
*/
type gateStore struct {
	*Store
	started chan struct{}
	release chan struct{}
	gated   atomic.Bool
	calls   atomic.Int32
}

func (g *gateStore) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	g.calls.Add(1)
	got, err := g.Store.GetByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if g.gated.CompareAndSwap(false, true) {
		close(g.started)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, fmt.Errorf("gate: %w", ctx.Err())
		}
	}
	return got, nil
}
