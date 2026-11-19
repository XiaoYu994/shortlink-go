/*
Package identity 定义管理请求的认证主体（Principal）和权限授权实现（阶段 11、ADR 0018）。

人与 API Key 是两种凭证，鉴权后统一写成 context 里的 Principal，
handler 只读 Principal 并通过 Authorizer 检查权限。
*/
package identity

import "shortlink/internal/tenant"

// ActorKind 区分管理请求的执行者类型：人（用户会话）或机器（API Key）。
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorAPIKey ActorKind = "api_key"
)

// Role 表示用户在组织内的固定角色。
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// Perm 表示具体操作权限，由路由或 handler 检查。
type Perm string

const (
	PermLinksRead    Perm = "links:read"
	PermLinksWrite   Perm = "links:write"
	PermStatsRead    Perm = "stats:read"
	PermMembersRead  Perm = "members:read"
	PermMembersWrite Perm = "members:write"
)

/*
AllAPIKeyScopes 返回存量 API Key 拥有的全量权限列表。
每次返回新切片，避免调用方修改影响全局默认值。
*/
func AllAPIKeyScopes() []Perm {
	return []Perm{
		PermLinksRead,
		PermLinksWrite,
		PermStatsRead,
	}
}

/*
Principal 是鉴权通过后放进 context 的身份。
执行者只有用户和 API Key 两种，用结构体加 ActorKind 区分，
比接口多态更直观（ADR 0018）。
*/
type Principal struct {
	Tenant    tenant.Tenant
	ActorKind ActorKind
	ActorID   int64
	Role      Role
	Scopes    []Perm
}
