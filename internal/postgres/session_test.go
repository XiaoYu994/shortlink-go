package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func TestCreateSessionForOIDCFirstAndRepeatLogin(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	first, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         " Alice@Example.COM ",
		EmailVerified: true,
		Name:          " Alice ",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("first CreateSessionForOIDC: %v", err)
	}
	if !identity.ValidSessionToken(first.Token) {
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
	if !bytes.Equal(storedHash, identity.HashSessionToken(first.Token)) {
		t.Fatal("stored token_hash does not match SHA-256 of plaintext token")
	}

	// 第二次登录同一个 (issuer, subject)：复用同一用户，同步最新资料，签发全新会话令牌。
	second, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         "alice-new@example.com",
		EmailVerified: false,
		Name:          "Alice Updated",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("second CreateSessionForOIDC: %v", err)
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
	otherProvider, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:        "https://other-idp.example.com",
		Subject:       "sub-alice",
		Email:         "alice-new@example.com",
		EmailVerified: true,
		Name:          "Alice Other",
	}, 24*time.Hour)
	if err != nil {
		t.Fatalf("otherProvider CreateSessionForOIDC: %v", err)
	}
	if otherProvider.User.ID == first.User.ID {
		t.Fatalf("different issuer with same email was merged into user %d", first.User.ID)
	}
}

func TestCreateSessionForOIDCConcurrentFirstLogin(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	id := identity.OIDCIdentity{
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
			results[idx], errs[idx] = store.CreateSessionForOIDC(ctx, id, time.Hour)
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

func TestCreateSessionForOIDCValidation(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "",
	}, time.Hour); !errors.Is(err, identity.ErrInvalidOIDCIdentity) {
		t.Fatalf("empty subject err = %v, want ErrInvalidOIDCIdentity", err)
	}

	if _, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
		Issuer:  "http://127.0.0.1:5556/dex",
		Subject: "sub-1",
	}, 0); !errors.Is(err, identity.ErrInvalidSessionTTL) {
		t.Fatalf("zero ttl err = %v, want ErrInvalidSessionTTL", err)
	}
}

func TestCreateSessionForOIDCKeepsProfileWhenClaimsAreAbsent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	id := identity.OIDCIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-keep",
		Email:         "keep@example.com",
		EmailVerified: true,
		Name:          "Keep",
	}
	first, err := store.CreateSessionForOIDC(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	omitted, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
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

	sameEmail, err := store.CreateSessionForOIDC(ctx, identity.OIDCIdentity{
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
