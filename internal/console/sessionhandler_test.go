package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/identity/fakegithub"
)

func TestLogout(t *testing.T) {
	validToken := "sls_" + strings.Repeat("12", 32)

	t.Run("revokes session and clears cookie", func(t *testing.T) {
		sessions := &stubSessionStore{}
		reg := newTestRegistry()
		srv, _ := newOIDCTestServerWithRegistry(t, "https://console.example.com/api/v1/auth/oidc/callback", sessions, reg)
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
			srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
			srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", &stubSessionStore{})
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
		srv, _ := newOIDCTestServerWithRegistry(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions, reg)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
		srv, _ := newOIDCTestServer(t, "http://127.0.0.1:8081/api/v1/auth/oidc/callback", sessions)
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
