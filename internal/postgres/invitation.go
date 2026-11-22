package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shortlink/internal/identity"
)

const (
	invitationPendingConstraint = "uq_invitations_pending_tenant_email"
	invitationExpiresConstraint = "ck_invitations_expires_after_created"
)

var (
	// ErrInvitationPending 表示同一租户对该邮箱已存在尚未过期、未撤销且未兑现的邀请。
	ErrInvitationPending = errors.New("pending invitation already exists")
	// ErrInvitationExpiresNotFuture 表示创建邀请时给出的过期时间不在将来。
	ErrInvitationExpiresNotFuture = errors.New("create invitation: expires_at must be in the future")
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
	if p.TenantID <= 0 {
		return identity.Invitation{}, ErrTenantNotFound
	}
	email, err := identity.NormalizeEmail(p.Email)
	if err != nil {
		return identity.Invitation{}, err
	}
	if !p.Role.Valid() {
		return identity.Invitation{}, fmt.Errorf("%w: %q", identity.ErrInvalidRole, p.Role)
	}
	if p.InvitedBy != nil && *p.InvitedBy <= 0 {
		return identity.Invitation{}, errors.New("create invitation: invited_by must be positive")
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return identity.Invitation{}, ErrInvitationExpiresNotFuture
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
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, p.TenantID).Scan(&exists); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: check tenant: %w", err)
	}
	if !exists {
		return identity.Invitation{}, ErrTenantNotFound
	}

	// 部分唯一索引只看 accepted_at 和 revoked_at，不看 expires_at；先关闭已过期旧邀请再插入。
	if _, err := tx.ExecContext(ctx, `
UPDATE invitations
SET revoked_at = now()
WHERE tenant_id = $1
  AND email = $2
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at IS NOT NULL
  AND expires_at <= now()`,
		p.TenantID, email,
	); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: expire stale: %w", err)
	}

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
	err = tx.QueryRowContext(ctx, `
INSERT INTO invitations (tenant_id, email, role, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, tenant_id, email, role, invited_by, created_at, expires_at`,
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
	if scannedExpiresAt.Valid {
		t := scannedExpiresAt.Time.UTC()
		inv.ExpiresAt = &t
	}

	if err := tx.Commit(); err != nil {
		return identity.Invitation{}, fmt.Errorf("create invitation: commit: %w", err)
	}
	return inv, nil
}
