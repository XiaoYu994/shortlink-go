package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

const (
	identityUniqueConstraint = "uq_user_identities_issuer_subject"
	maxOIDCLoginAttempts     = 2
)

var errIdentityConflict = errors.New("oidc identity unique conflict")

/*
CreateSessionForOIDC 按 (issuer, subject) 找到或创建用户，并在同一事务里签发一条新的服务端会话（阶段 11、ADR 0018）。
当本次已校验的 ID Token 携带了已验证邮箱（email_verified=true）时，也在同一事务里按该邮箱兑现
尚未过期且未撤销的组织邀请；未验证邮箱绝不兑现，防止借未核实邮箱冒领邀请。
再次登录只在新声明非空时覆盖邮箱和昵称；邮箱声明缺失时不改 email_verified。
同一邮箱上已经验证过的标记不会被后来的 false 降回去。邮箱本身变了，则采用新声明里的验证结果。

第一版不按邮箱自动关联账号：不同 (issuer, subject) 即使邮箱相同也是独立用户。
每次登录都生成全新随机 token 并只存 SHA-256，防止会话固定攻击。
如果同一 (issuer, subject) 并发首次登录撞上唯一约束，回滚本次事务（不留孤儿 users 行）后重试一次。
*/
func (s *Store) CreateSessionForOIDC(ctx context.Context, id identity.OIDCIdentity, ttl time.Duration) (identity.IssuedSession, error) {
	norm, err := id.Normalize()
	if err != nil {
		return identity.IssuedSession{}, err
	}
	if ttl < time.Microsecond {
		return identity.IssuedSession{}, identity.ErrInvalidSessionTTL
	}
	if err := ctx.Err(); err != nil {
		return identity.IssuedSession{}, err
	}

	token, hash, err := identity.SessionToken.Generate()
	if err != nil {
		return identity.IssuedSession{}, fmt.Errorf("create session for oidc: generate token: %w", err)
	}

	for range maxOIDCLoginAttempts {
		issued, err := s.createSessionForOIDCTx(ctx, norm, token, hash, ttl)
		if errors.Is(err, errIdentityConflict) {
			continue
		}
		return issued, err
	}
	return identity.IssuedSession{}, fmt.Errorf("create session for oidc: identity conflict after %d attempts", maxOIDCLoginAttempts)
}

func (s *Store) createSessionForOIDCTx(ctx context.Context, id identity.OIDCIdentity, token string, hash []byte, ttl time.Duration) (identity.IssuedSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.IssuedSession{}, fmt.Errorf("create session for oidc: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	user, err := upsertOIDCUserTx(ctx, tx, id)
	if err != nil {
		return identity.IssuedSession{}, err
	}
	if id.EmailVerified && id.Email != "" {
		if err := redeemInvitationsTx(ctx, tx, user.ID, id.Email); err != nil {
			return identity.IssuedSession{}, err
		}
	}

	/*
		会话到期时刻基于数据库时钟 now() 计算，避免 console 多副本系统时钟漂移导致
		触发 ck_sessions_expires_after_created 或提前过期。
	*/
	sess := identity.Session{UserID: user.ID}
	var dbNow time.Time
	err = tx.QueryRowContext(ctx, `
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, now() + ($3::bigint * interval '1 microsecond'))
RETURNING id, created_at, expires_at, now()`,
		user.ID, hash, ttl.Microseconds(),
	).Scan(&sess.ID, &sess.CreatedAt, &sess.ExpiresAt, &dbNow)
	if err != nil {
		return identity.IssuedSession{}, fmt.Errorf("create session for oidc: insert session: %w", err)
	}
	sess.CreatedAt = sess.CreatedAt.UTC()
	sess.ExpiresAt = sess.ExpiresAt.UTC()
	dbNow = dbNow.UTC()

	if err := tx.Commit(); err != nil {
		return identity.IssuedSession{}, fmt.Errorf("create session for oidc: commit: %w", err)
	}
	return identity.IssuedSession{
		User:         user,
		Session:      sess,
		Token:        token,
		CookieMaxAge: sess.ExpiresAt.Sub(dbNow),
	}, nil
}

func upsertOIDCUserTx(ctx context.Context, tx *sql.Tx, id identity.OIDCIdentity) (identity.User, error) {
	var userID int64
	err := tx.QueryRowContext(ctx, `
UPDATE user_identities
SET email = CASE WHEN $1 <> '' THEN $1 ELSE email END,
    email_verified = CASE
        WHEN $1 = '' THEN email_verified
        WHEN $1 = email AND email_verified THEN TRUE
        ELSE $2
    END
WHERE issuer = $3 AND subject = $4
RETURNING user_id`, id.Email, id.EmailVerified, id.Issuer, id.Subject).Scan(&userID)
	switch {
	case err == nil:
		var user identity.User
		err = tx.QueryRowContext(ctx, `
UPDATE users
SET email = CASE WHEN $1 <> '' THEN $1 ELSE email END,
    name = CASE WHEN $2 <> '' THEN $2 ELSE name END
WHERE id = $3
RETURNING id, email, name, created_at`,
			id.Email, id.Name, userID).Scan(&user.ID, &user.Email, &user.Name, &user.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.User{}, fmt.Errorf("create session for oidc: user %d missing for identity", userID)
		}
		if err != nil {
			return identity.User{}, fmt.Errorf("create session for oidc: update user: %w", err)
		}
		user.CreatedAt = user.CreatedAt.UTC()
		return user, nil

	case errors.Is(err, sql.ErrNoRows):
		var user identity.User
		err = tx.QueryRowContext(ctx, `
INSERT INTO users (email, name)
VALUES ($1, $2)
RETURNING id, email, name, created_at`,
			id.Email, id.Name).Scan(&user.ID, &user.Email, &user.Name, &user.CreatedAt)
		if err != nil {
			return identity.User{}, fmt.Errorf("create session for oidc: insert user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO user_identities (user_id, issuer, subject, email, email_verified)
VALUES ($1, $2, $3, $4, $5)`,
			user.ID, id.Issuer, id.Subject, id.Email, id.EmailVerified); err != nil {
			if isUniqueConstraintViolation(err, identityUniqueConstraint) {
				return identity.User{}, errIdentityConflict
			}
			return identity.User{}, fmt.Errorf("create session for oidc: insert identity: %w", err)
		}
		user.CreatedAt = user.CreatedAt.UTC()
		return user, nil

	default:
		return identity.User{}, fmt.Errorf("create session for oidc: upsert identity: %w", err)
	}
}

func redeemInvitationsTx(ctx context.Context, tx *sql.Tx, userID int64, email string) error {
	// 按 id 排序加行锁避免多邀请并发兑现死锁；已是组织成员时 ON CONFLICT 保留原角色，同时关闭待兑现邀请。
	if _, err := tx.ExecContext(ctx, `
WITH locked AS (
    SELECT i.id
    FROM invitations i
    WHERE i.email = $2
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL
      AND (i.expires_at IS NULL OR i.expires_at > now())
      AND EXISTS (SELECT 1 FROM tenants t WHERE t.id = i.tenant_id)
    ORDER BY i.id
    FOR UPDATE OF i
),
redeemed AS (
    UPDATE invitations i
    SET accepted_at = now(),
        accepted_by = $1
    FROM locked l
    WHERE i.id = l.id
    RETURNING i.tenant_id, i.role
)
INSERT INTO memberships (user_id, tenant_id, role)
SELECT $1, r.tenant_id, r.role
FROM redeemed r
ON CONFLICT (user_id, tenant_id) DO NOTHING`,
		userID, email,
	); err != nil {
		return fmt.Errorf("create session for oidc: redeem invitations: %w", err)
	}
	return nil
}

func isUniqueConstraintViolation(err error, constraint string) bool {
	return isConstraintViolation(err, "23505", constraint)
}

func isConstraintViolation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

/*
AuthenticateSessionForOrg 校验会话令牌并确认该用户在目标组织中的成员身份与角色。
令牌格式非法、未命中、已吊销或已过期统一返回 identity.ErrUnauthenticated（401）；
会话有效但用户不是 orgID 成员或组织不存在返回 identity.ErrOrgNotFound（404，不暴露组织是否存在）。
每次请求实时查库，确保登出、改角色、移出组织与调整限流后下一次请求立即生效（ADR 0018）。
*/
func (s *Store) AuthenticateSessionForOrg(ctx context.Context, token string, orgID int64) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, err
	}
	if !identity.SessionToken.Valid(token) {
		return identity.Principal{}, identity.ErrUnauthenticated
	}

	var (
		userID     int64
		tenantID   sql.NullInt64
		externalID sql.NullString
		rateLimit  sql.NullInt64
		roleStr    sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
SELECT u.id,
       t.id, t.external_id, t.rate_limit_per_second,
       m.role
FROM sessions s
JOIN users u ON u.id = s.user_id
LEFT JOIN memberships m ON m.user_id = u.id AND m.tenant_id = $2
LEFT JOIN tenants t ON t.id = m.tenant_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.expires_at > now()`,
		identity.HashToken(token), orgID,
	).Scan(&userID, &tenantID, &externalID, &rateLimit, &roleStr)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	if err != nil {
		return identity.Principal{}, fmt.Errorf("authenticate session: %w", err)
	}
	if !tenantID.Valid || !roleStr.Valid {
		return identity.Principal{}, identity.ErrOrgNotFound
	}
	role := identity.Role(roleStr.String)
	if !identity.ValidRole(role) {
		return identity.Principal{}, fmt.Errorf("authenticate session: invalid role %q", roleStr.String)
	}

	tn := tenant.Tenant{
		ID:        tenantID.Int64,
		RateLimit: int(rateLimit.Int64),
	}
	if externalID.Valid {
		tn.ExternalID = externalID.String
	}
	return identity.Principal{
		Tenant:    tn,
		ActorKind: identity.ActorUser,
		ActorID:   userID,
		Role:      role,
	}, nil
}

/*
GetSessionInfo 校验会话令牌并返回关联用户、会话过期时间及所属组织列表（按组织 ID 升序）。
用户和组织在同一条语句里读出，避免登出落在两次查询之间时仍返回 200。
令牌无效、未命中、已吊销或已过期返回 identity.ErrUnauthenticated。
*/
func (s *Store) GetSessionInfo(ctx context.Context, token string) (identity.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return identity.SessionInfo{}, err
	}
	if !identity.SessionToken.Valid(token) {
		return identity.SessionInfo{}, identity.ErrUnauthenticated
	}

	var (
		info     identity.SessionInfo
		orgsJSON []byte
	)
	err := s.db.QueryRowContext(ctx, `
SELECT u.id, u.email, u.name, u.created_at, s.expires_at,
       COALESCE((
           SELECT json_agg(
               json_build_object('id', t.id, 'name', t.name, 'role', m.role)
               ORDER BY t.id
           )
           FROM memberships m
           JOIN tenants t ON t.id = m.tenant_id
           WHERE m.user_id = u.id
       ), '[]'::json)
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.expires_at > now()`,
		identity.HashToken(token),
	).Scan(&info.User.ID, &info.User.Email, &info.User.Name, &info.User.CreatedAt, &info.ExpiresAt, &orgsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.SessionInfo{}, identity.ErrUnauthenticated
	}
	if err != nil {
		return identity.SessionInfo{}, fmt.Errorf("get session info: %w", err)
	}
	info.User.CreatedAt = info.User.CreatedAt.UTC()
	info.ExpiresAt = info.ExpiresAt.UTC()

	var orgs []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(orgsJSON, &orgs); err != nil {
		return identity.SessionInfo{}, fmt.Errorf("get session info: decode orgs: %w", err)
	}
	for _, org := range orgs {
		role := identity.Role(org.Role)
		if !identity.ValidRole(role) {
			return identity.SessionInfo{}, fmt.Errorf("get session info: invalid role %q", org.Role)
		}
		info.Orgs = append(info.Orgs, identity.UserOrg{
			ID:   org.ID,
			Name: org.Name,
			Role: role,
		})
	}
	return info, nil
}

/*
RevokeSession 吊销指定明文令牌对应的服务端会话，使其立刻失效。
格式非法、会话不存在或已吊销时均视为幂等成功。
*/
func (s *Store) RevokeSession(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !identity.SessionToken.Valid(token) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET revoked_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL`,
		identity.HashToken(token),
	); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}
