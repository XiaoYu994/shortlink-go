package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

func testAuditCtx(ctx context.Context) context.Context {
	return audit.WithMeta(ctx, audit.SystemMeta("test"))
}

func allBuiltinTestTxHooks() []StoreOption {
	return []StoreOption{
		WithTxHooks(
			RiskScanHook(),
			WebhookFanoutHook(),
			InvitationEmailHook(),
			CacheReconcileHook(),
			StatsPurgeHook(),
		),
	}
}

/*
newTestStore 每次测试前清空测试库里的租户、密钥和链接，再插入一个租户。
默认挂载全部内建事务钩子，确保全功能回归测试覆盖完整副作用；
验证“关闭某插件不产生副作用”的测试走 newTestStoreWithOptions。
没有设置 TEST_DATABASE_URL 时跳过；库名不以 _test 结尾时失败，避免误清开发库。
*/
func newTestStore(t testing.TB) (*Store, int64) {
	t.Helper()
	return newTestStoreWithOptions(t, allBuiltinTestTxHooks()...)
}

func newTestStoreWithOptions(t testing.TB, opts ...StoreOption) (*Store, int64) {
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

	ctx := testAuditCtx(context.Background())
	db, err := Open(ctx, dsn, DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	/*
		先占住测试库的锁，再清空。cmd/console 的集成测试用同一把锁，
		两个包并行跑时不会互相把刚写入的行清掉。
		Cleanup 后进先出，所以解锁发生在关闭连接池之前。
	*/
	release, err := HoldTestDBLock(ctx, db)
	if err != nil {
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(release)

	/*
		没有外键之后，TRUNCATE tenants CASCADE 不会再带上子表和关联表。
		所有表写在同一条语句里，有没有外键都能清空。
	*/
	if _, err := db.ExecContext(ctx, `TRUNCATE links, api_keys, tenants,
    link_clicks_daily, link_dimension_daily, link_visitors_daily,
    users, user_identities, memberships, sessions, invitations, jobs, audit_logs,
    webhooks, webhook_deliveries RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	var tenantID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return NewStore(db, opts...), tenantID
}

func mustLink(t testing.TB, rawURL string, tenantID int64) *link.Link {
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
	ctx := testAuditCtx(context.Background())

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
	if l.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", l.CreatedAt.Location())
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
	ctx := testAuditCtx(context.Background())

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
	ctx := testAuditCtx(context.Background())

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

func TestStoreCreateCollisionExhaustedRollsBackSideEffects(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	store.generate = func() (string, error) { return "samecode", nil }
	first := mustLink(t, "https://example.com/first", tenantID)
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second := mustLink(t, "https://example.com/second", tenantID)
	err := store.Create(ctx, second)
	if !errors.Is(err, errCreateCodeCollision) {
		t.Fatalf("second Create error = %v, want errCreateCodeCollision", err)
	}

	var usage, jobsCount, auditCount int
	if err := store.db.QueryRowContext(ctx, `SELECT link_usage FROM tenants WHERE id = $1`, tenantID).Scan(&usage); err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE tenant_id = $1`, tenantID).Scan(&jobsCount); err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
		t.Fatalf("query audit_logs: %v", err)
	}
	if usage != 1 || jobsCount != 1 || auditCount != 1 {
		t.Errorf("side effects after failed collisions: usage=%d jobs=%d audit=%d, want 1/1/1", usage, jobsCount, auditCount)
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
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
	ctx := testAuditCtx(context.Background())

	plaintext, hash, err := identity.APIKeyToken.Generate()
	if err != nil {
		t.Fatalf("APIKeyToken.Generate: %v", err)
	}
	var keyID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, $2, $3::text[]) RETURNING id`,
		tenantID, hash, permsToStrings(identity.AllAPIKeyScopes())).Scan(&keyID); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Tenant.ID != tenantID || got.ActorKind != identity.ActorAPIKey || got.ActorID != keyID {
		t.Errorf("Authenticate = %+v, want tenant %d api_key id %d", got, tenantID, keyID)
	}
	if !slices.Equal(got.Scopes, identity.AllAPIKeyScopes()) {
		t.Errorf("Authenticate scopes = %v, want %v", got.Scopes, identity.AllAPIKeyScopes())
	}

	other, _, err := identity.APIKeyToken.Generate()
	if err != nil {
		t.Fatalf("APIKeyToken.Generate: %v", err)
	}
	if _, err := store.Authenticate(ctx, other); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("unknown key error = %v, want ErrUnauthenticated", err)
	}
	if _, err := store.Authenticate(ctx, "not-a-key"); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("malformed key error = %v, want ErrUnauthenticated", err)
	}
}

/*
TestTenantReferenceShape 锁住 ADR 0010 的表结构。
外键删除后，不存在的 tenant_id 也能写入；NOT NULL 和 (tenant_id, id) 索引还在。
*/
func TestTenantReferenceShape(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

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

	// 数据库本身没有外键：直接 SQL 写入孤儿 tenant_id 仍能落库（ADR 0010）；
	// 但应用层 Store.Create 必须拦截不存在的租户并返回 ErrTenantNotFound，不产生孤儿链接或任务（ADR 0022）。
	missingTenant := tenantID + 1_000_000
	orphan := mustLink(t, "https://example.com/no-tenant", missingTenant)
	if err := store.Create(ctx, orphan); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Create with missing tenant err = %v, want ErrTenantNotFound", err)
	}
	zeroTenant := mustLink(t, "https://example.com/zero-tenant", 0)
	if err := store.Create(ctx, zeroTenant); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Create with zero tenant err = %v, want ErrTenantNotFound", err)
	}
	var orphanJobs int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`).Scan(&orphanJobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if orphanJobs != 0 {
		t.Errorf("orphan jobs = %d, want 0", orphanJobs)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO links (code, url, tenant_id) VALUES ('orph0001', 'https://example.com/raw-orphan', $1)`,
		missingTenant); err != nil {
		t.Fatalf("raw SQL insert orphan link without FK: %v", err)
	}
}
