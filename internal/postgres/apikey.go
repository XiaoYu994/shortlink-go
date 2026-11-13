package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"shortlink/internal/tenant"
)

var (
	// ErrIssueTarget 表示没有唯一确定把密钥发给哪个租户。
	ErrIssueTarget = errors.New("issue api key: provide either tenant id or a new tenant name")
	// ErrExternalIDOnExisting 表示给已有租户签发时又带了外部编号。
	ErrExternalIDOnExisting = errors.New("issue api key: external id only applies to a new tenant")
	// ErrExpiresNotFuture 表示签发时给出的到期时间不是将来。
	ErrExpiresNotFuture = errors.New("issue api key: expires_at must be in the future")
	// ErrTenantNotFound 表示租户不存在。
	ErrTenantNotFound = errors.New("tenant not found")
	// ErrAPIKeyNotFound 表示密钥行不存在。
	ErrAPIKeyNotFound = errors.New("api key not found")
	// ErrAPIKeyRevoked 表示这把密钥已经吊销过。
	ErrAPIKeyRevoked = errors.New("api key already revoked")
)

/*
APIKey 是可以给运维看的密钥状态。
没有明文，也没有哈希：哈希不能当凭证用，但也不必出现在列表里。
*/
type APIKey struct {
	ID        int64
	TenantID  int64
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// IssuedAPIKey 是签发结果。Plaintext 只在这一次交给调用方。
type IssuedAPIKey struct {
	Tenant    tenant.Tenant
	Key       APIKey
	Plaintext string
}

/*
IssueAPIKeyParams 指定把密钥发给谁。
TenantID 和 Name 只能填一个：前者给已有租户加一把密钥，后者新建租户再签发。
ExternalID 只在新建时写入；空字符串存成 NULL，避免占住唯一约束。
ExpiresAt 为 nil 表示不过期。
*/
type IssueAPIKeyParams struct {
	TenantID   int64
	Name       string
	ExternalID string
	ExpiresAt  *time.Time
}

/*
IssueAPIKey 新建或找到租户，生成一把密钥，把哈希和可选的到期时间写入 api_keys。
租户和密钥在同一个事务里：插入密钥失败时，新建的租户也会回滚。
*/
func (s *Store) IssueAPIKey(ctx context.Context, p IssueAPIKeyParams) (IssuedAPIKey, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.ExternalID = strings.TrimSpace(p.ExternalID)
	if err := validateIssue(p); err != nil {
		return IssuedAPIKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return IssuedAPIKey{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	tn, err := tenantForIssue(ctx, tx, p)
	if err != nil {
		return IssuedAPIKey{}, err
	}

	plaintext, hash, err := tenant.GenerateAPIKey()
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}

	var expires *time.Time
	if p.ExpiresAt != nil {
		t := p.ExpiresAt.UTC()
		expires = &t
	}
	key := APIKey{TenantID: tn.ID, ExpiresAt: expires}
	var scannedExpires sql.NullTime
	err = tx.QueryRowContext(ctx, `
INSERT INTO api_keys (tenant_id, key_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING id, created_at, expires_at`, tn.ID, hash, expires).Scan(&key.ID, &key.CreatedAt, &scannedExpires)
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}
	if scannedExpires.Valid {
		t := scannedExpires.Time.UTC()
		key.ExpiresAt = &t
	}

	if err := tx.Commit(); err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}
	return IssuedAPIKey{Tenant: tn, Key: key, Plaintext: plaintext}, nil
}

func validateIssue(p IssueAPIKeyParams) error {
	hasID := p.TenantID != 0
	hasName := p.Name != ""
	if hasID == hasName || p.TenantID < 0 {
		return ErrIssueTarget
	}
	if hasID && p.ExternalID != "" {
		return ErrExternalIDOnExisting
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return ErrExpiresNotFuture
	}
	return nil
}

func tenantForIssue(ctx context.Context, tx *sql.Tx, p IssueAPIKeyParams) (tenant.Tenant, error) {
	var tn tenant.Tenant
	if p.TenantID != 0 {
		var externalID sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT id, external_id FROM tenants WHERE id = $1`, p.TenantID).Scan(&tn.ID, &externalID)
		if errors.Is(err, sql.ErrNoRows) {
			return tenant.Tenant{}, ErrTenantNotFound
		}
		if err != nil {
			return tenant.Tenant{}, fmt.Errorf("issue api key: %w", err)
		}
		if externalID.Valid {
			tn.ExternalID = externalID.String
		}
		return tn, nil
	}

	var external any
	if p.ExternalID != "" {
		external = p.ExternalID
	}
	err := tx.QueryRowContext(ctx, `
INSERT INTO tenants (name, external_id) VALUES ($1, $2) RETURNING id`, p.Name, external).Scan(&tn.ID)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("issue api key: %w", err)
	}
	tn.ExternalID = p.ExternalID
	return tn, nil
}

/*
ListAPIKeys 返回一个租户的密钥状态，包含已吊销的行。
租户不存在时返回 ErrTenantNotFound，避免把写错的 ID 看成没有密钥。
*/
func (s *Store) ListAPIKeys(ctx context.Context, tenantID int64) ([]APIKey, error) {
	if tenantID <= 0 {
		return nil, ErrTenantNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	if !exists {
		return nil, ErrTenantNotFound
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id, tenant_id, created_at, expires_at, revoked_at
FROM api_keys
WHERE tenant_id = $1
ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []APIKey
	for rows.Next() {
		var key APIKey
		var expiresAt, revokedAt sql.NullTime
		if err := rows.Scan(&key.ID, &key.TenantID, &key.CreatedAt, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("list api keys: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time.UTC()
			key.ExpiresAt = &t
		}
		if revokedAt.Valid {
			t := revokedAt.Time.UTC()
			key.RevokedAt = &t
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return keys, nil
}

/*
RevokeAPIKey 把 revoked_at 写成当前时间。
已经吊销过返回 ErrAPIKeyRevoked，没有这一行返回 ErrAPIKeyNotFound。
调用方看不到明文，所以按密钥 ID 吊销；ID 在签发时打印过。
*/
func (s *Store) RevokeAPIKey(ctx context.Context, keyID int64) error {
	if keyID <= 0 {
		return ErrAPIKeyNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var id int64
	err := s.db.QueryRowContext(ctx, `
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING id`, keyID).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("revoke api key: %w", err)
	}

	var revoked bool
	err = s.db.QueryRowContext(ctx, `
SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, keyID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAPIKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if !revoked {
		return fmt.Errorf("revoke api key: row %d was not updated", keyID)
	}
	return ErrAPIKeyRevoked
}
