package console

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

const (
	// SessionCookieName 是服务端会话 Cookie 名，作用范围限定在 /api/，不发往 /r/{code}。
	SessionCookieName = "sl_session"
	// OIDCFlowCookieName 是 OIDC 授权码登录流程的短期 Cookie 名，只发往 OIDC 回调路径；state 相符后删除。
	OIDCFlowCookieName = "sl_oidc_flow"
	// GitHubFlowCookieName 是 GitHub OAuth 2.0 授权码登录流程的短期 Cookie 名，只发往 GitHub 回调路径。
	GitHubFlowCookieName = "sl_github_flow"

	oidcLoginPath      = apiV1Prefix + "/auth/login"
	oidcCallbackPath   = apiV1Prefix + "/auth/callback"
	githubLoginPath    = apiV1Prefix + "/auth/github/login"
	githubCallbackPath = apiV1Prefix + "/auth/github/callback"
	authLogoutPath     = apiV1Prefix + "/auth/logout"
	authProvidersPath  = apiV1Prefix + "/auth/providers"
	mePath             = apiV1Prefix + "/me"
	sessionCookiePath  = "/api/"

	flowCookieTTL      = 10 * time.Minute
	maxFlowCookieBytes = 1536
	maxReturnToBytes   = 256
)

/*
OIDCClient 封装 OIDC 授权地址构造和授权码兑换/ID Token 校验。
接口放在使用方，生产由 identity.OIDCProvider 实现（ADR 0018）。
*/
type OIDCClient interface {
	AuthCodeURL(state, codeVerifier, nonce string) string
	ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (identity.ExternalIdentity, error)
	SecureCookies() bool
	// FlowCookieKey 是流程 Cookie 的 HMAC 密钥。同一套 OIDC 配置的副本必须返回同一把密钥。
	FlowCookieKey() []byte
}

/*
GitHubClient 封装纯 OAuth 2.0 的 GitHub 授权地址构造与在线用户信息获取。
相比 OIDCClient 刻意不传 nonce（GitHub 不签发 ID Token）。生产由 identity.GitHubProvider 实现。
*/
type GitHubClient interface {
	AuthCodeURL(state, codeVerifier string) string
	ExchangeAndFetchUser(ctx context.Context, code, codeVerifier string) (identity.ExternalIdentity, error)
	SecureCookies() bool
	FlowCookieKey() []byte
}

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
authHandler 独立承接登录与会话相关路由（ADR 0019：不在 Server 上堆认证流程字段）。
它只持有认证流程必需的依赖，不依赖短链存储或租户限流器。
*/
type authHandler struct {
	logger        *slog.Logger
	oidc          OIDCClient
	github        GitHubClient
	sessions      SessionManager
	csrf          *http.CrossOriginProtection
	sessionTTL    time.Duration
	now           func() time.Time
	oidcFlowKey   []byte
	githubFlowKey []byte
}

func newAuthHandler(logger *slog.Logger, oidcClient OIDCClient, githubClient GitHubClient, sessions SessionManager, csrf *http.CrossOriginProtection, sessionTTL time.Duration) *authHandler {
	if sessionTTL <= 0 {
		sessionTTL = identity.DefaultSessionTTL
	}
	var oidcKey []byte
	if oidcClient != nil {
		oidcKey = append([]byte(nil), oidcClient.FlowCookieKey()...)
	}
	var githubKey []byte
	if githubClient != nil {
		githubKey = append([]byte(nil), githubClient.FlowCookieKey()...)
	}
	return &authHandler{
		logger:        logger,
		oidc:          oidcClient,
		github:        githubClient,
		sessions:      sessions,
		csrf:          csrf,
		sessionTTL:    sessionTTL,
		now:           time.Now,
		oidcFlowKey:   oidcKey,
		githubFlowKey: githubKey,
	}
}

func (h *authHandler) registerRoutes(mux *http.ServeMux) {
	if h.oidc != nil {
		mux.HandleFunc("GET "+oidcLoginPath, h.handleLogin)
		mux.HandleFunc("GET "+oidcCallbackPath, h.handleCallback)
	}
	if h.github != nil {
		mux.HandleFunc("GET "+githubLoginPath, h.handleGitHubLogin)
		mux.HandleFunc("GET "+githubCallbackPath, h.handleGitHubCallback)
	}
	if h.sessions != nil {
		mux.HandleFunc("POST "+authLogoutPath, h.handleLogout)
		mux.HandleFunc("GET "+mePath, h.handleMe)
	}
}

type flowCookieSpec struct {
	name         string
	path         string
	key          []byte
	secure       bool
	requireNonce bool
}

func (h *authHandler) oidcFlowSpec() flowCookieSpec {
	return flowCookieSpec{
		name:         OIDCFlowCookieName,
		path:         oidcCallbackPath,
		key:          h.oidcFlowKey,
		secure:       h.oidc != nil && h.oidc.SecureCookies(),
		requireNonce: true,
	}
}

func (h *authHandler) githubFlowSpec() flowCookieSpec {
	return flowCookieSpec{
		name:   GitHubFlowCookieName,
		path:   githubCallbackPath,
		key:    h.githubFlowKey,
		secure: h.github != nil && h.github.SecureCookies(),
	}
}

func (h *authHandler) logoutSecureCookies() bool {
	return (h.oidc != nil && h.oidc.SecureCookies()) || (h.github != nil && h.github.SecureCookies())
}

type authFlowCookie struct {
	State        string `json:"s"`
	CodeVerifier string `json:"v"`
	Nonce        string `json:"n,omitempty"`
	ReturnTo     string `json:"r,omitempty"`
	ExpiresAt    int64  `json:"e"`
}

type authProvidersResponse struct {
	OIDC   bool `json:"oidc"`
	GitHub bool `json:"github"`
}

func (h *authHandler) handleProviders(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, authProvidersResponse{
		OIDC:   h.oidc != nil,
		GitHub: h.github != nil,
	})
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
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// handleLogin 发起标准 OIDC 授权码登录（带 state、PKCE code_verifier 和 nonce）。
func (h *authHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	h.handleLoginFlow(w, r, h.oidcFlowSpec(), func() (authFlowCookie, string, error) {
		flow, err := identity.NewOIDCFlow()
		if err != nil {
			return authFlowCookie{}, "", err
		}
		return authFlowCookie{
			State:        flow.State,
			CodeVerifier: flow.CodeVerifier,
			Nonce:        flow.Nonce,
		}, h.oidc.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce), nil
	})
}

// handleGitHubLogin 发起 GitHub 纯 OAuth 2.0 授权码登录（带 state 和 PKCE code_verifier，无 nonce）。
func (h *authHandler) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	h.handleLoginFlow(w, r, h.githubFlowSpec(), func() (authFlowCookie, string, error) {
		flow, err := identity.NewOAuthFlow()
		if err != nil {
			return authFlowCookie{}, "", err
		}
		return authFlowCookie{
			State:        flow.State,
			CodeVerifier: flow.CodeVerifier,
		}, h.github.AuthCodeURL(flow.State, flow.CodeVerifier), nil
	})
}

func (h *authHandler) handleLoginFlow(
	w http.ResponseWriter,
	r *http.Request,
	spec flowCookieSpec,
	start func() (authFlowCookie, string, error),
) {
	if httpx.IsClientGone(h.logger, r) || !requireGetOnly(h.logger, w, r) {
		return
	}

	returnTo, ok := validateReturnTo(r.URL.Query().Get("return_to"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid return_to")
		return
	}

	flow, authURL, err := start()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "start external login", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	expiresAt := h.now().UTC().Add(flowCookieTTL)
	flow.ReturnTo = returnTo
	flow.ExpiresAt = expiresAt.Unix()
	cookieVal, err := encodeFlowCookie(flow, spec.key)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "encode flow cookie", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
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

func (h *authHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	h.handleCallbackFlow(w, r, h.oidcFlowSpec(), func(ctx context.Context, code string, flow authFlowCookie) (identity.ExternalIdentity, error) {
		return h.oidc.ExchangeAndVerify(ctx, code, flow.CodeVerifier, flow.Nonce)
	})
}

func (h *authHandler) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	h.handleCallbackFlow(w, r, h.githubFlowSpec(), func(ctx context.Context, code string, flow authFlowCookie) (identity.ExternalIdentity, error) {
		return h.github.ExchangeAndFetchUser(ctx, code, flow.CodeVerifier)
	})
}

/*
handleCallbackFlow 编排授权码回调的公共流程（适用于 OIDC 与 GitHub OAuth 2.0）：
1. 读取对应提供方的流程 Cookie，校验 HMAC、过期时间和必需字段；缺失或损坏时直接 400，不写 Set-Cookie；
2. 常量时间比对 state，不符时 400，同样不删除 Cookie，避免无关请求清掉正在进行的登录；
3. state 相符后立即删除一次性流程 Cookie，再检查上游 error 和授权码；
4. 调用具体提供方兑换令牌并提取标准化 ExternalIdentity；
5. 找到或创建用户，签发全新服务端会话并写入 HttpOnly Cookie，返回用户摘要 JSON。
*/
func (h *authHandler) handleCallbackFlow(
	w http.ResponseWriter,
	r *http.Request,
	spec flowCookieSpec,
	exchange func(ctx context.Context, code string, flow authFlowCookie) (identity.ExternalIdentity, error),
) {
	if httpx.IsClientGone(h.logger, r) || !requireGetOnly(h.logger, w, r) {
		return
	}
	if h.sessions == nil {
		h.logger.ErrorContext(r.Context(), "auth callback", "err", errors.New("session creator is nil"))
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	flow, ok := h.readFlowCookie(r, spec)
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}

	q := r.URL.Query()
	state := q.Get("state")
	if subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}
	// state 已经对上，这次回调消费掉流程 Cookie。对不上的请求不能删，否则跨站访问会清掉正在进行的登录。
	clearFlowCookie(w, spec)
	if authErr := q.Get("error"); authErr != "" {
		h.logger.WarnContext(r.Context(), "identity provider returned error", "provider_error", authErr)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
		return
	}

	code := q.Get("code")
	if code == "" {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "missing authorization code")
		return
	}

	id, err := exchange(r.Context(), code, flow)
	if err != nil {
		h.handleProviderError(w, r, err)
		return
	}

	issued, err := h.sessions.CreateSessionForIdentity(r.Context(), id, h.sessionTTL)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "create session", err)
		return
	}

	setSessionCookie(w, issued, spec.secure)
	if flow.ReturnTo != "" {
		http.Redirect(w, r, flow.ReturnTo, http.StatusFound)
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
			ID:   org.ID,
			Name: org.Name,
			Role: string(org.Role),
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

func (h *authHandler) readFlowCookie(r *http.Request, spec flowCookieSpec) (authFlowCookie, bool) {
	cookie, err := r.Cookie(spec.name)
	if err != nil {
		return authFlowCookie{}, false
	}
	flow, ok := decodeFlowCookie(cookie.Value, spec)
	if !ok {
		return authFlowCookie{}, false
	}
	if h.now().UTC().Unix() >= flow.ExpiresAt {
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

func (h *authHandler) handleProviderError(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		h.logger.WarnContext(r.Context(), "auth callback timed out", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
	case errors.Is(err, identity.ErrAuthFailed):
		h.logger.WarnContext(r.Context(), "external authentication failed", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
	case errors.Is(err, identity.ErrProviderUnavailable):
		h.logger.ErrorContext(r.Context(), "identity provider unavailable", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadGateway, "identity provider unavailable")
	default:
		h.logger.ErrorContext(r.Context(), "auth callback error", "err", err)
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
	}
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
