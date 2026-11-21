package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shortlink/internal/identity"
)

const (
	identityUniqueConstraint = "uq_user_identities_issuer_subject"
	maxOIDCLoginAttempts     = 2
)

var errIdentityConflict = errors.New("oidc identity unique conflict")

/*
CreateSessionForOIDC 按 (issuer, subject) 找到或创建用户，并在同一事务里签发一条新的服务端会话（阶段 11、ADR 0018）。
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

	token, hash, err := identity.GenerateSessionToken()
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

func isUniqueConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
