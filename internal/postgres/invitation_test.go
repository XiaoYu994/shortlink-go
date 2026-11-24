package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func TestCreateInvitation(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	inv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID:  tenant1,
		Email:     "  Alice@Example.COM ",
		Role:      identity.RoleOwner,
		ExpiresAt: &future,
	})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if inv.ID <= 0 || inv.TenantID != tenant1 || inv.Email != "alice@example.com" || inv.Role != identity.RoleOwner {
		t.Fatalf("inv = %+v", inv)
	}
	if inv.InvitedBy != nil || inv.CreatedAt.IsZero() || inv.ExpiresAt == nil || !inv.ExpiresAt.Equal(future) {
		t.Fatalf("inv metadata = %+v", inv)
	}

	// 同租户同邮箱还有待兑现邀请时再次创建返回 ErrInvitationPending。
	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "alice@example.com",
		Role:     identity.RoleMember,
	}); !errors.Is(err, ErrInvitationPending) {
		t.Fatalf("duplicate pending err = %v, want ErrInvitationPending", err)
	}

	// 撤销后可以再次邀请。
	if _, err := store.db.ExecContext(ctx, `UPDATE invitations SET revoked_at = now() WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("revoke invitation: %v", err)
	}
	second, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "alice@example.com",
		Role:     identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("re-invite after revoke: %v", err)
	}
	if second.ID == inv.ID || second.Role != identity.RoleAdmin {
		t.Fatalf("second = %+v", second)
	}

	// 模拟已过期但尚未关闭的历史待兑现邀请，再次邀请应将其标记为 superseded_at，保留 expired 语义而不覆盖 revoked_at（ADR 0022）。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 minute' WHERE id = $1`,
		second.ID,
	); err != nil {
		t.Fatalf("expire second invitation: %v", err)
	}
	inviter := int64(99)
	third, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID:  tenant1,
		Email:     "alice@example.com",
		Role:      identity.RoleViewer,
		InvitedBy: &inviter,
	})
	if err != nil {
		t.Fatalf("re-invite after expiry: %v", err)
	}
	if third.InvitedBy == nil || *third.InvitedBy != 99 || third.Role != identity.RoleViewer {
		t.Fatalf("third = %+v", third)
	}

	var staleRevoked, staleSuperseded sql.NullTime
	if err := store.db.QueryRowContext(ctx,
		`SELECT revoked_at, superseded_at FROM invitations WHERE id = $1`, second.ID,
	).Scan(&staleRevoked, &staleSuperseded); err != nil {
		t.Fatalf("check stale superseded_at: %v", err)
	}
	if !staleSuperseded.Valid {
		t.Error("expected expired invitation to be marked superseded_at on re-invite")
	}
	if staleRevoked.Valid {
		t.Error("expired invitation should keep revoked_at NULL so history remains expired rather than revoked")
	}

	listed, err := store.ListInvitations(ctx, tenant1)
	if err != nil {
		t.Fatalf("ListInvitations: %v", err)
	}
	for _, item := range listed {
		if item.ID == second.ID && item.Status(time.Now().UTC()) != "expired" {
			t.Errorf("superseded expired invitation status = %q, want expired", item.Status(time.Now().UTC()))
		}
	}
	if err := store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
		TenantID: tenant1, InvitationID: second.ID, ActorRole: identity.RoleOwner,
	}); !errors.Is(err, identity.ErrInvitationNotPending) {
		t.Errorf("revoke superseded invitation err = %v, want ErrInvitationNotPending", err)
	}
}

func TestCreateInvitationValidation(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := testAuditCtx(context.Background())
	past := time.Now().UTC().Add(-time.Minute)
	badInviter := int64(0)

	tests := []struct {
		name    string
		params  CreateInvitationParams
		wantErr error
	}{
		{
			name:    "nonexistent tenant",
			params:  CreateInvitationParams{TenantID: 999999, Email: "a@example.com", Role: identity.RoleMember},
			wantErr: ErrTenantNotFound,
		},
		{
			name:    "zero tenant",
			params:  CreateInvitationParams{TenantID: 0, Email: "a@example.com", Role: identity.RoleMember},
			wantErr: ErrTenantNotFound,
		},
		{
			name:    "invalid email",
			params:  CreateInvitationParams{TenantID: tenant1, Email: "bad-email", Role: identity.RoleMember},
			wantErr: identity.ErrInvalidEmail,
		},
		{
			name:    "invalid role",
			params:  CreateInvitationParams{TenantID: tenant1, Email: "a@example.com", Role: identity.Role("guest")},
			wantErr: identity.ErrInvalidRole,
		},
		{
			name:    "past expires_at",
			params:  CreateInvitationParams{TenantID: tenant1, Email: "a@example.com", Role: identity.RoleMember, ExpiresAt: &past},
			wantErr: ErrInvitationExpiresNotFuture,
		},
		{
			name:   "non-positive invited_by",
			params: CreateInvitationParams{TenantID: tenant1, Email: "a@example.com", Role: identity.RoleMember, InvitedBy: &badInviter},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.CreateInvitation(ctx, tt.params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateSessionForIdentityRedeemsInvitations(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	var tenant2, tenant3, tenant4 int64
	for _, pair := range []struct {
		name string
		dst  *int64
	}{
		{name: "org-two", dst: &tenant2},
		{name: "org-expired", dst: &tenant3},
		{name: "org-revoked", dst: &tenant4},
	} {
		if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, pair.name).Scan(pair.dst); err != nil {
			t.Fatalf("insert %s: %v", pair.name, err)
		}
	}

	inv1, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "Alice@Example.com",
		Role:     identity.RoleOwner,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant1: %v", err)
	}
	inv2, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant2,
		Email:    "alice@example.com",
		Role:     identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant2: %v", err)
	}
	// 已过期与已撤销邀请不能被兑现。
	invExp, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant3,
		Email:    "alice@example.com",
		Role:     identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant3: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`,
		invExp.ID,
	); err != nil {
		t.Fatalf("expire invExp: %v", err)
	}
	invRev, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant4,
		Email:    "alice@example.com",
		Role:     identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant4: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE invitations SET revoked_at = now() WHERE id = $1`, invRev.ID); err != nil {
		t.Fatalf("revoke invRev: %v", err)
	}

	// 1. email_verified=false 时绝不兑现邀请（即使以后账号还留在 users 里）。
	unverified, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         "alice@example.com",
		EmailVerified: false,
		Name:          "Alice",
	}, time.Hour)
	if err != nil {
		t.Fatalf("unverified login: %v", err)
	}
	info, err := store.GetSessionInfo(ctx, unverified.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo unverified: %v", err)
	}
	if len(info.Orgs) != 0 {
		t.Fatalf("orgs after unverified login = %+v, want empty", info.Orgs)
	}

	// 2. 同一用户之后用 email_verified=true 再次登录：兑现全部有效待兑现邀请，忽略已过期和已撤销。
	verified, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         "ALICE@EXAMPLE.COM",
		EmailVerified: true,
		Name:          "Alice",
	}, time.Hour)
	if err != nil {
		t.Fatalf("verified login: %v", err)
	}
	if verified.User.ID != unverified.User.ID {
		t.Fatalf("user id changed from %d to %d", unverified.User.ID, verified.User.ID)
	}
	info, err = store.GetSessionInfo(ctx, verified.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo verified: %v", err)
	}
	wantOrgs := []identity.UserOrg{
		{ID: tenant1, Name: "test", Role: identity.RoleOwner, LinkUsage: 0, LinkQuota: 1000},
		{ID: tenant2, Name: "org-two", Role: identity.RoleMember, LinkUsage: 0, LinkQuota: 1000},
	}
	if !slices.Equal(info.Orgs, wantOrgs) {
		t.Fatalf("orgs = %+v, want %+v", info.Orgs, wantOrgs)
	}

	// 检查 invitations 行的 accepted_at / accepted_by 状态。
	for _, id := range []int64{inv1.ID, inv2.ID} {
		var acceptedAt sql.NullTime
		var acceptedBy sql.NullInt64
		if err := store.db.QueryRowContext(ctx, `SELECT accepted_at, accepted_by FROM invitations WHERE id = $1`, id).Scan(&acceptedAt, &acceptedBy); err != nil {
			t.Fatalf("select inv %d: %v", id, err)
		}
		if !acceptedAt.Valid || !acceptedBy.Valid || acceptedBy.Int64 != verified.User.ID {
			t.Errorf("inv %d accepted = (%v, %v), want valid user %d", id, acceptedAt, acceptedBy, verified.User.ID)
		}
	}
	for _, id := range []int64{invExp.ID, invRev.ID} {
		var acceptedAt sql.NullTime
		if err := store.db.QueryRowContext(ctx, `SELECT accepted_at FROM invitations WHERE id = $1`, id).Scan(&acceptedAt); err != nil {
			t.Fatalf("select unredeemed inv %d: %v", id, err)
		}
		if acceptedAt.Valid {
			t.Errorf("inv %d unexpectedly redeemed", id)
		}
	}

	// 3. 另一个不同 (issuer, subject) 用相同已验证邮箱登录：已兑现邀请不能被重复领取。
	other, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "https://other-idp.example.com",
		Subject:       "sub-alice-other",
		Email:         "alice@example.com",
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("other provider login: %v", err)
	}
	otherInfo, err := store.GetSessionInfo(ctx, other.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo other: %v", err)
	}
	if len(otherInfo.Orgs) != 0 {
		t.Fatalf("other provider orgs = %+v, want empty", otherInfo.Orgs)
	}

	// 4. 已经是 tenant1 owner 时再发同邮箱 viewer 邀请并登录：保留原 owner 角色，并把新邀请标成已兑现。
	invDup, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "alice@example.com",
		Role:     identity.RoleViewer,
	})
	if err != nil {
		t.Fatalf("CreateInvitation duplicate org membership: %v", err)
	}
	again, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "sub-alice",
		Email:         "alice@example.com",
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("again login: %v", err)
	}
	againInfo, err := store.GetSessionInfo(ctx, again.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo again: %v", err)
	}
	if !slices.Equal(againInfo.Orgs, wantOrgs) {
		t.Fatalf("orgs after duplicate invitation = %+v, want unchanged %+v", againInfo.Orgs, wantOrgs)
	}
	var dupAccepted sql.NullTime
	if err := store.db.QueryRowContext(ctx, `SELECT accepted_at FROM invitations WHERE id = $1`, invDup.ID).Scan(&dupAccepted); err != nil {
		t.Fatalf("select invDup: %v", err)
	}
	if !dupAccepted.Valid {
		t.Error("expected invDup to be marked accepted even when membership already existed")
	}
}

func TestCreateSessionForIdentityRedeemConcurrent(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	inv, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "race-invite@example.com",
		Role:     identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
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
			results[idx], errs[idx] = store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-race-invite",
				Email:         "race-invite@example.com",
				EmailVerified: true,
			}, time.Hour)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d err = %v", i, err)
		}
	}
	info, err := store.GetSessionInfo(ctx, results[0].Token)
	if err != nil {
		t.Fatalf("GetSessionInfo: %v", err)
	}
	if len(info.Orgs) != 1 || info.Orgs[0].ID != tenant1 || info.Orgs[0].Role != identity.RoleAdmin {
		t.Fatalf("orgs = %+v, want single admin membership in %d", info.Orgs, tenant1)
	}
	var acceptedBy int64
	if err := store.db.QueryRowContext(ctx, `SELECT accepted_by FROM invitations WHERE id = $1`, inv.ID).Scan(&acceptedBy); err != nil {
		t.Fatalf("select accepted_by: %v", err)
	}
	if acceptedBy != results[0].User.ID {
		t.Errorf("accepted_by = %d, want %d", acceptedBy, results[0].User.ID)
	}
}
