package identity

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"shortlink/internal/tenant"
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

func TestValidRole(t *testing.T) {
	tests := []struct {
		role Role
		want bool
	}{
		{role: RoleOwner, want: true},
		{role: RoleAdmin, want: true},
		{role: RoleMember, want: true},
		{role: RoleViewer, want: true},
		{role: "", want: false},
		{role: Role("superadmin"), want: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := ValidRole(tt.role); got != tt.want {
				t.Errorf("ValidRole(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}

func TestValidUserPrincipal(t *testing.T) {
	validTenant := tenant.Tenant{ID: 7}
	tests := []struct {
		name string
		p    Principal
		want bool
	}{
		{
			name: "valid owner",
			p:    Principal{Tenant: validTenant, ActorKind: ActorUser, ActorID: 10, Role: RoleOwner},
			want: true,
		},
		{
			name: "valid viewer",
			p:    Principal{Tenant: validTenant, ActorKind: ActorUser, ActorID: 10, Role: RoleViewer},
			want: true,
		},
		{
			name: "api key actor rejected",
			p:    Principal{Tenant: validTenant, ActorKind: ActorAPIKey, ActorID: 10, Role: RoleOwner},
			want: false,
		},
		{
			name: "missing actor id rejected",
			p:    Principal{Tenant: validTenant, ActorKind: ActorUser, Role: RoleMember},
			want: false,
		},
		{
			name: "missing tenant id rejected",
			p:    Principal{ActorKind: ActorUser, ActorID: 10, Role: RoleMember},
			want: false,
		},
		{
			name: "invalid role rejected",
			p:    Principal{Tenant: validTenant, ActorKind: ActorUser, ActorID: 10, Role: Role("guest")},
			want: false,
		},
		{
			name: "user with scopes rejected",
			p:    Principal{Tenant: validTenant, ActorKind: ActorUser, ActorID: 10, Role: RoleMember, Scopes: AllAPIKeyScopes()},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidUserPrincipal(tt.p); got != tt.want {
				t.Errorf("ValidUserPrincipal(%+v) = %v, want %v", tt.p, got, tt.want)
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

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "lowercase and trim", in: "  Alice@Example.COM ", want: "alice@example.com"},
		{name: "plus tag preserved", in: "alice+ops@sub.example.com", want: "alice+ops@sub.example.com"},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "missing at", in: "alice.example.com", wantErr: true},
		{name: "missing dot in domain", in: "alice@localhost", wantErr: true},
		{name: "leading dot domain", in: "alice@.example.com", wantErr: true},
		{name: "trailing dot domain", in: "alice@example.com.", wantErr: true},
		{name: "consecutive dots domain", in: "alice@example..com", wantErr: true},
		{name: "display name rejected", in: "Alice <alice@example.com>", wantErr: true},
		{name: "angle brackets rejected", in: "<alice@example.com>", wantErr: true},
		{name: "internal space rejected", in: "alice smith@example.com", wantErr: true},
		{name: "control char rejected", in: "alice\n@example.com", wantErr: true},
		{name: "too long", in: strings.Repeat("a", 310) + "@example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmail(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidEmail) {
					t.Fatalf("NormalizeEmail(%q) err = %v, want ErrInvalidEmail", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEmail(%q) err = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
