package httpapi

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
	"strings"
	"time"

	"shortlink/internal/identity"
)

const (
	// SessionCookieName 是服务端会话 Cookie 名，作用范围限定在 /api/，不发往 /r/{code}。
	SessionCookieName = "sl_session"
	// OIDCFlowCookieName 是授权码登录流程的短期 Cookie 名，只发往回调路径；state 相符后删除。
	OIDCFlowCookieName = "sl_oidc_flow"

	oidcLoginPath     = apiV1Prefix + "/auth/login"
	oidcCallbackPath  = apiV1Prefix + "/auth/callback"
	sessionCookiePath = "/api/"

	oidcFlowCookieTTL  = 10 * time.Minute
	maxFlowCookieBytes = 1024
)

/*
OIDCClient 封装 OIDC 授权地址构造和授权码兑换/ID Token 校验。
接口放在使用方，生产由 identity.OIDCProvider 实现（ADR 0018）。
*/
type OIDCClient interface {
	AuthCodeURL(state, codeVerifier, nonce string) string
	ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (identity.OIDCIdentity, error)
	SecureCookies() bool
	// FlowCookieKey 是流程 Cookie 的 HMAC 密钥。同一套 OIDC 配置的副本必须返回同一把密钥。
	FlowCookieKey() []byte
}

/*
SessionCreator 为已验证的 OIDC 身份找到或创建用户，并签发一条新的服务端会话。
接口放在使用方，生产由 postgres.Store 实现（ADR 0018）。
*/
type SessionCreator interface {
	CreateSessionForOIDC(ctx context.Context, id identity.OIDCIdentity, ttl time.Duration) (identity.IssuedSession, error)
}

/*
authHandler 独立承接登录与会话相关路由（ADR 0019：先在包内拆结构体，不往 Server 堆字段）。
它只持有认证流程必需的依赖，不依赖短链存储、点击记录器或租户限流器。
*/
type authHandler struct {
	logger        *slog.Logger
	oidc          OIDCClient
	sessions      SessionCreator
	sessionTTL    time.Duration
	now           func() time.Time
	flowCookieKey []byte
}

func newAuthHandler(logger *slog.Logger, oidcClient OIDCClient, sessions SessionCreator, sessionTTL time.Duration) *authHandler {
	if sessionTTL <= 0 {
		sessionTTL = identity.DefaultSessionTTL
	}
	var flowKey []byte
	if oidcClient != nil {
		flowKey = append([]byte(nil), oidcClient.FlowCookieKey()...)
	}
	return &authHandler{
		logger:        logger,
		oidc:          oidcClient,
		sessions:      sessions,
		sessionTTL:    sessionTTL,
		now:           time.Now,
		flowCookieKey: flowKey,
	}
}

func (h *authHandler) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+oidcLoginPath, h.handleLogin)
	mux.HandleFunc("GET "+oidcCallbackPath, h.handleCallback)
}

type oidcFlowCookie struct {
	State        string `json:"s"`
	CodeVerifier string `json:"v"`
	Nonce        string `json:"n"`
	ExpiresAt    int64  `json:"e"`
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

/*
handleLogin 发起 OIDC 授权码登录：每次生成全新的 state、PKCE code_verifier 和 nonce，
存入 10 分钟的 HttpOnly 流程 Cookie，然后 302 跳转到身份提供方。
*/
func (h *authHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if isClientGone(h.logger, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	flow, err := identity.NewOIDCFlow()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "generate oidc flow", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	expiresAt := h.now().UTC().Add(oidcFlowCookieTTL)
	cookieVal, err := encodeFlowCookie(oidcFlowCookie{
		State:        flow.State,
		CodeVerifier: flow.CodeVerifier,
		Nonce:        flow.Nonce,
		ExpiresAt:    expiresAt.Unix(),
	}, h.flowCookieKey)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "encode oidc flow cookie", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name:     OIDCFlowCookieName,
		Value:    cookieVal,
		Path:     oidcCallbackPath,
		MaxAge:   int(oidcFlowCookieTTL / time.Second),
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   h.oidc.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.oidc.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce), http.StatusFound)
}

/*
handleCallback 处理 OIDC 回调：
1. 读取流程 Cookie，校验 HMAC 和过期时间；缺失或损坏时直接 400，不写 Set-Cookie；
2. 常量时间比对 state，不符时 400，同样不删除 Cookie，避免无关请求清掉正在进行的登录；
3. state 相符后立即删除一次性流程 Cookie，再检查上游 error 和授权码；
4. 用 code 和 PKCE code_verifier 换令牌，校验 ID Token 签名、iss、aud、exp 和 nonce；
5. 找到或创建用户，签发全新服务端会话并写入 HttpOnly Cookie，返回用户摘要 JSON。
*/
func (h *authHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if isClientGone(h.logger, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.sessions == nil {
		h.logger.ErrorContext(r.Context(), "oidc callback", "err", errors.New("session creator is nil"))
		writeErrorResponse(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	flow, ok := h.readFlowCookie(r)
	if !ok {
		writeErrorResponse(h.logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}

	q := r.URL.Query()
	state := q.Get("state")
	if subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		writeErrorResponse(h.logger, r.Context(), w, http.StatusBadRequest, "invalid login state")
		return
	}
	// state 已经对上，这次回调消费掉流程 Cookie。对不上的请求不能删，否则跨站访问会清掉正在进行的登录。
	h.clearFlowCookie(w)
	if oidcErr := q.Get("error"); oidcErr != "" {
		h.logger.WarnContext(r.Context(), "oidc provider returned error", "oidc_error", oidcErr)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
		return
	}

	code := q.Get("code")
	if code == "" {
		writeErrorResponse(h.logger, r.Context(), w, http.StatusBadRequest, "missing authorization code")
		return
	}

	id, err := h.oidc.ExchangeAndVerify(r.Context(), code, flow.CodeVerifier, flow.Nonce)
	if err != nil {
		h.handleOIDCError(w, r, err)
		return
	}

	issued, err := h.sessions.CreateSessionForOIDC(r.Context(), id, h.sessionTTL)
	if err != nil {
		handleStoreErr(h.logger, w, r, "create session", err)
		return
	}

	// Max-Age 用数据库时钟算出的剩余寿命。RFC 6265 在同时有 Max-Age 和 Expires 时只认 Max-Age，
	// 如果这里用应用进程的 TTL，时钟慢的副本会让浏览器把已经过期的会话继续带上。
	maxAge := int(issued.CookieMaxAge / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    issued.Token,
		Path:     sessionCookiePath,
		MaxAge:   maxAge,
		Expires:  issued.Session.ExpiresAt.UTC(),
		HttpOnly: true,
		Secure:   h.oidc.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})

	writeJSONResponse(h.logger, r.Context(), w, http.StatusOK, loginResponse{
		User: loginUserResponse{
			ID:    issued.User.ID,
			Email: issued.User.Email,
			Name:  issued.User.Name,
		},
		ExpiresAt: issued.Session.ExpiresAt.UTC(),
	})
}

func (h *authHandler) readFlowCookie(r *http.Request) (oidcFlowCookie, bool) {
	cookie, err := r.Cookie(OIDCFlowCookieName)
	if err != nil {
		return oidcFlowCookie{}, false
	}
	flow, ok := decodeFlowCookie(cookie.Value, h.flowCookieKey)
	if !ok {
		return oidcFlowCookie{}, false
	}
	if h.now().UTC().Unix() >= flow.ExpiresAt {
		return oidcFlowCookie{}, false
	}
	return flow, true
}

func (h *authHandler) clearFlowCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     OIDCFlowCookieName,
		Value:    "",
		Path:     oidcCallbackPath,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   h.oidc.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *authHandler) handleOIDCError(w http.ResponseWriter, r *http.Request, err error) {
	if isClientGone(h.logger, r) {
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		h.logger.WarnContext(r.Context(), "oidc callback timed out", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
	case errors.Is(err, identity.ErrOIDCAuthFailed):
		h.logger.WarnContext(r.Context(), "oidc authentication failed", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusUnauthorized, "authentication failed")
	case errors.Is(err, identity.ErrOIDCUnavailable):
		h.logger.ErrorContext(r.Context(), "oidc provider unavailable", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusBadGateway, "identity provider unavailable")
	default:
		h.logger.ErrorContext(r.Context(), "oidc callback error", "err", err)
		writeErrorResponse(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
	}
}

func encodeFlowCookie(flow oidcFlowCookie, key []byte) (string, error) {
	raw, err := json.Marshal(flow)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	// payload 和 MAC 都用 base64url，中间用 '.' 分开。没有 MAC 的旧格式一律拒绝。
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeFlowCookie(sealed string, key []byte) (oidcFlowCookie, bool) {
	if sealed == "" || len(sealed) > maxFlowCookieBytes {
		return oidcFlowCookie{}, false
	}
	payloadB64, macB64, ok := strings.Cut(sealed, ".")
	if !ok || payloadB64 == "" || macB64 == "" || strings.Contains(macB64, ".") {
		return oidcFlowCookie{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return oidcFlowCookie{}, false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return oidcFlowCookie{}, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return oidcFlowCookie{}, false
	}
	var flow oidcFlowCookie
	if err := json.Unmarshal(payload, &flow); err != nil {
		return oidcFlowCookie{}, false
	}
	if flow.State == "" || flow.CodeVerifier == "" || flow.Nonce == "" || flow.ExpiresAt <= 0 {
		return oidcFlowCookie{}, false
	}
	return flow, true
}
