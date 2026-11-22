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
	provider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
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
	provider, err := identity.NewGitHubProvider(identity.GitHubConfig{
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
	oidcProvider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
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
	ghProvider, err := identity.NewGitHubProvider(identity.GitHubConfig{
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

func TestLogout(t *testing.T) {
	validToken := "sls_" + strings.Repeat("12", 32)

	t.Run("revokes session and clears cookie", func(t *testing.T) {
		sessions := &stubSessionStore{}
		reg := newTestRegistry()
		srv, _ := newOIDCTestServerWithRegistry(t, "https://console.example.com/api/v1/auth/callback", sessions, reg)
		handler := srv.Routes()

		req := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204, body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		if sessions.revokeCalls != 1 || sessions.revokedToken != validToken {
			t.Errorf("revokeCalls=%d token=%q, want 1 / %q", sessions.revokeCalls, sessions.revokedToken, validToken)
		}

		cleared := findCookie(rec.Result().Cookies(), SessionCookieName)
		if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 || cleared.Path != "/api/" || !cleared.HttpOnly || !cleared.Secure || cleared.SameSite != http.SameSiteLaxMode {
			t.Errorf("cleared cookie = %+v, want deleted HttpOnly/Secure/Lax/Path=/api/", cleared)
		}
		if got := httpRequestCount(t, reg, "POST "+authLogoutPath, http.MethodPost, http.StatusNoContent); got != 1 {
			t.Errorf("logout metric = %v, want 1", got)
		}
	})

	t.Run("missing or malformed cookie is idempotent 204 without db call", func(t *testing.T) {
		for _, val := range []string{"", "bad-token"} {
			sessions := &stubSessionStore{}
			srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
			req := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
			if val != "" {
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: val})
			}
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			if sessions.revokeCalls != 0 {
				t.Errorf("revokeCalls = %d, want 0", sessions.revokeCalls)
			}
			if cleared := findCookie(rec.Result().Cookies(), SessionCookieName); cleared == nil || cleared.MaxAge >= 0 || cleared.Secure {
				t.Errorf("cleared cookie = %+v, want deleted non-secure cookie on http", cleared)
			}
		}
	})

	t.Run("cross-origin logout rejected with 403 without revoking or clearing cookie", func(t *testing.T) {
		for _, hdr := range []struct{ k, v string }{
			{k: "Sec-Fetch-Site", v: "cross-site"},
			{k: "Origin", v: "https://evil.example"},
		} {
			sessions := &stubSessionStore{}
			srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
			req := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
			req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
			req.Header.Set(hdr.k, hdr.v)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s=%s status = %d, want 403, body = %s", hdr.k, hdr.v, rec.Code, rec.Body.String())
			}
			if sessions.revokeCalls != 0 {
				t.Errorf("revokeCalls = %d, want 0", sessions.revokeCalls)
			}
			if cleared := findCookie(rec.Result().Cookies(), SessionCookieName); cleared != nil {
				t.Errorf("unexpected Set-Cookie on 403: %+v", cleared)
			}
		}
	})

	t.Run("duplicate session cookies return 401 without revoking or clearing", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		req := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "sls_" + strings.Repeat("ab", 32)})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
		}
		if sessions.revokeCalls != 0 {
			t.Errorf("revokeCalls = %d, want 0", sessions.revokeCalls)
		}
		if cleared := findCookie(rec.Result().Cookies(), SessionCookieName); cleared != nil {
			t.Errorf("cleared cookie on duplicate session cookies: %+v", cleared)
		}
	})

	t.Run("store error returns 500 without clearing cookie", func(t *testing.T) {
		sessions := &stubSessionStore{
			revokeFn: func(context.Context, string) error { return errors.New("db down") },
		}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		req := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if cleared := findCookie(rec.Result().Cookies(), SessionCookieName); cleared != nil {
			t.Errorf("cleared cookie on store error: %+v", cleared)
		}
	})

	t.Run("GET logout returns 405", func(t *testing.T) {
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", &stubSessionStore{})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, authLogoutPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET logout = %d, want 405", rec.Code)
		}
	})
}

func TestMe(t *testing.T) {
	validToken := "sls_" + strings.Repeat("34", 32)
	expires := time.Date(2026, 11, 29, 10, 0, 0, 0, time.UTC)

	t.Run("returns user and organizations and ignores X-Org-ID", func(t *testing.T) {
		sessions := &stubSessionStore{
			infoFn: func(_ context.Context, token string) (identity.SessionInfo, error) {
				if token != validToken {
					return identity.SessionInfo{}, identity.ErrUnauthenticated
				}
				return identity.SessionInfo{
					User:      identity.User{ID: 7, Email: "alice@example.com", Name: "Alice"},
					ExpiresAt: expires,
					Orgs: []identity.UserOrg{
						{ID: 1, Name: "acme", Role: identity.RoleOwner},
						{ID: 2, Name: "beta", Role: identity.RoleViewer},
					},
				}, nil
			},
		}
		reg := newTestRegistry()
		srv, _ := newOIDCTestServerWithRegistry(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions, reg)
		req := httptest.NewRequest(http.MethodGet, mePath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		req.Header.Set(orgIDHeader, "not-an-org")
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		var resp meResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /me: %v", err)
		}
		if resp.User.ID != 7 || resp.User.Email != "alice@example.com" || resp.User.Name != "Alice" || !resp.ExpiresAt.Equal(expires) {
			t.Errorf("resp user/expires = %+v", resp)
		}
		if len(resp.Organizations) != 2 || resp.Organizations[0] != (meOrgResponse{ID: 1, Name: "acme", Role: "owner"}) || resp.Organizations[1] != (meOrgResponse{ID: 2, Name: "beta", Role: "viewer"}) {
			t.Errorf("organizations = %+v", resp.Organizations)
		}
		if got := httpRequestCount(t, reg, "GET "+mePath, http.MethodGet, http.StatusOK); got != 1 {
			t.Errorf("/me metric = %v, want 1", got)
		}
	})

	t.Run("empty organizations encodes as JSON array", func(t *testing.T) {
		sessions := &stubSessionStore{
			infoFn: func(context.Context, string) (identity.SessionInfo, error) {
				return identity.SessionInfo{
					User:      identity.User{ID: 8, Email: "bob@example.com", Name: "Bob"},
					ExpiresAt: expires,
				}, nil
			},
		}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		req := httptest.NewRequest(http.MethodGet, mePath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"organizations":[]`) {
			t.Errorf("body = %s, want empty JSON array for organizations", rec.Body.String())
		}
	})

	t.Run("unauthorized cases return 401 without Bearer challenge", func(t *testing.T) {
		sessions := &stubSessionStore{
			infoFn: func(context.Context, string) (identity.SessionInfo, error) {
				return identity.SessionInfo{}, identity.ErrUnauthenticated
			},
		}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		handler := srv.Routes()

		cases := []struct {
			name string
			req  func() *http.Request
		}{
			{
				name: "missing cookie",
				req:  func() *http.Request { return httptest.NewRequest(http.MethodGet, mePath, nil) },
			},
			{
				name: "malformed cookie",
				req: func() *http.Request {
					r := httptest.NewRequest(http.MethodGet, mePath, nil)
					r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "invalid"})
					return r
				},
			},
			{
				name: "api key only",
				req: func() *http.Request {
					r := httptest.NewRequest(http.MethodGet, mePath, nil)
					authorize(r)
					return r
				},
			},
			{
				name: "unknown or expired session",
				req: func() *http.Request {
					r := httptest.NewRequest(http.MethodGet, mePath, nil)
					r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
					return r
				},
			},
			{
				name: "duplicate session cookies",
				req: func() *http.Request {
					r := httptest.NewRequest(http.MethodGet, mePath, nil)
					r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
					r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "sls_" + strings.Repeat("ab", 32)})
					return r
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, tc.req())
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "" {
					t.Errorf("WWW-Authenticate = %q, want empty on session-only /me", got)
				}
			})
		}
	})

	t.Run("store error returns 500", func(t *testing.T) {
		sessions := &stubSessionStore{
			infoFn: func(context.Context, string) (identity.SessionInfo, error) {
				return identity.SessionInfo{}, errors.New("db down")
			},
		}
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/callback", sessions)
		req := httptest.NewRequest(http.MethodGet, mePath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestGitHubAuthFlows(t *testing.T) {
	t.Run("standalone github enables /me and logout while leaving oidc 404", func(t *testing.T) {
		sessions := &stubSessionStore{
			infoFn: func(_ context.Context, token string) (identity.SessionInfo, error) {
				return identity.SessionInfo{
					User:      identity.User{ID: 12, Email: "octocat@example.com", Name: "The Octocat"},
					ExpiresAt: time.Now().UTC().Add(time.Hour),
				}, nil
			},
		}
		srv, _ := newGitHubTestServer(t, "http://127.0.0.1:8081/api/v1/auth/github/callback", sessions)
		handler := srv.Routes()

		for _, path := range []string{oidcLoginPath, oidcCallbackPath} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s without OIDC = %d, want 404", path, rec.Code)
			}
		}

		// 只配 GitHub 时登出与 /me 照常启用。
		token, _, err := identity.SessionToken.Generate()
		if err != nil {
			t.Fatalf("SessionToken.Generate: %v", err)
		}
		meRec := httptest.NewRecorder()
		meReq := httptest.NewRequest(http.MethodGet, mePath, nil)
		meReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		handler.ServeHTTP(meRec, meReq)
		if meRec.Code != http.StatusOK {
			t.Fatalf("GET /me = %d, want 200, body = %s", meRec.Code, meRec.Body.String())
		}

		logoutRec := httptest.NewRecorder()
		logoutReq := httptest.NewRequest(http.MethodPost, authLogoutPath, nil)
		logoutReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		handler.ServeHTTP(logoutRec, logoutReq)
		if logoutRec.Code != http.StatusNoContent {
			t.Fatalf("POST /logout = %d, want 204", logoutRec.Code)
		}
	})

	t.Run("login redirect sets sl_github_flow cookie without nonce", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			redirectURL string
			wantSecure  bool
		}{
			{name: "http", redirectURL: "http://127.0.0.1:8081/api/v1/auth/github/callback", wantSecure: false},
			{name: "https", redirectURL: "https://console.example.com/api/v1/auth/github/callback", wantSecure: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, fake := newGitHubTestServer(t, tc.redirectURL, &stubSessionStore{})
				rec := httptest.NewRecorder()
				srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, githubLoginPath, nil))
				if rec.Code != http.StatusFound {
					t.Fatalf("status = %d, want 302", rec.Code)
				}
				loc, err := url.Parse(rec.Header().Get("Location"))
				if err != nil {
					t.Fatalf("parse Location: %v", err)
				}
				if !strings.HasPrefix(loc.String(), fake.AuthURL()+"?") {
					t.Fatalf("Location = %q, want prefix %q", loc.String(), fake.AuthURL()+"?")
				}
				q := loc.Query()
				if q.Get("client_id") != "gh-console" || q.Get("redirect_uri") != tc.redirectURL || q.Get("scope") != "read:user user:email" ||
					q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Has("nonce") {
					t.Fatalf("github authorize query = %v", q)
				}
				flowCookie := findCookie(rec.Result().Cookies(), GitHubFlowCookieName)
				if flowCookie == nil || !flowCookie.HttpOnly || flowCookie.SameSite != http.SameSiteLaxMode ||
					flowCookie.Path != githubCallbackPath || flowCookie.MaxAge != int(flowCookieTTL/time.Second) || flowCookie.Secure != tc.wantSecure {
					t.Fatalf("flowCookie = %+v, want HttpOnly/Lax/Path=%s/MaxAge=600/Secure=%v", flowCookie, githubCallbackPath, tc.wantSecure)
				}
			})
		}
	})

	t.Run("callback success creates session and clears flow cookie", func(t *testing.T) {
		sessions := &stubSessionStore{}
		reg := newTestRegistry()
		srv, fake := newGitHubTestServerWithRegistry(t, "https://console.example.com/api/v1/auth/github/callback", sessions, reg)
		fake.SetUser(fakegithub.User{
			ID:     202601,
			Login:  "octo-dev",
			Name:   "Octo Dev",
			Emails: []fakegithub.Email{{Email: "octo@example.com", Primary: true, Verified: true}},
		})
		handler := srv.Routes()

		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, githubLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), GitHubFlowCookieName)
		cbURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		// 验证 HEAD 回调返回 405 且不消费一次性流程。
		headReq := httptest.NewRequest(http.MethodHead, cbURL.RequestURI(), nil)
		headReq.AddCookie(flowCookie)
		headRec := httptest.NewRecorder()
		handler.ServeHTTP(headRec, headReq)
		if headRec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("HEAD callback = %d, want 405", headRec.Code)
		}

		cbReq := httptest.NewRequest(http.MethodGet, cbURL.RequestURI(), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusOK {
			t.Fatalf("GET callback = %d, want 200, body = %s", cbRec.Code, cbRec.Body.String())
		}
		wantID := identity.ExternalIdentity{
			Issuer:        identity.GitHubIssuer,
			Subject:       "202601",
			Email:         "octo@example.com",
			EmailVerified: true,
			Name:          "Octo Dev",
		}
		if sessions.calls != 1 || sessions.lastID != wantID {
			t.Fatalf("sessions calls=%d lastID=%+v, want %+v", sessions.calls, sessions.lastID, wantID)
		}
		sessCookie := findCookie(cbRec.Result().Cookies(), SessionCookieName)
		if sessCookie == nil || !identity.SessionToken.Valid(sessCookie.Value) || !sessCookie.Secure {
			t.Fatalf("sessCookie = %+v, want valid Secure session cookie", sessCookie)
		}
		clearedFlow := findCookie(cbRec.Result().Cookies(), GitHubFlowCookieName)
		if clearedFlow == nil || clearedFlow.MaxAge >= 0 || clearedFlow.Path != githubCallbackPath {
			t.Fatalf("clearedFlow = %+v, want deleted cookie", clearedFlow)
		}
		if got := httpRequestCount(t, reg, "GET "+githubLoginPath, http.MethodGet, http.StatusFound); got != 1 {
			t.Errorf("github login metric = %v, want 1", got)
		}
		if got := httpRequestCount(t, reg, "GET "+githubCallbackPath, http.MethodGet, http.StatusOK); got != 1 {
			t.Errorf("github callback metric = %v, want 1", got)
		}
	})

	t.Run("cross provider flow cookie replay is rejected without clearing", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, _, _ := newDualAuthTestServer(t, sessions)
		handler := srv.Routes()

		oidcLoginRec := httptest.NewRecorder()
		handler.ServeHTTP(oidcLoginRec, httptest.NewRequest(http.MethodGet, oidcLoginPath, nil))
		oidcCookie := findCookie(oidcLoginRec.Result().Cookies(), OIDCFlowCookieName)
		oidcLoc, _ := url.Parse(oidcLoginRec.Header().Get("Location"))

		ghLoginRec := httptest.NewRecorder()
		handler.ServeHTTP(ghLoginRec, httptest.NewRequest(http.MethodGet, githubLoginPath, nil))
		ghCookie := findCookie(ghLoginRec.Result().Cookies(), GitHubFlowCookieName)
		ghLoc, _ := url.Parse(ghLoginRec.Header().Get("Location"))

		// OIDC Cookie 重放进 GitHub 回调：即使两者 secret 相同，purpose 前缀不同也会拒绝。
		reqToGH := httptest.NewRequest(http.MethodGet, githubCallbackPath+"?code=abc&state="+url.QueryEscape(oidcLoc.Query().Get("state")), nil)
		reqToGH.AddCookie(&http.Cookie{Name: GitHubFlowCookieName, Value: oidcCookie.Value})
		recToGH := httptest.NewRecorder()
		handler.ServeHTTP(recToGH, reqToGH)
		if recToGH.Code != http.StatusBadRequest {
			t.Fatalf("OIDC cookie on GitHub callback = %d, want 400", recToGH.Code)
		}
		if cleared := findCookie(recToGH.Result().Cookies(), GitHubFlowCookieName); cleared != nil {
			t.Fatalf("OIDC cookie replay unexpectedly cleared GitHub flow cookie: %+v", cleared)
		}

		// GitHub Cookie 重放进 OIDC 回调：既过不了 HMAC 校验，也缺 nonce。
		reqToOIDC := httptest.NewRequest(http.MethodGet, oidcCallbackPath+"?code=abc&state="+url.QueryEscape(ghLoc.Query().Get("state")), nil)
		reqToOIDC.AddCookie(&http.Cookie{Name: OIDCFlowCookieName, Value: ghCookie.Value})
		recToOIDC := httptest.NewRecorder()
		handler.ServeHTTP(recToOIDC, reqToOIDC)
		if recToOIDC.Code != http.StatusBadRequest {
			t.Fatalf("GitHub cookie on OIDC callback = %d, want 400", recToOIDC.Code)
		}
		if cleared := findCookie(recToOIDC.Result().Cookies(), OIDCFlowCookieName); cleared != nil {
			t.Fatalf("GitHub cookie replay unexpectedly cleared OIDC flow cookie: %+v", cleared)
		}
		if sessions.calls != 0 {
			t.Fatalf("sessions.calls = %d, want 0", sessions.calls)
		}
	})

	t.Run("error mappings and state mismatch preserve flow cookie", func(t *testing.T) {
		sessions := &stubSessionStore{}
		srv, fake := newGitHubTestServer(t, "http://127.0.0.1:8081/api/v1/auth/github/callback", sessions)
		handler := srv.Routes()

		// state 不符返回 400 且不删除正在进行的流程 Cookie。
		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, githubLoginPath, nil))
		flowCookie := findCookie(loginRec.Result().Cookies(), GitHubFlowCookieName)
		cbURL := runAuthorizeAtFake(t, loginRec.Header().Get("Location"))

		badStateReq := httptest.NewRequest(http.MethodGet, githubCallbackPath+"?code=abc&state=wrong", nil)
		badStateReq.AddCookie(flowCookie)
		badStateRec := httptest.NewRecorder()
		handler.ServeHTTP(badStateRec, badStateReq)
		if badStateRec.Code != http.StatusBadRequest || findCookie(badStateRec.Result().Cookies(), GitHubFlowCookieName) != nil {
			t.Fatalf("state mismatch = %d, cookies = %+v", badStateRec.Code, badStateRec.Result().Cookies())
		}

		// 紧接着同一份有效回调仍然能成功。
		okReq := httptest.NewRequest(http.MethodGet, cbURL.RequestURI(), nil)
		okReq.AddCookie(flowCookie)
		okRec := httptest.NewRecorder()
		handler.ServeHTTP(okRec, okReq)
		if okRec.Code != http.StatusOK {
			t.Fatalf("valid callback after mismatch = %d, want 200", okRec.Code)
		}

		errorCases := []struct {
			name       string
			tamper     fakegithub.Tamper
			query      func(state string) string
			wantStatus int
		}{
			{
				name: "provider error param returns 401",
				query: func(state string) string {
					return fmt.Sprintf("%s?error=access_denied&state=%s", githubCallbackPath, url.QueryEscape(state))
				},
				wantStatus: http.StatusUnauthorized,
			},
			{
				name:       "missing code returns 400",
				query:      func(state string) string { return githubCallbackPath + "?state=" + url.QueryEscape(state) },
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "bad_verification_code returns 401",
				tamper:     fakegithub.Tamper{TokenError: "bad_verification_code"},
				wantStatus: http.StatusUnauthorized,
			},
			{
				name:       "incorrect_client_credentials returns 502",
				tamper:     fakegithub.Tamper{TokenError: "incorrect_client_credentials"},
				wantStatus: http.StatusBadGateway,
			},
			{
				name:       "user api 500 returns 502",
				tamper:     fakegithub.Tamper{UserStatus: http.StatusInternalServerError},
				wantStatus: http.StatusBadGateway,
			},
		}
		for _, tc := range errorCases {
			t.Run(tc.name, func(t *testing.T) {
				fake.SetTamper(tc.tamper)
				t.Cleanup(func() { fake.SetTamper(fakegithub.Tamper{}) })

				lRec := httptest.NewRecorder()
				handler.ServeHTTP(lRec, httptest.NewRequest(http.MethodGet, githubLoginPath, nil))
				fCookie := findCookie(lRec.Result().Cookies(), GitHubFlowCookieName)
				loc, _ := url.Parse(lRec.Header().Get("Location"))

				var reqURI string
				if tc.query != nil {
					reqURI = tc.query(loc.Query().Get("state"))
				} else {
					reqURI = runAuthorizeAtFake(t, loc.String()).RequestURI()
				}
				cReq := httptest.NewRequest(http.MethodGet, reqURI, nil)
				cReq.AddCookie(fCookie)
				cRec := httptest.NewRecorder()
				handler.ServeHTTP(cRec, cReq)
				if cRec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d, body = %s", cRec.Code, tc.wantStatus, cRec.Body.String())
				}
			})
		}
	})
}
