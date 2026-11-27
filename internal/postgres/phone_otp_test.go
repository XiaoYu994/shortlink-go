package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func mustOTPParams(t *testing.T, phone, code string, ttl, cooldown time.Duration, maxPerHour, maxAttempts int) identity.PhoneOTPChallengeParams {
	t.Helper()
	salt := identity.NewOTPSalt()
	return identity.PhoneOTPChallengeParams{
		Phone:       phone,
		CodeSalt:    salt,
		CodeHash:    identity.HashOTPCode(salt, phone, code),
		TTL:         ttl,
		Cooldown:    cooldown,
		MaxPerHour:  maxPerHour,
		MaxAttempts: maxAttempts,
	}
}

func TestPhoneOTPChallengeLifecycleCooldownAndRateLimit(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	phone := "+8613800138001"

	// 1. 首次创建成功；立即重发命中冷却期返回 ErrOTPCooldown。
	id1, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "111111", 5*time.Minute, time.Minute, 5, 5))
	if err != nil || id1 <= 0 {
		t.Fatalf("first CreatePhoneOTPChallenge = (%d, %v)", id1, err)
	}
	_, err = store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "222222", 5*time.Minute, time.Minute, 5, 5))
	if !errors.Is(err, identity.ErrOTPCooldown) {
		t.Fatalf("second immediate create err = %v, want ErrOTPCooldown", err)
	}
	if retryAfter, ok := identity.OTPRetryAfter(err); !ok || retryAfter <= 0 {
		t.Fatalf("cooldown RetryAfter = (%v, %v), want positive", retryAfter, ok)
	}

	// 2. 删除未送达挑战后，冷却与小时额度一并回滚，立即重发成功。
	if err := store.DeleteUnsentPhoneOTPChallenge(ctx, id1); err != nil {
		t.Fatalf("DeleteUnsentPhoneOTPChallenge: %v", err)
	}
	id2, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "222222", 5*time.Minute, time.Minute, 2, 5))
	if err != nil || id2 <= 0 {
		t.Fatalf("create after rollback = (%d, %v)", id2, err)
	}

	// 3. 跳过冷却重发新码时，旧码自动作废，只有新码有效；达到每小时上限 2 次后触发 ErrOTPRateLimited。
	id3, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "333333", 5*time.Minute, 0, 2, 5))
	if err != nil || id3 <= 0 {
		t.Fatalf("second create within hourly cap = (%d, %v)", id3, err)
	}
	_, err = store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "444444", 5*time.Minute, 0, 2, 5))
	if !errors.Is(err, identity.ErrOTPRateLimited) {
		t.Fatalf("third create over hourly cap err = %v, want ErrOTPRateLimited", err)
	}

	// 旧码 222222 已作废，新码 333333 可核验一次，随后重放必须失败。
	if err := store.VerifyPhoneOTPChallenge(ctx, phone, "222222"); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("verify superseded code = %v, want ErrOTPInvalid", err)
	}
	if err := store.VerifyPhoneOTPChallenge(ctx, phone, "333333"); err != nil {
		t.Fatalf("verify active code = %v, want nil", err)
	}
	if err := store.VerifyPhoneOTPChallenge(ctx, phone, "333333"); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("replay consumed code = %v, want ErrOTPInvalid", err)
	}
}

func TestPhoneOTPChallengeMaxAttemptsAndConcurrentReplay(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// 1. 错试达到 max_attempts=3 后自动作废，即使随后输入正确验证码也被拒绝。
	phoneLock := "+8613800138002"
	if _, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phoneLock, "654321", 5*time.Minute, 0, 10, 3)); err != nil {
		t.Fatalf("CreatePhoneOTPChallenge: %v", err)
	}
	for i := range 3 {
		if err := store.VerifyPhoneOTPChallenge(ctx, phoneLock, "000000"); !errors.Is(err, identity.ErrOTPInvalid) {
			t.Fatalf("wrong attempt %d = %v, want ErrOTPInvalid", i+1, err)
		}
	}
	if err := store.VerifyPhoneOTPChallenge(ctx, phoneLock, "654321"); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("verify right code after lockout = %v, want ErrOTPInvalid", err)
	}

	// 2. 并发重放同一正确验证码：仅允许 1 个协程消费成功，其余全部 ErrOTPInvalid。
	phoneRace := "+8613800138003"
	if _, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phoneRace, "987654", 5*time.Minute, 0, 10, 5)); err != nil {
		t.Fatalf("CreatePhoneOTPChallenge(race): %v", err)
	}
	const workers = 8
	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int32
		invalid   atomic.Int32
	)
	for range workers {
		wg.Go(func() {
			<-start
			err := store.VerifyPhoneOTPChallenge(ctx, phoneRace, "987654")
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, identity.ErrOTPInvalid):
				invalid.Add(1)
			default:
				t.Errorf("unexpected concurrent verify error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if succeeded.Load() != 1 || invalid.Load() != workers-1 {
		t.Fatalf("concurrent replay results: succeeded=%d invalid=%d, want 1/%d", succeeded.Load(), invalid.Load(), workers-1)
	}
}

func TestPhoneOTPChallengeExpiryAndCleanup(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	phone := "+8613800138004"

	id, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "123456", 5*time.Minute, 0, 10, 5))
	if err != nil {
		t.Fatalf("CreatePhoneOTPChallenge: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE phone_otp_challenges SET created_at = now() - interval '48 hours', expires_at = now() - interval '47 hours' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("backdate otp challenge: %v", err)
	}

	if err := store.VerifyPhoneOTPChallenge(ctx, phone, "123456"); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("verify expired code = %v, want ErrOTPInvalid", err)
	}

	deleted, err := store.CleanupExpiredPhoneOTPChallenges(ctx, 24*time.Hour, 100)
	if err != nil || deleted != 1 {
		t.Fatalf("CleanupExpiredPhoneOTPChallenges = (%d, %v), want (1, nil)", deleted, err)
	}

	// 新建一条刚创建未过期的记录，不应被清理。
	if _, err := store.CreatePhoneOTPChallenge(ctx, mustOTPParams(t, phone, "234567", 5*time.Minute, 0, 10, 5)); err != nil {
		t.Fatalf("create fresh challenge: %v", err)
	}
	deleted, err = store.CleanupExpiredPhoneOTPChallenges(ctx, 24*time.Hour, 100)
	if err != nil || deleted != 0 {
		t.Fatalf("cleanup fresh challenge = (%d, %v), want (0, nil)", deleted, err)
	}
}

func TestPhoneIdentitySessionSkipsEmailInvitations(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	// 手机号登录产出空邮箱且 EmailVerified=false，可正常建档签发会话且可重复登录复用同一用户。
	id := identity.ExternalIdentity{
		Issuer:        identity.PhoneIssuer,
		Subject:       "+8613800138005",
		Email:         "",
		EmailVerified: false,
		Name:          identity.MaskPhone("+8613800138005"),
	}
	first, err := store.CreateSessionForIdentity(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("first phone session: %v", err)
	}
	if first.User.Email != "" || first.User.Name != "+86****8005" {
		t.Fatalf("first phone user = %+v", first.User)
	}
	second, err := store.CreateSessionForIdentity(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("second phone session: %v", err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("repeat phone login created new user: %d vs %d", second.User.ID, first.User.ID)
	}
}
