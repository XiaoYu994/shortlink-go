package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
)

const (
	invitationPendingConstraint = "uq_invitations_pending_tenant_email"
	invitationExpiresConstraint = "ck_invitations_expires_after_created"

	tenantExistsSQL = `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`

	// 部分唯一索引不看 expires_at；重邀前把已过期旧邀请标记为 superseded_at，保留其 expired 历史语义，不再借写 revoked_at（ADR 0022）。
	expireStaleInvitationsSQL = `UPDATE invitations
SET superseded_at = now()
WHERE tenant_id = $1
  AND email = $2
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND superseded_at IS NULL
  AND expires_at IS NOT NULL
  AND expires_at <= now()`

	insertInvitationSQL = `INSERT INTO invitations (tenant_id, email, role, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, tenant_id, email, role, invited_by, created_at, expires_at`

	listInvitationsSQL = `SELECT id, tenant_id, email, role, invited_by, created_at, expires_at, accepted_at, accepted_by, revoked_at
FROM invitations
WHERE tenant_id = $1
ORDER BY id DESC`

	lockInvitationForRevokeSQL = `SELECT email, role, accepted_at, revoked_at, superseded_at, (expires_at IS NOT NULL AND expires_at <= now()) AS expired
FROM invitations
WHERE id = $1 AND tenant_id = $2
FOR UPDATE`

	revokeInvitationSQL = `UPDATE invitations
SET revoked_at = now()
WHERE id = $1 AND tenant_id = $2`

	// 三条清理语句分别沿迁移 012 的部分索引按批次删除，禁止用 OR 混合条件。
	cleanupExpiredInvitationsByExpirySQL = `DELETE FROM invitations
WHERE id IN (
    SELECT id
    FROM invitations
    WHERE expires_at IS NOT NULL
      AND expires_at < now() - ($1::bigint * interval '1 microsecond')
      AND accepted_at IS NULL
    ORDER BY expires_at, id
    LIMIT $2
)`

	cleanupRevokedInvitationsSQL = `DELETE FROM invitations
WHERE id IN (
    SELECT id
    FROM invitations
    WHERE revoked_at IS NOT NULL
      AND revoked_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY revoked_at, id
    LIMIT $2
)`

	cleanupSupersededInvitationsSQL = `DELETE FROM invitations
WHERE id IN (
    SELECT id
    FROM invitations
    WHERE superseded_at IS NOT NULL
      AND superseded_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY superseded_at, id
    LIMIT $2
)`
)

var (
	// ErrInvitationPending 表示同一租户对该邮箱已存在尚未过期、未撤销且未兑现的邀请。
	ErrInvitationPending = identity.ErrInvitationPending
	// ErrInvitationExpiresNotFuture 表示创建邀请时给出的过期时间不在将来。
	ErrInvitationExpiresNotFuture = identity.ErrInvitationExpiresNotFuture
)

/*
CreateInvitationParams 指定要登记的组织成员邀请。
InvitedBy 为空表示由运维命令创建；ExpiresAt 为空表示永不过期。
*/
type CreateInvitationParams struct {
	TenantID  int64
	Email     string
	Role      identity.Role
	InvitedBy *int64
	ExpiresAt *time.Time
}

/*
CreateInvitation 在指定租户下登记一条待兑现的成员邀请。
同租户同邮箱若只剩下已经过期但尚未清理的邀请，会在同一事务里先将其标记为已撤销，
再写入新邀请，避免部分唯一索引永久阻塞过期后的重邀。
*/
func (s *Store) CreateInvitation(ctx context.Context, p CreateInvitationParams) (identity.Invitation, error) {
	email, err := validateCreateInvitation(p)
	if err != nil {
		return identity.Invitation{}, err
	}
	if err := ctx.Err(); err != nil {
		return identity.Invitation{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists bool
	if err := tx.QueryRowContext(ctx, tenantExistsSQL, p.TenantID).Scan(&exists); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: check tenant: %w", err)
	}
	if !exists {
		return identity.Invitation{}, ErrTenantNotFound
	}
	if _, err := tx.ExecContext(ctx, expireStaleInvitationsSQL, p.TenantID, email); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: expire stale: %w", err)
	}

	inv, err := insertInvitationTx(ctx, tx, p, email)
	if err != nil {
		return identity.Invitation{}, err
	}
	auditMeta := map[string]any{
		"email": inv.Email,
		"role":  string(inv.Role),
	}
	if inv.ExpiresAt != nil {
		auditMeta["expires_at"] = inv.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	fallback := audit.OperatorMeta("")
	if p.InvitedBy != nil {
		fallback = audit.UserMeta(*p.InvitedBy, "")
	}
	if err := insertAuditLogTx(ctx, tx, inv.TenantID, fallback, audit.ActionInvitationCreate, audit.TargetInvitation, strconv.FormatInt(inv.ID, 10), auditMeta); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: commit: %w", err)
	}
	return inv, nil
}

func validateCreateInvitation(p CreateInvitationParams) (string, error) {
	if p.TenantID <= 0 {
		return "", ErrTenantNotFound
	}
	email, err := identity.NormalizeEmail(p.Email)
	if err != nil {
		return "", err
	}
	if !p.Role.Valid() {
		return "", fmt.Errorf("%w: %q", identity.ErrInvalidRole, p.Role)
	}
	if p.InvitedBy != nil && *p.InvitedBy <= 0 {
		return "", errors.New("create invitation: invited_by must be positive")
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return "", ErrInvitationExpiresNotFuture
	}
	return email, nil
}

func insertInvitationTx(ctx context.Context, tx *sql.Tx, p CreateInvitationParams, email string) (identity.Invitation, error) {
	var invitedBy sql.NullInt64
	if p.InvitedBy != nil {
		invitedBy = sql.NullInt64{Int64: *p.InvitedBy, Valid: true}
	}
	var expiresAt sql.NullTime
	if p.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: p.ExpiresAt.UTC(), Valid: true}
	}

	var (
		inv              identity.Invitation
		scannedRole      string
		scannedInvitedBy sql.NullInt64
		scannedExpiresAt sql.NullTime
	)
	err := tx.QueryRowContext(ctx, insertInvitationSQL,
		p.TenantID, email, string(p.Role), invitedBy, expiresAt,
	).Scan(&inv.ID, &inv.TenantID, &inv.Email, &scannedRole, &scannedInvitedBy, &inv.CreatedAt, &scannedExpiresAt)
	if err != nil {
		if isConstraintViolation(err, "23505", invitationPendingConstraint) {
			return identity.Invitation{}, ErrInvitationPending
		}
		if isConstraintViolation(err, "23514", invitationExpiresConstraint) {
			return identity.Invitation{}, ErrInvitationExpiresNotFuture
		}
		return identity.Invitation{}, fmt.Errorf("create invitation: insert: %w", err)
	}

	inv.Role = identity.Role(scannedRole)
	inv.CreatedAt = inv.CreatedAt.UTC()
	if scannedInvitedBy.Valid {
		v := scannedInvitedBy.Int64
		inv.InvitedBy = &v
	}
	inv.ExpiresAt = nullTimeUTC(scannedExpiresAt)
	return inv, nil
}

// CreateOrgInvitation 给 HTTP 管理接口使用，复用 CreateInvitation 并记录发起人 user_id。
func (s *Store) CreateOrgInvitation(ctx context.Context, p identity.CreateInvitationParams) (identity.Invitation, error) {
	if p.InvitedBy <= 0 {
		return identity.Invitation{}, errors.New("create invitation: invited_by must be positive")
	}
	invitedBy := p.InvitedBy
	inv, err := s.CreateInvitation(ctx, CreateInvitationParams{
		TenantID:  p.TenantID,
		Email:     p.Email,
		Role:      p.Role,
		InvitedBy: &invitedBy,
		ExpiresAt: p.ExpiresAt,
	})
	if errors.Is(err, ErrTenantNotFound) {
		return identity.Invitation{}, identity.ErrOrgNotFound
	}
	return inv, err
}

// ListInvitations 按 id 降序返回指定组织的全部邀请（含待兑现、已兑现、已撤销和已过期）。
func (s *Store) ListInvitations(ctx context.Context, tenantID int64) ([]identity.Invitation, error) {
	if tenantID <= 0 {
		return nil, identity.ErrOrgNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, listInvitationsSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []identity.Invitation
	for rows.Next() {
		var (
			inv        identity.Invitation
			roleStr    string
			invitedBy  sql.NullInt64
			expiresAt  sql.NullTime
			acceptedAt sql.NullTime
			acceptedBy sql.NullInt64
			revokedAt  sql.NullTime
		)
		if err := rows.Scan(
			&inv.ID, &inv.TenantID, &inv.Email, &roleStr, &invitedBy,
			&inv.CreatedAt, &expiresAt, &acceptedAt, &acceptedBy, &revokedAt,
		); err != nil {
			return nil, fmt.Errorf("list invitations: %w", err)
		}
		inv.Role = identity.Role(roleStr)
		inv.CreatedAt = inv.CreatedAt.UTC()
		if invitedBy.Valid {
			v := invitedBy.Int64
			inv.InvitedBy = &v
		}
		inv.ExpiresAt = nullTimeUTC(expiresAt)
		inv.AcceptedAt = nullTimeUTC(acceptedAt)
		if acceptedBy.Valid {
			v := acceptedBy.Int64
			inv.AcceptedBy = &v
		}
		inv.RevokedAt = nullTimeUTC(revokedAt)
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	return out, nil
}

/*
RevokeInvitation 撤销指定组织下的一条待兑现邀请（ADR 0021）。
不存在或属于其他组织返回 identity.ErrInvitationNotFound；
admin 撤销 owner 邀请返回 identity.ErrMemberForbidden；
已兑现或已撤销返回 identity.ErrInvitationNotPending。
*/
func (s *Store) RevokeInvitation(ctx context.Context, p identity.RevokeInvitationParams) error {
	if p.TenantID <= 0 || p.InvitationID <= 0 {
		return identity.ErrInvitationNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		email        string
		roleStr      string
		acceptedAt   sql.NullTime
		revokedAt    sql.NullTime
		supersededAt sql.NullTime
		expired      bool
	)
	err = tx.QueryRowContext(ctx, lockInvitationForRevokeSQL, p.InvitationID, p.TenantID).Scan(
		&email, &roleStr, &acceptedAt, &revokedAt, &supersededAt, &expired,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrInvitationNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	targetRole := identity.Role(roleStr)
	if !identity.CanAssignMemberRole(p.ActorRole, targetRole) {
		return identity.ErrMemberForbidden
	}
	if acceptedAt.Valid || revokedAt.Valid || supersededAt.Valid || expired {
		return identity.ErrInvitationNotPending
	}

	if _, err := tx.ExecContext(ctx, revokeInvitationSQL, p.InvitationID, p.TenantID); err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if err := insertAuditLogTx(ctx, tx, p.TenantID, audit.SystemMeta(""), audit.ActionInvitationRevoke, audit.TargetInvitation, strconv.FormatInt(p.InvitationID, 10), map[string]any{
		"email": email,
		"role":  string(targetRole),
	}); err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("revoke invitation: commit: %w", err)
	}
	return nil
}

// CleanupExpiredInvitationsByExpiry 分批删除超过保留期的已过期未兑现邀请。
func (s *Store) CleanupExpiredInvitationsByExpiry(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup expired invitations", cleanupExpiredInvitationsByExpirySQL, retention, limit)
}

// CleanupRevokedInvitations 分批删除超过保留期的已撤销邀请。
func (s *Store) CleanupRevokedInvitations(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup revoked invitations", cleanupRevokedInvitationsSQL, retention, limit)
}

// CleanupSupersededInvitations 分批删除超过保留期的被重邀替代邀请。
func (s *Store) CleanupSupersededInvitations(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup superseded invitations", cleanupSupersededInvitationsSQL, retention, limit)
}
