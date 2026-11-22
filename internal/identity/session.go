package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultSessionTTL 是 OIDC 登录签发服务端会话的默认有效期。
	DefaultSessionTTL = 7 * 24 * time.Hour

	sessionTokenPrefix = "sls_"
	sessionTokenBytes  = 32

	maxIssuerBytes  = 512
	maxSubjectBytes = 255
	maxEmailBytes   = 320
	maxNameBytes    = 255
)

var (
	// ErrInvalidOIDCIdentity 表示 OIDC 身份声明缺少必填字段或超出数据库约束长度。
	ErrInvalidOIDCIdentity = errors.New("invalid oidc identity")
	// ErrInvalidSessionTTL 表示会话有效期不是正时长（至少 1 微秒）。
	ErrInvalidSessionTTL = errors.New("session ttl must be at least 1 microsecond")
	// ErrUnauthenticated 表示会话令牌格式非法、不存在、已过期或已吊销。
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrOrgNotFound 表示用户会话有效，但目标组织不存在或当前用户不是其成员。
	ErrOrgNotFound = errors.New("organization not found")
)

// User 是平台用户主档，对应 users 表。
type User struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

/*
Session 是服务端会话的持久状态，不含明文 token 和哈希。
RevokedAt 非空表示已主动登出或被吊销。
*/
type Session struct {
	ID        int64
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IssuedSession 是登录成功后的会话签发结果；Token 只在这一次交给调用方写入 HttpOnly Cookie。
type IssuedSession struct {
	User    User
	Session Session
	Token   string
	/*
		CookieMaxAge 是数据库时钟下 expires_at 与 now() 的差。
		浏览器在同时收到 Max-Age 和 Expires 时只认 Max-Age，这个值让 Cookie 寿命跟 sessions 行对齐。
	*/
	CookieMaxAge time.Duration
}

// UserOrg 表示用户所属的一个组织及其在该组织中的角色。
type UserOrg struct {
	ID   int64
	Name string
	Role Role
}

// SessionInfo 是当前有效会话关联的用户、到期时刻与所属组织列表。
type SessionInfo struct {
	User      User
	ExpiresAt time.Time
	Orgs      []UserOrg
}

/*
OIDCIdentity 是通过 OIDC ID Token 校验后提取的外部身份声明。
用户身份以 (Issuer, Subject) 为唯一键，第一版不按邮箱自动关联不同提供方（ADR 0018）。
*/
type OIDCIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

/*
Normalize 规范化并校验 OIDC 身份声明，使其满足 users 和 user_identities 的表约束：
Issuer 与 Subject 必填且不能含首尾空白；Email 去首尾空白并转小写；
空邮箱不能标为已验证；各字段长度不超过数据库上限。
*/
func (id OIDCIdentity) Normalize() (OIDCIdentity, error) {
	if id.Issuer == "" || len(id.Issuer) > maxIssuerBytes || strings.TrimSpace(id.Issuer) != id.Issuer {
		return OIDCIdentity{}, fmt.Errorf("%w: invalid issuer", ErrInvalidOIDCIdentity)
	}
	if id.Subject == "" || len(id.Subject) > maxSubjectBytes || strings.TrimSpace(id.Subject) != id.Subject {
		return OIDCIdentity{}, fmt.Errorf("%w: invalid subject", ErrInvalidOIDCIdentity)
	}
	email := strings.ToLower(strings.TrimSpace(id.Email))
	if len(email) > maxEmailBytes {
		return OIDCIdentity{}, fmt.Errorf("%w: email exceeds %d bytes", ErrInvalidOIDCIdentity, maxEmailBytes)
	}
	name := strings.TrimSpace(id.Name)
	if len(name) > maxNameBytes {
		return OIDCIdentity{}, fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidOIDCIdentity, maxNameBytes)
	}
	return OIDCIdentity{
		Issuer:        id.Issuer,
		Subject:       id.Subject,
		Email:         email,
		EmailVerified: id.EmailVerified && email != "",
		Name:          name,
	}, nil
}

// ValidSessionToken 检查会话令牌是不是 sls_ 加 64 位小写十六进制。
func ValidSessionToken(token string) bool {
	if len(token) != len(sessionTokenPrefix)+sessionTokenBytes*2 {
		return false
	}
	if token[:len(sessionTokenPrefix)] != sessionTokenPrefix {
		return false
	}
	for _, c := range token[len(sessionTokenPrefix):] {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// HashSessionToken 计算写入 sessions.token_hash 的 32 字节 SHA-256 哈希。
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

/*
GenerateSessionToken 用 crypto/rand 生成一条新的服务端会话令牌。
plaintext 写入 HttpOnly Cookie；hash 写入 sessions.token_hash。
*/
func GenerateSessionToken() (plaintext string, hash []byte, err error) {
	var buf [sessionTokenBytes]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", nil, err
	}
	plaintext = sessionTokenPrefix + hex.EncodeToString(buf[:])
	return plaintext, HashSessionToken(plaintext), nil
}
