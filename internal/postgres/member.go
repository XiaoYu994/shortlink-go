package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shortlink/internal/identity"
)

type memberScope struct {
	actorFound  bool
	actorRole   identity.Role
	targetFound bool
	targetRole  identity.Role
	ownerCount  int
}

/*
ListMembers 返回指定组织下的成员列表，按 user_id 升序排列。
复用 idx_memberships_tenant_id 复合索引，JoinedAt 统一返回 UTC。
*/
func (s *Store) ListMembers(ctx context.Context, tenantID int64) ([]identity.Member, error) {
	if tenantID <= 0 {
		return nil, identity.ErrOrgNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT m.user_id, u.email, u.name, m.role, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1
ORDER BY m.user_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []identity.Member
	for rows.Next() {
		var (
			member  identity.Member
			roleStr string
		)
		if err := rows.Scan(&member.UserID, &member.Email, &member.Name, &roleStr, &member.JoinedAt); err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		role := identity.Role(roleStr)
		if !identity.ValidRole(role) {
			return nil, fmt.Errorf("list members: invalid role %q for user %d", roleStr, member.UserID)
		}
		member.Role = role
		member.JoinedAt = member.JoinedAt.UTC()
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

/*
lockMemberScopeTx 在事务内按 user_id 升序锁住操作者、目标成员和该组织的全部 owner 行。
统一锁序消除并发死锁，同时防住“两名 owner 并发降级/移出彼此导致组织零 owner”
或“并发刚被降权/移出组织的执行者仍按旧角色操作”的 TOCTOU 竞态。
在运行后续写入前必须先关闭 rows，避免同一事务连接占着结果集导致驱动报错。
*/
func lockMemberScopeTx(ctx context.Context, tx *sql.Tx, tenantID, actorUserID, targetUserID int64) (memberScope, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT user_id, role
FROM memberships
WHERE tenant_id = $1
  AND (user_id = $2 OR user_id = $3 OR role = 'owner')
ORDER BY user_id
FOR UPDATE`, tenantID, actorUserID, targetUserID)
	if err != nil {
		return memberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var scope memberScope
	for rows.Next() {
		var (
			uid     int64
			roleStr string
		)
		if err := rows.Scan(&uid, &roleStr); err != nil {
			return memberScope{}, fmt.Errorf("lock member scope: %w", err)
		}
		role := identity.Role(roleStr)
		if !identity.ValidRole(role) {
			return memberScope{}, fmt.Errorf("lock member scope: invalid role %q for user %d", roleStr, uid)
		}
		if uid == actorUserID {
			scope.actorFound = true
			scope.actorRole = role
		}
		if uid == targetUserID {
			scope.targetFound = true
			scope.targetRole = role
		}
		if role == identity.RoleOwner {
			scope.ownerCount++
		}
	}
	if err := rows.Err(); err != nil {
		return memberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	if err := rows.Close(); err != nil {
		return memberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	return scope, nil
}

/*
UpdateMemberRole 在事务内校验执行者实时角色、目标成员关系与至少一名 owner 不变量，并更新角色。
校验顺序确保 admin 尝试授予 owner 权限时即使目标不存在也返回 ErrMemberForbidden，
非组织成员的操作者返回 ErrOrgNotFound，最后一名 owner 被降级时返回 ErrLastOwner。
*/
func (s *Store) UpdateMemberRole(ctx context.Context, tenantID, actorUserID, targetUserID int64, newRole identity.Role) (identity.Member, error) {
	if tenantID <= 0 || actorUserID <= 0 {
		return identity.Member{}, identity.ErrOrgNotFound
	}
	if targetUserID <= 0 {
		return identity.Member{}, identity.ErrMemberNotFound
	}
	if !identity.ValidRole(newRole) {
		return identity.Member{}, fmt.Errorf("%w: %q", identity.ErrInvalidRole, newRole)
	}
	if err := ctx.Err(); err != nil {
		return identity.Member{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scope, err := lockMemberScopeTx(ctx, tx, tenantID, actorUserID, targetUserID)
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	switch {
	case !scope.actorFound:
		return identity.Member{}, identity.ErrOrgNotFound
	case !identity.CanManageMembers(scope.actorRole):
		return identity.Member{}, identity.ErrMemberForbidden
	case !identity.CanAssignMemberRole(scope.actorRole, newRole):
		return identity.Member{}, identity.ErrMemberForbidden
	case !scope.targetFound:
		return identity.Member{}, identity.ErrMemberNotFound
	case !identity.CanManageMemberRole(scope.actorRole, scope.targetRole, newRole):
		return identity.Member{}, identity.ErrMemberForbidden
	case scope.targetRole == identity.RoleOwner && newRole != identity.RoleOwner && scope.ownerCount <= 1:
		return identity.Member{}, identity.ErrLastOwner
	}

	var (
		member  identity.Member
		roleStr string
	)
	err = tx.QueryRowContext(ctx, `
UPDATE memberships m
SET role = $3
FROM users u
WHERE m.tenant_id = $1
  AND m.user_id = $2
  AND u.id = m.user_id
RETURNING m.user_id, u.email, u.name, m.role, m.created_at`,
		tenantID, targetUserID, string(newRole),
	).Scan(&member.UserID, &member.Email, &member.Name, &roleStr, &member.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Member{}, fmt.Errorf("update member role: user %d missing", targetUserID)
	}
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	role := identity.Role(roleStr)
	if !identity.ValidRole(role) {
		return identity.Member{}, fmt.Errorf("update member role: invalid role %q", roleStr)
	}
	member.Role = role
	member.JoinedAt = member.JoinedAt.UTC()

	if err := tx.Commit(); err != nil {
		return identity.Member{}, fmt.Errorf("update member role: commit: %w", err)
	}
	return member, nil
}

/*
RemoveMember 在事务内校验执行者实时角色、目标成员关系与至少一名 owner 不变量，并删除成员关系。
只删除 memberships 行，不吊销 sessions：用户可能同时属于多个组织，移出当前组织后其会话在其余组织仍有效。
*/
func (s *Store) RemoveMember(ctx context.Context, tenantID, actorUserID, targetUserID int64) error {
	if tenantID <= 0 || actorUserID <= 0 {
		return identity.ErrOrgNotFound
	}
	if targetUserID <= 0 {
		return identity.ErrMemberNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scope, err := lockMemberScopeTx(ctx, tx, tenantID, actorUserID, targetUserID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	switch {
	case !scope.actorFound:
		return identity.ErrOrgNotFound
	case !identity.CanManageMembers(scope.actorRole):
		return identity.ErrMemberForbidden
	case !scope.targetFound:
		return identity.ErrMemberNotFound
	case !identity.CanRemoveMember(scope.actorRole, scope.targetRole):
		return identity.ErrMemberForbidden
	case scope.targetRole == identity.RoleOwner && scope.ownerCount <= 1:
		return identity.ErrLastOwner
	}

	res, err := tx.ExecContext(ctx, `
DELETE FROM memberships
WHERE tenant_id = $1 AND user_id = $2`, tenantID, targetUserID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove member: rows affected: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("remove member: expected 1 row deleted, got %d", affected)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove member: commit: %w", err)
	}
	return nil
}
