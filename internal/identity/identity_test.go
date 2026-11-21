package identity

import (
	"errors"
	"slices"
	"testing"
)

func TestAllAPIKeyScopesReturnsCopy(t *testing.T) {
	first := AllAPIKeyScopes()
	first[0] = PermMembersWrite

	second := AllAPIKeyScopes()
	want := []Perm{PermLinksRead, PermLinksWrite, PermStatsRead}
	if !slices.Equal(second, want) {
		t.Fatalf("AllAPIKeyScopes() = %v, want %v", second, want)
	}
}

func TestValidAPIKeyScopeMatchesAll(t *testing.T) {
	for _, perm := range AllAPIKeyScopes() {
		if !ValidAPIKeyScope(perm) {
			t.Errorf("ValidAPIKeyScope(%q) = false, want true", perm)
		}
	}
	if ValidAPIKeyScope(PermMembersWrite) {
		t.Error("ValidAPIKeyScope(members:write) = true, want false")
	}
}

func TestValidAPIKeyPrincipal(t *testing.T) {
	tests := []struct {
		name string
		p    Principal
		want bool
	}{
		{name: "api key with scopes", p: Principal{ActorKind: ActorAPIKey, Scopes: []Perm{PermLinksWrite}}, want: true},
		{name: "api key with no scopes", p: Principal{ActorKind: ActorAPIKey}, want: true},
		{name: "user", p: Principal{ActorKind: ActorUser, Role: RoleOwner, Scopes: AllAPIKeyScopes()}, want: false},
		{name: "member scope", p: Principal{ActorKind: ActorAPIKey, Scopes: []Perm{PermLinksRead, PermMembersWrite}}, want: false},
		{name: "empty kind", p: Principal{Scopes: AllAPIKeyScopes()}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidAPIKeyPrincipal(tt.p); got != tt.want {
				t.Errorf("ValidAPIKeyPrincipal(%+v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

func TestNormalizeAPIKeyScopes(t *testing.T) {
	tests := []struct {
		name    string
		in      []Perm
		want    []Perm
		wantErr bool
	}{
		{
			name: "single scope",
			in:   []Perm{PermLinksRead},
			want: []Perm{PermLinksRead},
		},
		{
			name: "dedup and canonical order",
			in:   []Perm{PermStatsRead, PermLinksRead, PermStatsRead, PermLinksWrite},
			want: []Perm{PermLinksRead, PermLinksWrite, PermStatsRead},
		},
		{
			name:    "nil slice rejected",
			in:      nil,
			wantErr: true,
		},
		{
			name:    "empty slice rejected",
			in:      []Perm{},
			wantErr: true,
		},
		{
			name:    "empty perm rejected",
			in:      []Perm{""},
			wantErr: true,
		},
		{
			name:    "members read rejected",
			in:      []Perm{PermMembersRead},
			wantErr: true,
		},
		{
			name:    "members write rejected",
			in:      []Perm{PermLinksRead, PermMembersWrite},
			wantErr: true,
		},
		{
			name:    "unknown scope rejected",
			in:      []Perm{Perm("links:delete")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeAPIKeyScopes(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidAPIKeyScope) {
					t.Fatalf("NormalizeAPIKeyScopes(%v) error = %v, want ErrInvalidAPIKeyScope", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAPIKeyScopes(%v) error = %v", tt.in, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("NormalizeAPIKeyScopes(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
