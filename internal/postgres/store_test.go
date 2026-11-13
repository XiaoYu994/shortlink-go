package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
	"shortlink/internal/tenant"
)

/*
newTestStore 每次测试前清空测试库里的租户、密钥和链接，再插入一个租户。
没有设置 TEST_DATABASE_URL 时跳过；库名不以 _test 结尾时失败，避免误清开发库。
*/
func newTestStore(t *testing.T) (*Store, int64) {
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

	/*
		没有外键之后，TRUNCATE tenants CASCADE 不会再带上 links 和 api_keys。
		三张表写在同一条语句里，有没有外键都能清空。
	*/
	if _, err := db.ExecContext(ctx, "TRUNCATE links, api_keys, tenants RESTART IDENTITY"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	var tenantID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return NewStore(db), tenantID
}

func mustLink(t *testing.T, rawURL string, tenantID int64) *link.Link {
	t.Helper()
	l, err := link.NewLink(rawURL, nil)
	if err != nil {
		t.Fatalf("NewLink(%q): %v", rawURL, err)
	}
	l.TenantID = tenantID
	return l
}

func TestStoreCreateAndGetByCode(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	l := mustLink(t, "https://example.com/path?q=1", tenantID)
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
	if got.ID != l.ID || got.Code != l.Code || got.URL != l.URL || got.TenantID != l.TenantID || !got.CreatedAt.Equal(l.CreatedAt) || got.ExpiresAt != nil {
		t.Errorf("GetByCode = %+v, want %+v", got, l)
	}
}

func TestGetByCodeHonorsExpiresAt(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	// timestamptz 的精度是微秒，纳秒会在往返里丢掉。
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	l, err := link.NewLink("https://example.com/exp", &future)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	l.TenantID = tenantID
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, future)
	}

	if _, err := store.db.ExecContext(ctx,
		"UPDATE "+linksTable+" SET expires_at = now() - interval '1 minute' WHERE id = $1", l.ID); err != nil {
		t.Fatalf("expire link: %v", err)
	}
	if _, err := store.GetByCode(ctx, l.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode expired error = %v, want link.ErrNotFound", err)
	}
}

func TestStoreGetByCodeNotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.GetByCode(context.Background(), "missing")
	if !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want link.ErrNotFound", err)
	}
}

func TestStoreCreateRetriesOnCollision(t *testing.T) {
	store, tenantID := newTestStore(t)
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

	first := mustLink(t, "https://example.com/1", tenantID)
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := mustLink(t, "https://example.com/2", tenantID)
	if err := store.Create(ctx, second); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second.Code != "collis02" {
		t.Errorf("second Code = %q, want %q", second.Code, "collis02")
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	const n = 50

	links := make([]*link.Link, n)
	for i := range links {
		links[i] = mustLink(t, "https://example.com/concurrent", tenantID)
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
	store, tenantID := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := mustLink(t, "https://example.com", tenantID)
	if err := store.Create(ctx, l); !errors.Is(err, context.Canceled) {
		t.Errorf("Create error = %v, want context.Canceled", err)
	}
	if _, err := store.GetByCode(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetByCode error = %v, want context.Canceled", err)
	}
	if _, err := store.Authenticate(ctx, "slk_"+strings.Repeat("ab", 32)); !errors.Is(err, context.Canceled) {
		t.Errorf("Authenticate error = %v, want context.Canceled", err)
	}
}

func TestAuthenticate(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	plaintext, hash, err := tenant.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO api_keys (tenant_id, key_hash) VALUES ($1, $2)`, tenantID, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tenantID {
		t.Errorf("tenant ID = %d, want %d", got.ID, tenantID)
	}

	other, _, err := tenant.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, err := store.Authenticate(ctx, other); !errors.Is(err, tenant.ErrUnauthenticated) {
		t.Errorf("unknown key error = %v, want ErrUnauthenticated", err)
	}
	if _, err := store.Authenticate(ctx, "not-a-key"); !errors.Is(err, tenant.ErrUnauthenticated) {
		t.Errorf("malformed key error = %v, want ErrUnauthenticated", err)
	}
}

func TestAddClicks(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	l := mustLink(t, "https://example.com/clicks", tenantID)
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := store.AddClicks(ctx, map[int64]int64{
		l.ID:           3,
		l.ID + 999_999: 1, // 不存在的 id 应被忽略
	})
	if err != nil {
		t.Fatalf("AddClicks: %v", err)
	}

	var clicks int64
	if err := store.db.QueryRowContext(ctx, "SELECT clicks FROM "+linksTable+" WHERE id = $1", l.ID).Scan(&clicks); err != nil {
		t.Fatalf("select clicks: %v", err)
	}
	if clicks != 3 {
		t.Errorf("clicks = %d, want 3", clicks)
	}

	// 再加一次。若 SQL 写成 SET clicks = data.n，这里会停在 4，覆盖而不是累加。
	if err := store.AddClicks(ctx, map[int64]int64{l.ID: 4}); err != nil {
		t.Fatalf("AddClicks again: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT clicks FROM "+linksTable+" WHERE id = $1", l.ID).Scan(&clicks); err != nil {
		t.Fatalf("select clicks: %v", err)
	}
	if clicks != 7 {
		t.Errorf("clicks = %d, want 7", clicks)
	}

	if err := store.AddClicks(ctx, nil); err != nil {
		t.Errorf("AddClicks(nil): %v", err)
	}
}

/*
TestTenantReferenceShape 锁住 ADR 0010 的表结构。
外键删除后，不存在的 tenant_id 也能写入；NOT NULL 和 (tenant_id, id) 索引还在。
*/
func TestTenantReferenceShape(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, `
SELECT count(*)
FROM pg_constraint
WHERE contype = 'f'
  AND conrelid IN ('links'::regclass, 'api_keys'::regclass)`).Scan(&foreignKeys); err != nil {
		t.Fatalf("count foreign keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Errorf("foreign keys = %d, want 0", foreignKeys)
	}

	for _, table := range []string{"links", "api_keys"} {
		var notNull bool
		err := store.db.QueryRowContext(ctx, `
SELECT attnotnull
FROM pg_attribute
WHERE attrelid = $1::regclass AND attname = 'tenant_id' AND NOT attisdropped`, table).Scan(&notNull)
		if err != nil {
			t.Fatalf("tenant_id nullability on %s: %v", table, err)
		}
		if !notNull {
			t.Errorf("%s.tenant_id allows NULL", table)
		}
	}

	for _, index := range []string{"idx_links_tenant_id", "idx_api_keys_tenant_id"} {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, index).Scan(&exists); err != nil {
			t.Fatalf("index %s: %v", index, err)
		}
		if !exists {
			t.Errorf("missing index %s", index)
		}
	}

	orphan := mustLink(t, "https://example.com/no-tenant", tenantID)
	orphan.TenantID = tenantID + 1_000_000
	if err := store.Create(ctx, orphan); err != nil {
		t.Fatalf("Create with missing tenant: %v", err)
	}
}
