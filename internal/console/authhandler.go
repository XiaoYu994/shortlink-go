package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

const (
	// SessionCookieName 是服务端会话 Cookie 名，作用范围限定在 /api/，不发往 /r/{code}。
	SessionCookieName = "sl_session"

	authLogoutPath    = apiV1Prefix + "/auth/logout"
	mePath            = apiV1Prefix + "/me"
	sessionCookiePath = "/api/"
)

/*
SessionManager 负责服务端会话的签发、当前状态查询与吊销。
接口放在使用方，方法数保持在 3 个以内，生产由 postgres.Store 实现（ADR 0018）。
*/
type SessionManager interface {
	CreateSessionForIdentity(ctx context.Context, id identity.ExternalIdentity, ttl time.Duration) (identity.IssuedSession, error)
	GetSessionInfo(ctx context.Context, token string) (identity.SessionInfo, error)
	RevokeSession(ctx context.Context, token string) error
}

// SessionStore 汇总管理进程需要的全部会话读写能力；postgres.Store 隐式实现它。
type SessionStore interface {
	SessionAuthenticator
	SessionManager
}

/*
authHandler 承接所有已注册 AuthMethod 的路由挂载、统一会话签发（SessionIssuer）以及 /logout 与 /me（ADR 0019、ADR 0029）。
不依赖短链存储或租户限流器，也不耦合具体登录协议（OAuth 跳转、手机号验证码等均经 AuthMethod 接入）。
*/
type authHandler struct {
	logger     *slog.Logger
	sessions   SessionManager
	methods    []AuthMethod
	csrf       *http.CrossOriginProtection
	sessionTTL time.Duration
}

func newAuthHandler(logger *slog.Logger, methods []AuthMethod, sessions SessionManager, csrf *http.CrossOriginProtection, sessionTTL time.Duration) *authHandler {
	if sessionTTL <= 0 {
		sessionTTL = identity.DefaultSessionTTL
	}
	var filtered []AuthMethod
	for _, m := range methods {
		if m != nil {
			filtered = append(filtered, m)
		}
	}
	if err := ValidateAuthMethods(filtered); err != nil {
		panic(fmt.Sprintf("console: %v", err))
	}
	return &authHandler{
		logger:     logger,
		sessions:   sessions,
		methods:    filtered,
		csrf:       csrf,
		sessionTTL: sessionTTL,
	}
}

func (h *authHandler) registerRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	if wrap == nil {
		wrap = func(next http.Handler) http.Handler { return next }
	}
	var issuer SessionIssuer
	if h.sessions != nil {
		issuer = h
	}
	tk := AuthToolkit{
		Mux:         mux,
		IPRateLimit: wrap,
		CSRF:        h.csrf,
		Issuer:      issuer,
		Logger:      h.logger,
	}
	for _, m := range h.methods {
		m.RegisterRoutes(tk)
	}
	if h.sessions != nil {
		mux.Handle("POST "+authLogoutPath, wrap(http.HandlerFunc(h.handleLogout)))
		mux.Handle("GET "+mePath, wrap(http.HandlerFunc(h.handleMe)))
	}
}

func (h *authHandler) descriptors() []AuthMethodDescriptor {
	out := make([]AuthMethodDescriptor, 0, len(h.methods))
	for _, m := range h.methods {
		out = append(out, m.Descriptor())
	}
	return out
}

func (h *authHandler) logoutSecureCookies() bool {
	for _, m := range h.methods {
		if m.SecureCookies() {
			return true
		}
	}
	return false
}

type loginResponse struct {
	User      loginUserResponse `json:"user"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type loginUserResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type meResponse struct {
	User          loginUserResponse `json:"user"`
	ExpiresAt     time.Time         `json:"expires_at"`
	Organizations []meOrgResponse   `json:"organizations"`
}

type meOrgResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	LinkUsage int64  `json:"link_usage"`
	LinkQuota int64  `json:"link_quota"`
}

/*
IssueSession 实现 SessionIssuer：在任意 AuthMethod 核验身份成功后，
统一完成审计元数据注入、服务端会话签发、HttpOnly Cookie 写入与 JSON/重定向响应。
*/
func (h *authHandler) IssueSession(w http.ResponseWriter, r *http.Request, id identity.ExternalIdentity, opts IssueSessionOptions) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if h.sessions == nil {
		h.logger.ErrorContext(r.Context(), "issue session", "err", errors.New("session creator is nil"))
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	auditCtx := audit.WithMeta(r.Context(), audit.SystemMeta(httpx.RequestIDFrom(r.Context())))
	issued, err := h.sessions.CreateSessionForIdentity(auditCtx, id, h.sessionTTL)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "create session", err)
		return
	}

	setSessionCookie(w, issued, opts.SecureCookie)
	if opts.ReturnTo != "" {
		http.Redirect(w, r, opts.ReturnTo, http.StatusFound)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, loginResponse{
		User: loginUserResponse{
			ID:    issued.User.ID,
			Email: issued.User.Email,
			Name:  issued.User.Name,
		},
		ExpiresAt: issued.Session.ExpiresAt.UTC(),
	})
}

func setSessionCookie(w http.ResponseWriter, issued identity.IssuedSession, secure bool) {
	// Max-Age 用数据库时钟算出的剩余寿命。RFC 6265 在同时有 Max-Age 和 Expires 时只认 Max-Age，
	// 如果这里用应用进程的 TTL，时钟慢的副本会让浏览器把已经过期的会话继续带上。
	maxAge := max(int(issued.CookieMaxAge/time.Second), 1)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    issued.Token,
		Path:     sessionCookiePath,
		MaxAge:   maxAge,
		Expires:  issued.Session.ExpiresAt.UTC(),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

/*
handleLogout 吊销当前服务端会话并清除 HttpOnly Cookie。
先过 CrossOriginProtection，防止第三方页面借自动携带的 Cookie 强行登出用户。
多份 sl_session 时返回 401，不吊销也不清 Cookie：无法判断哪一份是本机自己发的，
清掉 host-only 那份反而会留下父域种下的另一份。
无 Cookie 或会话早已失效时同样幂等返回 204 并清掉 Cookie；若吊销写库失败则保留 Cookie 并返回 5xx。
*/
func (h *authHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	if h.sessions == nil {
		h.logger.ErrorContext(r.Context(), "logout", "err", errors.New("session manager is nil"))
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	if !checkCrossOrigin(h.logger, h.csrf, w, r) {
		return
	}

	cookies := sessionCookies(r)
	if len(cookies) > 1 {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if len(cookies) == 1 && identity.SessionToken.Valid(cookies[0].Value) {
		if err := h.sessions.RevokeSession(r.Context(), cookies[0].Value); err != nil {
			httpx.HandleStoreError(h.logger, w, r, "revoke session", err)
			return
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     sessionCookiePath,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   h.logoutSecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

/*
handleMe 返回当前登录用户的档案、会话过期时间与所属组织列表。
该端点只认唯一的 sl_session Cookie，忽略 Authorization 和 X-Org-ID（即使尚未加入任何组织也能调用）。
同名 Cookie 多于一份时返回 401，不查询。
*/
func (h *authHandler) handleMe(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	if h.sessions == nil {
		h.logger.ErrorContext(r.Context(), "me", "err", errors.New("session manager is nil"))
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	cookies := sessionCookies(r)
	if len(cookies) != 1 || !identity.SessionToken.Valid(cookies[0].Value) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	info, err := h.sessions.GetSessionInfo(r.Context(), cookies[0].Value)
	if errors.Is(err, identity.ErrUnauthenticated) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "get session info", err)
		return
	}

	orgs := make([]meOrgResponse, 0, len(info.Orgs))
	for _, org := range info.Orgs {
		orgs = append(orgs, meOrgResponse{
			ID:        org.ID,
			Name:      org.Name,
			Role:      string(org.Role),
			LinkUsage: org.LinkUsage,
			LinkQuota: org.LinkQuota,
		})
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, meResponse{
		User: loginUserResponse{
			ID:    info.User.ID,
			Email: info.User.Email,
			Name:  info.User.Name,
		},
		ExpiresAt:     info.ExpiresAt.UTC(),
		Organizations: orgs,
	})
}
