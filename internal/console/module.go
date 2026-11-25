package console

import (
	"log/slog"
	"net/http"

	"shortlink/internal/identity"
	"shortlink/internal/safehttp"
	"shortlink/internal/webhook"
)

const (
	// ModuleOrgs 是自助创建组织模块名。
	ModuleOrgs = "orgs"
	// ModuleMembers 是组织成员管理模块名。
	ModuleMembers = "members"
	// ModuleInvitations 是组织邀请管理模块名。
	ModuleInvitations = "invitations"
	// ModuleAPIKeys 是组织 API Key 管理模块名。
	ModuleAPIKeys = "api_keys"
	// ModuleAudit 是组织审计日志查询模块名。
	ModuleAudit = "audit"
	// ModuleWebhooks 是 Webhook 订阅与投递管理模块名。
	ModuleWebhooks = "webhooks"
	// ModuleAnalytics 是点击统计查询模块名。
	ModuleAnalytics = "analytics"
)

/*
RouteRegistrar 向可选 HTTP 业务模块暴露受控的路由注册与通用中间件包装能力（ADR 0029）。
模块无需触碰 Server 内部字段，只依赖最小注册上下文。
*/
type RouteRegistrar struct {
	Mux         *http.ServeMux
	IPRateLimit func(http.Handler) http.Handler
	Console     func(perm identity.Perm, h http.HandlerFunc) http.Handler
	CSRF        *http.CrossOriginProtection
	Logger      *slog.Logger
}

// Module 是管理进程可选 HTTP 业务模块的统一插件接口（2 个方法，ADR 0029）。
type Module interface {
	Name() string
	RegisterRoutes(r RouteRegistrar)
}

type orgsModule struct {
	sessions SessionManager
	orgs     OrgStore
}

// NewOrgsModule 构造自助建组织模块（POST /api/v1/orgs）。
func NewOrgsModule(sessions SessionManager, orgs OrgStore) Module {
	if sessions == nil || orgs == nil {
		return nil
	}
	return &orgsModule{sessions: sessions, orgs: orgs}
}

func (m *orgsModule) Name() string { return ModuleOrgs }

func (m *orgsModule) RegisterRoutes(r RouteRegistrar) {
	h := newOrgHandler(r.Logger, m.sessions, m.orgs, r.CSRF)
	r.Mux.Handle("POST "+apiV1Prefix+"/orgs", r.IPRateLimit(http.HandlerFunc(h.handleCreate)))
}

type membersModule struct {
	members MemberStore
}

// NewMembersModule 构造组织成员管理模块（/api/v1/members*）。
func NewMembersModule(members MemberStore) Module {
	if members == nil {
		return nil
	}
	return &membersModule{members: members}
}

func (m *membersModule) Name() string { return ModuleMembers }

func (m *membersModule) RegisterRoutes(r RouteRegistrar) {
	h := newMemberHandler(r.Logger, m.members)
	r.Mux.Handle("GET "+apiV1Prefix+"/members", r.Console(identity.PermMembersRead, h.handleList))
	r.Mux.Handle("PATCH "+apiV1Prefix+"/members/{user_id}", r.Console(identity.PermMembersWrite, h.handleUpdateRole))
	r.Mux.Handle("DELETE "+apiV1Prefix+"/members/{user_id}", r.Console(identity.PermMembersWrite, h.handleRemove))
}

type invitationsModule struct {
	invitations InvitationStore
}

// NewInvitationsModule 构造组织邀请管理模块（/api/v1/invitations*）。
func NewInvitationsModule(invitations InvitationStore) Module {
	if invitations == nil {
		return nil
	}
	return &invitationsModule{invitations: invitations}
}

func (m *invitationsModule) Name() string { return ModuleInvitations }

func (m *invitationsModule) RegisterRoutes(r RouteRegistrar) {
	h := newInvitationHandler(r.Logger, m.invitations)
	r.Mux.Handle("GET "+apiV1Prefix+"/invitations", r.Console(identity.PermMembersRead, h.handleList))
	r.Mux.Handle("POST "+apiV1Prefix+"/invitations", r.Console(identity.PermMembersWrite, h.handleCreate))
	r.Mux.Handle("DELETE "+apiV1Prefix+"/invitations/{id}", r.Console(identity.PermMembersWrite, h.handleRevoke))
}

type apiKeysModule struct {
	apiKeys APIKeyStore
}

// NewAPIKeysModule 构造组织 API Key 管理模块（/api/v1/api-keys*）。
func NewAPIKeysModule(apiKeys APIKeyStore) Module {
	if apiKeys == nil {
		return nil
	}
	return &apiKeysModule{apiKeys: apiKeys}
}

func (m *apiKeysModule) Name() string { return ModuleAPIKeys }

func (m *apiKeysModule) RegisterRoutes(r RouteRegistrar) {
	h := newAPIKeyHandler(r.Logger, m.apiKeys)
	r.Mux.Handle("GET "+apiV1Prefix+"/api-keys", r.Console(identity.PermAPIKeysRead, h.handleList))
	r.Mux.Handle("POST "+apiV1Prefix+"/api-keys", r.Console(identity.PermAPIKeysWrite, h.handleIssue))
	r.Mux.Handle("DELETE "+apiV1Prefix+"/api-keys/{id}", r.Console(identity.PermAPIKeysWrite, h.handleRevoke))
}

type auditModule struct {
	logs AuditLogStore
}

// NewAuditModule 构造组织审计日志查询模块（GET /api/v1/audit-logs）。
func NewAuditModule(logs AuditLogStore) Module {
	if logs == nil {
		return nil
	}
	return &auditModule{logs: logs}
}

func (m *auditModule) Name() string { return ModuleAudit }

func (m *auditModule) RegisterRoutes(r RouteRegistrar) {
	h := newAuditHandler(r.Logger, m.logs)
	r.Mux.Handle("GET "+apiV1Prefix+"/audit-logs", r.Console(identity.PermAuditRead, h.handleList))
}

type webhooksModule struct {
	store   WebhookStore
	cipher  *webhook.SecretCipher
	urlOpts safehttp.URLValidationOptions
}

// NewWebhooksModule 构造 Webhook 订阅与投递记录管理模块（/api/v1/webhooks*）。
func NewWebhooksModule(store WebhookStore, cipher *webhook.SecretCipher, urlOpts safehttp.URLValidationOptions) Module {
	if store == nil || cipher == nil {
		return nil
	}
	return &webhooksModule{store: store, cipher: cipher, urlOpts: urlOpts}
}

func (m *webhooksModule) Name() string { return ModuleWebhooks }

func (m *webhooksModule) RegisterRoutes(r RouteRegistrar) {
	h := newWebhookHandler(r.Logger, m.store, m.cipher, m.urlOpts)
	r.Mux.Handle("GET "+apiV1Prefix+"/webhooks", r.Console(identity.PermWebhooksRead, h.handleList))
	r.Mux.Handle("POST "+apiV1Prefix+"/webhooks", r.Console(identity.PermWebhooksWrite, h.handleCreate))
	r.Mux.Handle("PATCH "+apiV1Prefix+"/webhooks/{id}", r.Console(identity.PermWebhooksWrite, h.handleUpdate))
	r.Mux.Handle("DELETE "+apiV1Prefix+"/webhooks/{id}", r.Console(identity.PermWebhooksWrite, h.handleDelete))
	r.Mux.Handle("POST "+apiV1Prefix+"/webhooks/{id}/rotate-secret", r.Console(identity.PermWebhooksWrite, h.handleRotateSecret))
	r.Mux.Handle("POST "+apiV1Prefix+"/webhooks/{id}/test", r.Console(identity.PermWebhooksWrite, h.handleTest))
	r.Mux.Handle("GET "+apiV1Prefix+"/webhooks/{id}/deliveries", r.Console(identity.PermWebhooksRead, h.handleListDeliveries))
}

type statsModule struct {
	stats LinkStatsStore
}

// NewStatsModule 构造短链点击统计查询模块（GET /api/v1/links/{code}/stats）。
func NewStatsModule(stats LinkStatsStore) Module {
	if stats == nil {
		return nil
	}
	return &statsModule{stats: stats}
}

func (m *statsModule) Name() string { return ModuleAnalytics }

func (m *statsModule) RegisterRoutes(r RouteRegistrar) {
	h := newStatsHandler(r.Logger, m.stats)
	r.Mux.Handle("GET "+apiV1Prefix+"/links/{code}/stats", r.Console(identity.PermStatsRead, h.handleStats))
}
