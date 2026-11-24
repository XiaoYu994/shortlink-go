package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
)

const (
	listMembersSQL = `SELECT m.user_id, u.email, u.name, m.role, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1
ORDER BY m.user_id`

	lockMemberScopeSQL = `SELECT user_id, role
FROM memberships
WHERE tenant_id = $1
  AND (user_id = $2 OR user_id = $3 OR role = 'owner')
ORDER BY user_id
FOR UPDATE`

	getMemberSQL = `SELECT m.user_id, u.email, u.name, m.role, m.created_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1
  AND m.user_id = $2`

	updateMemberRoleSQL = `UPDATE memberships m
SET role = $3
FROM users u
WHERE m.tenant_id = $1
  AND m.user_id = $2
  AND u.id = m.user_id
RETURNING m.user_id, u.email, u.name, m.role, m.created_at`

	removeMemberSQL = `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`
)

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

	rows, err := s.db.QueryContext(ctx, listMembersSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []identity.Member
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

func scanMember(scanner rowScanner) (identity.Member, error) {
	var (
		member  identity.Member
		roleStr string
	)
	if err := scanner.Scan(&member.UserID, &member.Email, &member.Name, &roleStr, &member.JoinedAt); err != nil {
		return identity.Member{}, err
	}
	role := identity.Role(roleStr)
	if !role.Valid() {
		return identity.Member{}, fmt.Errorf("invalid role %q for user %d", roleStr, member.UserID)
	}
	member.Role = role
	member.JoinedAt = member.JoinedAt.UTC()
	return member, nil
}

/*
lockMemberScopeTx 在事务内按 user_id 升序锁住操作者、目标成员和该组织的全部 owner 行。
统一锁序消除并发死锁，同时防住“两名 owner 并发降级/移出彼此导致组织零 owner”
或“并发刚被降权/移出组织的执行者仍按旧角色操作”的 TOCTOU 竞态。
在运行后续写入前必须先关闭 rows，避免同一事务连接占着结果集导致驱动报错。
*/
func lockMemberScopeTx(ctx context.Context, tx *sql.Tx, tenantID, actorUserID, targetUserID int64) (identity.MemberScope, error) {
	rows, err := tx.QueryContext(ctx, lockMemberScopeSQL, tenantID, actorUserID, targetUserID)
	if err != nil {
		return identity.MemberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var scope identity.MemberScope
	for rows.Next() {
		var (
			uid     int64
			roleStr string
		)
		if err := rows.Scan(&uid, &roleStr); err != nil {
			return identity.MemberScope{}, fmt.Errorf("lock member scope: %w", err)
		}
		role := identity.Role(roleStr)
		if !role.Valid() {
			return identity.MemberScope{}, fmt.Errorf("lock member scope: invalid role %q for user %d", roleStr, uid)
		}
		if uid == actorUserID {
			scope.ActorFound = true
			scope.ActorRole = role
		}
		if uid == targetUserID {
			scope.TargetFound = true
			scope.TargetRole = role
		}
		if role == identity.RoleOwner {
			scope.OwnerCount++
		}
	}
	if err := rows.Err(); err != nil {
		return identity.MemberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	if err := rows.Close(); err != nil {
		return identity.MemberScope{}, fmt.Errorf("lock member scope: %w", err)
	}
	return scope, nil
}

/*
UpdateMemberRole 在事务内校验执行者实时角色、目标成员关系与至少一名 owner 不变量，并更新角色。
校验顺序确保 admin 尝试授予 owner 权限时即使目标不存在也返回 ErrMemberForbidden，
非组织成员的操作者返回 ErrOrgNotFound，最后一名 owner 被降级时返回 ErrLastOwner。
*/
func (s *Store) UpdateMemberRole(ctx context.Context, p identity.UpdateMemberRoleParams) (identity.Member, error) {
	if p.TenantID <= 0 || p.ActorUserID <= 0 {
		return identity.Member{}, identity.ErrOrgNotFound
	}
	if p.TargetUserID <= 0 {
		return identity.Member{}, identity.ErrMemberNotFound
	}
	if !p.NewRole.Valid() {
		return identity.Member{}, fmt.Errorf("%w: %q", identity.ErrInvalidRole, p.NewRole)
	}
	if err := ctx.Err(); err != nil {
		return identity.Member{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scope, err := lockMemberScopeTx(ctx, tx, p.TenantID, p.ActorUserID, p.TargetUserID)
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	if err := scope.CanUpdateRole(p.NewRole); err != nil {
		return identity.Member{}, err
	}
	// 幂等空操作不写数据库、不产生审计日志噪音（ADR 0023）。
	if scope.TargetRole == p.NewRole {
		member, err := scanMember(tx.QueryRowContext(ctx, getMemberSQL, p.TenantID, p.TargetUserID))
		if errors.Is(err, sql.ErrNoRows) {
			return identity.Member{}, fmt.Errorf("update member role: user %d missing", p.TargetUserID)
		}
		if err != nil {
			return identity.Member{}, fmt.Errorf("update member role: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return identity.Member{}, fmt.Errorf("update member role: commit: %w", err)
		}
		return member, nil
	}

	member, err := scanMember(tx.QueryRowContext(ctx, updateMemberRoleSQL, p.TenantID, p.TargetUserID, string(p.NewRole)))
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Member{}, fmt.Errorf("update member role: user %d missing", p.TargetUserID)
	}
	if err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	if err := insertAuditLogTx(ctx, tx, p.TenantID, audit.UserMeta(p.ActorUserID, ""), audit.ActionMemberRoleUpdate, audit.TargetMember, strconv.FormatInt(p.TargetUserID, 10), map[string]any{
		"email":         member.Email,
		"previous_role": string(scope.TargetRole),
		"new_role":      string(member.Role),
	}); err != nil {
		return identity.Member{}, fmt.Errorf("update member role: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return identity.Member{}, fmt.Errorf("update member role: commit: %w", err)
	}
	return member, nil
}

/*
RemoveMember 在事务内校验执行者实时角色、目标成员关系与至少一名 owner 不变量，并删除成员关系。
只删除 memberships 行，不吊销 sessions：用户可能同时属于多个组织，移出当前组织后其会话在其余组织仍有效。
*/
func (s *Store) RemoveMember(ctx context.Context, p identity.RemoveMemberParams) error {
	if p.TenantID <= 0 || p.ActorUserID <= 0 {
		return identity.ErrOrgNotFound
	}
	if p.TargetUserID <= 0 {
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

	scope, err := lockMemberScopeTx(ctx, tx, p.TenantID, p.ActorUserID, p.TargetUserID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if err := scope.CanRemove(); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, removeMemberSQL, p.TenantID, p.TargetUserID)
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
	if err := insertAuditLogTx(ctx, tx, p.TenantID, audit.UserMeta(p.ActorUserID, ""), audit.ActionMemberRemove, audit.TargetMember, strconv.FormatInt(p.TargetUserID, 10), map[string]any{
		"previous_role": string(scope.TargetRole),
	}); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove member: commit: %w", err)
	}
	return nil
}
