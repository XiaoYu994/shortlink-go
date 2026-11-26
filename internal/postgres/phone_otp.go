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
	phoneOTPRateWindow = time.Hour

	lockPhoneOTPSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

	phoneOTPWindowStatsSQL = `SELECT COUNT(*) FILTER (WHERE created_at > now() - ($2::bigint * interval '1 microsecond'))::int,
       MAX(created_at),
       MIN(created_at) FILTER (WHERE created_at > now() - ($2::bigint * interval '1 microsecond')),
       now()
FROM phone_otp_challenges
WHERE phone = $1
  AND created_at > now() - ($3::bigint * interval '1 microsecond')`

	invalidateActivePhoneOTPSQL = `UPDATE phone_otp_challenges
SET invalidated_at = now()
WHERE phone = $1
  AND consumed_at IS NULL
  AND invalidated_at IS NULL`

	insertPhoneOTPChallengeSQL = `INSERT INTO phone_otp_challenges (phone, code_salt, code_hash, max_attempts, expires_at)
VALUES ($1, $2, $3, $4, now() + ($5::bigint * interval '1 microsecond'))
RETURNING id`

	deleteUnsentPhoneOTPChallengeSQL = `DELETE FROM phone_otp_challenges
WHERE id = $1
  AND consumed_at IS NULL`

	lockActivePhoneOTPChallengeSQL = `SELECT id, code_salt, code_hash, attempts, max_attempts, expires_at <= now()
FROM phone_otp_challenges
WHERE phone = $1
  AND consumed_at IS NULL
  AND invalidated_at IS NULL
FOR UPDATE`

	invalidatePhoneOTPChallengeByIDSQL = `UPDATE phone_otp_challenges
SET invalidated_at = now()
WHERE id = $1
  AND consumed_at IS NULL
  AND invalidated_at IS NULL`

	recordFailedPhoneOTPAttemptSQL = `UPDATE phone_otp_challenges
SET attempts = attempts + 1,
    invalidated_at = CASE WHEN attempts + 1 >= max_attempts THEN now() ELSE NULL END
WHERE id = $1
  AND consumed_at IS NULL
  AND invalidated_at IS NULL`

	consumePhoneOTPChallengeSQL = `UPDATE phone_otp_challenges
SET consumed_at = now()
WHERE id = $1
  AND consumed_at IS NULL
  AND invalidated_at IS NULL`

	cleanupExpiredPhoneOTPChallengesSQL = `DELETE FROM phone_otp_challenges
WHERE id IN (
    SELECT id
    FROM phone_otp_challenges
    WHERE expires_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY expires_at
    LIMIT $2
)`
)

/*
CreatePhoneOTPChallenge 在短事务内串行化校验单手机号的冷却期与小时发送上限，
作废同一手机号旧的活跃挑战并写入新的加盐摘要挑战（ADR 0030）。
事务内不做外网短信 I/O；若随后同步发短信失败，调用方应调用 DeleteUnsentPhoneOTPChallenge 回滚本行。
*/
func (s *Store) CreatePhoneOTPChallenge(ctx context.Context, p identity.PhoneOTPChallengeParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	phone, err := identity.NormalizePhone(p.Phone)
	if err != nil {
		return 0, fmt.Errorf("create phone otp challenge: %w", err)
	}
	if len(p.CodeSalt) != identity.OTPSaltBytes || len(p.CodeHash) != identity.OTPHashBytes {
		return 0, errors.New("create phone otp challenge: invalid otp hash or salt length")
	}
	if p.TTL < time.Microsecond {
		return 0, errors.New("create phone otp challenge: ttl must be positive")
	}
	if p.Cooldown < 0 {
		return 0, errors.New("create phone otp challenge: cooldown must not be negative")
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = identity.DefaultOTPMaxAttempts
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("create phone otp challenge: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 按手机号事务级加锁，杜绝并发双发同时穿过冷却检查或撞上唯一活跃索引。
	if _, err := tx.ExecContext(ctx, lockPhoneOTPSQL, phone); err != nil {
		return 0, fmt.Errorf("create phone otp challenge: lock phone: %w", err)
	}

	scanWindow := max(phoneOTPRateWindow, p.Cooldown)
	var (
		windowCount int
		latestAt    sql.NullTime
		oldestAt    sql.NullTime
		dbNow       time.Time
	)
	if err := tx.QueryRowContext(ctx, phoneOTPWindowStatsSQL, phone, phoneOTPRateWindow.Microseconds(), scanWindow.Microseconds()).Scan(
		&windowCount, &latestAt, &oldestAt, &dbNow,
	); err != nil {
		return 0, fmt.Errorf("create phone otp challenge: check rate window: %w", err)
	}

	if p.Cooldown > 0 && latestAt.Valid {
		if remaining := latestAt.Time.Add(p.Cooldown).Sub(dbNow); remaining > 0 {
			return 0, identity.OTPRetryAfterError{
				Err:        identity.ErrOTPCooldown,
				RetryAfter: remaining,
			}
		}
	}
	if p.MaxPerHour > 0 && windowCount >= p.MaxPerHour {
		remaining := phoneOTPRateWindow
		if oldestAt.Valid {
			remaining = oldestAt.Time.Add(phoneOTPRateWindow).Sub(dbNow)
		}
		if remaining < time.Second {
			remaining = time.Second
		}
		return 0, identity.OTPRetryAfterError{
			Err:        identity.ErrOTPRateLimited,
			RetryAfter: remaining,
		}
	}

	if _, err := tx.ExecContext(ctx, invalidateActivePhoneOTPSQL, phone); err != nil {
		return 0, fmt.Errorf("create phone otp challenge: invalidate previous: %w", err)
	}

	var id int64
	if err := tx.QueryRowContext(ctx, insertPhoneOTPChallengeSQL,
		phone, p.CodeSalt, p.CodeHash, maxAttempts, p.TTL.Microseconds(),
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("create phone otp challenge: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create phone otp challenge: commit: %w", err)
	}
	return id, nil
}

// DeleteUnsentPhoneOTPChallenge 删除尚未被消费的指定挑战行，用于同步短信发送失败时回滚冷却与额度。
func (s *Store) DeleteUnsentPhoneOTPChallenge(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, deleteUnsentPhoneOTPChallengeSQL, id); err != nil {
		return fmt.Errorf("delete unsent phone otp challenge: %w", err)
	}
	return nil
}

/*
VerifyPhoneOTPChallenge 原子锁住该手机号唯一活跃挑战并用常量时间校验摘要（ADR 0030）：
  - 未找到活跃挑战或已过期：返回 identity.ErrOTPInvalid（过期行顺手标为作废）；
  - 验证码不匹配：原子累加 attempts，达到 max_attempts 时同时作废并返回 identity.ErrOTPInvalid；
  - 匹配成功：原子置 consumed_at = now()，在 READ COMMITTED 下确保并发重放只有一次成功。
*/
func (s *Store) VerifyPhoneOTPChallenge(ctx context.Context, phone, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := identity.NormalizePhone(phone)
	if err != nil {
		return fmt.Errorf("verify phone otp challenge: %w", err)
	}
	if !identity.ValidOTPCode(code) {
		return identity.ErrOTPInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("verify phone otp challenge: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id          int64
		salt        []byte
		hash        []byte
		attempts    int
		maxAttempts int
		expired     bool
	)
	err = tx.QueryRowContext(ctx, lockActivePhoneOTPChallengeSQL, normalized).Scan(
		&id, &salt, &hash, &attempts, &maxAttempts, &expired,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrOTPInvalid
	}
	if err != nil {
		return fmt.Errorf("verify phone otp challenge: lock active: %w", err)
	}

	if expired || attempts >= maxAttempts {
		if _, err := tx.ExecContext(ctx, invalidatePhoneOTPChallengeByIDSQL, id); err != nil {
			return fmt.Errorf("verify phone otp challenge: invalidate expired: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("verify phone otp challenge: commit invalidation: %w", err)
		}
		return identity.ErrOTPInvalid
	}

	if !identity.VerifyOTPCodeHash(salt, hash, normalized, code) {
		if _, err := tx.ExecContext(ctx, recordFailedPhoneOTPAttemptSQL, id); err != nil {
			return fmt.Errorf("verify phone otp challenge: record attempt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("verify phone otp challenge: commit failed attempt: %w", err)
		}
		return identity.ErrOTPInvalid
	}

	if _, err := tx.ExecContext(ctx, consumePhoneOTPChallengeSQL, id); err != nil {
		return fmt.Errorf("verify phone otp challenge: consume: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("verify phone otp challenge: commit consume: %w", err)
	}
	return nil
}

// CleanupExpiredPhoneOTPChallenges 沿 idx_phone_otp_expires_at 分批删除过期早于 retention 的历史验证码记录。
func (s *Store) CleanupExpiredPhoneOTPChallenges(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup expired phone otp challenges", cleanupExpiredPhoneOTPChallengesSQL, retention, limit)
}
