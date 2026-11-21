package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

// APIKeyAuthenticator 按明文密钥查找租户与权限范围，返回对应的 Principal。接口放在使用方。
type APIKeyAuthenticator interface {
	Authenticate(ctx context.Context, token string) (identity.Principal, error)
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
	p, ok := principalFrom(r.Context())
	if !ok {
		s.logger.ErrorContext(r.Context(), "missing principal in request context")
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return identity.Principal{}, false
	}
	return p, true
}

/*
authenticate 只包管理 API。/r/、/healthz、/readyz、/metrics 不经过它。
密钥缺失、格式不对、未知、过期和吊销都是 401，响应不区分是哪一种。
数据库故障沿用存储错误的映射，不伪装成密钥无效。
查到的主体必须通过 ValidAPIKeyPrincipal：不是 API Key，或 scope 超出密钥允许的范围，
都是鉴权实现的错误，返回 500，不把用户角色或 members:write 放进后续授权。
*/
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clientGone(r) {
			return
		}
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
		if errors.Is(err, tenant.ErrUnauthenticated) {
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
	})
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

func (s *Server) writeUnauthorized(r *http.Request, w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	s.writeError(r.Context(), w, http.StatusUnauthorized, "unauthorized")
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
	if !tenant.ValidToken(token) {
		return "", false
	}
	return token, true
}
