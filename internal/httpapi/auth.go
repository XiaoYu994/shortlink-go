package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"shortlink/internal/identity"
)

const orgIDHeader = "X-Org-ID"

// APIKeyAuthenticator 按明文密钥查找租户与权限范围，返回对应的 Principal。接口放在使用方。
type APIKeyAuthenticator interface {
	Authenticate(ctx context.Context, token string) (identity.Principal, error)
}

// SessionAuthenticator 按会话 Cookie 令牌和 X-Org-ID 查找用户与组织角色。接口放在使用方。
type SessionAuthenticator interface {
	AuthenticateSessionForOrg(ctx context.Context, token string, orgID int64) (identity.Principal, error)
}

// Authorizer 判断当前执行者是否拥有指定权限。接口放在使用方（ADR 0018）。
type Authorizer interface {
	Can(p identity.Principal, perm identity.Perm) bool
}

// principalContextKey 用空结构体做键，避免和 request id 或其他包的字符串键撞上。
type principalContextKey struct{}

func withPrincipal(ctx context.Context, p identity.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func principalFrom(ctx context.Context) (identity.Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(identity.Principal)
	return p, ok
}

/*
principal 取出鉴权中间件写进 context 的执行主体。
管理 handler 在 authenticate 里面，理论上不会缺失；缺失说明路由组装错了，
返回 500，而不是把请求当成没有租户或放开访问。
*/
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	return principalOrInternalError(s.logger, w, r)
}

func principalOrInternalError(logger *slog.Logger, w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := principalFrom(r.Context())
	if !ok {
		logger.ErrorContext(r.Context(), "missing principal in request context")
		writeErrorResponse(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return identity.Principal{}, false
	}
	return p, true
}

/*
authenticate 只包管理 API。/r/、/healthz、/readyz、/metrics、登录与 /me 不经过它。
支持两种凭证，按 Authorization 头优先且绝不回退（ADR 0018）：
  - 带 Authorization 头时只按 API Key 鉴权，忽略 sl_session Cookie 和 X-Org-ID，不受 CSRF 限制；
  - 不带 Authorization 头时校验 sl_session Cookie：先过 CrossOriginProtection，
    再拒绝同名 Cookie 多于一份的请求，然后校验令牌格式、解析 X-Org-ID 并查库验证成员关系（非成员返回 404）。
    这条路径的响应一律 Cache-Control: no-store。带了会话 Cookie 却失败时返回 401，且不带 WWW-Authenticate。
*/
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clientGone(r) {
			return
		}
		if r.Header.Get("Authorization") != "" {
			s.authenticateAPIKey(w, r, next)
			return
		}
		s.authenticateSession(w, r, next)
	})
}

func (s *Server) authenticateAPIKey(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if s.keys == nil {
		s.logger.ErrorContext(r.Context(), "authenticate api key", "err", errors.New("api key authenticator is nil"))
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		s.writeUnauthorized(r, w)
		return
	}
	p, err := s.keys.Authenticate(r.Context(), token)
	if errors.Is(err, identity.ErrUnauthenticated) {
		s.writeUnauthorized(r, w)
		return
	}
	if err != nil {
		s.handleStoreError(w, r, "authenticate api key", err)
		return
	}
	if !identity.ValidAPIKeyPrincipal(p) {
		s.logger.ErrorContext(r.Context(), "authenticate api key", "err", errors.New("invalid api key principal"))
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}
	next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
}

func (s *Server) authenticateSession(w http.ResponseWriter, r *http.Request, next http.Handler) {
	/*
		会话请求没有 Authorization，共享缓存可以存储 GET。
		成功、400、401、403 都禁止缓存，否则登出或换用户后仍可能读到上一份链接列表。
	*/
	w.Header().Set("Cache-Control", "no-store")

	if s.keys == nil && s.sessions == nil {
		s.logger.ErrorContext(r.Context(), "authenticate request", "err", errors.New("no authenticator configured"))
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}
	// 没配会话存储时只剩 API Key。401 仍广告 Bearer，方便只使用密钥的客户端。
	if s.sessions == nil {
		s.writeUnauthorized(r, w)
		return
	}

	cookies := sessionCookies(r)
	if len(cookies) == 0 {
		s.writeUnauthorized(r, w)
		return
	}
	// 带了 Cookie 的写请求先拦截跨站，再决定用哪一份令牌（ADR 0018）。
	if !checkCrossOrigin(s.logger, s.csrf, w, r) {
		return
	}
	/*
		同名 Cookie 多于一份时不取第一份。
		父域上的另一份子域可以种下 Domain 版 sl_session，浏览器会把两份都带上，
		静默选中先出现的那份会把请求算进别人的组织。
	*/
	if len(cookies) != 1 || !identity.SessionToken.Valid(cookies[0].Value) {
		s.writeSessionUnauthorized(r, w)
		return
	}
	cookie := cookies[0]

	orgID, msg, ok := parseOrgIDHeader(r.Header)
	if !ok {
		s.writeError(r.Context(), w, http.StatusBadRequest, msg)
		return
	}

	p, err := s.sessions.AuthenticateSessionForOrg(r.Context(), cookie.Value, orgID)
	if errors.Is(err, identity.ErrUnauthenticated) {
		s.writeSessionUnauthorized(r, w)
		return
	}
	if errors.Is(err, identity.ErrOrgNotFound) {
		s.writeError(r.Context(), w, http.StatusNotFound, identity.ErrOrgNotFound.Error())
		return
	}
	if err != nil {
		s.handleStoreError(w, r, "authenticate session", err)
		return
	}
	if !identity.ValidUserPrincipal(p) {
		s.logger.ErrorContext(r.Context(), "authenticate session", "err", errors.New("invalid user principal"))
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}
	next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
}

func checkCrossOrigin(logger *slog.Logger, csrf *http.CrossOriginProtection, w http.ResponseWriter, r *http.Request) bool {
	if csrf == nil {
		return true
	}
	if err := csrf.Check(r); err != nil {
		logger.WarnContext(r.Context(), "cross-origin request rejected",
			"method", r.Method,
			"path", r.URL.Path,
			"err", err,
		)
		writeErrorResponse(logger, r.Context(), w, http.StatusForbidden, "cross-origin request rejected")
		return false
	}
	return true
}

func parseOrgIDHeader(h http.Header) (int64, string, bool) {
	vals := h.Values(orgIDHeader)
	if len(vals) == 0 || (len(vals) == 1 && vals[0] == "") {
		return 0, "X-Org-ID header is required", false
	}
	if len(vals) > 1 {
		return 0, "invalid X-Org-ID", false
	}
	id, ok := parsePositiveInt64(vals[0])
	if !ok {
		return 0, "invalid X-Org-ID", false
	}
	return id, "", true
}

func parsePositiveInt64(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

/*
requirePerm 在请求进入业务 handler 前校验当前主体是否具备 perm。
采用默认拒绝：未配置 Authorizer 返回 500；缺少执行主体同样返回 500；
权限不足返回 403，不透出权限名称。
*/
func (s *Server) requirePerm(perm identity.Perm, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clientGone(r) {
			return
		}
		if s.authz == nil {
			s.logger.ErrorContext(r.Context(), "authorize request", "err", errors.New("authorizer is nil"))
			s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
			return
		}
		p, ok := s.principal(w, r)
		if !ok {
			return
		}
		if !s.authz.Can(p, perm) {
			s.writeError(r.Context(), w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeUnauthorized 是 API Key 失败：广告 Bearer，让只使用密钥的客户端知道该带什么。
func (s *Server) writeUnauthorized(r *http.Request, w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	s.writeSessionUnauthorized(r, w)
}

/*
writeSessionUnauthorized 是会话失败。
不广告 Bearer，否则客户端可能丢掉 Cookie、改走存着的 API Key。
*/
func (s *Server) writeSessionUnauthorized(r *http.Request, w http.ResponseWriter) {
	s.writeError(r.Context(), w, http.StatusUnauthorized, "unauthorized")
}

/*
sessionCookies 返回请求里全部的 sl_session。
net/http 的 Request.Cookie 只给第一份，Cookie 投毒时不够用。
*/
func sessionCookies(r *http.Request) []*http.Cookie {
	var cookies []*http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == SessionCookieName {
			cookies = append(cookies, c)
		}
	}
	return cookies
}

/*
bearerToken 取出 Authorization 里的密钥。
方案名不区分大小写；密钥本身仍要是小写十六进制。
*/
func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := header[len(scheme):]
	if !identity.APIKeyToken.Valid(token) {
		return "", false
	}
	return token, true
}
