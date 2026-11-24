package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/link"
)

func TestAuditLogsLifecycleAndIsolation(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	// 1. CLI 新建租户并签发 API Key：同时写 org.create 与 apikey.issue，且绝不泄露明文或哈希。
	opCtx := audit.WithMeta(ctx, audit.OperatorMeta(""))
	issuedNewOrg, err := store.IssueAPIKey(opCtx, IssueAPIKeyParams{
		Name:       "Audit New Org",
		ExternalID: "ext-audit-org",
		Scopes:     identity.AllAPIKeyScopes(),
	})
	if err != nil {
		t.Fatalf("IssueAPIKey new org: %v", err)
	}
	tenant2 := issuedNewOrg.Tenant.ID

	// 通过管理员接口在 tenant1 中初始化 owner 与待邀请用户。
	ownerIssued, err := store.CreateSessionForIdentity(ctx, identity.ExternalIdentity{
		Issuer:        "https://dex.example",
		Subject:       "audit-owner",
		Email:         "owner-audit@example.com",
		Name:          "Audit Owner",
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity owner: %v", err)
	}
	ownerID := ownerIssued.User.ID

	invOwner, err := store.CreateInvitation(opCtx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "owner-audit@example.com",
		Role:     identity.RoleOwner,
	})
	if err != nil {
		t.Fatalf("CreateInvitation owner: %v", err)
	}
	// owner 再次登录兑现邀请：写 invitation.redeem。
	redeemCtx := audit.WithMeta(ctx, audit.SystemMeta("req-redeem-1"))
	if _, err := store.CreateSessionForIdentity(redeemCtx, identity.ExternalIdentity{
		Issuer:        "https://dex.example",
		Subject:       "audit-owner",
		Email:         "owner-audit@example.com",
		Name:          "Audit Owner",
		EmailVerified: true,
	}, time.Hour); err != nil {
		t.Fatalf("CreateSessionForIdentity redeem owner: %v", err)
	}

	// 2. owner 自助新建第三个组织：org.create (actor=user)。
	userCtx := audit.WithMeta(ctx, audit.UserMeta(ownerID, "req-user-1"))
	createdOrg, err := store.CreateOrgForUser(userCtx, ownerID, "Self Audit Org", 10)
	if err != nil {
		t.Fatalf("CreateOrgForUser: %v", err)
	}

	// 3. 租户配额修改：tenant.limits_update。
	newQuota := int64(2500)
	newRate := 50
	if _, err := store.SetTenantLimits(opCtx, tenant1, SetTenantLimitsParams{
		LinkQuota: &newQuota,
		RateLimit: &newRate,
	}); err != nil {
		t.Fatalf("SetTenantLimits: %v", err)
	}

	// 4. 邀请创建、撤销与兑现（member）。
	invPending, err := store.CreateOrgInvitation(userCtx, identity.CreateInvitationParams{
		TenantID:  tenant1,
		Email:     "pending-revoke@example.com",
		Role:      identity.RoleMember,
		InvitedBy: ownerID,
	})
	if err != nil {
		t.Fatalf("CreateOrgInvitation pending: %v", err)
	}
	if err := store.RevokeInvitation(userCtx, identity.RevokeInvitationParams{
		TenantID:     tenant1,
		InvitationID: invPending.ID,
		ActorRole:    identity.RoleOwner,
	}); err != nil {
		t.Fatalf("RevokeInvitation: %v", err)
	}

	if _, err := store.CreateOrgInvitation(userCtx, identity.CreateInvitationParams{
		TenantID:  tenant1,
		Email:     "member-audit@example.com",
		Role:      identity.RoleMember,
		InvitedBy: ownerID,
	}); err != nil {
		t.Fatalf("CreateOrgInvitation member: %v", err)
	}
	memberIssued, err := store.CreateSessionForIdentity(audit.WithMeta(ctx, audit.SystemMeta("req-redeem-2")), identity.ExternalIdentity{
		Issuer:        "https://dex.example",
		Subject:       "audit-member",
		Email:         "member-audit@example.com",
		Name:          "Audit Member",
		EmailVerified: true,
	}, time.Hour)
	if err != nil {
		t.Fatalf("CreateSessionForIdentity member: %v", err)
	}
	memberID := memberIssued.User.ID

	// 5. 成员改角色与移出组织：member.role_update、member.remove。
	if _, err := store.UpdateMemberRole(userCtx, identity.UpdateMemberRoleParams{
		TenantID:     tenant1,
		ActorUserID:  ownerID,
		TargetUserID: memberID,
		NewRole:      identity.RoleAdmin,
	}); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	if err := store.RemoveMember(userCtx, identity.RemoveMemberParams{
		TenantID:     tenant1,
		ActorUserID:  ownerID,
		TargetUserID: memberID,
	}); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	// 6. API Key 签发与吊销：apikey.issue、apikey.revoke。
	issuedOrgKey, err := store.IssueOrgAPIKey(userCtx, identity.IssueOrgAPIKeyParams{
		TenantID:  tenant1,
		CreatedBy: ownerID,
		Scopes:    []identity.Perm{identity.PermLinksRead, identity.PermLinksWrite},
	})
	if err != nil {
		t.Fatalf("IssueOrgAPIKey: %v", err)
	}
	if err := store.RevokeOrgAPIKey(userCtx, tenant1, issuedOrgKey.Key.ID); err != nil {
		t.Fatalf("RevokeOrgAPIKey: %v", err)
	}

	// 7. 短链全生命周期：create / update / ban (+ 幂等) / unban (+ 幂等) / delete。
	apiKeyCtx := audit.WithMeta(ctx, audit.APIKeyMeta(issuedOrgKey.Key.ID, "req-apikey-1"))
	l := mustLink(t, "https://example.com/audit-target", tenant1)
	if err := store.Create(apiKeyCtx, l); err != nil {
		t.Fatalf("Create link: %v", err)
	}
	newURL := "https://example.com/audit-updated"
	disabled := true
	if _, err := store.Update(userCtx, tenant1, l.Code, link.Update{
		URL:      &newURL,
		Disabled: &disabled,
	}); err != nil {
		t.Fatalf("Update link: %v", err)
	}

	riskCtx := audit.WithMeta(ctx, audit.SystemMeta("job:99"))
	if _, err := store.BanIfURL(riskCtx, l.Code, newURL, link.BanReasonPhishing, link.BanSourceRisk); err != nil {
		t.Fatalf("BanIfURL: %v", err)
	}
	// 幂等重复 Ban 不得产生第二条 link.ban 审计事件。
	if _, err := store.Ban(opCtx, l.Code, link.BanReasonSpam, link.BanSourceOperator); err != nil {
		t.Fatalf("repeat Ban: %v", err)
	}

	if _, err := store.Unban(opCtx, l.Code); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	// 幂等重复 Unban 不得产生第二条 link.unban 审计事件。
	if _, err := store.Unban(opCtx, l.Code); err != nil {
		t.Fatalf("repeat Unban: %v", err)
	}

	if err := store.Delete(userCtx, tenant1, l.Code); err != nil {
		t.Fatalf("Delete link: %v", err)
	}

	// 校验 tenant1 审计事件完整覆盖剩余 13 类动作（org.create 落在 tenant2 与 createdOrg.ID 上）。
	events1, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenant1, Limit: 50})
	if err != nil {
		t.Fatalf("ListAuditLogs tenant1: %v", err)
	}
	wantTenant1Actions := []audit.Action{
		audit.ActionInvitationCreate,   // owner invite (operator)
		audit.ActionInvitationRedeem,   // owner redeem (user, req-redeem-1)
		audit.ActionTenantLimitsUpdate, // limits (operator)
		audit.ActionInvitationCreate,   // pending invite (user)
		audit.ActionInvitationRevoke,   // pending revoke (user)
		audit.ActionInvitationCreate,   // member invite (user)
		audit.ActionInvitationRedeem,   // member redeem (user, req-redeem-2)
		audit.ActionMemberRoleUpdate,   // member role update (user)
		audit.ActionMemberRemove,       // member remove (user)
		audit.ActionAPIKeyIssue,        // org api key issue (user)
		audit.ActionAPIKeyRevoke,       // org api key revoke (user)
		audit.ActionLinkCreate,         // link create (api_key)
		audit.ActionLinkUpdate,         // link update (user)
		audit.ActionLinkBan,            // link ban (system, job:99)
		audit.ActionLinkUnban,          // link unban (operator)
		audit.ActionLinkDelete,         // link delete (user)
	}
	// 返回按 id DESC 倒序。
	slices.Reverse(wantTenant1Actions)
	gotActions := make([]audit.Action, 0, len(events1))
	for _, ev := range events1 {
		gotActions = append(gotActions, ev.Action)
		if ev.TenantID != tenant1 {
			t.Fatalf("cross-tenant leak: got tenant_id=%d, want %d", ev.TenantID, tenant1)
		}
		if strings.Contains(string(ev.Metadata), issuedOrgKey.Plaintext) {
			t.Fatalf("audit metadata leaked API key plaintext: %s", ev.Metadata)
		}
		if strings.Contains(string(ev.Metadata), issuedNewOrg.Plaintext) {
			t.Fatalf("audit metadata leaked CLI API key plaintext: %s", ev.Metadata)
		}
	}
	if !slices.Equal(gotActions, wantTenant1Actions) {
		t.Fatalf("tenant1 actions = %v, want %v", gotActions, wantTenant1Actions)
	}

	// 检查各角色与关联 user 字段、metadata 细节。
	byAction := make(map[audit.Action][]audit.Event)
	for _, ev := range events1 {
		byAction[ev.Action] = append(byAction[ev.Action], ev)
	}
	if banEv := byAction[audit.ActionLinkBan][0]; banEv.ActorKind != audit.ActorSystem || banEv.ActorID != nil || banEv.RequestID != "job:99" {
		t.Fatalf("link.ban event = %+v, want system job:99", banEv)
	}
	if createEv := byAction[audit.ActionLinkCreate][0]; createEv.ActorKind != audit.ActorAPIKey || createEv.ActorID == nil || *createEv.ActorID != issuedOrgKey.Key.ID || createEv.RequestID != "req-apikey-1" {
		t.Fatalf("link.create event = %+v, want api_key=%d", createEv, issuedOrgKey.Key.ID)
	}
	if keyIssueEv := byAction[audit.ActionAPIKeyIssue][0]; keyIssueEv.ActorEmail != "owner-audit@example.com" || keyIssueEv.ActorName != "Audit Owner" || keyIssueEv.RequestID != "req-user-1" {
		t.Fatalf("apikey.issue actor enrichment = %+v", keyIssueEv)
	}
	if limitsEv := byAction[audit.ActionTenantLimitsUpdate][0]; limitsEv.ActorKind != audit.ActorOperator || limitsEv.TargetID != strconv.FormatInt(tenant1, 10) {
		t.Fatalf("tenant.limits_update event = %+v", limitsEv)
	} else {
		var meta map[string]any
		if err := json.Unmarshal(limitsEv.Metadata, &meta); err != nil {
			t.Fatalf("unmarshal limits metadata: %v", err)
		}
		if meta["link_quota"] != float64(2500) || meta["rate_limit_per_second"] != float64(50) {
			t.Fatalf("limits metadata = %v", meta)
		}
	}
	if redeemOwner := byAction[audit.ActionInvitationRedeem][1]; redeemOwner.TargetID != strconv.FormatInt(invOwner.ID, 10) || redeemOwner.RequestID != "req-redeem-1" {
		t.Fatalf("owner invitation.redeem = %+v", redeemOwner)
	}

	// 检查 tenant2 与 createdOrg 的 org.create 审计事件。
	events2, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenant2, Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditLogs tenant2: %v", err)
	}
	if len(events2) != 2 || events2[0].Action != audit.ActionAPIKeyIssue || events2[1].Action != audit.ActionOrgCreate {
		t.Fatalf("tenant2 events = %+v, want [apikey.issue, org.create]", events2)
	}

	events3, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: createdOrg.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditLogs createdOrg: %v", err)
	}
	if len(events3) != 1 || events3[0].Action != audit.ActionOrgCreate || events3[0].ActorKind != audit.ActorUser || events3[0].ActorEmail != "owner-audit@example.com" {
		t.Fatalf("createdOrg events = %+v", events3)
	}

	// 8. action 筛选与游标分页。
	filtered, err := store.ListAuditLogs(ctx, audit.ListOptions{
		TenantID: tenant1,
		Action:   audit.ActionInvitationCreate,
		Limit:    2,
	})
	if err != nil || len(filtered) != 2 {
		t.Fatalf("filtered first page = %d items, err=%v", len(filtered), err)
	}
	filteredPage2, err := store.ListAuditLogs(ctx, audit.ListOptions{
		TenantID: tenant1,
		Action:   audit.ActionInvitationCreate,
		Limit:    2,
		Cursor:   &audit.ListCursor{ID: filtered[1].ID},
	})
	if err != nil || len(filteredPage2) != 1 || filteredPage2[0].TargetID != strconv.FormatInt(invOwner.ID, 10) {
		t.Fatalf("filtered page 2 = %+v, err=%v", filteredPage2, err)
	}
	if _, err := store.ListAuditLogs(ctx, audit.ListOptions{
		TenantID: tenant1,
		Limit:    10,
		Cursor:   &audit.ListCursor{ID: 0},
	}); err == nil {
		t.Fatal("ListAuditLogs with non-positive cursor id = nil error, want error")
	}

	// 9. 失败的变更操作与幂等空操作绝不产生审计日志。
	beforeCount := len(events1)
	if _, err := store.UpdateMemberRole(userCtx, identity.UpdateMemberRoleParams{
		TenantID:     tenant1,
		ActorUserID:  ownerID,
		TargetUserID: ownerID,
		NewRole:      identity.RoleOwner,
	}); err != nil {
		t.Fatalf("noop UpdateMemberRole err = %v, want nil", err)
	}
	if err := store.RevokeOrgAPIKey(userCtx, tenant1, issuedOrgKey.Key.ID); !errors.Is(err, identity.ErrAPIKeyRevoked) {
		t.Fatalf("repeat revoke api key err = %v, want ErrAPIKeyRevoked", err)
	}
	if err := store.RemoveMember(userCtx, identity.RemoveMemberParams{
		TenantID:     tenant1,
		ActorUserID:  ownerID,
		TargetUserID: ownerID,
	}); !errors.Is(err, identity.ErrLastOwner) {
		t.Fatalf("remove last owner err = %v, want ErrLastOwner", err)
	}
	zeroQuota := int64(0)
	if _, err := store.SetTenantLimits(opCtx, tenant1, SetTenantLimitsParams{LinkQuota: &zeroQuota}); err != nil {
		t.Fatalf("set zero quota: %v", err)
	}
	if err := store.Create(apiKeyCtx, mustLink(t, "https://example.com/over-quota", tenant1)); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Fatalf("over-quota create err = %v, want ErrQuotaExceeded", err)
	}
	afterEvents, err := store.ListAuditLogs(ctx, audit.ListOptions{TenantID: tenant1, Limit: 50})
	if err != nil {
		t.Fatalf("ListAuditLogs after failures: %v", err)
	}
	// 只增加了那次成功的 SetTenantLimits（+1），失败的三次都没有写入。
	if len(afterEvents) != beforeCount+1 {
		t.Fatalf("audit count after failed mutations = %d, want %d", len(afterEvents), beforeCount+1)
	}
}

/*
TestAuditFailClosedMissingOrInvalidMeta 验证存储写方法不再静默降级为 system/operator（ADR 0025）：
  - 缺少 audit.Meta 的上下文直接返回 audit.ErrMissingMeta，不落库；
  - 携带非法 Meta（如 ActorUser 缺少正数 ActorID，或 ActorSystem 携带 ActorID）同样失败，
    即使方法签名显式传入了 userID 也不绕过校验。
*/
func TestAuditFailClosedMissingOrInvalidMeta(t *testing.T) {
	store, tenantID := newTestStore(t)
	bareCtx := context.Background()
	opCtx := audit.WithMeta(bareCtx, audit.OperatorMeta("seed-op"))

	seeded := mustLink(t, "https://example.com/audit-fail-closed", tenantID)
	if err := store.Create(opCtx, seeded); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	issuedKey, err := store.IssueAPIKey(opCtx, IssueAPIKeyParams{
		TenantID: tenantID,
		Name:     "",
		Scopes:   identity.AllAPIKeyScopes(),
	})
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	inv, err := store.CreateInvitation(opCtx, CreateInvitationParams{
		TenantID: tenantID,
		Email:    "pending-failclosed@example.com",
		Role:     identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("seed invitation: %v", err)
	}

	newURL := "https://example.com/should-fail"
	quota := int64(99)
	checks := []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{"Create", func(ctx context.Context) error {
			return store.Create(ctx, mustLink(t, "https://example.com/no-meta", tenantID))
		}},
		{"Update", func(ctx context.Context) error {
			_, err := store.Update(ctx, tenantID, seeded.Code, link.Update{URL: &newURL})
			return err
		}},
		{"Delete", func(ctx context.Context) error {
			return store.Delete(ctx, tenantID, seeded.Code)
		}},
		{"Ban", func(ctx context.Context) error {
			_, err := store.Ban(ctx, seeded.Code, link.BanReasonSpam, link.BanSourceOperator)
			return err
		}},
		{"BanIfURL", func(ctx context.Context) error {
			_, err := store.BanIfURL(ctx, seeded.Code, seeded.URL, link.BanReasonSpam, link.BanSourceRisk)
			return err
		}},
		{"Unban", func(ctx context.Context) error {
			_, err := store.Unban(ctx, seeded.Code)
			return err
		}},
		{"SetTenantLimits", func(ctx context.Context) error {
			_, err := store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &quota})
			return err
		}},
		{"IssueAPIKey", func(ctx context.Context) error {
			_, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "Should Not Create", Scopes: identity.AllAPIKeyScopes()})
			return err
		}},
		{"RevokeAPIKey", func(ctx context.Context) error {
			return store.RevokeAPIKey(ctx, issuedKey.Key.ID)
		}},
		{"RevokeAPIKeyForTenant", func(ctx context.Context) error {
			return store.RevokeAPIKeyForTenant(ctx, tenantID, issuedKey.Key.ID)
		}},
		{"CreateInvitation", func(ctx context.Context) error {
			_, err := store.CreateInvitation(ctx, CreateInvitationParams{
				TenantID: tenantID,
				Email:    "blocked-invite@example.com",
				Role:     identity.RoleMember,
			})
			return err
		}},
		{"RevokeInvitation", func(ctx context.Context) error {
			return store.RevokeInvitation(ctx, identity.RevokeInvitationParams{
				TenantID:     tenantID,
				InvitationID: inv.ID,
				ActorRole:    identity.RoleOwner,
			})
		}},
	}
	for _, tc := range checks {
		t.Run("missing/"+tc.name, func(t *testing.T) {
			err := tc.fn(bareCtx)
			if !errors.Is(err, audit.ErrMissingMeta) {
				t.Fatalf("%s(bareCtx) err = %v, want audit.ErrMissingMeta", tc.name, err)
			}
		})
	}

	// 非法 Meta（哪怕带有显式 userID 的路径）也必须 fail-closed 拒绝。
	ownerSess, err := store.CreateSessionForIdentity(bareCtx, identity.ExternalIdentity{
		Issuer:        "https://dex.example",
		Subject:       "failclosed-owner",
		Email:         "failclosed-owner@example.com",
		EmailVerified: true,
		Name:          "FailClosed Owner",
	}, time.Hour)
	if err != nil {
		t.Fatalf("seed owner session: %v", err)
	}
	badActorID := int64(42)
	invalidMetas := []audit.Meta{
		{ActorKind: audit.ActorUser, ActorID: nil},
		{ActorKind: audit.ActorSystem, ActorID: &badActorID},
	}
	for i, badMeta := range invalidMetas {
		badCtx := audit.WithMeta(bareCtx, badMeta)
		if _, err := store.CreateOrgForUser(badCtx, ownerSess.User.ID, "Bad Org", 5); !errors.Is(err, audit.ErrMissingMeta) {
			t.Fatalf("case %d CreateOrgForUser(badMeta) err = %v, want audit.ErrMissingMeta", i, err)
		}
	}
}
