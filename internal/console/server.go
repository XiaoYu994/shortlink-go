// Package console 放管理进程（console）的 HTTP handler、路由、鉴权和按租户限流（ADR 0009、ADR 0019）。
package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/httpx"
	"shortlink/internal/link"
)

/*
LinkCreator 是管理创建接口需要的存储能力。
接口定义在使用方；内存版、PostgreSQL 版和装饰器都只要满足它。
*/
type LinkCreator interface {
	Create(ctx context.Context, l *link.Link) error
}

/*
LinkAdminStore 是管理接口的存储能力。
所有方法都带 tenantID，由 handler 从鉴权 context 取出，不能来自请求体。
*/
type LinkAdminStore interface {
	List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error)
	GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error)
	Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error)
	Delete(ctx context.Context, tenantID int64, code string) error
}

// LinkStatsStore 提供统计查询，不经过 Redis 缓存。
type LinkStatsStore interface {
	Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error)
}

// Store 是 console 需要的全部存储能力；cache 和 storemetrics 都实现它。
type Store interface {
	LinkCreator
	LinkAdminStore
	LinkStatsStore
}

// Options 是管理进程的鉴权、授权与可选外部登录（OIDC / GitHub OAuth 2.0）依赖。
type Options struct {
	Keys       APIKeyAuthenticator
	Authorizer Authorizer
	/*
		OIDC 非空时，Routes 注册 /api/v1/auth/login 和 /api/v1/auth/callback。
		GitHub 非空时，Routes 注册 /api/v1/auth/github/login 和 /api/v1/auth/github/callback。
		Sessions 非空时，管理 API 支持会话 Cookie + X-Org-ID 鉴权，并注册 logout 和 /me。
		Members / Orgs / Invitations / APIKeys 非空时，分别注册对应组织自助与成员管理路由（ADR 0018、ADR 0021）。
		全为空时不挂登录、会话与自助路由，只支持 API Key（ADR 0018）。
	*/
	OIDC        OIDCClient
	GitHub      GitHubClient
	Sessions    SessionStore
	Members     MemberStore
	Orgs        OrgStore
	Invitations InvitationStore
	APIKeys     APIKeyStore
	// WebDistDir 非空时在 / 挂载前端构建产物（SPA），不拦截 /api/*、/r/* 和运维端点（ADR 0021）。
	WebDistDir string
	// PublicBaseURL 是公开短链跳转基地址（如 https://s.example.com），随 /api/v1/auth/providers 下发给前端展示与复制。
	PublicBaseURL string
	// SessionTTL 是新建服务端会话的有效期；不大于 0 时使用 identity.DefaultSessionTTL。
	SessionTTL time.Duration
}

// Server 持有管理进程需要的依赖，复用 httpx 提供的运维端点与通用中间件。
type Server struct {
	base          *httpx.Server
	logger        *slog.Logger
	store         LinkCreator
	admin         LinkAdminStore
	stats         LinkStatsStore
	keys          APIKeyAuthenticator
	sessions      SessionAuthenticator
	authz         Authorizer
	csrf          *http.CrossOriginProtection
	auth          *authHandler
	members       *memberHandler
	orgs          *orgHandler
	invitations   *invitationHandler
	apiKeys       *apiKeyHandler
	web           http.Handler
	publicBaseURL string
	limiter       *tenantRateLimiter
	// rateLimited 只属于管理 API；在 newServer 里注册一次，不在 Routes 中重复注册。
	rateLimited prometheus.Counter
}

// serverDeps 是 newServer 的参数，避免一长串容易错位的位置参数。
type serverDeps struct {
	logger         *slog.Logger
	store          LinkCreator
	admin          LinkAdminStore
	stats          LinkStatsStore
	keys           APIKeyAuthenticator
	sessions       SessionAuthenticator
	authorizer     Authorizer
	oidc           OIDCClient
	github         GitHubClient
	sessionMgr     SessionManager
	members        MemberStore
	orgs           OrgStore
	invitations    InvitationStore
	apiKeys        APIKeyStore
	webDistDir     string
	publicBaseURL  string
	sessionTTL     time.Duration
	requestTimeout time.Duration
	registry       *prometheus.Registry
}

/*
New 组装管理进程，不注册跳转路由。
Authorizer 为 nil 时，已经通过鉴权的管理请求返回 500，而不是放开权限。
Keys 为 nil 时，带 Authorization 的请求返回 500；不带 Authorization 且 Sessions 非空时仍走会话鉴权。
Keys 和 Sessions 都为空时，管理请求返回 500。
Store 同时提供链接创建、管理 CRUD 和统计查询。
*/
func New(logger *slog.Logger, store Store, requestTimeout time.Duration, registry *prometheus.Registry, opts Options) *Server {
	return newServer(serverDeps{
		logger:         logger,
		store:          store,
		admin:          store,
		stats:          store,
		keys:           opts.Keys,
		sessions:       opts.Sessions,
		authorizer:     opts.Authorizer,
		oidc:           opts.OIDC,
		github:         opts.GitHub,
		sessionMgr:     opts.Sessions,
		members:        opts.Members,
		orgs:           opts.Orgs,
		invitations:    opts.Invitations,
		apiKeys:        opts.APIKeys,
		webDistDir:     opts.WebDistDir,
		publicBaseURL:  opts.PublicBaseURL,
		sessionTTL:     opts.SessionTTL,
		requestTimeout: requestTimeout,
		registry:       registry,
	})
}

func newServer(d serverDeps) *Server {
	base := httpx.New(d.logger, d.requestTimeout, d.registry)
	logger := base.Logger()
	csrf := http.NewCrossOriginProtection()
	var auth *authHandler
	if d.oidc != nil || d.github != nil || d.sessionMgr != nil {
		auth = newAuthHandler(logger, d.oidc, d.github, d.sessionMgr, csrf, d.sessionTTL)
	}
	var members *memberHandler
	if d.members != nil {
		members = newMemberHandler(logger, d.members)
	}
	var orgs *orgHandler
	if d.orgs != nil && d.sessionMgr != nil {
		orgs = newOrgHandler(logger, d.sessionMgr, d.orgs, csrf)
	}
	var invitations *invitationHandler
	if d.invitations != nil {
		invitations = newInvitationHandler(logger, d.invitations)
	}
	var apiKeys *apiKeyHandler
	if d.apiKeys != nil {
		apiKeys = newAPIKeyHandler(logger, d.apiKeys)
	}
	var web http.Handler
	if d.webDistDir != "" {
		web = newWebSPAHandler(d.webDistDir)
	}
	return &Server{
		base:          base,
		logger:        logger,
		store:         d.store,
		admin:         d.admin,
		stats:         d.stats,
		keys:          d.keys,
		sessions:      d.sessions,
		authz:         d.authorizer,
		csrf:          csrf,
		auth:          auth,
		members:       members,
		orgs:          orgs,
		invitations:   invitations,
		apiKeys:       apiKeys,
		web:           web,
		publicBaseURL: d.publicBaseURL,
		limiter:       newTenantRateLimiter(),
		rateLimited:   newRateLimitedCounter(d.registry),
	}
}

// SetReadiness 接入就绪检查；后一次调用覆盖前一次。
func (s *Server) SetReadiness(checks ...httpx.ReadyCheck) {
	s.base.SetReadiness(checks...)
}

// SetDraining 让 /readyz 开始返回 503，/healthz 不变。
func (s *Server) SetDraining() {
	s.base.SetDraining()
}

/*
handleAdminStoreError 统一管理接口的存储错误映射：
找不到（包括其他租户）一律 404；被封禁的链接 403，链接存在这件事不需要隐藏，
租户要能分清是自己删了还是被封了；其余交给 httpx.HandleStoreError。
*/
func (s *Server) handleAdminStoreError(w http.ResponseWriter, r *http.Request, op string, err error, code string) {
	switch {
	case errors.Is(err, link.ErrNotFound):
		s.writeLinkNotFound(r, w)
	case errors.Is(err, link.ErrBanned):
		httpx.WriteError(s.logger, r.Context(), w, http.StatusForbidden, link.ErrBanned.Error())
	default:
		httpx.HandleStoreError(s.logger, w, r, op, err, "code", code)
	}
}
