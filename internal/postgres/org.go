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
	lockUserForOrgCreateSQL = `SELECT id FROM users WHERE id = $1 FOR UPDATE`

	countOwnedOrgsSQL = `SELECT count(*) FROM memberships WHERE user_id = $1 AND role = 'owner'`

	insertOrgTenantSQL = `INSERT INTO tenants (name) VALUES ($1) RETURNING id`

	insertOrgOwnerMembershipSQL = `INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'owner') RETURNING created_at`
)

/*
CreateOrgForUser 在同一事务内为用户创建新组织（tenants 行）并写入 owner 成员关系（ADR 0021）。
先 FOR UPDATE 锁住 users 行再统计已拥有的 owner 组织数，防止同一用户并发建组织绕过上限。
*/
func (s *Store) CreateOrgForUser(ctx context.Context, userID int64, name string, maxOwned int) (identity.CreatedOrg, error) {
	if userID <= 0 {
		return identity.CreatedOrg{}, identity.ErrUnauthenticated
	}
	normName, err := identity.NormalizeOrgName(name)
	if err != nil {
		return identity.CreatedOrg{}, err
	}
	if maxOwned <= 0 {
		maxOwned = identity.DefaultMaxOwnedOrgs
	}
	if err := ctx.Err(); err != nil {
		return identity.CreatedOrg{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var lockedUserID int64
	err = tx.QueryRowContext(ctx, lockUserForOrgCreateSQL, userID).Scan(&lockedUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.CreatedOrg{}, identity.ErrUnauthenticated
	}
	if err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: lock user: %w", err)
	}

	var owned int
	if err := tx.QueryRowContext(ctx, countOwnedOrgsSQL, userID).Scan(&owned); err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: count owned orgs: %w", err)
	}
	if owned >= maxOwned {
		return identity.CreatedOrg{}, identity.ErrOrgLimitExceeded
	}

	var (
		orgID     int64
		createdAt time.Time
	)
	if err := tx.QueryRowContext(ctx, insertOrgTenantSQL, normName).Scan(&orgID); err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: insert tenant: %w", err)
	}
	if err := tx.QueryRowContext(ctx, insertOrgOwnerMembershipSQL, userID, orgID).Scan(&createdAt); err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: insert owner membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return identity.CreatedOrg{}, fmt.Errorf("create org: commit: %w", err)
	}

	return identity.CreatedOrg{
		ID:        orgID,
		Name:      normName,
		Role:      identity.RoleOwner,
		CreatedAt: createdAt.UTC(),
	}, nil
}
