package cache

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/link"
)

func TestStoreUpdateWritesThrough(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	newURL := "https://example.com/new"
	next := &fakeStore{
		update: func(_ context.Context, tenantID int64, gotCode string, u link.Update) (*link.AdminLink, error) {
			if tenantID != 1 || gotCode != code {
				t.Errorf("Update(%d, %q), want (1, %q)", tenantID, gotCode, code)
			}
			return &link.AdminLink{
				Link:   link.Link{ID: 7, Code: code, URL: *u.URL},
				Clicks: 3,
			}, nil
		},
		// 写了正缓存之后的复查；链接还在，保留正缓存。
		get: func(context.Context, string) (*link.Link, error) {
			return &link.Link{ID: 7, Code: code, URL: newURL}, nil
		},
	}
	s := newTestStore(t, next, rdb)
	ctx := context.Background()

	updated, err := s.Update(ctx, 1, code, link.Update{URL: &newURL})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.URL != newURL {
		t.Errorf("updated URL = %q, want %q", updated.URL, newURL)
	}

	want := &link.Link{ID: 7, Code: code, URL: newURL}
	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after Update")
		return nil, link.ErrNotFound
	}
	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.URL != want.URL || got.ID != want.ID {
		t.Errorf("GetByCode = %+v, want %+v", got, want)
	}
	if n := next.updates.Load(); n != 1 {
		t.Errorf("updates = %d, want 1", n)
	}
}

// TestStoreUpdateExpiredWritesNegative 验证只改 URL、链接原本已过期时写负缓存。
func TestStoreUpdateExpiredWritesNegative(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	next := &fakeStore{update: func(_ context.Context, _ int64, _ string, _ link.Update) (*link.AdminLink, error) {
		return &link.AdminLink{
			Link: link.Link{ID: 7, Code: code, URL: "https://example.com/expired", ExpiresAt: &past},
		}, nil
	}}
	s := newTestStore(t, next, rdb)
	if _, err := s.Update(ctx, 1, code, link.Update{URL: ptrString("https://example.com/expired")}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := rdb.Get(ctx, keyPrefix+code).Val(); got != "null" {
		t.Errorf("cached value = %q, want null", got)
	}
	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after negative cache")
		return nil, link.ErrNotFound
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want ErrNotFound", err)
	}
}

/*
TestStoreDeleteWritesNegative 验证删除后无条件写负缓存，
盖掉之前的正缓存；之后的跳转直接 404。
*/
func TestStoreDeleteWritesNegative(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	next := &fakeStore{deleteFn: func(_ context.Context, tenantID int64, gotCode string) error {
		if tenantID != 1 || gotCode != code {
			t.Errorf("Delete(%d, %q), want (1, %q)", tenantID, gotCode, code)
		}
		return nil
	}}
	s := newTestStore(t, next, rdb)

	_ = s.writeCache(ctx, keyPrefix+code, &link.Link{ID: 7, Code: code, URL: "https://example.com/old"}, writeOverwrite)
	if err := s.Delete(ctx, 1, code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := rdb.Get(ctx, keyPrefix+code).Val(); got != "null" {
		t.Errorf("cached value = %q, want null", got)
	}
	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after delete")
		return nil, link.ErrNotFound
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want ErrNotFound", err)
	}
}

/*
TestDeleteWritesCacheWhenRequestAlreadyCanceled 覆盖提交之后的缓存写入。
数据库已经成功，请求 ctx 却已经取消。负缓存仍要写上，
否则跳转会一直命中删除前的正缓存，直到 TTL。
*/
func TestDeleteWritesCacheWhenRequestAlreadyCanceled(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)

	next := &fakeStore{deleteFn: func(context.Context, int64, string) error { return nil }}
	s := newTestStore(t, next, rdb)
	_ = s.writeCache(context.Background(), keyPrefix+code, &link.Link{
		ID: 7, Code: code, URL: "https://example.com/old",
	}, writeOverwrite)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Delete(ctx, 1, code); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := rdb.Get(context.Background(), keyPrefix+code).Val(); got != "null" {
		t.Errorf("cached value = %q, want null", got)
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); n != 2 {
		t.Errorf("writes{result=success} = %v, want 2", n)
	}
}

/*
TestStoreOriginWriteDoesNotOverwriteNewValue 固定 ADR 0011 的交错语义：
在途回源读到旧值，修改提交后已经无条件覆盖缓存，旧回源的 SET NX 必须失败，
不能把新值盖回去。
*/
func TestStoreOriginWriteDoesNotOverwriteNewValue(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()
	key := keyPrefix + code

	s := newTestStore(t, &fakeStore{}, rdb)
	newLink := &link.Link{ID: 7, Code: code, URL: "https://example.com/new"}
	stale := &link.Link{ID: 7, Code: code, URL: "https://example.com/old"}

	_ = s.writeCache(ctx, key, newLink, writeOverwrite)
	_ = s.writeCache(ctx, key, stale, writeNX)

	data, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("get cached value: %v", err)
	}
	var cached *link.Link
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cached == nil || cached.URL != newLink.URL {
		t.Errorf("cached = %+v, want new URL %q", cached, newLink.URL)
	}
	if got := testutil.ToFloat64(s.writes.WithLabelValues(writeSkipped)); got != 1 {
		t.Errorf("writes{result=skipped} = %v, want 1", got)
	}
}

/*
TestStoreGetByCodeUnservableCacheEntry 命中带停用或封禁信息的正缓存时，
按找不到处理并删掉 key，防止写缓存的地方漏判。
*/
func TestStoreGetByCodeUnservableCacheEntry(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	tests := []struct {
		name  string
		state func(l *link.Link)
	}{
		{"expired", func(l *link.Link) { l.ExpiresAt = &past }},
		{"disabled", func(l *link.Link) { l.DisabledAt = &past }},
		{"banned", func(l *link.Link) {
			l.Ban = &link.Ban{At: now, Reason: link.BanReasonPhishing, Source: link.BanSourceOperator}
		}},
		{"disabled and banned", func(l *link.Link) {
			l.DisabledAt = &past
			l.Ban = &link.Ban{At: now, Reason: link.BanReasonSpam, Source: link.BanSourceRisk}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := newTestRedis(t)
			code := newTestCode(t)
			cleanupTestKey(t, rdb, code)
			ctx := context.Background()

			cached := &link.Link{ID: 7, Code: code, URL: "https://example.com/unservable"}
			tt.state(cached)
			body, err := json.Marshal(cached)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := rdb.Set(ctx, keyPrefix+code, body, time.Hour).Err(); err != nil {
				t.Fatalf("set cache: %v", err)
			}

			next := &fakeStore{get: func(context.Context, string) (*link.Link, error) {
				t.Error("unservable cache entry fell through to the store")
				return nil, link.ErrNotFound
			}}
			s := newTestStore(t, next, rdb)
			if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
				t.Fatalf("GetByCode error = %v, want ErrNotFound", err)
			}
			if n := rdb.Exists(ctx, keyPrefix+code).Val(); n != 0 {
				t.Errorf("cached key still exists")
			}
		})
	}
}

// TestStoreUpdateDisabledWritesNegative 验证停用提交后写负缓存，之后的跳转直接 404。
func TestStoreUpdateDisabledWritesNegative(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()
	disabledAt := time.Now().Add(-time.Minute)

	next := &fakeStore{update: func(_ context.Context, _ int64, _ string, u link.Update) (*link.AdminLink, error) {
		if u.Disabled == nil || !*u.Disabled {
			t.Errorf("update Disabled = %v, want true", u.Disabled)
		}
		return &link.AdminLink{
			Link: link.Link{ID: 7, Code: code, URL: "https://example.com/disabled", DisabledAt: &disabledAt},
		}, nil
	}}
	s := newTestStore(t, next, rdb)
	if _, err := s.Update(ctx, 1, code, link.Update{Disabled: ptrBool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := rdb.Get(ctx, keyPrefix+code).Val(); got != "null" {
		t.Errorf("cached value = %q, want null", got)
	}
	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after negative cache")
		return nil, link.ErrNotFound
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want ErrNotFound", err)
	}
}

// TestStoreUpdateEnabledWritesPositive 验证启用提交后写新的正缓存，之后直接跳转新值。
func TestStoreUpdateEnabledWritesPositive(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	next := &fakeStore{
		update: func(_ context.Context, _ int64, _ string, u link.Update) (*link.AdminLink, error) {
			if u.Disabled == nil || *u.Disabled {
				t.Errorf("update Disabled = %v, want false", u.Disabled)
			}
			return &link.AdminLink{
				Link: link.Link{ID: 7, Code: code, URL: "https://example.com/enabled"},
			}, nil
		},
		// 写了正缓存之后的复查；链接还在，保留正缓存。
		get: func(context.Context, string) (*link.Link, error) {
			return &link.Link{ID: 7, Code: code, URL: "https://example.com/enabled"}, nil
		},
	}
	s := newTestStore(t, next, rdb)
	if _, err := s.Update(ctx, 1, code, link.Update{Disabled: ptrBool(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after positive cache write")
		return nil, link.ErrNotFound
	}
	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.URL != "https://example.com/enabled" {
		t.Errorf("GetByCode URL = %q, want enabled URL", got.URL)
	}
}

func ptrString(s string) *string { return &s }

func ptrBool(v bool) *bool { return &v }

/*
TestStoreBanWritesNegative 封禁提交后无条件写负缓存，
之后的跳转直接命中负缓存，不再回源。
*/
func TestStoreBanWritesNegative(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	banned := &link.Link{
		ID: 7, Code: code, URL: "https://example.com/ban",
		Ban: &link.Ban{
			At:     time.Now().UTC().Truncate(time.Second),
			Reason: link.BanReasonPhishing,
			Source: link.BanSourceOperator,
		},
	}
	next := &fakeStore{ban: func(_ context.Context, gotCode string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
		if gotCode != code || reason != link.BanReasonPhishing || source != link.BanSourceOperator {
			t.Errorf("Ban(%q, %q, %q), want (%q, phishing, operator)", gotCode, reason, source, code)
		}
		return banned, nil
	}}
	s := newTestStore(t, next, rdb)

	got, err := s.Ban(ctx, code, link.BanReasonPhishing, link.BanSourceOperator)
	if err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if got != banned {
		t.Fatalf("Ban link = %+v, want %+v", got, banned)
	}
	if value := rdb.Get(ctx, keyPrefix+code).Val(); value != "null" {
		t.Errorf("cached value = %q, want null", value)
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); n != 1 {
		t.Errorf("writes{result=%q} = %v, want 1", writeSuccess, n)
	}

	next.get = func(context.Context, string) (*link.Link, error) {
		t.Error("GetByCode fell through to the store after ban")
		return nil, link.ErrNotFound
	}
	if _, err := s.GetByCode(ctx, code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want ErrNotFound", err)
	}
}

// TestStoreUnbanWritesCachedState 解封后按 Servable 写正缓存；仍然停用或已过期的写负缓存。
func TestStoreUnbanWritesCachedState(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	tests := []struct {
		name         string
		state        func(l *link.Link)
		wantNegative bool
	}{
		{name: "active"},
		{name: "still disabled", state: func(l *link.Link) { l.DisabledAt = &past }, wantNegative: true},
		{name: "expired", state: func(l *link.Link) { l.ExpiresAt = &past }, wantNegative: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := newTestRedis(t)
			code := newTestCode(t)
			cleanupTestKey(t, rdb, code)
			ctx := context.Background()

			unbanned := &link.Link{ID: 7, Code: code, URL: "https://example.com/unbanned"}
			if tt.state != nil {
				tt.state(unbanned)
			}
			next := &fakeStore{
				unban: func(_ context.Context, gotCode string) (*link.Link, error) {
					if gotCode != code {
						t.Errorf("Unban(%q), want %q", gotCode, code)
					}
					return unbanned, nil
				},
				get: func(_ context.Context, gotCode string) (*link.Link, error) {
					t.Error("Unban rechecked a link that should have been written as a negative cache")
					return nil, link.ErrNotFound
				},
			}
			if !tt.wantNegative {
				// 可跳转的分支会写正缓存，然后复查一次。
				next.get = func(_ context.Context, gotCode string) (*link.Link, error) {
					if gotCode != code {
						t.Errorf("recheck code = %q, want %q", gotCode, code)
					}
					return unbanned, nil
				}
			}
			s := newTestStore(t, next, rdb)
			_ = s.writeCache(ctx, keyPrefix+code, &link.Link{
				ID: 7, Code: code, URL: "https://example.com/old",
			}, writeOverwrite)

			got, err := s.Unban(ctx, code)
			if err != nil {
				t.Fatalf("Unban: %v", err)
			}
			if got != unbanned {
				t.Fatalf("Unban link = %+v, want %+v", got, unbanned)
			}

			if tt.wantNegative {
				if value := rdb.Get(ctx, keyPrefix+code).Val(); value != "null" {
					t.Errorf("cached value = %q, want null", value)
				}
			} else {
				data, err := rdb.Get(ctx, keyPrefix+code).Bytes()
				if err != nil {
					t.Fatalf("get cached value: %v", err)
				}
				var cached *link.Link
				if err := json.Unmarshal(data, &cached); err != nil {
					t.Fatalf("decode cached value: %v", err)
				}
				if cached == nil || cached.URL != unbanned.URL {
					t.Errorf("cached value = %+v, want URL %q", cached, unbanned.URL)
				}
			}

			next.get = func(context.Context, string) (*link.Link, error) {
				t.Error("GetByCode fell through to the store after unban")
				return nil, link.ErrNotFound
			}
			got, err = s.GetByCode(ctx, code)
			if tt.wantNegative {
				if !errors.Is(err, link.ErrNotFound) {
					t.Errorf("GetByCode error = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetByCode: %v", err)
			}
			if got.URL != unbanned.URL {
				t.Errorf("GetByCode URL = %q, want %q", got.URL, unbanned.URL)
			}
		})
	}
}

/*
TestStoreBanReportsCacheFailure 验证 Redis 不可用时 Ban 返回 ErrNotUpdated，
同时返回数据库行的 Link；下游封禁已经执行，重跑只是再写一次缓存。
*/
func TestStoreBanReportsCacheFailure(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	banned := &link.Link{ID: 7, Code: "down", URL: "https://example.com/ban"}
	next := &fakeStore{ban: func(context.Context, string, link.BanReason, link.BanSource) (*link.Link, error) {
		return banned, nil
	}}
	s := newTestStore(t, next, rdb)

	got, err := s.Ban(context.Background(), "down", link.BanReasonSpam, link.BanSourceOperator)
	if !errors.Is(err, ErrNotUpdated) {
		t.Fatalf("Ban error = %v, want ErrNotUpdated", err)
	}
	if got != banned {
		t.Fatalf("Ban link = %+v, want %+v", got, banned)
	}
	if n := next.bans.Load(); n != 1 {
		t.Errorf("store Ban calls = %d, want 1", n)
	}
	if n := testutil.ToFloat64(s.writes.WithLabelValues(writeError)); n != 1 {
		t.Errorf("writes{result=%q} = %v, want 1", writeError, n)
	}
}

// TestStoreBanRetryAfterRedisRecovers Redis 恢复后重跑同一条命令，负缓存写上。
func TestStoreBanRetryAfterRedisRecovers(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	banned := &link.Link{ID: 7, Code: code, URL: "https://example.com/ban"}
	next := &fakeStore{ban: func(context.Context, string, link.BanReason, link.BanSource) (*link.Link, error) {
		return banned, nil
	}}

	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = down.Close()
	if _, err := newTestStore(t, next, down).Ban(ctx, code, link.BanReasonSpam, link.BanSourceOperator); !errors.Is(err, ErrNotUpdated) {
		t.Fatalf("Ban with Redis down error = %v, want ErrNotUpdated", err)
	}

	got, err := newTestStore(t, next, rdb).Ban(ctx, code, link.BanReasonSpam, link.BanSourceOperator)
	if err != nil {
		t.Fatalf("Ban after Redis recovered: %v", err)
	}
	if got != banned {
		t.Fatalf("Ban link = %+v, want %+v", got, banned)
	}
	if value := rdb.Get(ctx, keyPrefix+code).Val(); value != "null" {
		t.Errorf("cached value = %q, want null", value)
	}
	if n := next.bans.Load(); n != 2 {
		t.Errorf("store Ban calls = %d, want 2", n)
	}
}

// TestStoreBanAndUnbanReturnDownstreamNotFound 下游 ErrNotFound 原样返回，缓存不动。
func TestStoreBanAndUnbanReturnDownstreamNotFound(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	tests := []struct {
		name string
		next *fakeStore
		call func(*Store) (*link.Link, error)
	}{
		{
			name: "ban",
			next: &fakeStore{ban: func(context.Context, string, link.BanReason, link.BanSource) (*link.Link, error) {
				return nil, link.ErrNotFound
			}},
			call: func(s *Store) (*link.Link, error) {
				return s.Ban(ctx, code, link.BanReasonSpam, link.BanSourceOperator)
			},
		},
		{
			name: "unban",
			next: &fakeStore{unban: func(context.Context, string) (*link.Link, error) {
				return nil, link.ErrNotFound
			}},
			call: func(s *Store) (*link.Link, error) {
				return s.Unban(ctx, code)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t, tt.next, rdb)
			got, err := tt.call(s)
			if !errors.Is(err, link.ErrNotFound) {
				t.Fatalf("%s error = %v, want ErrNotFound", tt.name, err)
			}
			if got != nil {
				t.Fatalf("%s link = %+v, want nil", tt.name, got)
			}
			if n := rdb.Exists(ctx, keyPrefix+code).Val(); n != 0 {
				t.Errorf("%s wrote a cache key on downstream error", tt.name)
			}
			if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); n != 0 {
				t.Errorf("%s writes{result=%q} = %v, want 0", tt.name, writeSuccess, n)
			}
		})
	}
}

/*
TestStoreUpdateRechecksAfterPositiveWrite 重放 ADR 0016 的复查：
正缓存写完再读一次库，读不到（或读库失败）就补写负缓存，读到链接就保留正缓存。
*/
func TestStoreUpdateRechecksAfterPositiveWrite(t *testing.T) {
	newURL := "https://example.com/rechecked"
	boom := errors.New("database down")
	tests := []struct {
		name         string
		get          func(ctx context.Context, code string) (*link.Link, error)
		wantNegative bool
	}{
		{
			name: "not found after write",
			get: func(context.Context, string) (*link.Link, error) {
				return nil, link.ErrNotFound
			},
			wantNegative: true,
		},
		{
			name: "recheck error",
			get: func(context.Context, string) (*link.Link, error) {
				return nil, boom
			},
			wantNegative: true,
		},
		{
			name: "still found",
			get: func(_ context.Context, code string) (*link.Link, error) {
				return &link.Link{ID: 7, Code: code, URL: newURL}, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := newTestRedis(t)
			code := newTestCode(t)
			cleanupTestKey(t, rdb, code)
			ctx := context.Background()

			next := &fakeStore{
				update: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
					return &link.AdminLink{Link: link.Link{ID: 7, Code: code, URL: newURL}}, nil
				},
				get: tt.get,
			}
			s := newTestStore(t, next, rdb)

			if _, err := s.Update(ctx, 1, code, link.Update{URL: &newURL}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if n := next.calls.Load(); n != 1 {
				t.Errorf("recheck GetByCode calls = %d, want 1", n)
			}

			if tt.wantNegative {
				if value := rdb.Get(ctx, keyPrefix+code).Val(); value != "null" {
					t.Errorf("cached value = %q, want null", value)
				}
				if n := testutil.ToFloat64(s.writes.WithLabelValues(writeSuccess)); n != 2 {
					t.Errorf("writes{result=%q} = %v, want 2", writeSuccess, n)
				}
				return
			}
			data, err := rdb.Get(ctx, keyPrefix+code).Bytes()
			if err != nil {
				t.Fatalf("get cached value: %v", err)
			}
			var cached *link.Link
			if err := json.Unmarshal(data, &cached); err != nil {
				t.Fatalf("decode cached value: %v", err)
			}
			if cached == nil || cached.URL != newURL {
				t.Errorf("cached value = %+v, want URL %q", cached, newURL)
			}
		})
	}
}

// TestStoreUnbanRecheckNotFoundWritesNegative 解封复查发现链接已经找不到时，补写负缓存。
func TestStoreUnbanRecheckNotFoundWritesNegative(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	unbanned := &link.Link{ID: 7, Code: code, URL: "https://example.com/unbanned"}
	next := &fakeStore{
		unban: func(context.Context, string) (*link.Link, error) {
			return unbanned, nil
		},
		get: func(context.Context, string) (*link.Link, error) {
			return nil, link.ErrNotFound
		},
	}
	s := newTestStore(t, next, rdb)

	got, err := s.Unban(ctx, code)
	if err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if got != unbanned {
		t.Fatalf("Unban link = %+v, want %+v", got, unbanned)
	}
	if value := rdb.Get(ctx, keyPrefix+code).Val(); value != "null" {
		t.Errorf("cached value = %q, want null", value)
	}
}

// TestUpdateRechecksWhenRequestAlreadyCanceled 复查和补写不跟随请求取消。
func TestUpdateRechecksWhenRequestAlreadyCanceled(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	newURL := "https://example.com/canceled"

	next := &fakeStore{
		update: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
			return &link.AdminLink{Link: link.Link{ID: 7, Code: code, URL: newURL}}, nil
		},
		get: func(ctx context.Context, _ string) (*link.Link, error) {
			if err := ctx.Err(); err != nil {
				t.Errorf("recheck used the canceled request ctx: %v", err)
			}
			return nil, link.ErrNotFound
		},
	}
	s := newTestStore(t, next, rdb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Update(ctx, 1, code, link.Update{URL: &newURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n := next.calls.Load(); n != 1 {
		t.Errorf("recheck GetByCode calls = %d, want 1", n)
	}
	if value := rdb.Get(context.Background(), keyPrefix+code).Val(); value != "null" {
		t.Errorf("cached value = %q, want null", value)
	}
}

// TestStoreCreateDoesNotRecheck 创建响应返回之前没人知道短码，不做复查。
func TestStoreCreateDoesNotRecheck(t *testing.T) {
	rdb := newTestRedis(t)
	code := newTestCode(t)
	cleanupTestKey(t, rdb, code)
	ctx := context.Background()

	next := &fakeStore{
		create: func(l *link.Link) error {
			l.Code = code
			return nil
		},
		get: func(context.Context, string) (*link.Link, error) {
			t.Error("Create triggered a recheck")
			return nil, link.ErrNotFound
		},
	}
	s := newTestStore(t, next, rdb)
	l := &link.Link{URL: "https://example.com/created"}
	if err := s.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := next.calls.Load(); n != 0 {
		t.Errorf("store GetByCode calls = %d, want 0", n)
	}
	got, err := s.GetByCode(ctx, code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.URL != l.URL {
		t.Errorf("GetByCode URL = %q, want %q", got.URL, l.URL)
	}
}
