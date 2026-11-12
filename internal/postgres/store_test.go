package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

/*
newTestStore 每次测试前清空 TEST_DATABASE_URL 指向的 links 表。
没有设置 TEST_DATABASE_URL 时跳过；库名不以 _test 结尾时失败，避免误清开发库。
*/
func newTestStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("refusing to truncate non-test database %q: name must end with _test", dbName)
	}

	ctx := context.Background()
	db, err := Open(ctx, dsn, DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "TRUNCATE "+linksTable+" RESTART IDENTITY"); err != nil {
		t.Fatalf("truncate %s: %v", linksTable, err)
	}
	return NewStore(db)
}

func mustLink(t *testing.T, rawURL string) *link.Link {
	t.Helper()
	l, err := link.NewLink(rawURL)
	if err != nil {
		t.Fatalf("NewLink(%q): %v", rawURL, err)
	}
	return l
}

func TestStoreCreateAndGetByCode(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	l := mustLink(t, "https://example.com/path?q=1")
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if l.ID <= 0 {
		t.Errorf("ID = %d, want > 0", l.ID)
	}
	if len(l.Code) != idgen.CodeLength {
		t.Errorf("Code length = %d, want %d", len(l.Code), idgen.CodeLength)
	}
	if l.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != l.ID || got.Code != l.Code || got.URL != l.URL || !got.CreatedAt.Equal(l.CreatedAt) {
		t.Errorf("GetByCode = %+v, want %+v", got, l)
	}
}

func TestStoreGetByCodeNotFound(t *testing.T) {
	store := newTestStore(t)

	_, err := store.GetByCode(context.Background(), "missing")
	if !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want link.ErrNotFound", err)
	}
}

func TestStoreCreateRetriesOnCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	codes := []string{"collis01", "collis01", "collis02"}
	store.generate = func() (string, error) {
		if len(codes) == 0 {
			t.Fatal("generate called too many times")
		}
		code := codes[0]
		codes = codes[1:]
		return code, nil
	}

	first := mustLink(t, "https://example.com/1")
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := mustLink(t, "https://example.com/2")
	if err := store.Create(ctx, second); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second.Code != "collis02" {
		t.Errorf("second Code = %q, want %q", second.Code, "collis02")
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const n = 50

	links := make([]*link.Link, n)
	for i := range links {
		links[i] = mustLink(t, "https://example.com/concurrent")
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range links {
		wg.Go(func() {
			errs[i] = store.Create(ctx, links[i])
		})
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if seen[links[i].Code] {
			t.Errorf("duplicate code %q", links[i].Code)
		}
		seen[links[i].Code] = true
	}
}

func TestStoreContextCanceled(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := mustLink(t, "https://example.com")
	if err := store.Create(ctx, l); !errors.Is(err, context.Canceled) {
		t.Errorf("Create error = %v, want context.Canceled", err)
	}
	if _, err := store.GetByCode(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetByCode error = %v, want context.Canceled", err)
	}
}
