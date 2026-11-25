package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

const (
	tenantRLSRole    = "shortlink_tenant"
	setTenantRoleSQL = `SET LOCAL ROLE ` + tenantRLSRole
	setTenantIDSQL   = `SELECT set_config('app.tenant_id', $1, true)`
)

/*
applyTenantRLSContext 在当前数据库事务内降权为受限角色 shortlink_tenant 并绑定本事务级租户上下文（ADR 0028）。
SET LOCAL 与 set_config(..., is_local=true) 只在当前事务内生效，Commit/Rollback 后物理连接归还池前自动清除，
避免复用连接时把上一个租户的上下文泄漏给后续请求。
*/
func applyTenantRLSContext(ctx context.Context, tx *sql.Tx, tenantID int64) error {
	if tenantID <= 0 {
		return fmt.Errorf("tenant rls context: tenant id must be positive, got %d", tenantID)
	}
	if _, err := tx.ExecContext(ctx, setTenantRoleSQL); err != nil {
		return fmt.Errorf("set tenant rls role: %w", err)
	}
	if _, err := tx.ExecContext(ctx, setTenantIDSQL, strconv.FormatInt(tenantID, 10)); err != nil {
		return fmt.Errorf("set tenant rls id: %w", err)
	}
	return nil
}

func (s *Store) withTenantTx(ctx context.Context, tenantID int64, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin tenant tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applyTenantRLSContext(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant tx: %w", err)
	}
	return nil
}

func (s *Store) withTenantReadTx(ctx context.Context, tenantID int64, fn func(*sql.Tx) error) error {
	return s.withTenantTx(ctx, tenantID, &sql.TxOptions{ReadOnly: true}, fn)
}
