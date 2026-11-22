package identity

import (
	"fmt"
	"slices"
	"testing"
)

func TestCanManageMembers(t *testing.T) {
	want := map[Role]bool{
		RoleOwner:          true,
		RoleAdmin:          true,
		RoleMember:         false,
		RoleViewer:         false,
		Role(""):           false,
		Role("superadmin"): false,
	}
	for role, ok := range want {
		t.Run(string(role), func(t *testing.T) {
			if got := CanManageMembers(role); got != ok {
				t.Errorf("CanManageMembers(%q) = %v, want %v", role, got, ok)
			}
		})
	}
}

func TestCanAssignMemberRole(t *testing.T) {
	allRoles := []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer, Role(""), Role("guest")}
	want := map[Role]map[Role]bool{
		RoleOwner: {
			RoleOwner:  true,
			RoleAdmin:  true,
			RoleMember: true,
			RoleViewer: true,
		},
		RoleAdmin: {
			RoleOwner:  false,
			RoleAdmin:  true,
			RoleMember: true,
			RoleViewer: true,
		},
		RoleMember: {},
		RoleViewer: {},
	}
	for _, actor := range allRoles {
		for _, next := range allRoles {
			t.Run(fmt.Sprintf("%s_assign_%s", actor, next), func(t *testing.T) {
				wantOK := want[actor][next]
				if got := CanAssignMemberRole(actor, next); got != wantOK {
					t.Errorf("CanAssignMemberRole(%q, %q) = %v, want %v", actor, next, got, wantOK)
				}
			})
		}
	}
}

func TestCanManageMemberRole(t *testing.T) {
	allRoles := []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer, Role(""), Role("guest")}
	allowedTargets := []struct {
		actor   Role
		targets []Role
		assigns []Role
	}{
		{
			actor:   RoleOwner,
			targets: []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer},
			assigns: []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer},
		},
		{
			actor:   RoleAdmin,
			targets: []Role{RoleAdmin, RoleMember, RoleViewer},
			assigns: []Role{RoleAdmin, RoleMember, RoleViewer},
		},
	}

	isAllowed := func(actor, target, next Role) bool {
		for _, rule := range allowedTargets {
			if rule.actor == actor {
				return slices.Contains(rule.targets, target) && slices.Contains(rule.assigns, next)
			}
		}
		return false
	}

	for _, actor := range allRoles {
		for _, target := range allRoles {
			for _, next := range allRoles {
				name := fmt.Sprintf("%s->%s_as_%s", actor, target, next)
				t.Run(name, func(t *testing.T) {
					want := isAllowed(actor, target, next)
					if got := CanManageMemberRole(actor, target, next); got != want {
						t.Errorf("CanManageMemberRole(%q, %q, %q) = %v, want %v", actor, target, next, got, want)
					}
				})
			}
		}
	}
}

func TestCanRemoveMember(t *testing.T) {
	allRoles := []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer, Role(""), Role("guest")}
	want := map[Role]map[Role]bool{
		RoleOwner: {
			RoleOwner:  true,
			RoleAdmin:  true,
			RoleMember: true,
			RoleViewer: true,
		},
		RoleAdmin: {
			RoleOwner:  false,
			RoleAdmin:  true,
			RoleMember: true,
			RoleViewer: true,
		},
		RoleMember: {},
		RoleViewer: {},
	}

	for _, actor := range allRoles {
		for _, target := range allRoles {
			name := fmt.Sprintf("%s_removes_%s", actor, target)
			t.Run(name, func(t *testing.T) {
				wantOK := want[actor][target]
				if got := CanRemoveMember(actor, target); got != wantOK {
					t.Errorf("CanRemoveMember(%q, %q) = %v, want %v", actor, target, got, wantOK)
				}
			})
		}
	}
}
