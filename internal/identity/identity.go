/*
Package identity 定义管理请求的认证主体（Principal）和权限授权实现（阶段 11、ADR 0018）。

人与 API Key 是两种凭证，鉴权后统一写成 context 里的 Principal，
handler 只读 Principal 并通过 Authorizer 检查权限。
*/
package identity

import (
	"errors"
	"fmt"
	"slices"

	"shortlink/internal/tenant"
)

// ErrInvalidAPIKeyScope 表示 API Key 的 scope 列表为空，或包含不允许的权限值。
var ErrInvalidAPIKeyScope = errors.New("invalid api key scope")

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
AllAPIKeyScopes 返回存量 API Key 拥有的全量权限列表，同时也是规范顺序。
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
ValidAPIKeyScope 报告 perm 是否属于允许授予 API Key 的权限范围。
允许的值就是 AllAPIKeyScopes，避免和规范顺序列表各写一份以后对不上。
*/
func ValidAPIKeyScope(perm Perm) bool {
	return slices.Contains(AllAPIKeyScopes(), perm)
}

/*
ValidAPIKeyPrincipal 报告 p 能不能作为 API Key 鉴权的结果放进请求。
执行者必须是 api_key。Scopes 可以为空（没有任何权限），但不能包含
API Key 范围以外的权限，例如 members:write。角色不在这里检查：
API Key 的权限只来自 scope，不能借 Role 走用户的权限表。
*/
func ValidAPIKeyPrincipal(p Principal) bool {
	if p.ActorKind != ActorAPIKey {
		return false
	}
	for _, scope := range p.Scopes {
		if !ValidAPIKeyScope(scope) {
			return false
		}
	}
	return true
}

/*
NormalizeAPIKeyScopes 校验并规范化 API Key 的权限列表：
拒绝空列表以及非 API Key 权限（例如 members:write），去重后按 AllAPIKeyScopes 的固定顺序返回。
*/
func NormalizeAPIKeyScopes(scopes []Perm) ([]Perm, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: at least one scope is required", ErrInvalidAPIKeyScope)
	}
	seen := make(map[Perm]bool, len(scopes))
	for _, scope := range scopes {
		if !ValidAPIKeyScope(scope) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidAPIKeyScope, scope)
		}
		seen[scope] = true
	}
	normalized := make([]Perm, 0, len(seen))
	for _, scope := range AllAPIKeyScopes() {
		if seen[scope] {
			normalized = append(normalized, scope)
		}
	}
	return normalized, nil
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
