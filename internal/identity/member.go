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

/*
MemberScope 描述在单次成员变更事务中锁定并读取到的执行者、目标成员及当前 owner 数量快照。
校验逻辑放在领域层，确保“无组织权限、禁止越权、目标不存在、禁止移除/降级最后一名 owner”
四条不变量与存储实现解耦（ADR 0024）。
*/
type MemberScope struct {
	ActorFound  bool
	ActorRole   Role
	TargetFound bool
	TargetRole  Role
	OwnerCount  int
}

// CanUpdateRole 校验执行者是否允许把目标成员角色更新为 newRole。
func (s MemberScope) CanUpdateRole(newRole Role) error {
	switch {
	case !s.ActorFound:
		return ErrOrgNotFound
	case !CanAssignMemberRole(s.ActorRole, newRole):
		return ErrMemberForbidden
	case !s.TargetFound:
		return ErrMemberNotFound
	case !CanManageMemberRole(s.ActorRole, s.TargetRole, newRole):
		return ErrMemberForbidden
	case s.TargetRole == RoleOwner && newRole != RoleOwner && s.OwnerCount <= 1:
		return ErrLastOwner
	default:
		return nil
	}
}

// CanRemove 校验执行者是否允许把目标成员移出组织。
func (s MemberScope) CanRemove() error {
	switch {
	case !s.ActorFound:
		return ErrOrgNotFound
	case !CanManageMembers(s.ActorRole):
		return ErrMemberForbidden
	case !s.TargetFound:
		return ErrMemberNotFound
	case !CanRemoveMember(s.ActorRole, s.TargetRole):
		return ErrMemberForbidden
	case s.TargetRole == RoleOwner && s.OwnerCount <= 1:
		return ErrLastOwner
	default:
		return nil
	}
}
