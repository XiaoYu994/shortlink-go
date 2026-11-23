package identity

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	// MaxOrgNameBytes 限制组织名称长度，避免超长输入写入 tenants.name。
	MaxOrgNameBytes = 100
	// DefaultMaxOwnedOrgs 是单用户通过自助接口作为 owner 创建组织的数量上限（ADR 0021）。
	DefaultMaxOwnedOrgs = 10
)

// Invitation 表示预先登记的组织成员邀请，对应 invitations 表。
type Invitation struct {
	ID         int64
	TenantID   int64
	Email      string
	Role       Role
	InvitedBy  *int64
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	AcceptedAt *time.Time
	AcceptedBy *int64
	RevokedAt  *time.Time
}

// Status 按固定优先级派生邀请当前状态：accepted、revoked、expired、pending。
func (inv Invitation) Status(now time.Time) string {
	switch {
	case inv.AcceptedAt != nil:
		return "accepted"
	case inv.RevokedAt != nil:
		return "revoked"
	case inv.ExpiresAt != nil && !inv.ExpiresAt.After(now):
		return "expired"
	default:
		return "pending"
	}
}

// CreateInvitationParams 是通过管理接口创建组织邀请的领域参数。
type CreateInvitationParams struct {
	TenantID  int64
	Email     string
	Role      Role
	InvitedBy int64
	ExpiresAt *time.Time
}

// RevokeInvitationParams 是通过管理接口撤销组织邀请的领域参数。
type RevokeInvitationParams struct {
	TenantID     int64
	InvitationID int64
	ActorRole    Role
}

// CreatedOrg 是自助创建组织成功后的结果。
type CreatedOrg struct {
	ID        int64
	Name      string
	Role      Role
	CreatedAt time.Time
}

// APIKeyInfo 是组织内 API Key 的只读元数据（不含明文与哈希）。
type APIKeyInfo struct {
	ID        int64
	TenantID  int64
	Scopes    []Perm
	CreatedBy *int64
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// Status 派生 API Key 当前状态：revoked、expired、active。
func (k APIKeyInfo) Status(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return "expired"
	default:
		return "active"
	}
}

// IssueOrgAPIKeyParams 是组织内自助签发 API Key 的参数。
type IssueOrgAPIKeyParams struct {
	TenantID  int64
	CreatedBy int64
	Scopes    []Perm
	ExpiresAt *time.Time
}

// IssuedOrgAPIKey 是组织内签发 API Key 的结果，Plaintext 只在签发响应返回一次。
type IssuedOrgAPIKey struct {
	Key       APIKeyInfo
	Plaintext string
}

// NormalizeOrgName 去除首尾空白并校验组织名称长度及非法控制字符。
func NormalizeOrgName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if len(name) == 0 || len(name) > MaxOrgNameBytes {
		return "", fmt.Errorf("%w: length must be between 1 and %d bytes", ErrInvalidOrgName, MaxOrgNameBytes)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: must not contain control characters", ErrInvalidOrgName)
	}
	return name, nil
}
