package console

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

func selfServiceLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeOrgStore struct {
	createFn func(ctx context.Context, userID int64, name string, maxOwned int) (identity.CreatedOrg, error)
}

func (f *fakeOrgStore) CreateOrgForUser(ctx context.Context, userID int64, name string, maxOwned int) (identity.CreatedOrg, error) {
	return f.createFn(ctx, userID, name, maxOwned)
}

type fakeInvitationStore struct {
	listFn   func(ctx context.Context, tenantID int64) ([]identity.Invitation, error)
	createFn func(ctx context.Context, p identity.CreateInvitationParams) (identity.Invitation, error)
	revokeFn func(ctx context.Context, p identity.RevokeInvitationParams) error
}

func (f *fakeInvitationStore) ListInvitations(ctx context.Context, tenantID int64) ([]identity.Invitation, error) {
	return f.listFn(ctx, tenantID)
}

func (f *fakeInvitationStore) CreateOrgInvitation(ctx context.Context, p identity.CreateInvitationParams) (identity.Invitation, error) {
	return f.createFn(ctx, p)
}

func (f *fakeInvitationStore) RevokeInvitation(ctx context.Context, p identity.RevokeInvitationParams) error {
	return f.revokeFn(ctx, p)
}

type fakeAPIKeyStore struct {
	listFn   func(ctx context.Context, tenantID int64) ([]identity.APIKeyInfo, error)
	issueFn  func(ctx context.Context, p identity.IssueOrgAPIKeyParams) (identity.IssuedOrgAPIKey, error)
	revokeFn func(ctx context.Context, tenantID, keyID int64) error
}

func (f *fakeAPIKeyStore) ListOrgAPIKeys(ctx context.Context, tenantID int64) ([]identity.APIKeyInfo, error) {
	return f.listFn(ctx, tenantID)
}

func (f *fakeAPIKeyStore) IssueOrgAPIKey(ctx context.Context, p identity.IssueOrgAPIKeyParams) (identity.IssuedOrgAPIKey, error) {
	return f.issueFn(ctx, p)
}

func (f *fakeAPIKeyStore) RevokeOrgAPIKey(ctx context.Context, tenantID, keyID int64) error {
	return f.revokeFn(ctx, tenantID, keyID)
}

type selfServiceSessionStore struct {
	userID int64
	role   identity.Role
	orgID  int64
}

func (s *selfServiceSessionStore) AuthenticateSessionForOrg(_ context.Context, token string, orgID int64) (identity.Principal, error) {
	if !identity.SessionToken.Valid(token) {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	if orgID != s.orgID {
		return identity.Principal{}, identity.ErrOrgNotFound
	}
	return identity.Principal{
		Tenant:    tenant.Tenant{ID: orgID},
		ActorKind: identity.ActorUser,
		ActorID:   s.userID,
		Role:      s.role,
	}, nil
}

func (s *selfServiceSessionStore) CreateSessionForIdentity(_ context.Context, id identity.ExternalIdentity, ttl time.Duration) (identity.IssuedSession, error) {
	tok, _, _ := identity.SessionToken.Generate()
	now := time.Now().UTC()
	return identity.IssuedSession{
		User:         identity.User{ID: s.userID, Email: id.Email, Name: id.Name},
		Session:      identity.Session{ID: 1, UserID: s.userID, CreatedAt: now, ExpiresAt: now.Add(ttl)},
		Token:        tok,
		CookieMaxAge: ttl,
	}, nil
}

func (s *selfServiceSessionStore) GetSessionInfo(_ context.Context, token string) (identity.SessionInfo, error) {
	if !identity.SessionToken.Valid(token) {
		return identity.SessionInfo{}, identity.ErrUnauthenticated
	}
	return identity.SessionInfo{
		User:      identity.User{ID: s.userID, Email: "alice@example.com", Name: "Alice"},
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, nil
}

func (s *selfServiceSessionStore) RevokeSession(context.Context, string) error { return nil }

type selfServiceOIDC struct{}

func (selfServiceOIDC) AuthCodeURL(state, _, _ string) string {
	return "https://idp.example/auth?state=" + state
}
func (selfServiceOIDC) ExchangeAndVerify(context.Context, string, string, string) (identity.ExternalIdentity, error) {
	return identity.ExternalIdentity{Issuer: "https://idp.example", Subject: "sub-1", Email: "alice@example.com", EmailVerified: true}, nil
}
func (selfServiceOIDC) SecureCookies() bool   { return false }
func (selfServiceOIDC) FlowCookieKey() []byte { return []byte("0123456789abcdef0123456789abcdef") }

type selfServiceAPIKeyAuth struct {
	scopes []identity.Perm
}

func (a selfServiceAPIKeyAuth) Authenticate(context.Context, string) (identity.Principal, error) {
	return identity.Principal{
		Tenant:    tenant.Tenant{ID: 1},
		ActorKind: identity.ActorAPIKey,
		ActorID:   99,
		Scopes:    a.scopes,
	}, nil
}

func TestAuthProvidersAndReturnTo(t *testing.T) {
	sess := &selfServiceSessionStore{userID: 7, role: identity.RoleOwner, orgID: 1}
	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:    identity.NewMapAuthorizer(),
		OIDC:          selfServiceOIDC{},
		Sessions:      sess,
		PublicBaseURL: "https://s.example.com",
	})
	h := srv.Routes()

	// 1. /api/v1/auth/providers 反映启用的提供方与公开短链基地址。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /providers = %d, want 200", rec.Code)
	}
	var prov authProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &prov); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	if !prov.OIDC || prov.GitHub || prov.PublicBaseURL != "https://s.example.com" {
		t.Fatalf("providers = %+v, want {OIDC:true GitHub:false PublicBaseURL:https://s.example.com}", prov)
	}

	// 2. 非法 return_to（含路径穿越）返回 400。
	for _, bad := range []string{"https://evil.com", "//evil.com", `/\evil`, "/api/v1/me", "/r/abc12345", "/x/../api/v1/me", "/./r/abc"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return_to="+bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("return_to=%q status = %d, want 400", bad, rec.Code)
		}
	}

	// 3. 合法 return_to=/links 经由签名 flow cookie 传递，回调返回 302 Location: /links。
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return_to=/links", nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginRec.Code)
	}
	loc := loginRec.Header().Get("Location")
	_, state, _ := strings.Cut(loc, "state=")
	var flowCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == OIDCFlowCookieName {
			flowCookie = c
		}
	}
	if flowCookie == nil {
		t.Fatal("missing flow cookie")
	}

	cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?state="+state+"&code=ok", nil)
	cbReq.AddCookie(flowCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusFound {
		t.Fatalf("callback with return_to status = %d, want 302 (body=%s)", cbRec.Code, cbRec.Body.String())
	}
	if got := cbRec.Header().Get("Location"); got != "/links" {
		t.Errorf("callback Location = %q, want /links", got)
	}
}

func TestCreateOrgHandler(t *testing.T) {
	sess := &selfServiceSessionStore{userID: 7, role: identity.RoleOwner, orgID: 1}
	orgs := &fakeOrgStore{
		createFn: func(_ context.Context, userID int64, name string, _ int) (identity.CreatedOrg, error) {
			if userID != 7 {
				t.Errorf("userID = %d, want 7", userID)
			}
			if name == "limit" {
				return identity.CreatedOrg{}, identity.ErrOrgLimitExceeded
			}
			return identity.CreatedOrg{ID: 42, Name: name, Role: identity.RoleOwner, CreatedAt: time.Now().UTC()}, nil
		},
	}
	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Keys:       selfServiceAPIKeyAuth{scopes: identity.AllAPIKeyScopes()},
		Authorizer: identity.NewMapAuthorizer(),
		Sessions:   sess,
		Orgs:       orgs,
	})
	h := srv.Routes()
	sessionTok, _, _ := identity.SessionToken.Generate()
	apiKeyTok, _, _ := identity.APIKeyToken.Generate()

	// 1. API Key 不允许创建组织。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orgs", strings.NewReader(`{"name":"Acme"}`))
	req.Header.Set("Authorization", "Bearer "+apiKeyTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("API Key POST /orgs = %d, want 401", rec.Code)
	}

	// 2. 跨站会话写请求被 CSRF 拦截。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orgs", strings.NewReader(`{"name":"Acme"}`))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST /orgs = %d, want 403", rec.Code)
	}

	// 3. 合法会话不带 X-Org-ID 创建成功。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orgs", strings.NewReader(`{"name":"Acme"}`))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /orgs = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	// 4. 超上限返回 409。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orgs", strings.NewReader(`{"name":"limit"}`))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("over limit POST /orgs = %d, want 409", rec.Code)
	}
}

func TestInvitationsAndAPIKeysPermissions(t *testing.T) {
	sessionTok, _, _ := identity.SessionToken.Generate()
	apiKeyTok, _, _ := identity.APIKeyToken.Generate()

	invStore := &fakeInvitationStore{
		listFn: func(context.Context, int64) ([]identity.Invitation, error) {
			return []identity.Invitation{{ID: 1, TenantID: 1, Email: "bob@example.com", Role: identity.RoleMember, CreatedAt: time.Now().UTC()}}, nil
		},
		createFn: func(_ context.Context, p identity.CreateInvitationParams) (identity.Invitation, error) {
			return identity.Invitation{ID: 2, TenantID: p.TenantID, Email: p.Email, Role: p.Role, InvitedBy: &p.InvitedBy, CreatedAt: time.Now().UTC()}, nil
		},
		revokeFn: func(context.Context, identity.RevokeInvitationParams) error { return nil },
	}
	keyStore := &fakeAPIKeyStore{
		listFn: func(context.Context, int64) ([]identity.APIKeyInfo, error) {
			return []identity.APIKeyInfo{{ID: 5, TenantID: 1, Scopes: identity.AllAPIKeyScopes(), CreatedAt: time.Now().UTC()}}, nil
		},
		issueFn: func(_ context.Context, p identity.IssueOrgAPIKeyParams) (identity.IssuedOrgAPIKey, error) {
			return identity.IssuedOrgAPIKey{
				Key:       identity.APIKeyInfo{ID: 6, TenantID: p.TenantID, Scopes: p.Scopes, CreatedBy: &p.CreatedBy, CreatedAt: time.Now().UTC()},
				Plaintext: apiKeyTok,
			}, nil
		},
		revokeFn: func(_ context.Context, _, id int64) error {
			if id == 404 {
				return identity.ErrAPIKeyNotFound
			}
			return nil
		},
	}

	makeHandler := func(role identity.Role) http.Handler {
		return New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
			Keys:        selfServiceAPIKeyAuth{scopes: identity.AllAPIKeyScopes()},
			Authorizer:  identity.NewMapAuthorizer(),
			Sessions:    &selfServiceSessionStore{userID: 7, role: role, orgID: 1},
			Invitations: invStore,
			APIKeys:     keyStore,
		}).Routes()
	}

	// admin 不能邀请 owner（403），可以邀请 member（201）和签发/吊销 API Key（201/204）。
	hAdmin := makeHandler(identity.RoleAdmin)
	doUser := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
		req.Header.Set("X-Org-ID", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := doUser(hAdmin, http.MethodPost, "/api/v1/invitations", `{"email":"boss@example.com","role":"owner"}`); rec.Code != http.StatusForbidden {
		t.Errorf("admin invite owner = %d, want 403", rec.Code)
	}
	if rec := doUser(hAdmin, http.MethodPost, "/api/v1/invitations", `{"email":"dev@example.com","role":"member"}`); rec.Code != http.StatusCreated {
		t.Errorf("admin invite member = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if rec := doUser(hAdmin, http.MethodDelete, "/api/v1/invitations/2", ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin revoke invitation = %d, want 204", rec.Code)
	}

	issueRec := doUser(hAdmin, http.MethodPost, "/api/v1/api-keys", `{"scopes":["links:read"]}`)
	if issueRec.Code != http.StatusCreated || !strings.Contains(issueRec.Body.String(), `"api_key":"`+apiKeyTok+`"`) {
		t.Errorf("admin issue api key = %d body=%s", issueRec.Code, issueRec.Body.String())
	}
	if issueRec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("issue api key Cache-Control = %q, want no-store", issueRec.Header().Get("Cache-Control"))
	}
	if rec := doUser(hAdmin, http.MethodDelete, "/api/v1/api-keys/6", ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin revoke api key = %d, want 204", rec.Code)
	}
	if rec := doUser(hAdmin, http.MethodDelete, "/api/v1/api-keys/404", ""); rec.Code != http.StatusNotFound {
		t.Errorf("revoke missing api key = %d, want 404", rec.Code)
	}

	// member 不能查看或签发 API Key（403），也不能发邀请（403），但可以看邀请列表（200）。
	hMember := makeHandler(identity.RoleMember)
	if rec := doUser(hMember, http.MethodGet, "/api/v1/invitations", ""); rec.Code != http.StatusOK {
		t.Errorf("member list invitations = %d, want 200", rec.Code)
	}
	if rec := doUser(hMember, http.MethodPost, "/api/v1/invitations", `{"email":"x@example.com","role":"viewer"}`); rec.Code != http.StatusForbidden {
		t.Errorf("member create invitation = %d, want 403", rec.Code)
	}
	if rec := doUser(hMember, http.MethodGet, "/api/v1/api-keys", ""); rec.Code != http.StatusForbidden {
		t.Errorf("member list api keys = %d, want 403", rec.Code)
	}

	// API Key 不能操作 /api/v1/api-keys（403）。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/api-keys", strings.NewReader(`{"scopes":["links:read"]}`))
	req.Header.Set("Authorization", "Bearer "+apiKeyTok)
	rec := httptest.NewRecorder()
	hAdmin.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("API Key issue api key = %d, want 403", rec.Code)
	}
}

func TestWebSPAHandlerIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><html><body>console-app</body></html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o750); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('ok')"), 0o600); err != nil {
		t.Fatalf("write app.js: %v", err)
	}

	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer: identity.NewMapAuthorizer(),
		WebDistDir: dir,
	})
	h := srv.Routes()

	// 首页与前端路由回退到 index.html。
	for _, p := range []string{"/", "/links", "/members"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console-app") {
			t.Errorf("GET %s = %d %q, want 200 index.html", p, rec.Code, rec.Body.String())
		}
	}

	// 静态资源直接返回并带长缓存。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Errorf("GET /assets/app.js = %d %q", rec.Code, rec.Body.String())
	}

	// /r/* 与不存在的 /api/* 绝不回退到 index.html，保持 404 路由隔离。
	for _, reserved := range []string{"/r/AbC12345", "/api/v1/does-not-exist", "/api/unknown"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, reserved, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (must not fallback to SPA)", reserved, rec.Code)
		}
	}
}
