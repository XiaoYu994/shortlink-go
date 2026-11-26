package postgres

import (
	"context"
	"fmt"
	"os"
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
	ctx := testAuditCtx(context.Background())
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
	ctx := testAuditCtx(context.Background())

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
	// 只有第一次在途回源查了库；Update 不再做多余复查，最后一次直接命中缓存（ADR 0031）。
	if calls := gate.calls.Load(); calls != 1 {
		t.Errorf("store GetByCode calls = %d, want 1", calls)
	}
}

/*
gateStore 在读完数据库之后停住，让管理写插到回源写缓存之前。
停在查询完成之后：连接已经还回池，Update 不会被这次读取堵住。
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
