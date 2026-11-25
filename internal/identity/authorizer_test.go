package identity

import "testing"

func TestMapAuthorizerRoleMatrix(t *testing.T) {
	authz := NewMapAuthorizer()
	allPerms := []Perm{
		PermLinksRead,
		PermLinksWrite,
		PermStatsRead,
		PermMembersRead,
		PermMembersWrite,
		PermAPIKeysRead,
		PermAPIKeysWrite,
		PermAuditRead,
		PermWebhooksRead,
		PermWebhooksWrite,
	}

	wantByRole := map[Role]map[Perm]bool{
		RoleOwner: {
			PermLinksRead:     true,
			PermLinksWrite:    true,
			PermStatsRead:     true,
			PermMembersRead:   true,
			PermMembersWrite:  true,
			PermAPIKeysRead:   true,
			PermAPIKeysWrite:  true,
			PermAuditRead:     true,
			PermWebhooksRead:  true,
			PermWebhooksWrite: true,
		},
		RoleAdmin: {
			PermLinksRead:     true,
			PermLinksWrite:    true,
			PermStatsRead:     true,
			PermMembersRead:   true,
			PermMembersWrite:  true,
			PermAPIKeysRead:   true,
			PermAPIKeysWrite:  true,
			PermAuditRead:     true,
			PermWebhooksRead:  true,
			PermWebhooksWrite: true,
		},
		RoleMember: {
			PermLinksRead:     true,
			PermLinksWrite:    true,
			PermStatsRead:     true,
			PermMembersRead:   true,
			PermMembersWrite:  false,
			PermAPIKeysRead:   false,
			PermAPIKeysWrite:  false,
			PermAuditRead:     false,
			PermWebhooksRead:  false,
			PermWebhooksWrite: false,
		},
		RoleViewer: {
			PermLinksRead:     true,
			PermLinksWrite:    false,
			PermStatsRead:     true,
			PermMembersRead:   true,
			PermMembersWrite:  false,
			PermAPIKeysRead:   false,
			PermAPIKeysWrite:  false,
			PermAuditRead:     false,
			PermWebhooksRead:  false,
			PermWebhooksWrite: false,
		},
	}

	for role, permMap := range wantByRole {
		for _, perm := range allPerms {
			t.Run(string(role)+"/"+string(perm), func(t *testing.T) {
				p := Principal{ActorKind: ActorUser, ActorID: 1, Role: role}
				if got := authz.Can(p, perm); got != permMap[perm] {
					t.Errorf("Can(%s, %s) = %v, want %v", role, perm, got, permMap[perm])
				}
			})
		}
	}
}

func TestMapAuthorizerAPIKeyAndFailClosed(t *testing.T) {
	authz := NewMapAuthorizer()

	tests := []struct {
		name      string
		authz     *MapAuthorizer
		principal Principal
		perm      Perm
		want      bool
	}{
		{
			name:      "all api key scopes allow links read",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermLinksRead,
			want:      true,
		},
		{
			name:      "all api key scopes allow links write",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermLinksWrite,
			want:      true,
		},
		{
			name:      "all api key scopes allow stats read",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermStatsRead,
			want:      true,
		},
		{
			name:      "all api key scopes deny members read",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermMembersRead,
			want:      false,
		},
		{
			name:      "all api key scopes deny members write",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermMembersWrite,
			want:      false,
		},
		{
			name:      "all api key scopes deny audit read",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermAuditRead,
			want:      false,
		},
		{
			name:      "all api key scopes deny webhooks read",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermWebhooksRead,
			want:      false,
		},
		{
			name:      "all api key scopes deny webhooks write",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: AllAPIKeyScopes()},
			perm:      PermWebhooksWrite,
			want:      false,
		},
		{
			name:      "subset scope allows matching perm",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: []Perm{PermLinksRead}},
			perm:      PermLinksRead,
			want:      true,
		},
		{
			name:      "subset scope denies other perm",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey, Scopes: []Perm{PermLinksRead}},
			perm:      PermLinksWrite,
			want:      false,
		},
		{
			name:      "empty api key scopes deny",
			authz:     authz,
			principal: Principal{ActorKind: ActorAPIKey},
			perm:      PermLinksRead,
			want:      false,
		},
		{
			name:      "unknown role denies",
			authz:     authz,
			principal: Principal{ActorKind: ActorUser, Role: Role("guest")},
			perm:      PermLinksRead,
			want:      false,
		},
		{
			name:      "unknown actor kind denies",
			authz:     authz,
			principal: Principal{ActorKind: ActorKind("service"), Role: RoleOwner, Scopes: AllAPIKeyScopes()},
			perm:      PermLinksRead,
			want:      false,
		},
		{
			name:      "zero principal denies",
			authz:     authz,
			principal: Principal{},
			perm:      PermLinksRead,
			want:      false,
		},
		{
			name:      "empty perm denies",
			authz:     authz,
			principal: Principal{ActorKind: ActorUser, Role: RoleOwner},
			perm:      "",
			want:      false,
		},
		{
			name:      "nil authorizer denies",
			authz:     nil,
			principal: Principal{ActorKind: ActorUser, Role: RoleOwner},
			perm:      PermLinksRead,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.authz.Can(tt.principal, tt.perm); got != tt.want {
				t.Errorf("Can() = %v, want %v", got, tt.want)
			}
		})
	}
}
