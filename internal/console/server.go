// Package console 放管理进程（console）的 HTTP handler、路由、鉴权、按租户限流与可插拔业务模块（ADR 0009、ADR 0019、ADR 0029）。
package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/httpx"
	"shortlink/internal/link"
	"shortlink/internal/ratelimit"
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

// LinkStore 是管理进程核心短链接口需要的存储能力（不含可选统计查询）。
type LinkStore interface {
	LinkCreator
	LinkAdminStore
}

// Store 是包含统计查询的完整管理存储能力；cache 和 storemetrics 都实现它。
type Store interface {
	LinkStore
	LinkStatsStore
}

// Options 是管理进程的鉴权、授权、可插拔认证方式与业务模块装配选项（ADR 0029）。
type Options struct {
	Keys       APIKeyAuthenticator
	Authorizer Authorizer
	// Sessions 非空时支持会话 Cookie + X-Org-ID 鉴权，并注册 logout 和 /me。
	Sessions SessionStore
	// AuthMethods 是启用的用户登录方式插件（如 OIDC、GitHub 或未来手机号 OTP）。
	AuthMethods []AuthMethod
	// Modules 是启用的可选 HTTP 业务模块（orgs、members、invitations、api_keys、audit、webhooks、analytics 等）。
	Modules []Module
	// RiskScanEnabled 与 EmailEnabled 用于在 /api/v1/capabilities 报告非 HTTP 后台特性状态。
	RiskScanEnabled bool
	EmailEnabled    bool
	// WebDistDir 非空时在 / 挂载前端构建产物（SPA），不拦截 /api/*、/r/* 和运维端点（ADR 0021）。
	WebDistDir string
	// PublicBaseURL 是公开短链跳转基地址（如 https://s.example.com），随 /api/v1/capabilities 下发给前端展示与复制。
	PublicBaseURL string
	// SessionTTL 是新建服务端会话的有效期；不大于 0 时使用 identity.DefaultSessionTTL。
	SessionTTL time.Duration
	// TrustProxyHeaders 与 TrustedProxyCIDRs 控制访问日志与鉴权前按 IP 限流的客户端 IP 解析。
	TrustProxyHeaders bool
	TrustedProxyCIDRs []netip.Prefix
	// IPRateLimit 与 IPRateBurst 控制鉴权前按源 IP 限流（RPS 与突发容量；<=0 时不启用）。
	IPRateLimit int
	IPRateBurst int
}

// Server 持有管理进程核心依赖与已注册的可选模块，复用 httpx 提供的运维端点与通用中间件。
type Server struct {
	base            *httpx.Server
	logger          *slog.Logger
	store           LinkCreator
	admin           LinkAdminStore
	keys            APIKeyAuthenticator
	sessions        SessionAuthenticator
	authz           Authorizer
	csrf            *http.CrossOriginProtection
	auth            *authHandler
	modules         []Module
	moduleSet       map[string]struct{}
	riskScanEnabled bool
	emailEnabled    bool
	web             http.Handler
	publicBaseURL   string
	limiter         *ratelimit.Limiter[int64]
	ipLimiter       *ipRateLimiter
	// rateLimited 与 ipRateLimited 只属于管理 API；在 newServer 里注册一次，不在 Routes 中重复注册。
	rateLimited   prometheus.Counter
	ipRateLimited prometheus.Counter
}

// serverDeps 是 newServer 的参数，避免一长串容易错位的位置参数。
type serverDeps struct {
	logger            *slog.Logger
	store             LinkCreator
	admin             LinkAdminStore
	keys              APIKeyAuthenticator
	sessions          SessionAuthenticator
	authorizer        Authorizer
	sessionMgr        SessionManager
	authMethods       []AuthMethod
	modules           []Module
	riskScanEnabled   bool
	emailEnabled      bool
	webDistDir        string
	publicBaseURL     string
	sessionTTL        time.Duration
	trustProxyHeaders bool
	trustedProxyCIDRs []netip.Prefix
	ipRateLimit       int
	ipRateBurst       int
	requestTimeout    time.Duration
	registry          *prometheus.Registry
}

/*
New 组装管理进程，不注册跳转路由。
Authorizer 为 nil 时，已经通过鉴权的管理请求返回 500，而不是放开权限。
Keys 为 nil 时，带 Authorization 的请求返回 500；不带 Authorization 且 Sessions 非空时仍走会话鉴权。
Keys 和 Sessions 都为空时，管理请求返回 500。
*/
func New(logger *slog.Logger, store LinkStore, requestTimeout time.Duration, registry *prometheus.Registry, opts Options) *Server {
	return newServer(serverDeps{
		logger:            logger,
		store:             store,
		admin:             store,
		keys:              opts.Keys,
		sessions:          opts.Sessions,
		authorizer:        opts.Authorizer,
		sessionMgr:        opts.Sessions,
		authMethods:       opts.AuthMethods,
		modules:           opts.Modules,
		riskScanEnabled:   opts.RiskScanEnabled,
		emailEnabled:      opts.EmailEnabled,
		webDistDir:        opts.WebDistDir,
		publicBaseURL:     opts.PublicBaseURL,
		sessionTTL:        opts.SessionTTL,
		trustProxyHeaders: opts.TrustProxyHeaders,
		trustedProxyCIDRs: opts.TrustedProxyCIDRs,
		ipRateLimit:       opts.IPRateLimit,
		ipRateBurst:       opts.IPRateBurst,
		requestTimeout:    requestTimeout,
		registry:          registry,
	})
}

func newServer(d serverDeps) *Server {
	base := httpx.New(d.logger, d.requestTimeout, d.registry)
	base.SetTrustedProxies(d.trustProxyHeaders, d.trustedProxyCIDRs)
	logger := base.Logger()
	csrf := http.NewCrossOriginProtection()

	var auth *authHandler
	hasNonNilMethod := false
	for _, m := range d.authMethods {
		if m != nil {
			hasNonNilMethod = true
			break
		}
	}
	if hasNonNilMethod || d.sessionMgr != nil {
		auth = newAuthHandler(logger, d.authMethods, d.sessionMgr, csrf, d.sessionTTL)
	}

	modules, moduleSet, err := indexModules(d.modules)
	if err != nil {
		panic(fmt.Sprintf("console: %v", err))
	}

	var web http.Handler
	if d.webDistDir != "" {
		web = newWebSPAHandler(d.webDistDir)
	}
	return &Server{
		base:            base,
		logger:          logger,
		store:           d.store,
		admin:           d.admin,
		keys:            d.keys,
		sessions:        d.sessions,
		authz:           d.authorizer,
		csrf:            csrf,
		auth:            auth,
		modules:         modules,
		moduleSet:       moduleSet,
		riskScanEnabled: d.riskScanEnabled,
		emailEnabled:    d.emailEnabled,
		web:             web,
		publicBaseURL:   d.publicBaseURL,
		limiter:         ratelimit.New[int64](ratelimit.Options{}),
		ipLimiter:       newIPRateLimiter(d.ipRateLimit, d.ipRateBurst, defaultMaxIPBuckets),
		rateLimited:     newRateLimitedCounter(d.registry),
		ipRateLimited:   newIPRateLimitedCounter(d.registry),
	}
}

func (s *Server) hasModule(name string) bool {
	_, ok := s.moduleSet[name]
	return ok
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
	handleAdminStoreError(s.logger, w, r, op, err, code)
}

func handleAdminStoreError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, op string, err error, code string) {
	switch {
	case errors.Is(err, link.ErrNotFound):
		writeLinkNotFound(logger, r, w)
	case errors.Is(err, link.ErrBanned):
		httpx.WriteError(logger, r.Context(), w, http.StatusForbidden, link.ErrBanned.Error())
	default:
		httpx.HandleStoreError(logger, w, r, op, err, "code", code)
	}
}
