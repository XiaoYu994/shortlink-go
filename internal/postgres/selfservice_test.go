package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func TestCreateOrgForUserAndLimitConcurrency(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	sess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "https://dex.example",
		Subject:       "org-creator",
		Email:         "creator@example.com",
		EmailVerified: true,
		Name:          "Creator",
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity: %v", err)
	}

	created, err := store.CreateOrgForUser(ctx, sess.User.ID, "  Acme Labs  ", 3)
	if err != nil {
		t.Fatalf("CreateOrgForUser: %v", err)
	}
	if created.ID <= 0 || created.Name != "Acme Labs" || created.Role != identity.RoleOwner {
		t.Fatalf("created = %+v", created)
	}

	// GET /me 立即包含新创建的组织。
	info, err := store.GetSessionInfo(ctx, sess.Token)
	if err != nil {
		t.Fatalf("GetSessionInfo: %v", err)
	}
	if len(info.Orgs) != 1 || info.Orgs[0].ID != created.ID || info.Orgs[0].Role != identity.RoleOwner {
		t.Fatalf("orgs = %+v, want [%d owner]", info.Orgs, created.ID)
	}

	// 并发创建 6 个组织，maxOwned=3 时再成功 2 个、其余全部被 ErrOrgLimitExceeded 拦下。
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int32
		limited   atomic.Int32
	)
	for i := range 6 {
		wg.Go(func() {
			_, cerr := store.CreateOrgForUser(ctx, sess.User.ID, "Org-"+string(rune('A'+i)), 3)
			switch {
			case cerr == nil:
				succeeded.Add(1)
			case errors.Is(cerr, identity.ErrOrgLimitExceeded):
				limited.Add(1)
			default:
				t.Errorf("unexpected CreateOrgForUser error: %v", cerr)
			}
		})
	}
	wg.Wait()
	if succeeded.Load() != 2 || limited.Load() != 4 {
		t.Fatalf("concurrent create: succeeded=%d limited=%d, want 2 and 4", succeeded.Load(), limited.Load())
	}

	if _, err := store.CreateOrgForUser(ctx, sess.User.ID, "   ", 10); !errors.Is(err, identity.ErrInvalidOrgName) {
		t.Errorf("blank name error = %v, want ErrInvalidOrgName", err)
	}
}

func TestOrgInvitationsListAndRevoke(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	issuedB, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "other-org", Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey tenantB: %v", err)
	}
	tenantB := issuedB.Tenant.ID

	invOwner, err := store.CreateOrgInvitation(ctx, identity.CreateInvitationParams{
		TenantID:  tenantA,
		Email:     "boss@example.com",
		Role:      identity.RoleOwner,
		InvitedBy: 101,
	})
	if err != nil {
		t.Fatalf("CreateOrgInvitation owner: %v", err)
	}
	invMember, err := store.CreateOrgInvitation(ctx, identity.CreateInvitationParams{
		TenantID:  tenantA,
		Email:     "dev@example.com",
		Role:      identity.RoleMember,
		InvitedBy: 101,
	})
	if err != nil {
		t.Fatalf("CreateOrgInvitation member: %v", err)
	}

	list, err := store.ListInvitations(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListInvitations: %v", err)
	}
	if len(list) != 2 || list[0].ID != invMember.ID || list[1].ID != invOwner.ID {
		t.Fatalf("list = %+v, want [%d, %d]", list, invMember.ID, invOwner.ID)
	}

	// 跨组织撤销必须返回 ErrInvitationNotFound。
	if err := store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
		TenantID: tenantB, InvitationID: invMember.ID, ActorRole: identity.RoleOwner,
	}); !errors.Is(err, identity.ErrInvitationNotFound) {
		t.Errorf("cross-tenant revoke error = %v, want ErrInvitationNotFound", err)
	}

	// admin 不可撤销 owner 邀请。
	if err := store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
		TenantID: tenantA, InvitationID: invOwner.ID, ActorRole: identity.RoleAdmin,
	}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Errorf("admin revoke owner invitation error = %v, want ErrMemberForbidden", err)
	}

	// admin 可撤销 member 邀请；重复撤销返回 ErrInvitationNotPending。
	if err := store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
		TenantID: tenantA, InvitationID: invMember.ID, ActorRole: identity.RoleAdmin,
	}); err != nil {
		t.Fatalf("admin revoke member invitation: %v", err)
	}
	if err := store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
		TenantID: tenantA, InvitationID: invMember.ID, ActorRole: identity.RoleOwner,
	}); !errors.Is(err, identity.ErrInvitationNotPending) {
		t.Errorf("second revoke error = %v, want ErrInvitationNotPending", err)
	}
}

func TestOrgAPIKeysCreatedByAndTenantIsolation(t *testing.T) {
	store, tenantA := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	issuedB, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "other-org", Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey tenantB: %v", err)
	}
	tenantB := issuedB.Tenant.ID

	issued, err := store.IssueOrgAPIKey(ctx, identity.IssueOrgAPIKeyParams{
		TenantID:  tenantA,
		CreatedBy: 42,
		Scopes:    []identity.Perm{identity.PermLinksRead, identity.PermLinksWrite},
	})
	if err != nil {
		t.Fatalf("IssueOrgAPIKey: %v", err)
	}
	if issued.Key.CreatedBy == nil || *issued.Key.CreatedBy != 42 {
		t.Fatalf("CreatedBy = %v, want 42", issued.Key.CreatedBy)
	}

	list, err := store.ListOrgAPIKeys(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListOrgAPIKeys: %v", err)
	}
	if len(list) != 1 || list[0].ID != issued.Key.ID || list[0].CreatedBy == nil || *list[0].CreatedBy != 42 {
		t.Fatalf("list = %+v", list)
	}

	// 跨租户吊销返回 ErrAPIKeyNotFound，不暴露密钥存在。
	if err := store.RevokeOrgAPIKey(ctx, tenantB, issued.Key.ID); !errors.Is(err, identity.ErrAPIKeyNotFound) {
		t.Fatalf("cross-tenant revoke error = %v, want ErrAPIKeyNotFound", err)
	}

	if err := store.RevokeOrgAPIKey(ctx, tenantA, issued.Key.ID); err != nil {
		t.Fatalf("RevokeOrgAPIKey: %v", err)
	}
	if err := store.RevokeOrgAPIKey(ctx, tenantA, issued.Key.ID); !errors.Is(err, identity.ErrAPIKeyRevoked) {
		t.Fatalf("second revoke error = %v, want ErrAPIKeyRevoked", err)
	}
}
