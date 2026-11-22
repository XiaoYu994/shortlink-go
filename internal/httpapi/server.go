// Package httpapi 放 HTTP 的 handler、路由和中间件。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

/*
LinkStore 是跳转和创建需要的存储能力。
接口定义在使用方，内存版和 PostgreSQL 版都只要满足它。
*/
type LinkStore interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
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

// ConsoleStore 是 console 需要的全部能力；cache 和 storemetrics 都实现它。
type ConsoleStore interface {
	LinkStore
	LinkAdminStore
	LinkStatsStore
}

/*
ClickRecorder 接收一次成功的跳转。
Record 必须马上返回。nil 表示这个 Server 不记点击，现有测试走这条路径。
*/
type ClickRecorder interface {
	Record(click.Click)
}

// RedirectOptions 是跳转进程的额外依赖。
type RedirectOptions struct {
	/*
		Clicks 为 nil 时跳转不记点击。传入的必须是未装箱的 nil：
		带类型的 nil 指针放进接口后不等于 nil，调用 Record 会 panic。
	*/
	Clicks ClickRecorder
	// VisitorSalt 是按天访客哈希的 HMAC 密钥；为空时不记访客，点击本身照常记录。
	VisitorSalt []byte
	/*
		TrustProxyHeaders 为 true 时从 X-Forwarded-For 的最右一段取客户端 IP。
		最左一段在代理追加头时可以由客户端伪造。只能开在可信反向代理后面。
	*/
	TrustProxyHeaders bool
}

// ConsoleOptions 是管理进程的鉴权、授权与可选 OIDC 登录依赖。
type ConsoleOptions struct {
	Keys       APIKeyAuthenticator
	Authorizer Authorizer
	/*
		OIDC 非空时，ConsoleRoutes 会注册 /api/v1/auth/login 和 /api/v1/auth/callback。
		Sessions 非空时，管理 API 支持会话 Cookie + X-Org-ID 鉴权，并注册 logout 和 /me。
		Members 非空时，ConsoleRoutes 注册 /api/v1/members 成员管理路由（同样依赖有效会话和成员校验）。
		三者都为空时不挂登录、会话与成员路由，只支持 API Key（ADR 0018）。
	*/
	OIDC     OIDCClient
	Sessions SessionStore
	Members  MemberStore
	// SessionTTL 是新建服务端会话的有效期；不大于 0 时使用 identity.DefaultSessionTTL。
	SessionTTL time.Duration
}

// Server 持有 handler 需要的依赖。用 NewRedirector 或 NewConsole 构造。
type Server struct {
	logger         *slog.Logger
	store          LinkStore
	admin          LinkAdminStore
	stats          LinkStatsStore
	keys           APIKeyAuthenticator
	sessions       SessionAuthenticator
	authz          Authorizer
	csrf           *http.CrossOriginProtection
	auth           *authHandler
	members        *memberHandler
	requestTimeout time.Duration
	registry       *prometheus.Registry
	metrics        *httpMetrics
	// limiter 是管理 API 的进程内按租户令牌桶，redirector 不用。
	limiter     *tenantRateLimiter
	clicks      ClickRecorder
	visitorSalt []byte
	trustProxy  bool
	/*
		ready 是 /readyz 要问的检查项，readyWired 记录 SetReadiness 有没有调用过。
		两者分开：没接线要返回 503，接了线但一项都不查是合法的（redirector 就是这样）。
	*/
	ready      []ReadyCheck
	readyWired bool
	/*
		draining 为 true 时 /readyz 直接返回 503。关停时先置上，等探针把副本摘掉，再关闭监听。
		/healthz 不看它：进程还活着，不该被当成需要重启。
	*/
	draining atomic.Bool
}

// serverDeps 是 newServer 的参数，避免一长串容易错位的位置参数。
type serverDeps struct {
	logger            *slog.Logger
	store             LinkStore
	admin             LinkAdminStore
	stats             LinkStatsStore
	keys              APIKeyAuthenticator
	sessions          SessionAuthenticator
	authorizer        Authorizer
	oidc              OIDCClient
	sessionMgr        SessionManager
	members           MemberStore
	sessionTTL        time.Duration
	requestTimeout    time.Duration
	registry          *prometheus.Registry
	clicks            ClickRecorder
	visitorSalt       []byte
	trustProxyHeaders bool
}

// NewRedirector 组装公开跳转进程，不注册管理路由。
func NewRedirector(logger *slog.Logger, store LinkStore, requestTimeout time.Duration, registry *prometheus.Registry, opts RedirectOptions) *Server {
	return newServer(serverDeps{
		logger:            logger,
		store:             store,
		requestTimeout:    requestTimeout,
		registry:          registry,
		clicks:            opts.Clicks,
		visitorSalt:       opts.VisitorSalt,
		trustProxyHeaders: opts.TrustProxyHeaders,
	})
}

/*
NewConsole 组装管理进程。
Authorizer 为 nil 时，已经通过鉴权的管理请求返回 500，而不是放开权限。
Keys 为 nil 时，带 Authorization 的请求返回 500；不带 Authorization 且 Sessions 非空时仍走会话鉴权。
Keys 和 Sessions 都为空时，管理请求返回 500。
ConsoleStore 同时提供跳转读取、管理 CRUD 和统计查询。
*/
func NewConsole(logger *slog.Logger, store ConsoleStore, requestTimeout time.Duration, registry *prometheus.Registry, opts ConsoleOptions) *Server {
	return newServer(serverDeps{
		logger:         logger,
		store:          store,
		admin:          store,
		stats:          store,
		keys:           opts.Keys,
		sessions:       opts.Sessions,
		authorizer:     opts.Authorizer,
		oidc:           opts.OIDC,
		sessionMgr:     opts.Sessions,
		members:        opts.Members,
		sessionTTL:     opts.SessionTTL,
		requestTimeout: requestTimeout,
		registry:       registry,
	})
}

/*
newServer 是两个构造函数共用的实现，让 main 只拿到自己那一半依赖：
redirector 没有 keys 和 admin，console 没有 clicks。测试在同一个包内，
也可以直接用它同时组装两组路由，或者只给其中一部分依赖。

logger 包上一层 requestIDHandler：带请求 context 的日志会自动写上 request_id。
requestTimeout 是每个请求的处理时限，由 timeout 中间件执行，必须为正。
registry 由 main 创建并注入，/metrics 从这里收集；测试每次传一个新的 Registry，
避免同名指标重复注册。HTTP 指标在这个构造函数里注册一次，两个路由方法只组装中间件。
*/
func newServer(d serverDeps) *Server {
	logger := slog.New(requestIDHandler{Handler: d.logger.Handler()})
	csrf := http.NewCrossOriginProtection()
	var auth *authHandler
	if d.oidc != nil || d.sessionMgr != nil {
		auth = newAuthHandler(logger, d.oidc, d.sessionMgr, csrf, d.sessionTTL)
	}
	var members *memberHandler
	if d.members != nil {
		members = newMemberHandler(logger, d.members)
	}
	return &Server{
		logger:         logger,
		store:          d.store,
		admin:          d.admin,
		stats:          d.stats,
		keys:           d.keys,
		sessions:       d.sessions,
		authz:          d.authorizer,
		csrf:           csrf,
		auth:           auth,
		members:        members,
		requestTimeout: d.requestTimeout,
		registry:       d.registry,
		metrics:        newHTTPMetrics(d.registry),
		limiter:        newTenantRateLimiter(),
		clicks:         d.clicks,
		visitorSalt:    d.visitorSalt,
		trustProxy:     d.trustProxyHeaders,
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

// clientGone 报告请求是否因客户端断开而被取消，这时响应已经没人接收，handler 直接返回，不要记成服务端错误。
func (s *Server) clientGone(r *http.Request) bool {
	return isClientGone(s.logger, r)
}

func isClientGone(logger *slog.Logger, r *http.Request) bool {
	if r.Context().Err() == context.Canceled {
		logger.DebugContext(r.Context(), "request canceled by client", "path", r.URL.Path)
		return true
	}
	return false
}

func (s *Server) writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	writeJSONResponse(s.logger, ctx, w, status, v)
}

func writeJSONResponse(logger *slog.Logger, ctx context.Context, w http.ResponseWriter, status int, v any) {
	/*
		先编码到内存，成功后再写响应。直接 Encoder.Encode 会在状态码已经发出后才可能失败，客户端只能收到半截 body。
		ctx 只负责把 request_id 带进日志，不用它取消已经开始的写出。
	*/
	data, err := json.Marshal(v)
	if err != nil {
		logger.ErrorContext(ctx, "marshal JSON response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		logger.ErrorContext(ctx, "write JSON response", "err", err)
	}
}

func (s *Server) writeError(ctx context.Context, w http.ResponseWriter, status int, msg string) {
	writeErrorResponse(s.logger, ctx, w, status, msg)
}

func writeErrorResponse(logger *slog.Logger, ctx context.Context, w http.ResponseWriter, status int, msg string) {
	writeJSONResponse(logger, ctx, w, status, errorResponse{Error: msg})
}

/*
temporaryError 由临时过载错误实现，比如 cache 在 Redis 故障时回源并发已满。
HTTP 层不认识具体存储实现，只看这个能力；这类失败返回 503，让客户端稍后重试。
*/
type temporaryError interface {
	Temporary() bool
}

/*
handleStoreError 把存储调用的失败映射成响应。
客户端断开时什么都不写；请求超时返回 504；临时过载返回 503；其余情况记错误日志并返回 500。
attrs 是额外的日志字段。跳转失败时带上短码；创建失败时短码还没生成，不传。

超时和过载是正常生效的机制，不是存储故障，所以记 Warn：
数据库慢或负载高时这类日志会成片出现，不该和真正的 bug 混在一个级别。
*/
func (s *Server) handleStoreError(w http.ResponseWriter, r *http.Request, op string, err error, attrs ...any) {
	handleStoreErr(s.logger, w, r, op, err, attrs...)
}

func handleStoreErr(logger *slog.Logger, w http.ResponseWriter, r *http.Request, op string, err error, attrs ...any) {
	if isClientGone(logger, r) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		args := append([]any{"op", op, "err", err}, attrs...)
		logger.WarnContext(r.Context(), "request timed out", args...)
		writeErrorResponse(logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
		return
	}
	var temp temporaryError
	if errors.As(err, &temp) && temp.Temporary() {
		args := append([]any{"op", op, "err", err}, attrs...)
		logger.WarnContext(r.Context(), "store overloaded", args...)
		writeErrorResponse(logger, r.Context(), w, http.StatusServiceUnavailable, "service temporarily overloaded")
		return
	}
	args := append([]any{"err", err}, attrs...)
	logger.ErrorContext(r.Context(), op, args...)
	writeErrorResponse(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
}

/*
handleAdminStoreError 统一管理接口的存储错误映射：
找不到（包括其他租户）一律 404；被封禁的链接 403，链接存在这件事不需要隐藏，
租户要能分清是自己删了还是被封了；其余交给 handleStoreError。
*/
func (s *Server) handleAdminStoreError(w http.ResponseWriter, r *http.Request, op string, err error, code string) {
	switch {
	case errors.Is(err, link.ErrNotFound):
		s.writeLinkNotFound(r, w)
	case errors.Is(err, link.ErrBanned):
		s.writeError(r.Context(), w, http.StatusForbidden, link.ErrBanned.Error())
	default:
		s.handleStoreError(w, r, op, err, "code", code)
	}
}
