package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"shortlink/internal/logx"
)

const (
	// PhoneIssuer 是手机号验证码登录产出 ExternalIdentity 时使用的固定 Issuer（ADR 0030）。
	PhoneIssuer = "urn:shortlink:phone"

	// OTPCodeLength 是短信一次性验证码的固定十进制位数。
	OTPCodeLength = 6
	// OTPSaltBytes 是单条验证码挑战随机盐的长度（字节）。
	OTPSaltBytes = 16
	// OTPHashBytes 是验证码 HMAC-SHA256 摘要长度（字节）。
	OTPHashBytes = sha256.Size

	DefaultOTPTTL         = 5 * time.Minute
	DefaultOTPCooldown    = 60 * time.Second
	DefaultOTPMaxPerHour  = 5
	DefaultOTPMaxAttempts = 5
	DefaultOTPRetention   = 24 * time.Hour

	minPhoneBytes = 8  // '+' + 7 digits
	maxPhoneBytes = 16 // '+' + 15 digits (E.164)
)

var (
	// ErrInvalidPhone 表示手机号不符合严格 E.164 格式（如 +8613800138000）。
	ErrInvalidPhone = errors.New("invalid phone")
	// ErrOTPInvalid 表示验证码错误、已过期、已被消费或达到最大试错次数。
	ErrOTPInvalid = errors.New("invalid verification code")
	// ErrOTPCooldown 表示同一手机号仍在两次发码冷却期内。
	ErrOTPCooldown = errors.New("otp cooldown active")
	// ErrOTPRateLimited 表示同一手机号在统计窗口内的发码次数已达上限。
	ErrOTPRateLimited = errors.New("otp rate limit exceeded")

	otpCodeMax = big.NewInt(1_000_000)
)

/*
OTPRetryAfterError 为 ErrOTPCooldown 或 ErrOTPRateLimited 附带精确剩余等待时长，
供 HTTP 层返回标准的 Retry-After 响应头。
*/
type OTPRetryAfterError struct {
	Err        error
	RetryAfter time.Duration
}

func (e OTPRetryAfterError) Error() string { return e.Err.Error() }
func (e OTPRetryAfterError) Unwrap() error { return e.Err }

// OTPRetryAfter 提取验证码限流错误携带的建议重试等待时长。
func OTPRetryAfter(err error) (time.Duration, bool) {
	re, ok := errors.AsType[OTPRetryAfterError](err)
	if !ok {
		return 0, false
	}
	if re.RetryAfter < time.Second {
		return time.Second, true
	}
	return re.RetryAfter, true
}

// PhoneOTPChallengeParams 描述一次手机验证码挑战持久化与防刷校验所需的参数（ADR 0030）。
type PhoneOTPChallengeParams struct {
	Phone       string
	CodeSalt    []byte
	CodeHash    []byte
	TTL         time.Duration
	Cooldown    time.Duration
	MaxPerHour  int
	MaxAttempts int
}

/*
NormalizePhone 去除首尾空白并按严格 E.164 格式（^\+[1-9][0-9]{6,14}$）校验手机号。
要求显式包含国际区号，消除跨国家/地区号码归属歧义。
*/
func NormalizePhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) < minPhoneBytes || len(s) > maxPhoneBytes || s[0] != '+' {
		return "", ErrInvalidPhone
	}
	if s[1] < '1' || s[1] > '9' {
		return "", ErrInvalidPhone
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", ErrInvalidPhone
		}
	}
	return s, nil
}

// ValidOTPCode 报告验证码是否为固定 6 位纯数字字符串。
func ValidOTPCode(code string) bool {
	if len(code) != OTPCodeLength {
		return false
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

/*
MaskPhone 脱敏手机号中间位（如 +8613800138000 -> +86****8000），
用于用户默认展示名与安全日志，避免完整手机号直接暴露在组织成员列表或日志里。
*/
func MaskPhone(phone string) string {
	return logx.RedactPhone(phone)
}

// NewOTPCode 使用 crypto/rand 均匀生成 6 位数字验证码，避免取模偏差。
func NewOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, otpCodeMax)
	if err != nil {
		return "", fmt.Errorf("generate otp code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// NewOTPSalt 生成 16 字节随机盐，每条验证码挑战独立使用。
func NewOTPSalt() ([]byte, error) {
	salt := make([]byte, OTPSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate otp salt: %w", err)
	}
	return salt, nil
}

// HashOTPCode 计算绑定手机号与随机盐的 HMAC-SHA256 摘要，数据库绝不存明文验证码。
func HashOTPCode(salt []byte, phone, code string) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(phone))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

// VerifyOTPCodeHash 用常量时间比对验证手机号与验证码摘要。
func VerifyOTPCodeHash(salt, expectedHash []byte, phone, code string) bool {
	if len(salt) != OTPSaltBytes || len(expectedHash) != OTPHashBytes || !ValidOTPCode(code) {
		return false
	}
	return hmac.Equal(HashOTPCode(salt, phone, code), expectedHash)
}
