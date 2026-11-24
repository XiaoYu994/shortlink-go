package postgres

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

func TestCreateSessionForIdentityFirstAndRepeatLogin(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	first, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         " Alice@Example.COM ",
		EmailVerified: true,
		Name:          " Alice ",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("first CreateSessionForIdentity: %v", err)
	}
	if !identity.SessionToken.Valid(first.Token) {
		t.Fatalf("first token %q is not a valid session token", first.Token)
	}
	if first.User.ID <= 0 || first.User.Email != "alice@example.com" || first.User.Name != "Alice" {
		t.Fatalf("first user = %+v, want normalized alice", first.User)
	}
	if first.Session.ID <= 0 || first.Session.UserID != first.User.ID || !first.Session.ExpiresAt.After(first.Session.CreatedAt) {
		t.Fatalf("first session = %+v", first.Session)
	}
	if first.CookieMaxAge < 24*time.Hour-time.Second || first.CookieMaxAge > 24*time.Hour+time.Second {
		t.Fatalf("CookieMaxAge = %v, want about 24h", first.CookieMaxAge)
	}

	var storedHash []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT token_hash FROM sessions WHERE id = $1`, first.Session.ID,
	).Scan(&storedHash); err != nil {
		t.Fatalf("select session hash: %v", err)
	}
	if !bytes.Equal(storedHash, identity.HashToken(first.Token)) {
		t.Fatal("stored token_hash does not match SHA-256 of plaintext token")
	}

	// 第二次登录同一个 (issuer, subject)：复用同一用户，同步最新资料，签发全新会话令牌。
	second, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         "alice-new@example.com",
		EmailVerified: false,
		Name:          "Alice Updated",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("second CreateSessionForIdentity: %v", err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("second user id = %d, want %d", second.User.ID, first.User.ID)
	}
	if second.User.Email != "alice-new@example.com" || second.User.Name != "Alice Updated" {
		t.Errorf("second user = %+v, want synced profile", second.User)
	}
	if second.Session.ID == first.Session.ID || second.Token == first.Token {
		t.Fatalf("repeat login reused session or token: first=%+v second=%+v", first.Session, second.Session)
	}

	var identityEmail string
	var identityVerified bool
	if err := store.db.QueryRowContext(ctx,
		`SELECT email, email_verified FROM user_identities WHERE user_id = $1`, first.User.ID,
	).Scan(&identityEmail, &identityVerified); err != nil {
		t.Fatalf("select user_identities: %v", err)
	}
	if identityEmail != "alice-new@example.com" || identityVerified {
		t.Errorf("identity = (%q, %v), want (%q, false)", identityEmail, identityVerified, "alice-new@example.com")
	}

	// 不同提供方即使邮箱相同，也不能按邮箱自动合并成同一个用户（ADR 0018）。
	otherProvider, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "https://other-idp.example.com",
		Subject:       "sub-alice",
		Email:         "alice-new@example.com",
		EmailVerified: true,
		Name:          "Alice Other",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("otherProvider CreateSessionForIdentity: %v", err)
	}
	if otherProvider.User.ID == first.User.ID {
		t.Fatalf("different issuer with same email was merged into user %d", first.User.ID)
	}
}

func TestCreateSessionForIdentityConcurrentFirstLogin(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	id := identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-race",
		Email:         "race@example.com",
		EmailVerified: true,
		Name:          "Race User",
	}

	const workers = 4
	start := make(chan struct{})
	results := make([]identity.IssuedSession, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = store.CreateSessionForIdentity(ctx, id, time.Hour)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d err = %v", i, err)
		}
	}
	wantUserID := results[0].User.ID
	tokens := make(map[string]bool, workers)
	for i, res := range results {
		if res.User.ID != wantUserID {
			t.Errorf("worker %d user id = %d, want %d", i, res.User.ID, wantUserID)
		}
		tokens[res.Token] = true
	}
	if len(tokens) != workers {
		t.Errorf("distinct tokens = %d, want %d", len(tokens), workers)
	}

	var usersCount, identitiesCount, sessionsCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&usersCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM user_identities`).Scan(&identitiesCount); err != nil {
		t.Fatalf("count user_identities: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionsCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if usersCount != 1 || identitiesCount != 1 || sessionsCount != workers {
		t.Fatalf("counts (users=%d, identities=%d, sessions=%d), want (1, 1, %d)",
			usersCount, identitiesCount, sessionsCount, workers)
	}
}

func TestCreateSessionForIdentityValidation(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	if _, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "",
	}, time.Hour); !errors.Is(err, identity.ErrInvalidExternalIdentity) {
		t.Fatalf("empty subject err = %v, want ErrInvalidExternalIdentity", err)
	}

	if _, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "sub-1",
	}, 0); !errors.Is(err, identity.ErrInvalidSessionTTL) {
		t.Fatalf("zero ttl err = %v, want ErrInvalidSessionTTL", err)
	}
}

func TestCreateSessionForIdentityKeepsProfileWhenClaimsAreAbsent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	id := identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-keep",
		Email:         "keep@example.com",
		EmailVerified: true,
		Name:          "Keep",
	}
	first, err := store.CreateSessionForIdentity(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	omitted, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:  id.Issuer,
		Subject: id.Subject,
	}, time.Hour)
	if err != nil {
		t.Fatalf("omitted claims: %v", err)
	}
	if omitted.User.ID != first.User.ID || omitted.User.Email != "keep@example.com" || omitted.User.Name != "Keep" {
		t.Fatalf("omitted claims user = %+v, want profile kept", omitted.User)
	}
	var email string
	var verified bool
	if err := store.db.QueryRowContext(ctx, `SELECT email, email_verified FROM user_identities WHERE user_id = $1`, first.User.ID).Scan(&email, &verified); err != nil {
		t.Fatalf("select identity: %v", err)
	}
	if email != "keep@example.com" || !verified {
		t.Fatalf("identity after omitted claims = (%q, %v), want verified keep@example.com", email, verified)
	}

	sameEmail, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        id.Issuer,
		Subject:       id.Subject,
		Email:         "keep@example.com",
		EmailVerified: false,
	}, time.Hour)
	if err != nil {
		t.Fatalf("same email: %v", err)
	}
	if sameEmail.User.Email != "keep@example.com" || sameEmail.User.Name != "Keep" {
		t.Fatalf("same email user = %+v, want name kept", sameEmail.User)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT email, email_verified FROM user_identities WHERE user_id = $1`, first.User.ID).Scan(&email, &verified); err != nil {
		t.Fatalf("select identity after same email: %v", err)
	}
	if email != "keep@example.com" || !verified {
		t.Fatalf("identity after unverified repeat = (%q, %v), want still verified", email, verified)
	}
}

func TestAuthenticateSessionForOrg(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	if _, err := store.db.ExecContext(ctx,
		`UPDATE tenants SET external_id = 'acme-ext', rate_limit_per_second = 25 WHERE id = $1`,
		tenantID,
	); err != nil {
		t.Fatalf("update tenant: %v", err)
	}
	var otherTenantID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name) VALUES ('other-org') RETURNING id`,
	).Scan(&otherTenantID); err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}

	issued, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-auth",
		Email:         "auth@example.com",
		EmailVerified: true,
		Name:          "Auth User",
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity: %v", err)
	}

	// 尚未加入任何组织，访问任意组织都应返回 ErrOrgNotFound。
	if _, err := store.AuthenticateSessionForOrg(ctx, issued.Token, tenantID); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Fatalf("non-member err = %v, want ErrOrgNotFound", err)
	}

	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'viewer')`,
		issued.User.ID, tenantID,
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	p, err := store.AuthenticateSessionForOrg(ctx, issued.Token, tenantID)
	if err != nil {
		t.Fatalf("AuthenticateSessionForOrg: %v", err)
	}
	want := identity.Principal{
		Tenant:    tenant.Tenant{ID: tenantID, ExternalID: "acme-ext", RateLimit: 25},
		ActorKind: identity.ActorUser,
		ActorID:   issued.User.ID,
		Role:      identity.RoleViewer,
	}
	if p.Tenant != want.Tenant || p.ActorKind != want.ActorKind || p.ActorID != want.ActorID || p.Role != want.Role || len(p.Scopes) != 0 {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}

	// 非成员组织与不存在的组织统一返回 ErrOrgNotFound。
	if _, err := store.AuthenticateSessionForOrg(ctx, issued.Token, otherTenantID); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Errorf("other org err = %v, want ErrOrgNotFound", err)
	}
	if _, err := store.AuthenticateSessionForOrg(ctx, issued.Token, 999999); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Errorf("nonexistent org err = %v, want ErrOrgNotFound", err)
	}

	// 改角色与改限流在下一次查询立刻生效。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memberships SET role = 'admin' WHERE user_id = $1 AND tenant_id = $2`,
		issued.User.ID, tenantID,
	); err != nil {
		t.Fatalf("update role: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE tenants SET rate_limit_per_second = 77 WHERE id = $1`, tenantID,
	); err != nil {
		t.Fatalf("update rate limit: %v", err)
	}
	p, err = store.AuthenticateSessionForOrg(ctx, issued.Token, tenantID)
	if err != nil {
		t.Fatalf("AuthenticateSessionForOrg after role update: %v", err)
	}
	if p.Role != identity.RoleAdmin || p.Tenant.RateLimit != 77 {
		t.Errorf("updated principal = %+v, want role=admin rateLimit=77", p)
	}

	// 移出组织后立刻返回 ErrOrgNotFound。
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM memberships WHERE user_id = $1 AND tenant_id = $2`,
		issued.User.ID, tenantID,
	); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	if _, err := store.AuthenticateSessionForOrg(ctx, issued.Token, tenantID); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Errorf("after removal err = %v, want ErrOrgNotFound", err)
	}

	// 恢复成员后测试非法、未知、过期和吊销令牌，全部返回 ErrUnauthenticated。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'member')`,
		issued.User.ID, tenantID,
	); err != nil {
		t.Fatalf("re-insert membership: %v", err)
	}
	unknownToken, _, err := identity.SessionToken.Generate()
	if err != nil {
		t.Fatalf("SessionToken.Generate: %v", err)
	}
	for _, badToken := range []string{"", "not-a-token", unknownToken} {
		if _, err := store.AuthenticateSessionForOrg(ctx, badToken, tenantID); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("token %q err = %v, want ErrUnauthenticated", badToken, err)
		}
	}

	expired, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "sub-auth",
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity expired: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`,
		expired.Session.ID,
	); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, err := store.AuthenticateSessionForOrg(ctx, expired.Token, tenantID); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("expired session err = %v, want ErrUnauthenticated", err)
	}

	if err := store.RevokeSession(ctx, issued.Token); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := store.AuthenticateSessionForOrg(ctx, issued.Token, tenantID); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("revoked session err = %v, want ErrUnauthenticated", err)
	}
}

func TestGetSessionInfoAndRevokeSession(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name) VALUES ('acme-two') RETURNING id`,
	).Scan(&tenant2); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}

	sess1, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-info",
		Email:         "info@example.com",
		EmailVerified: true,
		Name:          "Info User",
	}, time.Hour)
	if err != nil {
		t.Fatalf("first CreateSessionForIdentity: %v", err)
	}
	// 同一用户的第二个会话，用于确认吊销其中一个不影响另一个。
	sess2, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "sub-info",
	}, time.Hour)
	if err != nil {
		t.Fatalf("second CreateSessionForIdentity: %v", err)
	}

	// 无组织时 Orgs 为空。
	info, err := store.GetSessionInfo(ctx, sess1.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo without orgs: %v", err)
	}
	if info.User.ID != sess1.User.ID || info.User.Email != "info@example.com" || info.User.Name != "Info User" || !info.ExpiresAt.Equal(sess1.Session.ExpiresAt) {
		t.Errorf("info = %+v, want user/expires matching %+v", info, sess1)
	}
	if len(info.Orgs) != 0 {
		t.Errorf("orgs = %+v, want empty", info.Orgs)
	}

	// 按逆序插入成员关系，确认 GetSessionInfo 按组织 ID 升序返回。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'viewer'), ($1, $3, 'owner')`,
		sess1.User.ID, tenant2, tenant1,
	); err != nil {
		t.Fatalf("insert memberships: %v", err)
	}
	info, err = store.GetSessionInfo(ctx, sess1.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo with orgs: %v", err)
	}
	wantOrgs := []identity.UserOrg{
		{ID: tenant1, Name: "test", Role: identity.RoleOwner, LinkUsage: 0, LinkQuota: 1000},
		{ID: tenant2, Name: "acme-two", Role: identity.RoleViewer, LinkUsage: 0, LinkQuota: 1000},
	}
	if !slices.Equal(info.Orgs, wantOrgs) {
		t.Errorf("orgs = %+v, want %+v", info.Orgs, wantOrgs)
	}

	// 吊销 sess1：重复吊销、非法令牌均幂等；sess1 立刻 401，sess2 仍然有效。
	if err := store.RevokeSession(ctx, sess1.Token); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if err := store.RevokeSession(ctx, sess1.Token); err != nil {
		t.Fatalf("RevokeSession repeat: %v", err)
	}
	if err := store.RevokeSession(ctx, "bad-token"); err != nil {
		t.Fatalf("RevokeSession invalid token: %v", err)
	}
	if _, err := store.GetSessionInfo(ctx, sess1.Token); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("revoked sess1 GetSessionInfo err = %v, want ErrUnauthenticated", err)
	}
	if _, err := store.GetSessionInfo(ctx, sess2.Token); err != nil {
		t.Errorf("sess2 GetSessionInfo after revoking sess1 err = %v, want nil", err)
	}

	// 过期会话返回 ErrUnauthenticated。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`,
		sess2.Session.ID,
	); err != nil {
		t.Fatalf("expire sess2: %v", err)
	}
	if _, err := store.GetSessionInfo(ctx, sess2.Token); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("expired sess2 err = %v, want ErrUnauthenticated", err)
	}
}
