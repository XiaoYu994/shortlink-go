package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/httpapi"
	"shortlink/internal/identity"
	"shortlink/internal/identity/fakeoidc"
)

/*
TestHTTPListCursorRoundTrip 把列表游标从 HTTP 响应送回下一次查询。
编码在 httpapi，排序在 SQL。两边分开测时，游标格式对不上也会全绿。
*/
func TestHTTPListCursorRoundTrip(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	first := createLink(t, store, tenantID, "https://example.com/cursor-1")
	second := createLink(t, store, tenantID, "https://example.com/cursor-2")
	third := createLink(t, store, tenantID, "https://example.com/cursor-3")
	setClicks(t, store, first.ID, 1)
	setClicks(t, store, second.ID, 5)
	setClicks(t, store, third.ID, 3)
	setTodayClicks(t, store, tenantID, first.ID, 4)
	setTodayClicks(t, store, tenantID, second.ID, 2)
	setTodayClicks(t, store, tenantID, third.ID, 9)

	issued, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID, Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	handler := newConsoleHandler(store)

	tests := []struct {
		sort string
		want []string
	}{
		{sort: "id", want: []string{third.Code, second.Code, first.Code}},
		{sort: "clicks", want: []string{second.Code, third.Code, first.Code}},
		{sort: "clicks_today", want: []string{third.Code, first.Code, second.Code}},
	}
	for _, tt := range tests {
		t.Run(tt.sort, func(t *testing.T) {
			got := listAllCodes(t, handler, issued.Plaintext, tt.sort)
			if len(got) != len(tt.want) {
				t.Fatalf("codes = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("codes = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

/*
TestHTTPRejectsExpiredAndRevokedAPIKeys 让过期和吊销的密钥走完管理路由。
Authenticate 已经覆盖这两种状态；这里确认中间件把它们写成 401，并且没有创建链接。
*/
func TestHTTPRejectsExpiredAndRevokedAPIKeys(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	handler := newConsoleHandler(store)

	expired, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID, Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.Key.ID); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	assertUnauthorizedCreate(t, handler, expired.Plaintext)

	revoked, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID, Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if err := store.RevokeAPIKey(ctx, revoked.Key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	assertUnauthorizedCreate(t, handler, revoked.Plaintext)

	if n := countLinks(t, store); n != 0 {
		t.Fatalf("links = %d, want 0 after rejected creates", n)
	}

	valid, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID, Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	rec := postCreate(handler, valid.Plaintext)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid key status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if n := countLinks(t, store); n != 1 {
		t.Fatalf("links = %d, want 1", n)
	}
}

/*
TestHTTPAPIKeyScopesEnforced 端到端验证数据库里的 api_keys.scopes 经过
Authenticate、Principal 和 MapAuthorizer 真正约束每个管理端点。
*/
func TestHTTPAPIKeyScopesEnforced(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := context.Background()
	existing := createLink(t, store, tenantID, "https://example.com/scoped")
	handler := newConsoleHandler(store)
	// 默认每秒 10 次。这里连续打完整张权限表，测的是 scope，不是限流。
	if _, err := store.db.ExecContext(ctx, `UPDATE tenants SET rate_limit_per_second = 0 WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("disable rate limit: %v", err)
	}

	readOnly, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: tenantID,
		Scopes:   []identity.Perm{identity.PermLinksRead},
	})
	if err != nil {
		t.Fatalf("IssueAPIKey readOnly: %v", err)
	}
	statsOnly, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: tenantID,
		Scopes:   []identity.Perm{identity.PermStatsRead},
	})
	if err != nil {
		t.Fatalf("IssueAPIKey statsOnly: %v", err)
	}
	writeOnly, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: tenantID,
		Scopes:   []identity.Perm{identity.PermLinksWrite},
	})
	if err != nil {
		t.Fatalf("IssueAPIKey writeOnly: %v", err)
	}

	tests := []struct {
		name       string
		token      string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "readOnly list ok", token: readOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links", wantStatus: http.StatusOK},
		{name: "readOnly detail ok", token: readOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links/" + existing.Code, wantStatus: http.StatusOK},
		{name: "readOnly create forbidden", token: readOnly.Plaintext, method: http.MethodPost, path: "/api/v1/links", body: `{"url":"https://example.com/blocked"}`, wantStatus: http.StatusForbidden},
		{name: "readOnly update forbidden", token: readOnly.Plaintext, method: http.MethodPatch, path: "/api/v1/links/" + existing.Code, body: `{"disabled":true}`, wantStatus: http.StatusForbidden},
		{name: "readOnly delete forbidden", token: readOnly.Plaintext, method: http.MethodDelete, path: "/api/v1/links/" + existing.Code, wantStatus: http.StatusForbidden},
		{name: "readOnly stats forbidden", token: readOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links/" + existing.Code + "/stats", wantStatus: http.StatusForbidden},
		{name: "statsOnly stats ok", token: statsOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links/" + existing.Code + "/stats", wantStatus: http.StatusOK},
		{name: "statsOnly list forbidden", token: statsOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links", wantStatus: http.StatusForbidden},
		{name: "writeOnly create ok", token: writeOnly.Plaintext, method: http.MethodPost, path: "/api/v1/links", body: `{"url":"https://example.com/write-only"}`, wantStatus: http.StatusCreated},
		{name: "writeOnly list forbidden", token: writeOnly.Plaintext, method: http.MethodGet, path: "/api/v1/links", wantStatus: http.StatusForbidden},
		{name: "writeOnly update hides link", token: writeOnly.Plaintext, method: http.MethodPatch, path: "/api/v1/links/" + existing.Code, body: `{"url":"https://example.com/write-only-updated"}`, wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+tt.token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusForbidden && rec.Body.String() != `{"error":"forbidden"}` {
				t.Errorf("body = %q, want forbidden", rec.Body.String())
			}
			if tt.wantStatus == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
		})
	}

	detail := httptest.NewRequest(http.MethodGet, "/api/v1/links/"+existing.Code, nil)
	detail.Header.Set("Authorization", "Bearer "+readOnly.Plaintext)
	detailRec := httptest.NewRecorder()
	handler.ServeHTTP(detailRec, detail)
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "https://example.com/write-only-updated") {
		t.Fatalf("read back status = %d, body = %s", detailRec.Code, detailRec.Body.String())
	}

	if n := countLinks(t, store); n != 2 {
		t.Fatalf("links = %d, want 2 (existing + writeOnly create)", n)
	}
}

/*
TestHTTPOIDCLoginPersistsSession 端到端串联 fakeoidc、identity.OIDCProvider、
httpapi 登录/回调路由和真实 PostgreSQL 存储，验证首次与重复登录落库和会话 Cookie 哈希一致。
*/
func TestHTTPOIDCLoginPersistsSession(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	const (
		clientID     = "shortlink-console"
		clientSecret = "shortlink-console-secret"
		redirectURL  = "http://127.0.0.1:8081/api/v1/auth/callback"
	)
	fake := fakeoidc.Start(t, clientID, clientSecret)
	provider, err := identity.NewOIDCProvider(ctx, identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpapi.NewConsole(logger, store, 5*time.Second, prometheus.NewRegistry(), httpapi.ConsoleOptions{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
		OIDC:       provider,
		Sessions:   store,
	})
	handler := srv.ConsoleRoutes()

	completeLogin := func() (token string, userID int64) {
		t.Helper()
		loginRec := httptest.NewRecorder()
		handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil))
		if loginRec.Code != http.StatusFound {
			t.Fatalf("login status = %d, want 302", loginRec.Code)
		}
		var flowCookie *http.Cookie
		for _, c := range loginRec.Result().Cookies() {
			if c.Name == httpapi.OIDCFlowCookieName {
				flowCookie = c
			}
		}
		if flowCookie == nil {
			t.Fatal("missing flow cookie")
		}

		noRedirect := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		authResp, err := noRedirect.Get(loginRec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("GET fake authorize: %v", err)
		}
		defer func() { _ = authResp.Body.Close() }()
		if authResp.StatusCode != http.StatusFound {
			t.Fatalf("fake authorize status = %d, want 302", authResp.StatusCode)
		}
		cbURL, err := url.Parse(authResp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("parse callback url: %v", err)
		}

		cbReq := httptest.NewRequest(http.MethodGet, cbURL.RequestURI(), nil)
		cbReq.AddCookie(flowCookie)
		cbRec := httptest.NewRecorder()
		handler.ServeHTTP(cbRec, cbReq)
		if cbRec.Code != http.StatusOK {
			t.Fatalf("callback status = %d, want 200, body = %s", cbRec.Code, cbRec.Body.String())
		}

		var sessCookie *http.Cookie
		for _, c := range cbRec.Result().Cookies() {
			if c.Name == httpapi.SessionCookieName {
				sessCookie = c
			}
		}
		if sessCookie == nil {
			t.Fatal("missing session cookie")
		}
		var resp struct {
			User struct {
				ID    int64  `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
		}
		if err := json.Unmarshal(cbRec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode callback response: %v", err)
		}
		if resp.User.Email != "alice@example.com" {
			t.Errorf("email = %q, want alice@example.com", resp.User.Email)
		}
		return sessCookie.Value, resp.User.ID
	}

	firstToken, firstUserID := completeLogin()
	secondToken, secondUserID := completeLogin()
	if firstUserID != secondUserID {
		t.Fatalf("second login user id = %d, want %d", secondUserID, firstUserID)
	}
	if firstToken == secondToken {
		t.Fatal("second login reused the same session token")
	}

	var storedHash []byte
	var storedUserID int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT user_id, token_hash FROM sessions WHERE token_hash = $1`,
		identity.HashSessionToken(firstToken),
	).Scan(&storedUserID, &storedHash); err != nil {
		t.Fatalf("lookup first session by hash: %v", err)
	}
	if storedUserID != firstUserID || !bytes.Equal(storedHash, identity.HashSessionToken(firstToken)) {
		t.Fatalf("stored session mismatch: user=%d", storedUserID)
	}

	var usersCount, sessionsCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&usersCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionsCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if usersCount != 1 || sessionsCount != 2 {
		t.Fatalf("counts (users=%d, sessions=%d), want (1, 2)", usersCount, sessionsCount)
	}
}

/*
TestHTTPSessionAuthOrgSwitchAndLogoutImmediateEffect 端到端验证：
1. 登录但未加入组织时 /me 返回空组织数组，携带 X-Org-ID 调管理 API 返回 404；
2. 加入多组织后通过 X-Org-ID 在不同组织与角色间切换；
3. 改角色、移出组织与登出后，同一个会话的下一次请求立刻生效；
4. 带会话 Cookie 的跨站写请求被 CrossOriginProtection 拒绝。
*/
func TestHTTPSessionAuthOrgSwitchAndLogoutImmediateEffect(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, rate_limit_per_second) VALUES ('org-two', 0) RETURNING id`,
	).Scan(&tenant2); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tenants SET rate_limit_per_second = 0 WHERE id = $1`, tenant1); err != nil {
		t.Fatalf("disable tenant1 rate limit: %v", err)
	}

	const (
		clientID     = "shortlink-console"
		clientSecret = "shortlink-console-secret"
		redirectURL  = "http://127.0.0.1:8081/api/v1/auth/callback"
	)
	fake := fakeoidc.Start(t, clientID, clientSecret)
	provider, err := identity.NewOIDCProvider(ctx, identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpapi.NewConsole(logger, store, 5*time.Second, prometheus.NewRegistry(), httpapi.ConsoleOptions{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
		OIDC:       provider,
		Sessions:   store,
	})
	handler := srv.ConsoleRoutes()

	// 1. 走完 OIDC 登录拿到会话 Cookie。
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil))
	var flowCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == httpapi.OIDCFlowCookieName {
			flowCookie = c
		}
	}
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	authResp, err := noRedirect.Get(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("GET fake authorize: %v", err)
	}
	defer func() { _ = authResp.Body.Close() }()
	cbURL, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse callback url: %v", err)
	}
	cbReq := httptest.NewRequest(http.MethodGet, cbURL.RequestURI(), nil)
	cbReq.AddCookie(flowCookie)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body = %s", cbRec.Code, cbRec.Body.String())
	}
	var sessCookie *http.Cookie
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == httpapi.SessionCookieName {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("missing session cookie")
	}
	var loginBody struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(cbRec.Body.Bytes(), &loginBody); err != nil {
		t.Fatalf("decode callback body: %v", err)
	}
	userID := loginBody.User.ID

	doWithSession := func(method, path string, orgID int64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(sessCookie)
		if orgID > 0 {
			req.Header.Set("X-Org-ID", strconv.FormatInt(orgID, 10))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 2. 无成员关系时 /me 返回空组织数组，管理 API 返回 404。
	meRec := doWithSession(http.MethodGet, "/api/v1/me", 0, "")
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), `"organizations":[]`) {
		t.Fatalf("/me before membership = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list before membership = %d, want 404", rec.Code)
	}

	// 3. 加入 tenant1(viewer) 和 tenant2(admin)，验证 /me 和 X-Org-ID 跨组织切换。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'viewer'), ($1, $3, 'admin')`,
		userID, tenant1, tenant2,
	); err != nil {
		t.Fatalf("insert memberships: %v", err)
	}
	meRec = doWithSession(http.MethodGet, "/api/v1/me", 0, "")
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), `"role":"viewer"`) || !strings.Contains(meRec.Body.String(), `"role":"admin"`) {
		t.Fatalf("/me with memberships = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer list tenant1 = %d, want 200", rec.Code)
	}
	if rec := doWithSession(http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/t1-denied"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create tenant1 = %d, want 403", rec.Code)
	}
	createT2 := doWithSession(http.MethodPost, "/api/v1/links", tenant2, `{"url":"https://example.com/t2-ok"}`)
	if createT2.Code != http.StatusCreated {
		t.Fatalf("admin create tenant2 = %d, want 201, body = %s", createT2.Code, createT2.Body.String())
	}

	// 4. 改角色为 member，同一个会话的下一次请求立刻放行；跨站 POST 仍被拒绝。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memberships SET role = 'member' WHERE user_id = $1 AND tenant_id = $2`, userID, tenant1,
	); err != nil {
		t.Fatalf("promote role: %v", err)
	}
	if rec := doWithSession(http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/t1-ok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("member create tenant1 after promotion = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/csrf"}`))
	csrfReq.AddCookie(sessCookie)
	csrfReq.Header.Set("X-Org-ID", strconv.FormatInt(tenant1, 10))
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfRec := httptest.NewRecorder()
	handler.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site create = %d, want 403", csrfRec.Code)
	}

	// 5. 移出 tenant1，同一个会话的下一次请求立刻 404，tenant2 仍可用。
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM memberships WHERE user_id = $1 AND tenant_id = $2`, userID, tenant1,
	); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list tenant1 after removal = %d, want 404", rec.Code)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant2, ""); rec.Code != http.StatusOK {
		t.Fatalf("list tenant2 while still member = %d, want 200", rec.Code)
	}

	// 6. 登出后，同一个会话的下一次 /me 和管理 API 请求立刻 401。
	logoutRec := doWithSession(http.MethodPost, "/api/v1/auth/logout", 0, "")
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204, body = %s", logoutRec.Code, logoutRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/me", 0, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/me after logout = %d, want 401", rec.Code)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant2, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list after logout = %d, want 401", rec.Code)
	}
}

func newConsoleHandler(store *Store) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpapi.NewConsole(logger, store, 5*time.Second, prometheus.NewRegistry(), httpapi.ConsoleOptions{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
	})
	return srv.ConsoleRoutes()
}

func postCreate(handler http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/from-key"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertUnauthorizedCreate(t *testing.T, handler http.Handler, token string) {
	t.Helper()
	rec := postCreate(handler, token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", rec.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Errorf("body = %s, want unauthorized", rec.Body.String())
	}
}

func countLinks(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM links`).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

/*
listAllCodes 用 limit=1 逐页跟 next_cursor，直到没有下一页。
游标原样放进 query，服务端按 sort 解码后再交给 SQL。
*/
func listAllCodes(t *testing.T, handler http.Handler, token, sort string) []string {
	t.Helper()
	var (
		codes  []string
		cursor string
	)
	for page := 0; page < 5; page++ {
		query := url.Values{}
		query.Set("limit", "1")
		query.Set("sort", sort)
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/links?"+query.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, body = %s", page, rec.Code, rec.Body.String())
		}
		var resp struct {
			Items []struct {
				Code string `json:"code"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode page %d: %v", page, err)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("page %d items = %d, want 1", page, len(resp.Items))
		}
		codes = append(codes, resp.Items[0].Code)
		if resp.NextCursor == "" {
			return codes
		}
		if resp.NextCursor == cursor {
			t.Fatalf("page %d cursor %q did not advance", page, cursor)
		}
		cursor = resp.NextCursor
	}
	t.Fatal("list did not end")
	return nil
}
