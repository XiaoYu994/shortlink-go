package console

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

const (
	// OIDCFlowCookieName 是 id="oidc" 时的短期流程 Cookie 名，只发往 OIDC 回调路径。
	OIDCFlowCookieName = "sl_oidc_flow"
	// GitHubFlowCookieName 是 id="github" 时的短期流程 Cookie 名，只发往 GitHub 回调路径。
	GitHubFlowCookieName = "sl_github_flow"

	flowCookieTTL      = 10 * time.Minute
	maxFlowCookieBytes = 1536
	maxReturnToBytes   = 256
)

/*
OAuthClient 统一封装跳转型 OAuth 2.0 / OIDC 身份提供方的授权地址构造与授权码兑换（ADR 0029）。
接口放在使用方，生产由 oauth.OIDCProvider（RequiresNonce=true）与 oauth.GitHubProvider（RequiresNonce=false）实现。
*/
type OAuthClient interface {
	AuthCodeURL(state, codeVerifier, nonce string) string
	Exchange(ctx context.Context, code, codeVerifier, expectedNonce string) (identity.ExternalIdentity, error)
	RequiresNonce() bool
	SecureCookies() bool
	// FlowCookieKey 是流程 Cookie 的 HMAC 密钥。同一套提供方配置的副本必须返回同一把密钥。
	FlowCookieKey() []byte
}

/*
OAuthAuthMethod 是所有跳转型 OAuth/OIDC 登录方式的公共模板适配器（模板方法模式）：
统一编排 state/PKCE/nonce 生成、HMAC 签名流程 Cookie、防开放重定向 return_to 校验、
防 HEAD 消费、回调常量时间比对与错误映射，并统一挂载到 /api/v1/auth/{id}/login 与 /callback。
*/
type OAuthAuthMethod struct {
	id           string
	loginPath    string
	callbackPath string
	cookieName   string
	client       OAuthClient
	flowKey      []byte
	now          func() time.Time
}

// NewOAuthAuthMethod 为给定 OAuthClient 构造跳转型登录插件，路由统一落在 /api/v1/auth/{id}/login·callback。
func NewOAuthAuthMethod(id string, client OAuthClient) (*OAuthAuthMethod, error) {
	if !validAuthMethodID(id) {
		return nil, fmt.Errorf("oauth auth method: invalid id %q", id)
	}
	if client == nil {
		return nil, fmt.Errorf("oauth auth method %q: client is nil", id)
	}
	rawKey := client.FlowCookieKey()
	if len(rawKey) == 0 {
		return nil, fmt.Errorf("oauth auth method %q: flow cookie key is empty", id)
	}
	// 将插件 ID 绑定进流程 Cookie HMAC 密钥派生，防止多 OAuth 实例误用相同密钥时跨回调重放。
	h := sha256.New()
	h.Write([]byte("shortlink-oauth-method:"))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(rawKey)
	key := h.Sum(nil)
	base := apiV1Prefix + "/auth/" + id
	return &OAuthAuthMethod{
		id:           id,
		loginPath:    base + "/login",
		callbackPath: base + "/callback",
		cookieName:   "sl_" + id + "_flow",
		client:       client,
		flowKey:      key,
		now:          time.Now,
	}, nil
}

// Descriptor 返回下发给前端的能力描述，包含跳转登录入口路径。
func (m *OAuthAuthMethod) Descriptor() AuthMethodDescriptor {
	return AuthMethodDescriptor{
		ID:       m.id,
		Kind:     AuthFlowRedirect,
		LoginURL: m.loginPath,
	}
}

// SecureCookies 返回该提供方回调 URL 决定的 Cookie Secure 属性。
func (m *OAuthAuthMethod) SecureCookies() bool {
	return m.client.SecureCookies()
}

// RegisterRoutes 在 /api/v1/auth/{id}/login 与 /api/v1/auth/{id}/callback 挂载登录与回调处理器。
func (m *OAuthAuthMethod) RegisterRoutes(tk AuthToolkit) {
	wrap := tk.IPRateLimit
	if wrap == nil {
		wrap = func(next http.Handler) http.Handler { return next }
	}
	tk.Mux.Handle("GET "+m.loginPath, wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleLogin(w, r, tk.Logger)
	})))
	tk.Mux.Handle("GET "+m.callbackPath, wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleCallback(w, r, tk.Logger, tk.Issuer)
	})))
}

type flowCookieSpec struct {
	name         string
	path         string
	key          []byte
	secure       bool
	requireNonce bool
}

func (m *OAuthAuthMethod) flowSpec() flowCookieSpec {
	return flowCookieSpec{
		name:         m.cookieName,
		path:         m.callbackPath,
		key:          m.flowKey,
		secure:       m.client.SecureCookies(),
		requireNonce: m.client.RequiresNonce(),
	}
}

type authFlowCookie struct {
	State        string `json:"s"`
	CodeVerifier string `json:"v"`
	Nonce        string `json:"n,omitempty"`
	ReturnTo     string `json:"r,omitempty"`
	ExpiresAt    int64  `json:"e"`
}

func (m *OAuthAuthMethod) handleLogin(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	if httpx.IsClientGone(logger, r) || !requireGetOnly(logger, w, r) {
		return
	}

	returnTo, ok := validateReturnTo(r.URL.Query().Get("return_to"))
	if !ok {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "invalid return_to")
		return
	}

	var (
		flow    authFlowCookie
		authURL string
	)
	if m.client.RequiresNonce() {
		oidcFlow, err := identity.NewOIDCFlow()
		if err != nil {
			logger.ErrorContext(r.Context(), "start external login", "provider", m.id, "err", err)
			httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
			return
		}
		flow = authFlowCookie{
			State:        oidcFlow.State,
			CodeVerifier: oidcFlow.CodeVerifier,
			Nonce:        oidcFlow.Nonce,
		}
		authURL = m.client.AuthCodeURL(oidcFlow.State, oidcFlow.CodeVerifier, oidcFlow.Nonce)
	} else {
		oauthFlow, err := identity.NewOAuthFlow()
		if err != nil {
			logger.ErrorContext(r.Context(), "start external login", "provider", m.id, "err", err)
			httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
			return
		}
		flow = authFlowCookie{
			State:        oauthFlow.State,
			CodeVerifier: oauthFlow.CodeVerifier,
		}
		authURL = m.client.AuthCodeURL(oauthFlow.State, oauthFlow.CodeVerifier, "")
	}

	spec := m.flowSpec()
	expiresAt := m.now().UTC().Add(flowCookieTTL)
	flow.ReturnTo = returnTo
	flow.ExpiresAt = expiresAt.Unix()
	cookieVal, err := encodeFlowCookie(flow, spec.key)
	if err != nil {
		logger.ErrorContext(r.Context(), "encode flow cookie", "provider", m.id, "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name:     spec.name,
		Value:    cookieVal,
		Path:     spec.path,
		MaxAge:   int(flowCookieTTL / time.Second),
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   spec.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (m *OAuthAuthMethod) handleCallback(w http.ResponseWriter, r *http.Request, logger *slog.Logger, issuer SessionIssuer) {
	if httpx.IsClientGone(logger, r) || !requireGetOnly(logger, w, r) {
		return
	}
	if issuer == nil {
		logger.ErrorContext(r.Context(), "auth callback", "provider", m.id, "err", errors.New("session issuer is nil"))
		httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	spec := m.flowSpec()
	w.Header().Set("Cache-Control", "no-store")
	flow, ok := m.readFlowCookie(r, spec)
	if !ok {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}

	q := r.URL.Query()
	state := q.Get("state")
	if subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}
	// state 已经对上，这次回调消费掉流程 Cookie。对不上的请求不能删，否则跨站访问会清掉正在进行的登录。
	clearFlowCookie(w, spec)
	if authErr := q.Get("error"); authErr != "" {
		logger.WarnContext(r.Context(), "identity provider returned error", "provider", m.id, "provider_error", authErr)
		httpx.WriteError(logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
		return
	}

	code := q.Get("code")
	if code == "" {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "missing authorization code")
		return
	}

	id, err := m.client.Exchange(r.Context(), code, flow.CodeVerifier, flow.Nonce)
	if err != nil {
		handleProviderError(logger, w, r, err)
		return
	}

	issuer.IssueSession(w, r, id, IssueSessionOptions{
		SecureCookie: spec.secure,
		ReturnTo:     flow.ReturnTo,
	})
}

func (m *OAuthAuthMethod) readFlowCookie(r *http.Request, spec flowCookieSpec) (authFlowCookie, bool) {
	cookie, err := r.Cookie(spec.name)
	if err != nil {
		return authFlowCookie{}, false
	}
	flow, ok := decodeFlowCookie(cookie.Value, spec)
	if !ok {
		return authFlowCookie{}, false
	}
	if m.now().UTC().Unix() >= flow.ExpiresAt {
		return authFlowCookie{}, false
	}
	return flow, true
}

func clearFlowCookie(w http.ResponseWriter, spec flowCookieSpec) {
	http.SetCookie(w, &http.Cookie{
		Name:     spec.name,
		Value:    "",
		Path:     spec.path,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   spec.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func handleProviderError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	if httpx.IsClientGone(logger, r) {
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logger.WarnContext(r.Context(), "auth callback timed out", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
	case errors.Is(err, identity.ErrAuthFailed):
		logger.WarnContext(r.Context(), "external authentication failed", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
	case errors.Is(err, identity.ErrProviderUnavailable):
		logger.ErrorContext(r.Context(), "identity provider unavailable", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusBadGateway, "identity provider unavailable")
	default:
		logger.ErrorContext(r.Context(), "auth callback error", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
	}
}

/*
requireGetOnly 拦截打到有状态 GET 端点（login、callback）上的 HEAD 请求。
ServeMux 的 "GET /..." 模式会同时匹配 HEAD；只读查询端点允许 HEAD，
但登录跳转与回调会种下或消费一次性流程 Cookie，必须拒绝 HEAD 探测。
*/
func requireGetOnly(logger *slog.Logger, w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpx.WriteError(logger, r.Context(), w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

func encodeFlowCookie(flow authFlowCookie, key []byte) (string, error) {
	raw, err := json.Marshal(flow)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	// payload 和 MAC 都用 base64url，中间用 '.' 分开。没有 MAC 的旧格式一律拒绝。
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeFlowCookie(sealed string, spec flowCookieSpec) (authFlowCookie, bool) {
	if sealed == "" || len(sealed) > maxFlowCookieBytes {
		return authFlowCookie{}, false
	}
	payloadB64, macB64, ok := strings.Cut(sealed, ".")
	if !ok || payloadB64 == "" || macB64 == "" || strings.Contains(macB64, ".") {
		return authFlowCookie{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return authFlowCookie{}, false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return authFlowCookie{}, false
	}
	mac := hmac.New(sha256.New, spec.key)
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return authFlowCookie{}, false
	}
	var flow authFlowCookie
	if err := json.Unmarshal(payload, &flow); err != nil {
		return authFlowCookie{}, false
	}
	if flow.State == "" || flow.CodeVerifier == "" || flow.ExpiresAt <= 0 {
		return authFlowCookie{}, false
	}
	if spec.requireNonce && flow.Nonce == "" {
		return authFlowCookie{}, false
	}
	if !spec.requireNonce && flow.Nonce != "" {
		return authFlowCookie{}, false
	}
	if _, ok := validateReturnTo(flow.ReturnTo); !ok {
		return authFlowCookie{}, false
	}
	return flow, true
}

/*
validateReturnTo 只允许空串或站内前端相对路径，防止开放重定向攻击（ADR 0021）。
拒绝协议相对路径（//evil.com）、反斜杠、控制字符，以及指向 /api/ 或 /r/ 的路径。
*/
func validateReturnTo(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if len(raw) > maxReturnToBytes || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "", false
	}
	for i := range len(raw) {
		c := raw[i]
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "", false
		}
	}
	pathOnly := raw
	if idx := strings.IndexAny(pathOnly, "?#"); idx >= 0 {
		pathOnly = pathOnly[:idx]
	}
	cleanPath := path.Clean(pathOnly)
	if cleanPath == "/api" || strings.HasPrefix(cleanPath, "/api/") || cleanPath == "/r" || strings.HasPrefix(cleanPath, "/r/") {
		return "", false
	}
	return raw, true
}
