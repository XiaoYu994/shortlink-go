package identity

import "time"

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
