package identity

import (
	"errors"
	"time"
)

var (
	// ErrMemberNotFound 表示目标用户不是当前组织的成员。
	ErrMemberNotFound = errors.New("member not found")
	// ErrMemberForbidden 表示当前执行者无权对目标成员执行该操作。
	ErrMemberForbidden = errors.New("forbidden")
	// ErrLastOwner 表示组织至少需要保留一名 owner，不能降级或移出最后一位 owner。
	ErrLastOwner = errors.New("organization must have at least one owner")
)

// Member 表示用户与组织之间的成员关系快照。
type Member struct {
	UserID   int64
	Email    string
	Name     string
	Role     Role
	JoinedAt time.Time
}

/*
UpdateMemberRoleParams 是修改组织成员角色的参数。
使用具名结构体避免连续三个 int64 位置参数在调用方写反。
*/
type UpdateMemberRoleParams struct {
	TenantID     int64
	ActorUserID  int64
	TargetUserID int64
	NewRole      Role
}

/*
RemoveMemberParams 是将成员移出组织的参数。
使用具名结构体避免连续三个 int64 位置参数在调用方写反。
*/
type RemoveMemberParams struct {
	TenantID     int64
	ActorUserID  int64
	TargetUserID int64
}

// CanManageMembers 判断执行者角色是否拥有管理组织成员的基础资格（owner 或 admin）。
func CanManageMembers(actorRole Role) bool {
	return actorRole == RoleOwner || actorRole == RoleAdmin
}

/*
CanAssignMemberRole 判断执行者角色是否有权在组织内授予 newRole。
采取默认拒绝：任一角色非法均返回 false；owner 可授予任意合法角色，
admin 不能把任何人设为 owner，其余角色无权授予成员角色。
*/
func CanAssignMemberRole(actorRole, newRole Role) bool {
	if !CanManageMembers(actorRole) || !newRole.Valid() {
		return false
	}
	return actorRole == RoleOwner || newRole != RoleOwner
}

/*
CanManageMemberRole 判断执行者（actorRole）是否有权把目标成员（targetRole）的角色改为 newRole。
采取默认拒绝：任一角色非法均返回 false；owner 可修改任意角色（最后一名 owner 不变量由存储层事务保证）；
admin 不能修改 owner，也不能把任何人设为 owner，但可以调整 admin、member、viewer（含自我降级）。
*/
func CanManageMemberRole(actorRole, targetRole, newRole Role) bool {
	return CanAssignMemberRole(actorRole, newRole) && CanRemoveMember(actorRole, targetRole)
}

/*
CanRemoveMember 判断执行者（actorRole）是否有权把目标成员（targetRole）移出组织。
owner 可移出任意成员（最后一名 owner 不变量由存储层事务保证）；
admin 不能移出 owner，但可以移出 admin、member、viewer（含自行退出）。
*/
func CanRemoveMember(actorRole, targetRole Role) bool {
	if !CanManageMembers(actorRole) || !targetRole.Valid() {
		return false
	}
	return actorRole == RoleOwner || targetRole != RoleOwner
}
