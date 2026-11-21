/*
Package fakeoidc 提供仅用于测试的内存版标准 OIDC 提供方（阶段 11、ADR 0018）。

该包放在独立子包内，只允许被 *_test.go 引用，不会编译进任何生产二进制。
提供发现文档、/authorize、/token 和 /jwks 四个精确路径，支持通过 Tamper 注入
state 篡改、授权码过期、签名错误、iss/aud/nonce 不符和 ID Token 过期等故障。
*/
package fakeoidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	keyID     = "fake-oidc-key-1"
	codeTTL   = 5 * time.Minute
	tokenLife = time.Hour
)

/*
sharedKeys 在单个测试进程内只生成一次 RSA 密钥对，避免每个子测试重复生成 2048 位密钥。
primary 的公钥发布在 /jwks；wrong 不出现在 /jwks，专门用来模拟签名校验失败。
*/
var sharedKeys = sync.OnceValues(func() (*rsaKeyPair, error) {
	primary, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate primary rsa key: %w", err)
	}
	wrong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate wrong rsa key: %w", err)
	}
	return &rsaKeyPair{primary: primary, wrong: wrong}, nil
})

type rsaKeyPair struct {
	primary *rsa.PrivateKey
	wrong   *rsa.PrivateKey
}

// User 表示假 OIDC 服务写进 ID Token 的用户声明。
type User struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

/*
Tamper 控制假 OIDC 服务注入的异常行为，
用于离线覆盖 state 不符、code 过期、签名错误、aud/iss/nonce 不符和令牌过期。
*/
type Tamper struct {
	// State 非空时，/authorize 跳回回调地址的 state 参数会被替换成该值。
	State string
	// CodeExpired 为 true 时，/authorize 发出的授权码立即过期，/token 返回 invalid_grant。
	CodeExpired bool
	// WrongSignature 为 true 时，/token 用不在 /jwks 里的私钥签发 ID Token（沿用同一 kid 触发验签失败）。
	WrongSignature bool
	// Issuer 非空时覆盖 ID Token 载荷里的 iss 声明。
	Issuer string
	// Audience 非空时覆盖 ID Token 载荷里的 aud 声明。
	Audience string
	// Nonce 非空时覆盖 ID Token 载荷里的 nonce 声明。
	Nonce string
	// ExpiredIDToken 为 true 时，ID Token 的 exp 设为过去时刻。
	ExpiredIDToken bool
	// TokenError 非空时，客户端凭据校验通过后 /token 直接返回该 OAuth error，HTTP 状态 400。
	TokenError string
	// JWKSFailure 为 true 时，/jwks 返回 500，用来模拟签名密钥拉不下来。
	JWKSFailure bool
}

// AuthorizeRequest 记录最近一次打到 /authorize 的查询参数，用于断言登录跳转参数。
type AuthorizeRequest struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
}

type issuedAuthCode struct {
	clientID      string
	redirectURI   string
	nonce         string
	codeChallenge string
	user          User
	expiresAt     time.Time
}

/*
Server 封装 httptest.Server 和内存状态。
每个测试应各自调用 Start 创建独立实例，不要在并发子测试间共享修改 SetUser 或 SetTamper。
*/
type Server struct {
	clientID     string
	clientSecret string
	keys         *rsaKeyPair
	httpServer   *httptest.Server
	issuer       string

	mu            sync.Mutex
	user          User
	tamper        Tamper
	codes         map[string]issuedAuthCode
	lastAuthorize AuthorizeRequest
	hasAuthorize  bool
}

/*
Start 启动一个监听本机回环地址的假 OIDC 服务，并在测试结束时自动关闭。
默认登录用户为已验证邮箱的 alice@example.com。
*/
func Start(t testing.TB, clientID, clientSecret string) *Server {
	t.Helper()
	keys, err := sharedKeys()
	if err != nil {
		t.Fatalf("fakeoidc.Start: %v", err)
	}
	s := &Server{
		clientID:     clientID,
		clientSecret: clientSecret,
		keys:         keys,
		user: User{
			Subject:       "08a8684b-db88-4b73-90a9-3cd1661f5466",
			Email:         "alice@example.com",
			EmailVerified: true,
			Name:          "Alice",
		},
		codes: make(map[string]issuedAuthCode),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /jwks", s.handleJWKS)
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /token", s.handleToken)

	ts := httptest.NewUnstartedServer(mux)
	ts.Start()
	t.Cleanup(ts.Close)
	s.httpServer = ts
	s.issuer = ts.URL
	return s
}

// URL 返回假 OIDC 服务的根地址，同时也是发现文档和默认 ID Token 里的 issuer。
func (s *Server) URL() string {
	return s.issuer
}

// Client 返回可访问该测试服务的 HTTP 客户端。
func (s *Server) Client() *http.Client {
	return s.httpServer.Client()
}

// SetUser 切换后续 /authorize 绑定的登录用户。
func (s *Server) SetUser(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user = u
}

// SetTamper 设置或重置故障注入选项。传零值 Tamper{} 恢复正常行为。
func (s *Server) SetTamper(tamper Tamper) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tamper = tamper
}

// LastAuthorize 返回最近一次收到的合法 /authorize 请求参数。
func (s *Server) LastAuthorize() (AuthorizeRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthorize, s.hasAuthorize
}

func (s *Server) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/authorize",
		"token_endpoint":                        s.issuer + "/token",
		"jwks_uri":                              s.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"claims_supported":                      []string{"iss", "sub", "aud", "exp", "iat", "nonce", "email", "email_verified", "name"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	fail := s.tamper.JWKSFailure
	s.mu.Unlock()
	if fail {
		http.Error(w, "jwks unavailable", http.StatusInternalServerError)
		return
	}
	pub := &s.keys.primary.PublicKey
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"kid": keyID,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := AuthorizeRequest{
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
		Nonce:               q.Get("nonce"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
	}

	if q.Get("response_type") != "code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "response_type must be code")
		return
	}
	if req.ClientID != s.clientID {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "unknown client_id")
		return
	}
	redirectURL, err := url.Parse(req.RedirectURI)
	if err != nil || (redirectURL.Scheme != "http" && redirectURL.Scheme != "https") || redirectURL.Host == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid redirect_uri")
		return
	}
	if !slices.Contains(strings.Fields(req.Scope), "openid") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "openid scope is required")
		return
	}
	if req.State == "" || req.Nonce == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "state and nonce are required")
		return
	}
	if req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "PKCE S256 code_challenge is required")
		return
	}

	code, err := randomHexToken(16)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "generate code failed")
		return
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.lastAuthorize = req
	s.hasAuthorize = true
	expiresAt := now.Add(codeTTL)
	if s.tamper.CodeExpired {
		expiresAt = now.Add(-time.Second)
	}
	s.codes[code] = issuedAuthCode{
		clientID:      req.ClientID,
		redirectURI:   req.RedirectURI,
		nonce:         req.Nonce,
		codeChallenge: req.CodeChallenge,
		user:          s.user,
		expiresAt:     expiresAt,
	}
	state := req.State
	if s.tamper.State != "" {
		state = s.tamper.State
	}
	s.mu.Unlock()

	cbQuery := redirectURL.Query()
	cbQuery.Set("code", code)
	cbQuery.Set("state", state)
	redirectURL.RawQuery = cbQuery.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	clientID, clientSecret, ok := extractClientCredentials(r)
	if !ok || clientID != s.clientID || clientSecret != s.clientSecret {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake-oidc"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client credentials")
		return
	}
	s.mu.Lock()
	tokenErr := s.tamper.TokenError
	s.mu.Unlock()
	if tokenErr != "" {
		writeOAuthError(w, http.StatusBadRequest, tokenErr, "injected token error")
		return
	}

	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code")
		return
	}

	code := r.PostForm.Get("code")
	s.mu.Lock()
	issued, exists := s.codes[code]
	// 授权码是一次性凭证，无论后续校验成功与否都立即消费掉，防止重放。
	delete(s.codes, code)
	tamper := s.tamper
	s.mu.Unlock()

	if !exists {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or already used code")
		return
	}
	now := time.Now().UTC()
	if !issued.expiresAt.After(now) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}
	if r.PostForm.Get("redirect_uri") != issued.redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	if verifier == "" || pkceS256Challenge(verifier) != issued.codeChallenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "pkce code_verifier mismatch")
		return
	}

	iss := s.issuer
	if tamper.Issuer != "" {
		iss = tamper.Issuer
	}
	aud := issued.clientID
	if tamper.Audience != "" {
		aud = tamper.Audience
	}
	nonce := issued.nonce
	if tamper.Nonce != "" {
		nonce = tamper.Nonce
	}
	exp := now.Add(tokenLife)
	if tamper.ExpiredIDToken {
		exp = now.Add(-time.Minute)
	}

	claims := map[string]any{
		"iss":            iss,
		"sub":            issued.user.Subject,
		"aud":            aud,
		"iat":            now.Unix(),
		"exp":            exp.Unix(),
		"nonce":          nonce,
		"email":          issued.user.Email,
		"email_verified": issued.user.EmailVerified,
		"name":           issued.user.Name,
	}
	signingKey := s.keys.primary
	if tamper.WrongSignature {
		signingKey = s.keys.wrong
	}
	idToken, err := signRS256JWT(signingKey, keyID, claims)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "sign id_token failed")
		return
	}
	accessToken, err := randomHexToken(16)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "generate access_token failed")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(tokenLife.Seconds()),
		"id_token":     idToken,
	})
}

/*
extractClientCredentials 同时支持 client_secret_basic 和 client_secret_post。
RFC 6749 §2.3.1 规定 Basic Auth 里的 client_id/client_secret 需按表单 URL 编码，
golang.org/x/oauth2 按该规则发送，这里需要做 QueryUnescape 才能兼容含特殊字符的密钥。
*/
func extractClientCredentials(r *http.Request) (clientID, clientSecret string, ok bool) {
	if rawID, rawSecret, hasBasic := r.BasicAuth(); hasBasic {
		id, err := url.QueryUnescape(rawID)
		if err != nil {
			return "", "", false
		}
		secret, err := url.QueryUnescape(rawSecret)
		if err != nil {
			return "", "", false
		}
		return id, secret, true
	}
	id := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	if id == "" || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func signRS256JWT(priv *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	headerJSON, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"typ": "JWT",
		"kid": kid,
	})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func randomHexToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
