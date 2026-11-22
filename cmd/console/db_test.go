package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/postgres"
	"shortlink/internal/tenant"
)

/*
TestConsoleKeyCommandsUseDatabase 用真实的 openStore 走签发、列表和吊销。
命令行单测注入的是假存储，库测试直接调 postgres，两边都不经过这层解析和打印。
*/
func TestConsoleKeyCommandsUseDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	externalID := "seam-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var stdout, stderr bytes.Buffer
	err := run([]string{"key", "issue", "--name", "seam", "--external-id", externalID}, &stdout, &stderr, openStore, failLinkOpen(t))
	if err != nil {
		t.Fatalf("issue: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("issue stderr = %q", stderr.String())
	}
	tenantID := parseID(t, stdout.String(), "tenant_id")
	keyID := parseID(t, stdout.String(), "key_id")
	if gotScopes := parseField(t, stdout.String(), "scopes"); gotScopes != "links:read,links:write,stats:read" {
		t.Fatalf("default scopes = %q, want links:read,links:write,stats:read", gotScopes)
	}
	plaintext := parseField(t, stdout.String(), "api_key")
	if !tenant.ValidToken(plaintext) {
		t.Fatalf("api_key is not a valid token")
	}

	var gotHash []byte
	if err := db.QueryRowContext(ctx, `SELECT key_hash FROM api_keys WHERE id = $1`, keyID).Scan(&gotHash); err != nil {
		t.Fatalf("select key hash: %v", err)
	}
	if !bytes.Equal(gotHash, tenant.HashAPIKey(plaintext)) {
		t.Fatal("stored hash does not match the printed api_key")
	}

	stdout.Reset()
	stderr.Reset()
	err = run([]string{"key", "issue", "--tenant", strconv.FormatInt(tenantID, 10), "--scope", "stats:read", "--scope", "links:read"},
		&stdout, &stderr, openStore, failLinkOpen(t))
	if err != nil {
		t.Fatalf("issue scoped key: %v\nstderr: %s", err, stderr.String())
	}
	scopedKeyID := parseID(t, stdout.String(), "key_id")
	if gotScopes := parseField(t, stdout.String(), "scopes"); gotScopes != "links:read,stats:read" {
		t.Fatalf("scoped key scopes = %q, want links:read,stats:read", gotScopes)
	}
	scopedPlaintext := parseField(t, stdout.String(), "api_key")
	gotPrincipal, err := postgres.NewStore(db).Authenticate(ctx, scopedPlaintext)
	if err != nil {
		t.Fatalf("Authenticate scoped key: %v", err)
	}
	wantScopes := []identity.Perm{identity.PermLinksRead, identity.PermStatsRead}
	if gotPrincipal.Tenant.ID != tenantID || gotPrincipal.ActorID != scopedKeyID || !slices.Equal(gotPrincipal.Scopes, wantScopes) {
		t.Fatalf("Authenticate scoped = %+v, want tenant %d key %d scopes %v", gotPrincipal, tenantID, scopedKeyID, wantScopes)
	}

	stdout.Reset()
	stderr.Reset()
	err = run([]string{"key", "list", "--tenant", strconv.FormatInt(tenantID, 10)}, &stdout, &stderr, openStore, failLinkOpen(t))
	if err != nil {
		t.Fatalf("list: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), strconv.FormatInt(keyID, 10)+"\t") ||
		!strings.Contains(stdout.String(), "\tlinks:read,links:write,stats:read\n") ||
		!strings.Contains(stdout.String(), strconv.FormatInt(scopedKeyID, 10)+"\t") ||
		!strings.Contains(stdout.String(), "\tlinks:read,stats:read\n") ||
		strings.Contains(stdout.String(), plaintext) ||
		strings.Contains(stdout.String(), scopedPlaintext) {
		t.Fatalf("list stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	err = run([]string{"key", "revoke", "--id", strconv.FormatInt(keyID, 10)}, &stdout, &stderr, openStore, failLinkOpen(t))
	if err != nil {
		t.Fatalf("revoke: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.String() != "revoked: "+strconv.FormatInt(keyID, 10)+"\n" {
		t.Fatalf("revoke stdout = %q", stdout.String())
	}
	if _, err := postgres.NewStore(db).Authenticate(ctx, plaintext); !errors.Is(err, tenant.ErrUnauthenticated) {
		t.Fatalf("Authenticate after revoke error = %v, want ErrUnauthenticated", err)
	}
}

/*
TestConsoleMemberInviteUsesDatabase 用真实 openStore 跑 console member invite，
并验证通过 OIDC 登录仅在 email_verified=true 时兑现邀请写入组织成员关系。
*/
func TestConsoleMemberInviteUsesDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	var tenantID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('invite-seam') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	email := "invite-" + suffix + "@example.com"
	mixedEmail := "  Invite-" + suffix + "@Example.COM "

	var stdout, stderr bytes.Buffer
	err := run([]string{"member", "invite", "--tenant", strconv.FormatInt(tenantID, 10), "--email", mixedEmail, "--role", "admin"},
		&stdout, &stderr, openStore, failLinkOpen(t))
	if err != nil {
		t.Fatalf("member invite: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("member invite stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "id\ttenant_id\temail\trole\tcreated_at\texpires_at\n") ||
		!strings.Contains(stdout.String(), "\t"+strconv.FormatInt(tenantID, 10)+"\t"+email+"\tadmin\t") {
		t.Fatalf("member invite stdout = %q", stdout.String())
	}

	var storedEmail, storedRole string
	var invitedBy *int64
	if err := db.QueryRowContext(ctx,
		`SELECT email, role, invited_by FROM invitations WHERE tenant_id = $1 AND email = $2`,
		tenantID, email,
	).Scan(&storedEmail, &storedRole, &invitedBy); err != nil {
		t.Fatalf("select invitation: %v", err)
	}
	if storedEmail != email || storedRole != "admin" || invitedBy != nil {
		t.Fatalf("stored invitation = (%q, %q, %v), want (%q, admin, nil)", storedEmail, storedRole, invitedBy, email)
	}

	// 同租户重复邀请同邮箱返回 ErrInvitationPending；不存在租户返回 ErrTenantNotFound。
	stdout.Reset()
	err = run([]string{"member", "invite", "--tenant", strconv.FormatInt(tenantID, 10), "--email", email, "--role", "viewer"},
		&stdout, &stderr, openStore, failLinkOpen(t))
	if !errors.Is(err, postgres.ErrInvitationPending) {
		t.Fatalf("duplicate invite err = %v, want ErrInvitationPending", err)
	}
	err = run([]string{"member", "invite", "--tenant", "999999999", "--email", email, "--role", "viewer"},
		&stdout, &stderr, openStore, failLinkOpen(t))
	if !errors.Is(err, postgres.ErrTenantNotFound) {
		t.Fatalf("missing tenant invite err = %v, want ErrTenantNotFound", err)
	}

	store := postgres.NewStore(db)
	sub := "sub-invite-" + suffix
	unverified, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       sub,
		Email:         email,
		EmailVerified: false,
	}, time.Hour)
	if err != nil {
		t.Fatalf("unverified CreateSessionForOIDC: %v", err)
	}
	info, err := store.GetSessionInfo(ctx, unverified.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo unverified: %v", err)
	}
	if len(info.Orgs) != 0 {
		t.Fatalf("unverified orgs = %+v, want empty", info.Orgs)
	}

	verified, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       sub,
		Email:         email,
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("verified CreateSessionForOIDC: %v", err)
	}
	info, err = store.GetSessionInfo(ctx, verified.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo verified: %v", err)
	}
	if len(info.Orgs) != 1 || info.Orgs[0].ID != tenantID || info.Orgs[0].Role != identity.RoleAdmin {
		t.Fatalf("verified orgs = %+v, want admin in tenant %d", info.Orgs, tenantID)
	}
}

func openTestDB(t *testing.T) *sql.DB {
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
		t.Fatalf("refusing to use non-test database %q: name must end with _test", dbName)
	}

	ctx := context.Background()
	db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	release, err := postgres.HoldTestDBLock(ctx, db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(func() {
		release()
		_ = db.Close()
	})
	// openStore 读的是 DATABASE_URL。指到测试库，避免命令写进开发库。
	t.Setenv("DATABASE_URL", dsn)
	return db
}

func parseID(t *testing.T, text, name string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(parseField(t, text, name), 10, 64)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return n
}

func parseField(t *testing.T, text, name string) string {
	t.Helper()
	prefix := name + ": "
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("stdout missing %s: %q", name, text)
	return ""
}
