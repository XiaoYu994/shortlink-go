/*
Package identity 定义管理请求的认证主体（Principal）、API Key 与会话令牌格式/哈希、
OIDC / GitHub OAuth 2.0 客户端、权限授权实现与组织成员管理规则（阶段 11、ADR 0018/0019）。

人与 API Key 是两种凭证，鉴权后统一写成 context 里的 Principal，
handler 只读 Principal 并通过 Authorizer 检查权限。
OIDC 发现、ID Token 验签与 GitHub OAuth 2.0 授权码/用户信息拉取也收口在本包，console 只编排 HTTP 跳转、Cookie 和错误响应。
*/
package identity

import (
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"unicode"

	"shortlink/internal/tenant"
)

var (
	// ErrUnauthenticated 表示 API Key 或会话令牌格式非法、不存在、已过期或已吊销。HTTP 层映射成 401。
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrInvalidAPIKeyScope 表示 API Key 的 scope 列表为空，或包含不允许的权限值。
	ErrInvalidAPIKeyScope = errors.New("invalid api key scope")
	// ErrInvalidRole 表示组织角色不是系统允许的枚举值。
	ErrInvalidRole = errors.New("invalid role")
	// ErrInvalidEmail 表示邮箱格式不合法或超出数据库约束长度。
	ErrInvalidEmail = errors.New("invalid email")
	// ErrInvalidOrgName 表示组织名称为空、过长或包含非法控制字符。
	ErrInvalidOrgName = errors.New("invalid organization name")
	// ErrOrgLimitExceeded 表示用户作为 owner 创建的组织数已达上限。
	ErrOrgLimitExceeded = errors.New("organization limit exceeded")
	// ErrInvitationPending 表示同一租户对该邮箱已存在待兑现邀请。
	ErrInvitationPending = errors.New("pending invitation already exists")
	// ErrInvitationExpiresNotFuture 表示创建邀请时给出的过期时间不在将来。
	ErrInvitationExpiresNotFuture = errors.New("expires_at must be in the future")
	// ErrInvitationNotFound 表示邀请不存在或不属于当前组织。
	ErrInvitationNotFound = errors.New("invitation not found")
	// ErrInvitationNotPending 表示邀请已兑现或已撤销，不能再次撤销。
	ErrInvitationNotPending = errors.New("invitation is no longer pending")
	// ErrAPIKeyNotFound 表示密钥不存在或不属于当前组织。
	ErrAPIKeyNotFound = errors.New("api key not found")
	// ErrAPIKeyRevoked 表示这把密钥已经吊销过。
	ErrAPIKeyRevoked = errors.New("api key already revoked")
	// ErrExpiresNotFuture 表示签发密钥时给出的过期时间不在将来。
	ErrExpiresNotFuture = errors.New("expires_at must be in the future")
)

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

// Valid 报告 r 是否属于系统定义的固定组织角色。
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	default:
		return false
	}
}

/*
NormalizeEmail 规范化并校验邮箱地址：去首尾空白、转小写、限制长度在 3..320 字节，
只接受裸 addr-spec（拒绝显示名和尖括号），且域名必须包含非首尾的点号。
*/
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < 3 || len(email) > maxEmailBytes {
		return "", fmt.Errorf("%w: invalid length", ErrInvalidEmail)
	}
	if strings.ContainsFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%w: contains whitespace or control character", ErrInvalidEmail)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, raw)
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, raw)
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, raw)
	}
	return email, nil
}

// Perm 表示具体操作权限，由路由或 handler 检查。
type Perm string

const (
	PermLinksRead    Perm = "links:read"
	PermLinksWrite   Perm = "links:write"
	PermStatsRead    Perm = "stats:read"
	PermMembersRead  Perm = "members:read"
	PermMembersWrite Perm = "members:write"
	PermAPIKeysRead  Perm = "apikeys:read"
	PermAPIKeysWrite Perm = "apikeys:write"
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
执行者必须是 api_key。Scopes 可以为空（没有任何权限，授权时一律拒绝），但不能包含
API Key 范围以外的权限，例如 members:write。角色不在这里检查：
API Key 的权限只来自 scope，不能借 Role 走用户的权限表。
这里允许空和 NormalizeAPIKeyScopes 拒绝空并不矛盾：后者管签发，不能发出没有权限的密钥；
前者管鉴权结果，Authenticate 丢掉未知 scope 后可能为空，这时请求应当 403 而不是 500。
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
ValidUserPrincipal 报告 p 能不能作为用户会话鉴权的结果放进请求。
执行者必须是 user，具备正的 ActorID 与 Tenant.ID，角色属于固定枚举，
且不能携带 API Key 的 Scopes（用户的权限只来自角色映射）。
*/
func ValidUserPrincipal(p Principal) bool {
	return p.ActorKind == ActorUser &&
		p.ActorID > 0 &&
		p.Tenant.ID > 0 &&
		p.Role.Valid() &&
		len(p.Scopes) == 0
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
