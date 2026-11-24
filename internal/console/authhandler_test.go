package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/identity"
	"shortlink/internal/identity/fakegithub"
	"shortlink/internal/identity/fakeoidc"
	"shortlink/internal/identity/oauth"
	"shortlink/internal/memorystore"
)

type stubSessionStore struct {
	calls        int
	lastID       identity.ExternalIdentity
	lastTTL      time.Duration
	err          error
	nextSess     int64
	authFn       func(ctx context.Context, token string, orgID int64) (identity.Principal, error)
	infoFn       func(ctx context.Context, token string) (identity.SessionInfo, error)
	revokeFn     func(ctx context.Context, token string) error
	revokedToken string
	revokeCalls  int
}

func (s *stubSessionStore) AuthenticateSessionForOrg(ctx context.Context, token string, orgID int64) (identity.Principal, error) {
	if s.authFn != nil {
		return s.authFn(ctx, token, orgID)
	}
	return identity.Principal{}, identity.ErrUnauthenticated
}

func (s *stubSessionStore) CreateSessionForIdentity(_ context.Context, id identity.ExternalIdentity, ttl time.Duration) (identity.IssuedSession, error) {
	s.calls++
	s.lastID = id
	s.lastTTL = ttl
	if s.err != nil {
		return identity.IssuedSession{}, s.err
	}
	s.nextSess++
	token, _, err := identity.SessionToken.Generate()
	if err != nil {
		return identity.IssuedSession{}, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	return identity.IssuedSession{
		User: identity.User{
			ID:        100,
			Email:     id.Email,
			Name:      id.Name,
			CreatedAt: now,
		},
		Session: identity.Session{
			ID:        s.nextSess,
			UserID:    100,
			CreatedAt: now,
			ExpiresAt: now.Add(ttl),
		},
		Token:        token,
		CookieMaxAge: ttl,
	}, nil
}

func (s *stubSessionStore) GetSessionInfo(ctx context.Context, token string) (identity.SessionInfo, error) {
	if s.infoFn != nil {
		return s.infoFn(ctx, token)
	}
	return identity.SessionInfo{}, identity.ErrUnauthenticated
}

func (s *stubSessionStore) RevokeSession(ctx context.Context, token string) error {
	s.revokeCalls++
	s.revokedToken = token
	if s.revokeFn != nil {
		return s.revokeFn(ctx, token)
	}
	return nil
}

func newOIDCTestServer(t *testing.T, redirectURL string, sessions SessionStore) (*Server, *fakeoidc.Server) {
	t.Helper()
	return newOIDCTestServerWithRegistry(t, redirectURL, sessions, newTestRegistry())
}

func newOIDCTestServerWithRegistry(t *testing.T, redirectURL string, sessions SessionStore, reg *prometheus.Registry) (*Server, *fakeoidc.Server) {
	t.Helper()
	const (
		clientID     = "shortlink-console"
		clientSecret = "shortlink-console-secret"
	)
	fake := fakeoidc.Start(t, clientID, clientSecret)
	provider, err := oauth.NewOIDCProvider(context.Background(), oauth.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       reg,
		keys:           allowKey{},
		sessions:       sessions,
		authorizer:     identity.NewMapAuthorizer(),
		oidc:           provider,
		sessionMgr:     sessions,
		sessionTTL:     24 * time.Hour,
	})
	return srv, fake
}

func newGitHubTestServer(t *testing.T, redirectURL string, sessions SessionStore) (*Server, *fakegithub.Server) {
	t.Helper()
	return newGitHubTestServerWithRegistry(t, redirectURL, sessions, newTestRegistry())
}

func newGitHubTestServerWithRegistry(t *testing.T, redirectURL string, sessions SessionStore, reg *prometheus.Registry) (*Server, *fakegithub.Server) {
	t.Helper()
	const (
		clientID     = "gh-console"
		clientSecret = "gh-console-secret"
	)
	fake := fakegithub.Start(t, clientID, clientSecret)
	provider, err := oauth.NewGitHubProvider(oauth.GitHubConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      fake.AuthURL(),
		TokenURL:     fake.TokenURL(),
		APIBaseURL:   fake.APIBaseURL(),
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       reg,
		keys:           allowKey{},
		sessions:       sessions,
		authorizer:     identity.NewMapAuthorizer(),
		github:         provider,
		sessionMgr:     sessions,
		sessionTTL:     24 * time.Hour,
	})
	return srv, fake
}

func newDualAuthTestServer(t *testing.T, sessions SessionStore) (*Server, *fakeoidc.Server, *fakegithub.Server) {
	t.Helper()
	// 故意让两个提供方共用同一把 clientSecret，验证流程 Cookie 仍靠 purpose 前缀做域隔离。
	const sharedSecret = "shared-console-secret"
	fakeOIDC := fakeoidc.Start(t, "shortlink-console", sharedSecret)
	oidcProvider, err := oauth.NewOIDCProvider(context.Background(), oauth.OIDCConfig{
		Issuer:       fakeOIDC.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: sharedSecret,
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
		HTTPClient:   fakeOIDC.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	fakeGH := fakegithub.Start(t, "gh-console", sharedSecret)
	ghProvider, err := oauth.NewGitHubProvider(oauth.GitHubConfig{
		ClientID:     "gh-console",
		ClientSecret: sharedSecret,
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/github/callback",
		AuthURL:      fakeGH.AuthURL(),
		TokenURL:     fakeGH.TokenURL(),
		APIBaseURL:   fakeGH.APIBaseURL(),
		HTTPClient:   fakeGH.Client(),
	})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
		sessions:       sessions,
		authorizer:     identity.NewMapAuthorizer(),
		oidc:           oidcProvider,
		github:         ghProvider,
		sessionMgr:     sessions,
		sessionTTL:     24 * time.Hour,
	})
	return srv, fakeOIDC, fakeGH
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func runAuthorizeAtFake(t *testing.T, authorizeURL string) *url.URL {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("fake authorize status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse callback Location: %v", err)
	}
	return loc
}

func TestOIDCRoutesDisabledWhenUnconfigured(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	routes := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: oidcLoginPath},
		{method: http.MethodGet, path: oidcCallbackPath},
		{method: http.MethodGet, path: githubLoginPath},
		{method: http.MethodGet, path: githubCallbackPath},
		{method: http.MethodPost, path: authLogoutPath},
		{method: http.MethodGet, path: mePath},
	}
	for _, rt := range routes {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s without OIDC = %d, want 404", rt.method, rt.path, rec.Code)
		}
	}
}

func TestOIDCLoginRedirectAndFlowCookie(t *testing.T) {
	tests := []struct {
		name        string
		redirectURL string
		wantSecure  bool
	}{
		{name: "http local", redirectURL: "http://127.0.0.1:8081/api/v1/auth/callback", wantSecure: false},
		{name: "https prod", redirectURL: "https://console.example.com/api/v1/auth/callback", wantSecure: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fake := newOIDCTestServer(t, tt.redirectURL, &stubSessionStore{})
			handler := srv.Routes()

			rec1 := httptest.NewRecorder()
			handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
			if rec1.Code != http.StatusFound {
				t.Fatalf("login status = %d, want 302, body = %s", rec1.Code, rec1.Body.String())
			}
			if got := rec1.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}

			loc1, err := url.Parse(rec1.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if !strings.HasPrefix(loc1.String(), fake.URL()+"/authorize?") {
				t.Fatalf("Location = %q, want prefix %q", loc1.String(), fake.URL()+"/authorize?")
			}
			q1 := loc1.Query()
			if q1.Get("response_type") != "code" ||
				q1.Get("client_id") != "shortlink-console" ||
				q1.Get("redirect_uri") != tt.redirectURL ||
				!strings.Contains(q1.Get("scope"), "openid") ||
				q1.Get("state") == "" ||
				q1.Get("nonce") == "" ||
				q1.Get("code_challenge") == "" ||
				q1.Get("code_challenge_method") != "S256" {
				t.Fatalf("authorize query = %v, missing required OIDC/PKCE params", q1)
			}

			flowCookie := findCookie(rec1.Result().Cookies(), OIDCFlowCookieName)
			if flowCookie == nil {
				t.Fatal("missing flow cookie")
			}
			if !flowCookie.HttpOnly || flowCookie.SameSite != http.SameSiteLaxMode || flowCookie.Path != oidcCallbackPath || flowCookie.MaxAge != int(flowCookieTTL/time.Second) || flowCookie.Secure != tt.wantSecure {
				t.Errorf("flow cookie = %+v, want HttpOnly/Lax/Path=%s/MaxAge=%d/Secure=%v",
					flowCookie, oidcCallbackPath, int(flowCookieTTL/time.Second), tt.wantSecure)
			}

			// 连续两次登录必须生成不同的 state、nonce 和 code_challenge。
			rec2 := httptest.NewRecorder()
			handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
			loc2, err := url.Parse(rec2.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse second Location: %v", err)
			}
			q2 := loc2.Query()
			if q2.Get("state") == q1.Get("state") || q2.Get("nonce") == q1.Get("nonce") || q2.Get("code_challenge") == q1.Get("code_challenge") {
				t.Fatal("second login reused state, nonce, or code_challenge")
			}
		})
	}
}

func TestOIDCCallbackSuccessCreatesSessionAndClearsFlowCookie(t *testing.T) {
	sessions := &stubSessionStore{}
	const redirectURL = "https://console.example.com/api/v1/auth/callback"
	reg := newTestRegistry()
	srv, fake := newOIDCTestServerWithRegistry(t, redirectURL, sessions, reg)
	handler := srv.Routes()

	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginRec.Code)
	}
	flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
	if flowCookie == nil {
		t.Fatal("missing flow cookie")
	}

	callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))
	cbReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
	cbReq.AddCookie(flowCookie)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)

	if cbRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200, body = %s", cbRec.Code, cbRec.Body.String())
	}
	if got := cbRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if sessions.calls != 1 {
		t.Fatalf("CreateSessionForIdentity calls = %d, want 1", sessions.calls)
	}
	wantID := identity.ExternalIdentity{
		Issuer:        fake.URL(),
		Subject:       "08a8684b-db88-4b73-90a9-3cd1661f5466",
		Email:         "alice@example.com",
		EmailVerified: true,
		Name:          "Alice",
	}
	if sessions.lastID != wantID {
		t.Errorf("created session identity = %+v, want %+v", sessions.lastID, wantID)
	}
	if sessions.lastTTL != 24*time.Hour {
		t.Errorf("session ttl = %v, want 24h", sessions.lastTTL)
	}

	cookies := cbRec.Result().Cookies()
	sessCookie := findCookie(cookies, SessionCookieName)
	if sessCookie == nil {
		t.Fatal("missing session cookie")
	}
	if !identity.SessionToken.Valid(sessCookie.Value) {
		t.Errorf("session cookie value %q is not a valid session token", sessCookie.Value)
	}
	wantMaxAge := int((24 * time.Hour) / time.Second)
	if !sessCookie.HttpOnly || !sessCookie.Secure || sessCookie.SameSite != http.SameSiteLaxMode || sessCookie.Path != "/api/" || sessCookie.MaxAge != wantMaxAge {
		t.Errorf("session cookie = %+v, want HttpOnly/Secure/Lax/Path=/api//MaxAge=%d", sessCookie, wantMaxAge)
	}

	clearedFlow := findCookie(cookies, OIDCFlowCookieName)
	if clearedFlow == nil || clearedFlow.MaxAge >= 0 || clearedFlow.Value != "" || clearedFlow.Path != oidcCallbackPath {
		t.Errorf("flow cookie after callback = %+v, want deleted cookie", clearedFlow)
	}

	var resp loginResponse
	if err := json.Unmarshal(cbRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode callback response: %v", err)
	}
	if resp.User.ID != 100 || resp.User.Email != "alice@example.com" || resp.User.Name != "Alice" || resp.ExpiresAt.IsZero() {
		t.Errorf("callback response = %+v", resp)
	}
	// 会话明文只能进 HttpOnly Cookie，绝不能出现在响应体里。
	if strings.Contains(cbRec.Body.String(), sessCookie.Value) {
		t.Error("callback response body leaked plaintext session token")
	}

	// 验证 HTTP 指标记录了正确的路由模式。
	if got := httpRequestCount(t, reg, "GET "+oidcLoginPath, http.MethodGet, http.StatusFound); got != 1 {
		t.Errorf("login metric = %v, want 1", got)
	}
	if got := httpRequestCount(t, reg, "GET "+oidcCallbackPath, http.MethodGet, http.StatusOK); got != 1 {
		t.Errorf("callback metric = %v, want 1", got)
	}
}

func TestOIDCCallbackRejectsTamperedAndInvalidFlows(t *testing.T) {
	tamperTests := []struct {
		name       string
		tamper     fakeoidc.Tamper
		wantStatus int
		wantClear  bool
	}{
		{name: "state mismatch", tamper: fakeoidc.Tamper{State: "tampered-state"}, wantStatus: http.StatusBadRequest},
		{name: "expired authorization code", tamper: fakeoidc.Tamper{CodeExpired: true}, wantStatus: http.StatusUnauthorized, wantClear: true},
		{name: "wrong signature", tamper: fakeoidc.Tamper{WrongSignature: true}, wantStatus: http.StatusUnauthorized, wantClear: true},
		{name: "wrong audience", tamper: fakeoidc.Tamper{Audience: "other-client"}, wantStatus: http.StatusUnauthorized, wantClear: true},
		{name: "wrong issuer", tamper: fakeoidc.Tamper{Issuer: "https://evil.example.com"}, wantStatus: http.StatusUnauthorized, wantClear: true},
		{name: "expired id token", tamper: fakeoidc.Tamper{ExpiredIDToken: true}, wantStatus: http.StatusUnauthorized, wantClear: true},
		{name: "nonce mismatch", tamper: fakeoidc.Tamper{Nonce: "wrong-nonce"}, wantStatus: http.StatusUnauthorized, wantClear: true},
	}

	for _, tt := range tamperTests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := &stubSessionStore{}
			srv, fake := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
			fake.SetTamper(tt.tamper)
			handler := srv.Routes()

			loginRec := httptest.NewRecorder()
			handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
			flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
			if flowCookie == nil {
				t.Fatal("missing flow cookie")
			}

			callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))
			cbReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
			cbReq.AddCookie(flowCookie)
			cbRec := httptest.NewRecorder()
			handler.ServeHTTP(cbRec, cbReq)

			if cbRec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", cbRec.Code, tt.wantStatus, cbRec.Body.String())
			}
			if sessions.calls != 0 {
				t.Errorf("CreateSessionForIdentity calls = %d, want 0", sessions.calls)
			}
			if sessCookie := findCookie(cbRec.Result().Cookies(), SessionCookieName); sessCookie != nil {
				t.Errorf("unexpected session cookie set on failure: %+v", sessCookie)
			}
			cleared := findCookie(cbRec.Result().Cookies(), OIDCFlowCookieName)
			if tt.wantClear {
				if cleared == nil || cleared.MaxAge >= 0 {
					t.Errorf("flow cookie = %+v, want cleared", cleared)
				}
			} else if cleared != nil {
				t.Errorf("flow cookie = %+v, want left untouched", cleared)
			}
		})
	}
}

func TestOIDCCallbackEdgeCases(t *testing.T) {
	t.Run("missing flow cookie returns 400", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?code=abc&state=xyz", nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
		if cleared := findCookie(rec.Result().Cookies(), OIDCFlowCookieName); cleared != nil {
			t.Errorf("missing cookie still wrote Set-Cookie: %+v", cleared)
		}
	})

	t.Run("malformed flow cookie returns 400", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		req := httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?code=abc&state=xyz", nil)
		req.AddCookie(&http.Cookie{Name: OIDCFlowCookieName, Value: "not-valid-base64!!!"})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
	})

	t.Run("expired flow cookie returns 400", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
		srv.auth.now = func() time.Time { return now }
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		// 刚好到 10 分钟过期界限。
		srv.auth.now = func() time.Time { return now.Add(flowCookieTTL) }
		cbReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", cbRec.Code, cbRec.Body.String())
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
	})

	t.Run("provider error parameter returns 401", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		loc, _ := url.Parse(loginRec.Header().Get("Location"))
		state := loc.Query().Get("state")

		cbReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s?error=access_denied&state=%s", oidcCallbackPath, url.QueryEscape(state)), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401, body = %s", cbRec.Code, cbRec.Body.String())
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
	})

	t.Run("missing code returns 400", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		loc, _ := url.Parse(loginRec.Header().Get("Location"))
		state := loc.Query().Get("state")

		cbReq := httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?state="+url.QueryEscape(state), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", cbRec.Code, cbRec.Body.String())
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
	})

	t.Run("HEAD on callback returns 405 without consuming session", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		headReq := httptest.NewRequest(http.MethodHead, callbackURL.RequestURI(), nil)
		headReq.AddCookie(flowCookie)
		headRec := httptest.NewRecorder()
		handler.ServeHTTP(headRec, headReq)
		if headRec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("HEAD status = %d, want 405", headRec.Code)
		}
		if sessions.calls != 0 {
			t.Fatalf("sessions.calls after HEAD = %d, want 0", sessions.calls)
		}

		// 原来的授权码和流程 Cookie 未被 HEAD 消费，紧接着的 GET 仍应成功。
		getReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		getReq.AddCookie(flowCookie)
		getRec := httptest.NewRecorder()
		handler.ServeHTTP(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET after HEAD status = %d, want 200, body = %s", getRec.Code, getRec.Body.String())
		}
	})

	t.Run("replaying same callback fails with 401", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		cb1 := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		cb1.AddCookie(flowCookie)
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, cb1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first callback = %d, want 200", rec1.Code)
		}

		// 即使客户端强行重放旧流程 Cookie 和授权码，令牌端点的授权码也已消费。
		cb2 := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		cb2.AddCookie(flowCookie)
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, cb2)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("replayed callback = %d, want 401, body = %s", rec2.Code, rec2.Body.String())
		}
		if sessions.calls != 1 {
			t.Errorf("sessions.calls = %d, want 1", sessions.calls)
		}
	})

	t.Run("nil session creator fails closed with 500", func(t *testing.T) {
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?code=abc&state=xyz", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("session store error returns 500 without cookie", func(t *testing.T) {
		sessions := &stubSessionStore{err: errors.New("db down")}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		cbReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500, body = %s", cbRec.Code, cbRec.Body.String())
		}
		if sessCookie := findCookie(cbRec.Result().Cookies(), SessionCookieName); sessCookie != nil {
			t.Errorf("unexpected session cookie set on store error: %+v", sessCookie)
		}
	})

	t.Run("state mismatch does not burn the in-progress login", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		bad := httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?code=abc&state=not-this-login", nil)
		bad.AddCookie(flowCookie)
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, bad)
		if badRec.Code != http.StatusBadRequest {
			t.Fatalf("mismatch status = %d, want 400", badRec.Code)
		}
		if cleared := findCookie(badRec.Result().Cookies(), OIDCFlowCookieName); cleared != nil {
			t.Fatalf("mismatch cleared flow cookie: %+v", cleared)
		}

		good := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		good.AddCookie(flowCookie)
		goodRec := httptest.NewRecorder()
		handler.ServeHTTP(goodRec, good)
		if goodRec.Code != http.StatusOK {
			t.Fatalf("callback after mismatch = %d, want 200, body = %s", goodRec.Code, goodRec.Body.String())
		}
	})

	t.Run("forged flow cookie is rejected and does not replace the real one", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		forged := *flowCookie
		forged.Value = "x" + forged.Value
		bad := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		bad.AddCookie(&forged)
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, bad)
		if badRec.Code != http.StatusBadRequest {
			t.Fatalf("forged status = %d, want 400, body = %s", badRec.Code, badRec.Body.String())
		}
		if cleared := findCookie(badRec.Result().Cookies(), OIDCFlowCookieName); cleared != nil {
			t.Fatalf("forged cookie cleared flow cookie: %+v", cleared)
		}

		good := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		good.AddCookie(flowCookie)
		goodRec := httptest.NewRecorder()
		handler.ServeHTTP(goodRec, good)
		if goodRec.Code != http.StatusOK {
			t.Fatalf("callback after forged cookie = %d, want 200, body = %s", goodRec.Code, goodRec.Body.String())
		}
	})

	t.Run("jwks failure returns 502", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, fake := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), OIDCFlowCookieName)
		callbackURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))
		fake.SetTamper(fakeoidc.Tamper{JWKSFailure: true})

		cbReq := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502, body = %s", cbRec.Code, cbRec.Body.String())
		}
		if sessions.calls != 0 {
			t.Errorf("sessions.calls = %d, want 0", sessions.calls)
		}
	})
}
