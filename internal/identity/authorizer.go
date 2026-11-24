package identity

import "slices"

/*
MapAuthorizer 按固定角色权限表和 API Key scope 做权限判断。
它是 console.Authorizer 策略接口的默认实现；以后替换成策略引擎时，
handler 和中间件不需要改动。
*/
type MapAuthorizer struct {
	rolePerms map[Role]map[Perm]struct{}
}

// NewMapAuthorizer 构造固定角色到权限的映射表。
func NewMapAuthorizer() *MapAuthorizer {
	return &MapAuthorizer{
		rolePerms: map[Role]map[Perm]struct{}{
			RoleOwner: permSet(
				PermLinksRead,
				PermLinksWrite,
				PermStatsRead,
				PermMembersRead,
				PermMembersWrite,
				PermAPIKeysRead,
				PermAPIKeysWrite,
				PermAuditRead,
			),
			RoleAdmin: permSet(
				PermLinksRead,
				PermLinksWrite,
				PermStatsRead,
				PermMembersRead,
				PermMembersWrite,
				PermAPIKeysRead,
				PermAPIKeysWrite,
				PermAuditRead,
			),
			RoleMember: permSet(
				PermLinksRead,
				PermLinksWrite,
				PermStatsRead,
				PermMembersRead,
			),
			RoleViewer: permSet(
				PermLinksRead,
				PermStatsRead,
				PermMembersRead,
			),
		},
	}
}

func permSet(perms ...Perm) map[Perm]struct{} {
	set := make(map[Perm]struct{}, len(perms))
	for _, perm := range perms {
		set[perm] = struct{}{}
	}
	return set
}

/*
Can 判断执行者是否拥有指定权限，采取默认拒绝（fail-closed）：
接收者为空、权限为空、未知执行者类型或未映射的角色一律返回 false。
*/
func (a *MapAuthorizer) Can(p Principal, perm Perm) bool {
	if a == nil || perm == "" {
		return false
	}
	switch p.ActorKind {
	case ActorUser:
		perms, ok := a.rolePerms[p.Role]
		if !ok {
			return false
		}
		_, allowed := perms[perm]
		return allowed
	case ActorAPIKey:
		return slices.Contains(p.Scopes, perm)
	default:
		return false
	}
}
