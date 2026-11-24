package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

const (
	insertAPIKeySQL = `INSERT INTO api_keys (tenant_id, key_hash, expires_at, scopes, created_by)
VALUES ($1, $2, $3, $4::text[], $5)
RETURNING id, created_at, expires_at`

	selectTenantForIssueSQL = `SELECT id, external_id FROM tenants WHERE id = $1`

	insertTenantForIssueSQL = `INSERT INTO tenants (name, external_id) VALUES ($1, $2) RETURNING id`

	listAPIKeysSQL = `SELECT id, tenant_id, scopes, created_by, created_at, expires_at, revoked_at
FROM api_keys
WHERE tenant_id = $1
ORDER BY id`

	revokeAPIKeySQL = `WITH target AS (
    SELECT id, tenant_id, (revoked_at IS NOT NULL) AS already_revoked
    FROM api_keys
    WHERE id = $1
    FOR UPDATE
),
revoked AS (
    UPDATE api_keys k
    SET revoked_at = now()
    FROM target t
    WHERE k.id = t.id AND NOT t.already_revoked
    RETURNING k.id, k.tenant_id
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT r.tenant_id,
           $2::text,
           $3::bigint,
           $5::text,
           $6::text,
           r.id::text,
           $4::text,
           '{}'::jsonb
    FROM revoked r
)
SELECT t.already_revoked FROM target t`

	revokeAPIKeyForTenantSQL = `WITH target AS (
    SELECT id, tenant_id, (revoked_at IS NOT NULL) AS already_revoked
    FROM api_keys
    WHERE id = $1 AND tenant_id = $2
    FOR UPDATE
),
revoked AS (
    UPDATE api_keys k
    SET revoked_at = now()
    FROM target t
    WHERE k.id = t.id AND NOT t.already_revoked
    RETURNING k.id, k.tenant_id
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT r.tenant_id,
           $3::text,
           $4::bigint,
           $6::text,
           $7::text,
           r.id::text,
           $5::text,
           '{}'::jsonb
    FROM revoked r
)
SELECT t.already_revoked FROM target t`

	authenticateAPIKeySQL = `SELECT k.id, k.scopes, t.id, t.external_id, t.rate_limit_per_second
FROM api_keys k
JOIN tenants t ON t.id = k.tenant_id
WHERE k.key_hash = $1
  AND k.revoked_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > now())`
)

var (
	// ErrIssueTarget 表示没有唯一确定把密钥发给哪个租户。
	ErrIssueTarget = errors.New("issue api key: provide either tenant id or a new tenant name")
	// ErrExternalIDOnExisting 表示给已有租户签发时又带了外部编号。
	ErrExternalIDOnExisting = errors.New("issue api key: external id only applies to a new tenant")
	// ErrExpiresNotFuture 表示签发时给出的到期时间不是将来。
	ErrExpiresNotFuture = identity.ErrExpiresNotFuture
	// ErrTenantNotFound 表示租户不存在。
	ErrTenantNotFound = tenant.ErrNotFound
	// ErrAPIKeyNotFound 表示密钥行不存在。
	ErrAPIKeyNotFound = identity.ErrAPIKeyNotFound
	// ErrAPIKeyRevoked 表示这把密钥已经吊销过。
	ErrAPIKeyRevoked = identity.ErrAPIKeyRevoked
)

/*
APIKey 是可以给运维看的密钥状态。
没有明文，也没有哈希：哈希不能当凭证用，但也不必出现在列表里。
*/
type APIKey struct {
	ID        int64
	TenantID  int64
	Scopes    []identity.Perm
	CreatedBy *int64
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
IssueAPIKeyParams 指定把密钥发给谁以及授予哪些权限。
TenantID 和 Name 只能填一个：前者给已有租户加一把密钥，后者新建租户再签发。
ExternalID 只在新建时写入；空字符串存成 NULL，避免占住唯一约束。
CreatedBy 为空表示由运维 CLI 签发；非空表示由组织管理员通过 HTTP 接口签发。
ExpiresAt 为 nil 表示不过期。
Scopes 必填，nil 和空切片都拒绝：零值不能表示全权限。
只允许 links:read、links:write、stats:read，写入前会去重并按规范顺序排列。
要授予全部密钥权限时传入 identity.AllAPIKeyScopes()。
*/
type IssueAPIKeyParams struct {
	TenantID   int64
	Name       string
	ExternalID string
	CreatedBy  *int64
	ExpiresAt  *time.Time
	Scopes     []identity.Perm
}

/*
IssueAPIKey 新建或找到租户，生成一把密钥，把哈希、权限范围和可选的到期时间写入 api_keys。
租户和密钥在同一个事务里：插入密钥失败时，新建的租户也会回滚。
*/
func (s *Store) IssueAPIKey(ctx context.Context, p IssueAPIKeyParams) (IssuedAPIKey, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.ExternalID = strings.TrimSpace(p.ExternalID)
	if err := validateIssue(p); err != nil {
		return IssuedAPIKey{}, err
	}
	scopes, err := identity.NormalizeAPIKeyScopes(p.Scopes)
	if err != nil {
		return IssuedAPIKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return IssuedAPIKey{}, err
	}
	var explicitUserID int64
	if p.CreatedBy != nil {
		explicitUserID = *p.CreatedBy
	}
	if _, _, _, err := requireAuditMetaOrUser(ctx, explicitUserID); err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
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
	if p.TenantID == 0 {
		orgMeta := map[string]any{"name": p.Name}
		if p.ExternalID != "" {
			orgMeta["external_id"] = p.ExternalID
		}
		if err := insertAuditLogTx(ctx, tx, tn.ID, explicitUserID, audit.ActionOrgCreate, audit.TargetOrg, strconv.FormatInt(tn.ID, 10), orgMeta); err != nil {
			return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
		}
	}

	plaintext, hash, err := identity.APIKeyToken.Generate()
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}

	var expires *time.Time
	if p.ExpiresAt != nil {
		t := p.ExpiresAt.UTC()
		expires = &t
	}
	var createdBy sql.NullInt64
	var keyCreatedBy *int64
	if p.CreatedBy != nil {
		createdBy = sql.NullInt64{Int64: *p.CreatedBy, Valid: true}
		v := *p.CreatedBy
		keyCreatedBy = &v
	}
	key := APIKey{TenantID: tn.ID, Scopes: scopes, CreatedBy: keyCreatedBy, ExpiresAt: expires}
	var scannedExpires sql.NullTime
	err = tx.QueryRowContext(ctx, insertAPIKeySQL, tn.ID, hash, expires, permsToStrings(scopes), createdBy).Scan(
		&key.ID, &key.CreatedAt, &scannedExpires,
	)
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}
	key.CreatedAt = key.CreatedAt.UTC()
	if scannedExpires.Valid {
		key.ExpiresAt = nullTimeUTC(scannedExpires)
	}

	// 只记录非敏感元数据（scopes 与可选过期时间），绝不写入明文或哈希。
	keyMeta := map[string]any{"scopes": permsToStrings(scopes)}
	if key.ExpiresAt != nil {
		keyMeta["expires_at"] = key.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if err := insertAuditLogTx(ctx, tx, tn.ID, explicitUserID, audit.ActionAPIKeyIssue, audit.TargetAPIKey, strconv.FormatInt(key.ID, 10), keyMeta); err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return IssuedAPIKey{}, fmt.Errorf("issue api key: %w", err)
	}
	return IssuedAPIKey{Tenant: tn, Key: key, Plaintext: plaintext}, nil
}

func permsToStrings(perms []identity.Perm) []string {
	out := make([]string, len(perms))
	for i, perm := range perms {
		out[i] = string(perm)
	}
	return out
}

/*
authScopes 把库里的 scopes 转成鉴权用的权限列表，丢掉不认识的值。
只用在 Authenticate：滚动升级时，新版本签发的密钥可能带旧副本不认识的 scope，
严格解析会让这把密钥的所有请求变成 500。丢掉未知值只会让权限变少，仍然是 fail-closed。
ListAPIKeys 给运维看真实内容，继续用严格的 parseAPIKeyScopes。
*/
func authScopes(raw []string) []identity.Perm {
	perms := make([]identity.Perm, 0, len(raw))
	for _, item := range raw {
		if perm := identity.Perm(item); identity.ValidAPIKeyScope(perm) {
			perms = append(perms, perm)
		}
	}
	return perms
}

func parseAPIKeyScopes(raw []string) ([]identity.Perm, error) {
	perms := make([]identity.Perm, len(raw))
	for i, item := range raw {
		perms[i] = identity.Perm(item)
	}
	return identity.NormalizeAPIKeyScopes(perms)
}

func validateIssue(p IssueAPIKeyParams) error {
	hasID := p.TenantID != 0
	hasName := p.Name != ""
	if hasID == hasName || p.TenantID < 0 {
		return ErrIssueTarget
	}
	if hasName {
		if _, err := identity.NormalizeOrgName(p.Name); err != nil {
			return err
		}
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
		err := tx.QueryRowContext(ctx, selectTenantForIssueSQL, p.TenantID).Scan(&tn.ID, &externalID)
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
	if err := tx.QueryRowContext(ctx, insertTenantForIssueSQL, p.Name, external).Scan(&tn.ID); err != nil {
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
	if err := s.db.QueryRowContext(ctx, tenantExistsSQL, tenantID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	if !exists {
		return nil, ErrTenantNotFound
	}

	rows, err := s.db.QueryContext(ctx, listAPIKeysSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []APIKey
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list api keys: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return keys, nil
}

func scanAPIKey(scanner rowScanner) (APIKey, error) {
	var (
		key       APIKey
		rawScopes []string
		createdBy sql.NullInt64
		expiresAt sql.NullTime
		revokedAt sql.NullTime
	)
	if err := scanner.Scan(&key.ID, &key.TenantID, &rawScopes, &createdBy, &key.CreatedAt, &expiresAt, &revokedAt); err != nil {
		return APIKey{}, err
	}
	scopes, err := parseAPIKeyScopes(rawScopes)
	if err != nil {
		return APIKey{}, err
	}
	key.Scopes = scopes
	if createdBy.Valid {
		v := createdBy.Int64
		key.CreatedBy = &v
	}
	key.CreatedAt = key.CreatedAt.UTC()
	key.ExpiresAt = nullTimeUTC(expiresAt)
	key.RevokedAt = nullTimeUTC(revokedAt)
	return key, nil
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

	actorKind, actorID, reqID, err := requireAuditMeta(ctx)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	var alreadyRevoked bool
	err = s.db.QueryRowContext(ctx, revokeAPIKeySQL,
		keyID, actorKind, actorID, reqID,
		string(audit.ActionAPIKeyRevoke), string(audit.TargetAPIKey)).Scan(&alreadyRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAPIKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if alreadyRevoked {
		return ErrAPIKeyRevoked
	}
	return nil
}

/*
RevokeAPIKeyForTenant 按 (keyID, tenantID) 强制租户隔离地吊销一把密钥（ADR 0021）。
跨租户或不存在一律返回 ErrAPIKeyNotFound，不暴露其他租户密钥是否存在；已吊销返回 ErrAPIKeyRevoked。
*/
func (s *Store) RevokeAPIKeyForTenant(ctx context.Context, tenantID, keyID int64) error {
	if tenantID <= 0 || keyID <= 0 {
		return ErrAPIKeyNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	actorKind, actorID, reqID, err := requireAuditMeta(ctx)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	var alreadyRevoked bool
	err = s.db.QueryRowContext(ctx, revokeAPIKeyForTenantSQL,
		keyID, tenantID, actorKind, actorID, reqID,
		string(audit.ActionAPIKeyRevoke), string(audit.TargetAPIKey)).Scan(&alreadyRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAPIKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if alreadyRevoked {
		return ErrAPIKeyRevoked
	}
	return nil
}

// ListOrgAPIKeys 给 HTTP 管理接口使用，返回 identity.APIKeyInfo 列表以保持 console 不依赖 postgres。
func (s *Store) ListOrgAPIKeys(ctx context.Context, tenantID int64) ([]identity.APIKeyInfo, error) {
	keys, err := s.ListAPIKeys(ctx, tenantID)
	if errors.Is(err, ErrTenantNotFound) {
		return nil, identity.ErrOrgNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]identity.APIKeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyInfo(k))
	}
	return out, nil
}

// IssueOrgAPIKey 给 HTTP 管理接口使用，在当前组织内签发密钥并记录 created_by。
func (s *Store) IssueOrgAPIKey(ctx context.Context, p identity.IssueOrgAPIKeyParams) (identity.IssuedOrgAPIKey, error) {
	if p.CreatedBy <= 0 {
		return identity.IssuedOrgAPIKey{}, errors.New("issue api key: created_by must be positive")
	}
	createdBy := p.CreatedBy
	issued, err := s.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID:  p.TenantID,
		CreatedBy: &createdBy,
		ExpiresAt: p.ExpiresAt,
		Scopes:    p.Scopes,
	})
	if errors.Is(err, ErrTenantNotFound) {
		return identity.IssuedOrgAPIKey{}, identity.ErrOrgNotFound
	}
	if err != nil {
		return identity.IssuedOrgAPIKey{}, err
	}
	return identity.IssuedOrgAPIKey{
		Key:       toAPIKeyInfo(issued.Key),
		Plaintext: issued.Plaintext,
	}, nil
}

// RevokeOrgAPIKey 给 HTTP 管理接口使用，按组织隔离吊销 API Key。
func (s *Store) RevokeOrgAPIKey(ctx context.Context, tenantID, keyID int64) error {
	return s.RevokeAPIKeyForTenant(ctx, tenantID, keyID)
}

func toAPIKeyInfo(k APIKey) identity.APIKeyInfo {
	return identity.APIKeyInfo{
		ID:        k.ID,
		TenantID:  k.TenantID,
		Scopes:    k.Scopes,
		CreatedBy: k.CreatedBy,
		CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt,
		RevokedAt: k.RevokedAt,
	}
}

/*
Authenticate 用 API Key 的哈希查找租户、密钥编号、权限范围和限流配置，返回对应的 Principal。
格式不对、查无此密钥、已过期、已吊销都返回 identity.ErrUnauthenticated。
这几种情况共用一个错误，调用方返回同一个 401，不透露密钥是否存在过。
其他错误原样往上送，避免把数据库故障说成密钥无效。
到期时刻本身算过期，界限和链接一样是 expires_at > now()。
*/
func (s *Store) Authenticate(ctx context.Context, token string) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, err
	}
	if !identity.APIKeyToken.Valid(token) {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	var (
		keyID      int64
		rawScopes  []string
		tn         tenant.Tenant
		externalID sql.NullString
	)
	err := s.db.QueryRowContext(ctx, authenticateAPIKeySQL, identity.HashToken(token)).Scan(
		&keyID, &rawScopes, &tn.ID, &externalID, &tn.RateLimit,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	if err != nil {
		return identity.Principal{}, fmt.Errorf("authenticate api key: %w", err)
	}
	if externalID.Valid {
		tn.ExternalID = externalID.String
	}
	return identity.Principal{
		Tenant:    tn,
		ActorKind: identity.ActorAPIKey,
		ActorID:   keyID,
		Scopes:    authScopes(rawScopes),
	}, nil
}
