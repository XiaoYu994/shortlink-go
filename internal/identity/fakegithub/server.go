/*
Package fakegithub 提供仅用于测试的内存版 GitHub OAuth 2.0 与用户信息服务（阶段 11）。

该包放在独立子包内，只允许被 *_test.go 引用，不会编译进任何生产二进制。
模拟 /login/oauth/authorize、/login/oauth/access_token（含 HTTP 200 + form-urlencoded error 语义）、
GET /user 和 GET /user/emails，支持注入授权错误、令牌故障与 REST API 状态码异常。
*/
package fakegithub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const codeTTL = 5 * time.Minute

// Email 表示 /user/emails 返回的一条邮箱记录。
type Email struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// User 表示假 GitHub 服务持有的测试用户资料与邮箱列表。
type User struct {
	ID     int64
	Login  string
	Name   string
	Emails []Email
}

// Tamper 控制假 GitHub 服务注入的异常行为，用于离线覆盖各类 OAuth 2.0 边界与故障分类。
type Tamper struct {
	// AuthorizeError 非空时，/login/oauth/authorize 带 error=<该值> 和原 state 跳回回调地址。
	AuthorizeError string
	// CodeExpired 为 true 时，发出的授权码立即过期，换取令牌时返回 bad_verification_code。
	CodeExpired bool
	// TokenError 非空时，/login/oauth/access_token 直接返回该 OAuth error（默认 HTTP 200）。
	TokenError string
	// TokenStatus 非零时覆盖 /login/oauth/access_token 的错误 HTTP 状态码（默认 200）。
	TokenStatus int
	// EmptyAccessToken 为 true 时，/login/oauth/access_token 返回 200 但不带 access_token。
	EmptyAccessToken bool
	// UserStatus 非零时，GET /user 返回该 HTTP 状态码。
	UserStatus int
	// UserIDOverride 非空时覆盖 GET /user 返回的 id。
	UserIDOverride *int64
	// EmailsStatus 非零时，GET /user/emails 返回该 HTTP 状态码。
	EmailsStatus int
}

// AuthorizeRequest 记录最近一次打到 /login/oauth/authorize 的查询参数。
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
	codeChallenge string
	scopes        []string
	user          User
	expiresAt     time.Time
}

type issuedToken struct {
	user   User
	scopes []string
}

// Server 封装 httptest.Server 和内存态授权码/访问令牌状态。
type Server struct {
	clientID     string
	clientSecret string
	httpServer   *httptest.Server

	mu            sync.Mutex
	user          User
	tamper        Tamper
	codes         map[string]issuedAuthCode
	tokens        map[string]issuedToken
	lastAuthorize AuthorizeRequest
	hasAuthorize  bool
}

/*
Start 启动一个监听本机回环地址的假 GitHub 服务，并在测试结束时自动关闭。
默认用户为 ID 10001、login octocat、主邮箱 octocat@example.com（已验证）。
*/
func Start(t testing.TB, clientID, clientSecret string) *Server {
	t.Helper()
	s := &Server{
		clientID:     clientID,
		clientSecret: clientSecret,
		user: User{
			ID:    10001,
			Login: "octocat",
			Name:  "The Octocat",
			Emails: []Email{
				{Email: "octocat@example.com", Primary: true, Verified: true},
			},
		},
		codes:  make(map[string]issuedAuthCode),
		tokens: make(map[string]issuedToken),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /login/oauth/access_token", s.handleToken)
	mux.HandleFunc("GET /user", s.handleUser)
	mux.HandleFunc("GET /user/emails", s.handleEmails)

	ts := httptest.NewUnstartedServer(mux)
	ts.Start()
	t.Cleanup(ts.Close)
	s.httpServer = ts
	return s
}

// AuthURL 返回授权端点地址。
func (s *Server) AuthURL() string {
	return s.httpServer.URL + "/login/oauth/authorize"
}

// TokenURL 返回令牌兑换端点地址。
func (s *Server) TokenURL() string {
	return s.httpServer.URL + "/login/oauth/access_token"
}

// APIBaseURL 返回 GitHub REST API 根地址。
func (s *Server) APIBaseURL() string {
	return s.httpServer.URL
}

// Client 返回可访问该测试服务的 HTTP 客户端。
func (s *Server) Client() *http.Client {
	return s.httpServer.Client()
}

// SetUser 切换后续 /login/oauth/authorize 绑定的登录用户。
func (s *Server) SetUser(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user = u
}

// SetTamper 设置或重置故障注入选项。
func (s *Server) SetTamper(tamper Tamper) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tamper = tamper
}

// LastAuthorize 返回最近一次收到的合法授权请求参数。
func (s *Server) LastAuthorize() (AuthorizeRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthorize, s.hasAuthorize
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
	if req.ClientID != s.clientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	redirectURL, err := url.Parse(req.RedirectURI)
	if err != nil || (redirectURL.Scheme != "http" && redirectURL.Scheme != "https") || redirectURL.Host == "" {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	if req.State == "" || req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
		http.Error(w, "state and PKCE S256 are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.lastAuthorize = req
	s.hasAuthorize = true
	authErr := s.tamper.AuthorizeError
	codeExpired := s.tamper.CodeExpired
	user := copyUser(s.user)
	s.mu.Unlock()

	if authErr != "" {
		cbQuery := redirectURL.Query()
		cbQuery.Set("error", authErr)
		cbQuery.Set("state", req.State)
		redirectURL.RawQuery = cbQuery.Encode()
		http.Redirect(w, r, redirectURL.String(), http.StatusFound)
		return
	}

	code, err := randomHex(16)
	if err != nil {
		http.Error(w, "generate code failed", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().UTC().Add(codeTTL)
	if codeExpired {
		expiresAt = time.Now().UTC().Add(-time.Second)
	}

	s.mu.Lock()
	s.codes[code] = issuedAuthCode{
		clientID:      req.ClientID,
		redirectURI:   req.RedirectURI,
		codeChallenge: req.CodeChallenge,
		scopes:        strings.Fields(req.Scope),
		user:          user,
		expiresAt:     expiresAt,
	}
	s.mu.Unlock()

	cbQuery := redirectURL.Query()
	cbQuery.Set("code", code)
	cbQuery.Set("state", req.State)
	redirectURL.RawQuery = cbQuery.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeGitHubTokenError(w, r, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	clientID := r.PostForm.Get("client_id")
	clientSecret := r.PostForm.Get("client_secret")
	if clientID != s.clientID || clientSecret != s.clientSecret {
		// 真实 GitHub 在客户端凭据错误时默认仍返回 HTTP 200 + error=incorrect_client_credentials。
		writeGitHubTokenError(w, r, http.StatusOK, "incorrect_client_credentials", "The client_id and/or client_secret passed are incorrect.")
		return
	}

	s.mu.Lock()
	tamper := s.tamper
	code := r.PostForm.Get("code")
	issued, exists := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()

	if tamper.TokenError != "" {
		status := tamper.TokenStatus
		if status == 0 {
			status = http.StatusOK
		}
		writeGitHubTokenError(w, r, status, tamper.TokenError, "injected token error")
		return
	}

	if !exists || !issued.expiresAt.After(time.Now().UTC()) {
		writeGitHubTokenError(w, r, http.StatusOK, "bad_verification_code", "The code passed is incorrect or expired.")
		return
	}
	if r.PostForm.Get("redirect_uri") != issued.redirectURI {
		writeGitHubTokenError(w, r, http.StatusOK, "redirect_uri_mismatch", "The redirect_uri MUST match the registered callback URL.")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	if verifier == "" || pkceS256Challenge(verifier) != issued.codeChallenge {
		writeGitHubTokenError(w, r, http.StatusOK, "bad_verification_code", "The code verifier is invalid.")
		return
	}

	accessToken := ""
	if !tamper.EmptyAccessToken {
		var err error
		accessToken, err = randomHex(20)
		if err != nil {
			http.Error(w, "generate token failed", http.StatusInternalServerError)
			return
		}
		accessToken = "gho_" + accessToken
		s.mu.Lock()
		s.tokens[accessToken] = issuedToken{
			user:   issued.user,
			scopes: issued.scopes,
		}
		s.mu.Unlock()
	}

	scopeStr := strings.Join(issued.scopes, ",")
	w.Header().Set("Cache-Control", "no-store")
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": accessToken,
			"scope":        scopeStr,
			"token_type":   "bearer",
		})
		return
	}
	vals := url.Values{
		"access_token": {accessToken},
		"scope":        {scopeStr},
		"token_type":   {"bearer"},
	}
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(vals.Encode()))
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	tok, tamper, ok := s.authorizeAPI(w, r)
	if !ok {
		return
	}
	if tamper.UserStatus != 0 {
		writeJSON(w, tamper.UserStatus, map[string]string{"message": "injected /user error"})
		return
	}
	id := tok.user.ID
	if tamper.UserIDOverride != nil {
		id = *tamper.UserIDOverride
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"login": tok.user.Login,
		"name":  tok.user.Name,
		"email": nil,
	})
}

func (s *Server) handleEmails(w http.ResponseWriter, r *http.Request) {
	tok, tamper, ok := s.authorizeAPI(w, r)
	if !ok {
		return
	}
	if !slices.Contains(tok.scopes, "user:email") {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing user:email scope"})
		return
	}
	if tamper.EmailsStatus != 0 {
		writeJSON(w, tamper.EmailsStatus, map[string]string{"message": "injected /user/emails error"})
		return
	}
	emails := tok.user.Emails
	if emails == nil {
		emails = []Email{}
	}
	writeJSON(w, http.StatusOK, emails)
}

func (s *Server) authorizeAPI(w http.ResponseWriter, r *http.Request) (issuedToken, Tamper, bool) {
	if strings.TrimSpace(r.Header.Get("User-Agent")) == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "User-Agent required"})
		return issuedToken{}, Tamper{}, false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return issuedToken{}, Tamper{}, false
	}
	token := auth[len(prefix):]

	s.mu.Lock()
	tok, exists := s.tokens[token]
	tamper := s.tamper
	s.mu.Unlock()
	if !exists {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return issuedToken{}, Tamper{}, false
	}
	return tok, tamper, true
}

func writeGitHubTokenError(w http.ResponseWriter, r *http.Request, status int, code, desc string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{
			"error":             code,
			"error_description": desc,
		})
		return
	}
	vals := url.Values{
		"error":             {code},
		"error_description": {desc},
	}
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(vals.Encode()))
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
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

func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func copyUser(u User) User {
	out := u
	out.Emails = append([]Email(nil), u.Emails...)
	return out
}
