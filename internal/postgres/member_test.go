package postgres

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func createTestMember(t *testing.T, store *Store, tenantID int64, sub, email string, role identity.Role) int64 {
	t.Helper()
	ctx := context.Background()
	issued, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       sub,
		Email:         email,
		EmailVerified: true,
		Name:          sub,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity(%s): %v", sub, err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, $3)`,
		issued.User.ID, tenantID, string(role),
	); err != nil {
		t.Fatalf("insert membership(%s, %d, %s): %v", sub, tenantID, role, err)
	}
	return issued.User.ID
}

func TestListMembers(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2, emptyTenant int64
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('org-two') RETURNING id`).Scan(&tenant2); err != nil {
		t.Fatalf("insert tenant2: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('org-empty') RETURNING id`).Scan(&emptyTenant); err != nil {
		t.Fatalf("insert emptyTenant: %v", err)
	}

	u1 := createTestMember(t, store, tenant1, "u1", "u1@example.com", identity.RoleOwner)
	u2 := createTestMember(t, store, tenant1, "u2", "u2@example.com", identity.RoleViewer)
	u3 := createTestMember(t, store, tenant2, "u3", "u3@example.com", identity.RoleAdmin)
	// u2 同时属于 tenant2，互不串租户。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'member')`, u2, tenant2,
	); err != nil {
		t.Fatalf("insert cross-org membership: %v", err)
	}

	got1, err := store.ListMembers(ctx, tenant1)
	if err != nil {
		t.Fatalf("ListMembers(tenant1): %v", err)
	}
	if len(got1) != 2 || got1[0].UserID != u1 || got1[0].Role != identity.RoleOwner || got1[0].Email != "u1@example.com" ||
		got1[1].UserID != u2 || got1[1].Role != identity.RoleViewer || got1[1].JoinedAt.Location() != time.UTC {
		t.Fatalf("tenant1 members = %+v", got1)
	}

	got2, err := store.ListMembers(ctx, tenant2)
	if err != nil {
		t.Fatalf("ListMembers(tenant2): %v", err)
	}
	wantIDs := []int64{u2, u3}
	gotIDs := []int64{got2[0].UserID, got2[1].UserID}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("tenant2 member IDs = %v, want %v", gotIDs, wantIDs)
	}

	gotEmpty, err := store.ListMembers(ctx, emptyTenant)
	if err != nil {
		t.Fatalf("ListMembers(emptyTenant): %v", err)
	}
	if len(gotEmpty) != 0 {
		t.Fatalf("emptyTenant members = %+v, want empty", gotEmpty)
	}

	if _, err := store.ListMembers(ctx, 0); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Fatalf("ListMembers(0) err = %v, want ErrOrgNotFound", err)
	}
}

func TestUpdateMemberRole(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('org-two') RETURNING id`).Scan(&tenant2); err != nil {
		t.Fatalf("insert tenant2: %v", err)
	}

	owner1 := createTestMember(t, store, tenant1, "owner1", "owner1@example.com", identity.RoleOwner)
	admin1 := createTestMember(t, store, tenant1, "admin1", "admin1@example.com", identity.RoleAdmin)
	member1 := createTestMember(t, store, tenant1, "member1", "member1@example.com", identity.RoleMember)
	viewer1 := createTestMember(t, store, tenant1, "viewer1", "viewer1@example.com", identity.RoleViewer)
	outsider := createTestMember(t, store, tenant2, "outsider", "outsider@example.com", identity.RoleOwner)

	// 1. 仅有一名 owner 时不能降级自己，但保留 owner -> owner 允许。
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: owner1, NewRole: identity.RoleAdmin}); !errors.Is(err, identity.ErrLastOwner) {
		t.Fatalf("demote sole owner err = %v, want ErrLastOwner", err)
	}
	sameOwner, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: owner1, NewRole: identity.RoleOwner})
	if err != nil || sameOwner.Role != identity.RoleOwner {
		t.Fatalf("sole owner owner->owner = (%+v, %v)", sameOwner, err)
	}

	// 2. owner 提拔 member 为 owner，再自我降级为 admin（此时仍有另一名 owner）。
	promoted, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: member1, NewRole: identity.RoleOwner})
	if err != nil || promoted.Role != identity.RoleOwner || promoted.Email != "member1@example.com" {
		t.Fatalf("promote to owner = (%+v, %v)", promoted, err)
	}
	selfDemoted, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: owner1, NewRole: identity.RoleAdmin})
	if err != nil || selfDemoted.Role != identity.RoleAdmin {
		t.Fatalf("self demote with 2 owners = (%+v, %v)", selfDemoted, err)
	}
	// owner1 先变回 owner，member1 变回 member，方便测后续规则。
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: member1, TargetUserID: owner1, NewRole: identity.RoleOwner}); err != nil {
		t.Fatalf("restore owner1: %v", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: member1, NewRole: identity.RoleMember}); err != nil {
		t.Fatalf("restore member1: %v", err)
	}

	// 3. admin 可以调整 admin/member/viewer，含自我降级；但不能动 owner，也不能把任何人（含不存在目标）升为 owner。
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: owner1, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("admin demote owner err = %v, want ErrMemberForbidden", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: viewer1, NewRole: identity.RoleOwner}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("admin promote viewer to owner err = %v, want ErrMemberForbidden", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: 999999, NewRole: identity.RoleOwner}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("admin promote missing target to owner err = %v, want ErrMemberForbidden", err)
	}
	gotViewer, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: viewer1, NewRole: identity.RoleAdmin})
	if err != nil || gotViewer.Role != identity.RoleAdmin {
		t.Fatalf("admin promote viewer to admin = (%+v, %v)", gotViewer, err)
	}
	// admin 自我降级为 member 后不再拥有权限。
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: admin1, NewRole: identity.RoleMember}); err != nil {
		t.Fatalf("admin self demote: %v", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: member1, NewRole: identity.RoleViewer}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("demoted admin err = %v, want ErrMemberForbidden", err)
	}

	// 4. member/viewer 操作拒绝；非组织成员返回 ErrOrgNotFound；他组织/不存在目标返回 ErrMemberNotFound；非法角色返回 ErrInvalidRole。
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: member1, TargetUserID: viewer1, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("member actor err = %v, want ErrMemberForbidden", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: outsider, TargetUserID: viewer1, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Fatalf("outsider actor err = %v, want ErrOrgNotFound", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: outsider, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrMemberNotFound) {
		t.Fatalf("other org target err = %v, want ErrMemberNotFound", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: 999999, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrMemberNotFound) {
		t.Fatalf("missing target err = %v, want ErrMemberNotFound", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: 0, NewRole: identity.RoleMember}); !errors.Is(err, identity.ErrMemberNotFound) {
		t.Fatalf("zero target err = %v, want ErrMemberNotFound", err)
	}
	if _, err := store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: member1, NewRole: identity.Role("guest")}); !errors.Is(err, identity.ErrInvalidRole) {
		t.Fatalf("invalid role err = %v, want ErrInvalidRole", err)
	}
}

func TestRemoveMember(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ('org-two') RETURNING id`).Scan(&tenant2); err != nil {
		t.Fatalf("insert tenant2: %v", err)
	}

	owner1 := createTestMember(t, store, tenant1, "owner1", "owner1@example.com", identity.RoleOwner)
	owner2 := createTestMember(t, store, tenant1, "owner2", "owner2@example.com", identity.RoleOwner)
	admin1 := createTestMember(t, store, tenant1, "admin1", "admin1@example.com", identity.RoleAdmin)
	admin2 := createTestMember(t, store, tenant1, "admin2", "admin2@example.com", identity.RoleAdmin)
	member1 := createTestMember(t, store, tenant1, "member1", "member1@example.com", identity.RoleMember)
	viewer1 := createTestMember(t, store, tenant1, "viewer1", "viewer1@example.com", identity.RoleViewer)
	outsider := createTestMember(t, store, tenant2, "outsider", "outsider@example.com", identity.RoleOwner)

	// 让 member1 也属于 tenant2 并持有一个有效会话，验证移出 tenant1 不影响其会话与 tenant2。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'admin')`, member1, tenant2,
	); err != nil {
		t.Fatalf("insert member1 tenant2: %v", err)
	}
	member1Sess, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "http://127.0.0.1:5556/dex",
		Subject:       "member1",
		Email:         "member1@example.com",
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity member1: %v", err)
	}

	// admin 不能移出 owner；viewer/member 不能移出任何人；非成员操作返回 ErrOrgNotFound；他组织目标返回 ErrMemberNotFound。
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: owner1}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("admin remove owner err = %v, want ErrMemberForbidden", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: member1, TargetUserID: viewer1}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("member remove viewer err = %v, want ErrMemberForbidden", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: viewer1, TargetUserID: member1}); !errors.Is(err, identity.ErrMemberForbidden) {
		t.Fatalf("viewer remove member err = %v, want ErrMemberForbidden", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: outsider, TargetUserID: viewer1}); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Fatalf("outsider remove err = %v, want ErrOrgNotFound", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: outsider}); !errors.Is(err, identity.ErrMemberNotFound) {
		t.Fatalf("remove other org member err = %v, want ErrMemberNotFound", err)
	}

	// admin 可以移出 viewer、其他 admin 以及自己。
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: viewer1}); err != nil {
		t.Fatalf("admin remove viewer: %v", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: admin2}); err != nil {
		t.Fatalf("admin remove admin2: %v", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: admin1, TargetUserID: admin1}); err != nil {
		t.Fatalf("admin remove self: %v", err)
	}

	// owner1 移出 member1：tenant1 立即失去成员身份，但 member1 的会话在 tenant2 仍然有效。
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: member1}); err != nil {
		t.Fatalf("owner remove member1: %v", err)
	}
	if _, err := store.AuthenticateSessionForOrg(ctx, member1Sess.Token, tenant1); !errors.Is(err, identity.ErrOrgNotFound) {
		t.Fatalf("AuthenticateSessionForOrg(tenant1) after removal = %v, want ErrOrgNotFound", err)
	}
	p2, err := store.AuthenticateSessionForOrg(ctx, member1Sess.Token, tenant2)
	if err != nil || p2.Tenant.ID != tenant2 || p2.Role != identity.RoleAdmin {
		t.Fatalf("AuthenticateSessionForOrg(tenant2) = (%+v, %v), want admin in tenant2", p2, err)
	}

	// 两名 owner 时 owner1 可以移出 owner2；只剩 owner1 时禁止再移出自己。
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: owner2}); err != nil {
		t.Fatalf("owner1 remove owner2: %v", err)
	}
	if err := store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: owner1, TargetUserID: owner1}); !errors.Is(err, identity.ErrLastOwner) {
		t.Fatalf("remove last owner err = %v, want ErrLastOwner", err)
	}
}

func TestLastOwnerProtectionConcurrent(t *testing.T) {
	t.Run("remove", func(t *testing.T) {
		store, tenant1 := newTestStore(t)
		ctx := context.Background()
		ownerA := createTestMember(t, store, tenant1, "owner-a", "a@example.com", identity.RoleOwner)
		ownerB := createTestMember(t, store, tenant1, "owner-b", "b@example.com", identity.RoleOwner)

		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			errs[0] = store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: ownerA, TargetUserID: ownerB})
		})
		wg.Go(func() {
			<-start
			errs[1] = store.RemoveMember(ctx, identity.RemoveMemberParams{TenantID: tenant1, ActorUserID: ownerB, TargetUserID: ownerA})
		})
		close(start)
		wg.Wait()

		assertOneSucceededOneBlocked(t, errs)
		assertOneOwnerRemains(t, store, tenant1)
	})

	t.Run("demote", func(t *testing.T) {
		store, tenant1 := newTestStore(t)
		ctx := context.Background()
		ownerA := createTestMember(t, store, tenant1, "owner-a", "a@example.com", identity.RoleOwner)
		ownerB := createTestMember(t, store, tenant1, "owner-b", "b@example.com", identity.RoleOwner)

		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			_, errs[0] = store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: ownerA, TargetUserID: ownerB, NewRole: identity.RoleMember})
		})
		wg.Go(func() {
			<-start
			_, errs[1] = store.UpdateMemberRole(ctx, identity.UpdateMemberRoleParams{TenantID: tenant1, ActorUserID: ownerB, TargetUserID: ownerA, NewRole: identity.RoleMember})
		})
		close(start)
		wg.Wait()

		assertOneSucceededOneBlocked(t, errs)
		assertOneOwnerRemains(t, store, tenant1)
	})
}

func assertOneSucceededOneBlocked(t *testing.T, errs []error) {
	t.Helper()
	var okCount, blockedCount int
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, identity.ErrLastOwner) || errors.Is(err, identity.ErrOrgNotFound) || errors.Is(err, identity.ErrMemberForbidden):
			blockedCount++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if okCount != 1 || blockedCount != 1 {
		t.Fatalf("concurrent outcomes: ok=%d blocked=%d errs=%v, want (1, 1)", okCount, blockedCount, errs)
	}
}

func assertOneOwnerRemains(t *testing.T, store *Store, tenantID int64) {
	t.Helper()
	var owners int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, tenantID,
	).Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 1 {
		t.Fatalf("remaining owners = %d, want 1", owners)
	}
}
