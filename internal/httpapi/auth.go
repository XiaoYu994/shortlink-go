package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"shortlink/internal/tenant"
)

// APIKeyAuthenticator 按明文密钥查找租户。接口放在使用方。
type APIKeyAuthenticator interface {
	Authenticate(ctx context.Context, token string) (tenant.Tenant, error)
}

// principalContextKey 用空结构体做键，避免和 request id 或其他包的字符串键撞上。
type principalContextKey struct{}

func withPrincipal(ctx context.Context, tn tenant.Tenant) context.Context {
	return context.WithValue(ctx, principalContextKey{}, tn)
}

func principalFrom(ctx context.Context) (tenant.Tenant, bool) {
	tn, ok := ctx.Value(principalContextKey{}).(tenant.Tenant)
	return tn, ok
}

/*
authenticate 只包管理 API。/r/、/healthz、/metrics 不经过它。
密钥格式不对和未知密钥都是 401。数据库故障沿用存储错误的映射，不伪装成密钥无效。
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
		tn, err := s.keys.Authenticate(r.Context(), token)
		if errors.Is(err, tenant.ErrUnauthenticated) {
			s.writeUnauthorized(r, w)
			return
		}
		if err != nil {
			s.handleStoreError(w, r, "authenticate api key", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), tn)))
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
