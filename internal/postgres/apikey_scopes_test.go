package postgres

import (
	"slices"
	"testing"

	"shortlink/internal/identity"
)

func TestAuthScopesDropsUnknown(t *testing.T) {
	tests := []struct {
		name string
		raw  []string
		want []identity.Perm
	}{
		{name: "known", raw: []string{"links:read", "stats:read"}, want: []identity.Perm{identity.PermLinksRead, identity.PermStatsRead}},
		{name: "unknown dropped", raw: []string{"links:read", "webhooks:write"}, want: []identity.Perm{identity.PermLinksRead}},
		{name: "member scope dropped", raw: []string{"members:write"}, want: []identity.Perm{}},
		{name: "empty", raw: nil, want: []identity.Perm{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := authScopes(tt.raw)
			if !slices.Equal(got, tt.want) {
				t.Errorf("authScopes(%v) = %v, want %v", tt.raw, got, tt.want)
			}
			if !identity.ValidAPIKeyPrincipal(identity.Principal{ActorKind: identity.ActorAPIKey, Scopes: got}) {
				t.Errorf("principal built from %v must stay valid", got)
			}
		})
	}
}
