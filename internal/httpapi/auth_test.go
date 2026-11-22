package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/link"
	"shortlink/internal/memorystore"
	"shortlink/internal/tenant"
)

func TestAuthenticate(t *testing.T) {
	const otherKey = "slk_ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	scopedPrincipal := identity.Principal{
		Tenant:    tenant.Tenant{ID: 7},
		ActorKind: identity.ActorAPIKey,
		ActorID:   42,
		Scopes:    []identity.Perm{identity.PermLinksRead, identity.PermStatsRead},
	}
	tests := []struct {
		name          string
		header        string
		keys          APIKeyAuthenticator
		wantStatus    int
		wantPrincipal identity.Principal
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "basic", header: "Basic " + testAPIKey, wantStatus: http.StatusUnauthorized},
		{name: "malformed", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "unknown", header: "Bearer " + otherKey, wantStatus: http.StatusUnauthorized},
		{
			name:       "ok",
			header:     "Bearer " + testAPIKey,
			wantStatus: http.StatusNoContent,
			wantPrincipal: identity.Principal{
				Tenant:    tenant.Tenant{ID: 1},
				ActorKind: identity.ActorAPIKey,
				ActorID:   1,
				Scopes:    identity.AllAPIKeyScopes(),
			},
		},
		{
			name:       "lowercase scheme",
			header:     "bearer " + testAPIKey,
			wantStatus: http.StatusNoContent,
			wantPrincipal: identity.Principal{
				Tenant:    tenant.Tenant{ID: 1},
				ActorKind: identity.ActorAPIKey,
				ActorID:   1,
				Scopes:    identity.AllAPIKeyScopes(),
			},
		},
		{
			name:          "preserves actor id and subset scopes",
			header:        "Bearer " + testAPIKey,
			keys:          principalKey{testAPIKey: scopedPrincipal},
			wantStatus:    http.StatusNoContent,
			wantPrincipal: scopedPrincipal,
		},
		{
			name:       "db error",
			header:     "Bearer " + testAPIKey,
			keys:       failingKey{err: errors.New("db down")},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:   "user principal rejected",
			header: "Bearer " + testAPIKey,
			keys: principalKey{testAPIKey: {
				Tenant:    tenant.Tenant{ID: 1},
				ActorKind: identity.ActorUser,
				Role:      identity.RoleOwner,
			}},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:   "member scope rejected",
			header: "Bearer " + testAPIKey,
			keys: principalKey{testAPIKey: {
				Tenant:    tenant.Tenant{ID: 1},
				ActorKind: identity.ActorAPIKey,
				Scopes:    []identity.Perm{identity.PermLinksRead, identity.PermMembersWrite},
			}},
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := tt.keys
			if keys == nil {
				keys = allowKey{}
			}
			var got identity.Principal
			var saw bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, saw = principalFrom(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			srv := newServer(serverDeps{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: memorystore.New(), requestTimeout: 5 * time.Second, registry: newTestRegistry(), keys: keys})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/links", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			srv.authenticate(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want Bearer", got)
				}
				if !strings.Contains(rec.Body.String(), "unauthorized") {
					t.Errorf("body = %s, want unauthorized", rec.Body.String())
				}
			}
			if tt.wantStatus == http.StatusNoContent {
				if !saw || got.Tenant != tt.wantPrincipal.Tenant ||
					got.ActorKind != tt.wantPrincipal.ActorKind ||
					got.ActorID != tt.wantPrincipal.ActorID ||
					!slices.Equal(got.Scopes, tt.wantPrincipal.Scopes) {
					t.Errorf("principal = %+v, saw = %v, want %+v", got, saw, tt.wantPrincipal)
				}
			}
		})
	}
}

func TestCreateAssignsTenantFromAPIKey(t *testing.T) {
	mem := memorystore.New()
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), mem, 5*time.Second, newTestRegistry(), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/tenant"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	srv.ConsoleRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := mem.GetByCode(context.Background(), resp.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.TenantID != 1 {
		t.Errorf("TenantID = %d, want 1", got.TenantID)
	}
}

func TestCreateWithoutAPIKeyDoesNotStore(t *testing.T) {
	store := &countingStore{}
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), nil)
	rec := httptest.NewRecorder()
	srv.ConsoleRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if store.creates != 0 {
		t.Errorf("creates = %d, want 0", store.creates)
	}
}

type countingStore struct{ creates int }

func (s *countingStore) Create(context.Context, *link.Link) error {
	s.creates++
	return nil
}

func (s *countingStore) GetByCode(context.Context, string) (*link.Link, error) {
	return nil, link.ErrNotFound
}

type failingKey struct{ err error }

func (f failingKey) Authenticate(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, f.err
}

type principalKey map[string]identity.Principal

func (k principalKey) Authenticate(_ context.Context, token string) (identity.Principal, error) {
	p, ok := k[token]
	if !ok {
		return identity.Principal{}, tenant.ErrUnauthenticated
	}
	return p, nil
}

type stubAuthorizer struct {
	allow bool
	perms []identity.Perm
}

func (a *stubAuthorizer) Can(_ identity.Principal, perm identity.Perm) bool {
	a.perms = append(a.perms, perm)
	return a.allow
}

func TestRequirePerm(t *testing.T) {
	tests := []struct {
		name       string
		authz      Authorizer
		principal  *identity.Principal
		perm       identity.Perm
		wantStatus int
		wantNext   bool
		wantBody   string
	}{
		{
			name:       "viewer can read links",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleViewer},
			perm:       identity.PermLinksRead,
			wantStatus: http.StatusNoContent,
			wantNext:   true,
		},
		{
			name:       "viewer cannot write links",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleViewer},
			perm:       identity.PermLinksWrite,
			wantStatus: http.StatusForbidden,
			wantBody:   `{"error":"forbidden"}`,
		},
		{
			name:       "member can write links",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleMember},
			perm:       identity.PermLinksWrite,
			wantStatus: http.StatusNoContent,
			wantNext:   true,
		},
		{
			name:       "member cannot write members",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleMember},
			perm:       identity.PermMembersWrite,
			wantStatus: http.StatusForbidden,
			wantBody:   `{"error":"forbidden"}`,
		},
		{
			name:       "admin can write members",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleAdmin},
			perm:       identity.PermMembersWrite,
			wantStatus: http.StatusNoContent,
			wantNext:   true,
		},
		{
			name:       "owner can write members",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleOwner},
			perm:       identity.PermMembersWrite,
			wantStatus: http.StatusNoContent,
			wantNext:   true,
		},
		{
			name:       "readonly api key can read links",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorAPIKey, Scopes: []identity.Perm{identity.PermLinksRead}},
			perm:       identity.PermLinksRead,
			wantStatus: http.StatusNoContent,
			wantNext:   true,
		},
		{
			name:       "readonly api key cannot write links",
			authz:      identity.NewMapAuthorizer(),
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorAPIKey, Scopes: []identity.Perm{identity.PermLinksRead}},
			perm:       identity.PermLinksWrite,
			wantStatus: http.StatusForbidden,
			wantBody:   `{"error":"forbidden"}`,
		},
		{
			name:       "missing principal fails closed with 500",
			authz:      identity.NewMapAuthorizer(),
			perm:       identity.PermLinksRead,
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"internal error"}`,
		},
		{
			name:       "nil authorizer fails closed with 500",
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleOwner},
			perm:       identity.PermLinksRead,
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"internal error"}`,
		},
		{
			name:       "custom authorizer deny skips next",
			authz:      &stubAuthorizer{allow: false},
			principal:  &identity.Principal{Tenant: tenant.Tenant{ID: 1}, ActorKind: identity.ActorUser, Role: identity.RoleOwner},
			perm:       identity.PermLinksRead,
			wantStatus: http.StatusForbidden,
			wantBody:   `{"error":"forbidden"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			srv := newServer(serverDeps{
				logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:          memorystore.New(),
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				authorizer:     tt.authz,
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
			if tt.principal != nil {
				req = req.WithContext(withPrincipal(req.Context(), *tt.principal))
			}
			rec := httptest.NewRecorder()
			srv.requirePerm(tt.perm, next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != tt.wantNext {
				t.Errorf("next called = %v, want %v", called, tt.wantNext)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestConsoleRoutePermissions(t *testing.T) {
	tests := []struct {
		method    string
		path      string
		body      string
		wantPerm  identity.Perm
		wantAllow []identity.Perm
		wantOK    int
	}{
		{
			method:   http.MethodPost,
			path:     "/api/v1/links",
			body:     `{"url":"https://example.com/authz"}`,
			wantPerm: identity.PermLinksWrite,
			wantOK:   http.StatusCreated,
		},
		{
			method:   http.MethodGet,
			path:     "/api/v1/links",
			wantPerm: identity.PermLinksRead,
			wantOK:   http.StatusOK,
		},
		{
			method:   http.MethodGet,
			path:     "/api/v1/links/AbC123xy",
			wantPerm: identity.PermLinksRead,
			wantOK:   http.StatusOK,
		},
		{
			method:    http.MethodPatch,
			path:      "/api/v1/links/AbC123xy",
			body:      `{"disabled":true}`,
			wantPerm:  identity.PermLinksWrite,
			wantAllow: []identity.Perm{identity.PermLinksWrite, identity.PermLinksRead},
			wantOK:    http.StatusOK,
		},
		{
			method:   http.MethodDelete,
			path:     "/api/v1/links/AbC123xy",
			wantPerm: identity.PermLinksWrite,
			wantOK:   http.StatusNoContent,
		},
		{
			method:   http.MethodGet,
			path:     "/api/v1/links/AbC123xy/stats",
			wantPerm: identity.PermStatsRead,
			wantOK:   http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			store := &countingStore{}
			adminCalls := 0
			admin := &stubAdmin{
				listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) {
					adminCalls++
					return nil, nil
				},
				getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
					adminCalls++
					return &link.AdminLink{Link: link.Link{ID: 1, Code: "AbC123xy", URL: "https://example.com"}}, nil
				},
				updFn: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
					adminCalls++
					return &link.AdminLink{Link: link.Link{ID: 1, Code: "AbC123xy", URL: "https://example.com"}}, nil
				},
				delFn: func(context.Context, int64, string) error {
					adminCalls++
					return nil
				},
				statsFn: func(_ context.Context, _ int64, code string, from, to time.Time) (*link.Stats, error) {
					adminCalls++
					return &link.Stats{Code: code, From: from, To: to}, nil
				},
			}

			deny := &stubAuthorizer{allow: false}
			denySrv := newServer(serverDeps{
				logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:          store,
				admin:          admin,
				stats:          admin,
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				keys:           allowKey{},
				authorizer:     deny,
			})
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			authorize(req)
			rec := httptest.NewRecorder()
			denySrv.ConsoleRoutes().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("deny status = %d, want 403, body = %s", rec.Code, rec.Body.String())
			}
			if !slices.Equal(deny.perms, []identity.Perm{tt.wantPerm}) {
				t.Errorf("deny asked perms = %v, want [%s]", deny.perms, tt.wantPerm)
			}
			if store.creates != 0 {
				t.Errorf("store creates = %d, want 0 when forbidden", store.creates)
			}
			if adminCalls != 0 {
				t.Errorf("admin calls = %d, want 0 when forbidden", adminCalls)
			}

			allow := &stubAuthorizer{allow: true}
			allowSrv := newServer(serverDeps{
				logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:          store,
				admin:          admin,
				stats:          admin,
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				keys:           allowKey{},
				authorizer:     allow,
			})
			req = httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			authorize(req)
			rec = httptest.NewRecorder()
			allowSrv.ConsoleRoutes().ServeHTTP(rec, req)
			if rec.Code != tt.wantOK {
				t.Fatalf("allow status = %d, want %d, body = %s", rec.Code, tt.wantOK, rec.Body.String())
			}
			wantAllow := tt.wantAllow
			if wantAllow == nil {
				wantAllow = []identity.Perm{tt.wantPerm}
			}
			if !slices.Equal(allow.perms, wantAllow) {
				t.Errorf("allow asked perms = %v, want %v", allow.perms, wantAllow)
			}
		})
	}
}

/*
TestRateLimitRunsBeforeRequirePerm 验证限流在鉴权之后、权限校验之前执行：
超限请求应先返回 429 而不是 403，并且不再调用 Authorizer。
*/
func TestRateLimitRunsBeforeRequirePerm(t *testing.T) {
	authz := &stubAuthorizer{allow: false}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           tenantKey{testAPIKey: {ID: 1, RateLimit: 1}},
		authorizer:     authz,
	})
	handler := srv.ConsoleRoutes()

	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/1"}`))
	authorize(req1)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusForbidden {
		t.Fatalf("first status = %d, want 403, body = %s", rec1.Code, rec1.Body.String())
	}
	if len(authz.perms) != 1 {
		t.Fatalf("authz calls after first request = %d, want 1", len(authz.perms))
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/2"}`))
	authorize(req2)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429, body = %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
	if len(authz.perms) != 1 {
		t.Errorf("authz calls after rate limited request = %d, want 1", len(authz.perms))
	}
}

/*
TestConsoleRouteAPIKeyScopes 用真实的 MapAuthorizer 走完管理路由，
按 API Key 的 scope 矩阵验证所有端点的放行和 403 拒绝。
*/
func TestConsoleRouteAPIKeyScopes(t *testing.T) {
	routes := []struct {
		name     string
		method   string
		path     string
		body     string
		needPerm identity.Perm
		wantOK   int
	}{
		{name: "create", method: http.MethodPost, path: "/api/v1/links", body: `{"url":"https://example.com/s"}`, needPerm: identity.PermLinksWrite, wantOK: http.StatusCreated},
		{name: "list", method: http.MethodGet, path: "/api/v1/links", needPerm: identity.PermLinksRead, wantOK: http.StatusOK},
		{name: "detail", method: http.MethodGet, path: "/api/v1/links/AbC123xy", needPerm: identity.PermLinksRead, wantOK: http.StatusOK},
		{name: "update", method: http.MethodPatch, path: "/api/v1/links/AbC123xy", body: `{"disabled":true}`, needPerm: identity.PermLinksWrite, wantOK: http.StatusOK},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/links/AbC123xy", needPerm: identity.PermLinksWrite, wantOK: http.StatusNoContent},
		{name: "stats", method: http.MethodGet, path: "/api/v1/links/AbC123xy/stats", needPerm: identity.PermStatsRead, wantOK: http.StatusOK},
	}

	keySets := []struct {
		name   string
		scopes []identity.Perm
	}{
		{name: "all", scopes: identity.AllAPIKeyScopes()},
		{name: "links_read", scopes: []identity.Perm{identity.PermLinksRead}},
		{name: "links_write", scopes: []identity.Perm{identity.PermLinksWrite}},
		{name: "stats_read", scopes: []identity.Perm{identity.PermStatsRead}},
		{name: "read_and_stats", scopes: []identity.Perm{identity.PermLinksRead, identity.PermStatsRead}},
	}

	for _, ks := range keySets {
		for _, rt := range routes {
			t.Run(ks.name+"/"+rt.name, func(t *testing.T) {
				store := &countingStore{}
				admin := &stubAdmin{
					listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) { return nil, nil },
					getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
						return &link.AdminLink{Link: link.Link{Code: "AbC123xy", URL: "https://example.com"}}, nil
					},
					updFn: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
						return &link.AdminLink{Link: link.Link{Code: "AbC123xy", URL: "https://example.com"}}, nil
					},
					delFn: func(context.Context, int64, string) error { return nil },
					statsFn: func(_ context.Context, _ int64, code string, from, to time.Time) (*link.Stats, error) {
						return &link.Stats{Code: code, From: from, To: to}, nil
					},
				}
				srv := newServer(serverDeps{
					logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
					store:          store,
					admin:          admin,
					stats:          admin,
					requestTimeout: 5 * time.Second,
					registry:       newTestRegistry(),
					keys: principalKey{testAPIKey: {
						Tenant:    tenant.Tenant{ID: 1},
						ActorKind: identity.ActorAPIKey,
						ActorID:   10,
						Scopes:    ks.scopes,
					}},
					authorizer: identity.NewMapAuthorizer(),
				})

				req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
				authorize(req)
				rec := httptest.NewRecorder()
				srv.ConsoleRoutes().ServeHTTP(rec, req)

				want := http.StatusForbidden
				if slices.Contains(ks.scopes, rt.needPerm) {
					want = rt.wantOK
					// 只有 links:write 的 PATCH 不返回链接对象。
					if rt.name == "update" && !slices.Contains(ks.scopes, identity.PermLinksRead) {
						want = http.StatusNoContent
					}
				}
				if rec.Code != want {
					t.Fatalf("status = %d, want %d, body = %s", rec.Code, want, rec.Body.String())
				}
				if rt.name == "update" && want == http.StatusNoContent && rec.Body.Len() != 0 {
					t.Errorf("body = %q, want empty", rec.Body.String())
				}
				if rt.name == "update" && want == http.StatusOK && !strings.Contains(rec.Body.String(), `"url":"https://example.com"`) {
					t.Errorf("body = %s, want updated link", rec.Body.String())
				}
				if want == http.StatusForbidden && rec.Body.String() != `{"error":"forbidden"}` {
					t.Errorf("body = %q, want forbidden", rec.Body.String())
				}
			})
		}
	}
}

type stubSessionAuth struct {
	calls int
	fn    func(ctx context.Context, token string, orgID int64) (identity.Principal, error)
}

func (s *stubSessionAuth) AuthenticateSessionForOrg(ctx context.Context, token string, orgID int64) (identity.Principal, error) {
	s.calls++
	if s.fn != nil {
		return s.fn(ctx, token, orgID)
	}
	return identity.Principal{}, identity.ErrUnauthenticated
}

func TestAuthenticateSessionAndOrgHeader(t *testing.T) {
	validToken := "sls_" + strings.Repeat("ab", 32)
	userPrincipal := identity.Principal{
		Tenant:    tenant.Tenant{ID: 42, ExternalID: "org-42", RateLimit: 15},
		ActorKind: identity.ActorUser,
		ActorID:   9,
		Role:      identity.RoleAdmin,
	}

	tests := []struct {
		name            string
		setupReq        func(*http.Request)
		keys            APIKeyAuthenticator
		sessions        SessionAuthenticator
		wantStatus      int
		wantBody        string
		wantAuthCalls   int
		wantPrincipal   identity.Principal
		bearerChallenge bool
	}{
		{
			name: "valid session and org header",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			keys: allowKey{},
			sessions: &stubSessionAuth{
				fn: func(_ context.Context, token string, orgID int64) (identity.Principal, error) {
					if token == validToken && orgID == 42 {
						return userPrincipal, nil
					}
					return identity.Principal{}, identity.ErrOrgNotFound
				},
			},
			wantStatus:    http.StatusNoContent,
			wantAuthCalls: 1,
			wantPrincipal: userPrincipal,
		},
		{
			name: "missing X-Org-ID returns 400 without querying store",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"X-Org-ID header is required"}`,
			wantAuthCalls: 0,
		},
		{
			name: "empty X-Org-ID returns 400",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"X-Org-ID header is required"}`,
			wantAuthCalls: 0,
		},
		{
			name: "non-numeric X-Org-ID returns 400",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "acme")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"invalid X-Org-ID"}`,
			wantAuthCalls: 0,
		},
		{
			name: "zero X-Org-ID returns 400",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "0")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"invalid X-Org-ID"}`,
			wantAuthCalls: 0,
		},
		{
			name: "signed X-Org-ID returns 400",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "+42")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"invalid X-Org-ID"}`,
			wantAuthCalls: 0,
		},
		{
			name: "duplicate X-Org-ID headers return 400",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Add(orgIDHeader, "1")
				r.Header.Add(orgIDHeader, "2")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusBadRequest,
			wantBody:      `{"error":"invalid X-Org-ID"}`,
			wantAuthCalls: 0,
		},
		{
			name: "non-member organization returns 404",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "999")
			},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return identity.Principal{}, identity.ErrOrgNotFound
				},
			},
			wantStatus:    http.StatusNotFound,
			wantBody:      `{"error":"organization not found"}`,
			wantAuthCalls: 1,
		},
		{
			name: "duplicate session cookies return 401 without querying store",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "sls_" + strings.Repeat("ef", 32)})
				r.Header.Set(orgIDHeader, "42")
			},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return userPrincipal, nil
				},
			},
			wantStatus:    http.StatusUnauthorized,
			wantAuthCalls: 0,
		},
		{
			name: "malformed session cookie returns 401 without querying store",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "not-a-valid-token"})
				r.Header.Set(orgIDHeader, "42")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusUnauthorized,
			wantAuthCalls: 0,
		},
		{
			name: "unauthenticated session returns 401",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			sessions:      &stubSessionAuth{},
			wantStatus:    http.StatusUnauthorized,
			wantAuthCalls: 1,
		},
		{
			name: "session store error returns 500",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return identity.Principal{}, errors.New("db down")
				},
			},
			wantStatus:    http.StatusInternalServerError,
			wantAuthCalls: 1,
		},
		{
			name: "invalid user principal rejected with 500",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return identity.Principal{
						Tenant:    tenant.Tenant{ID: 42},
						ActorKind: identity.ActorUser,
						ActorID:   9,
						Role:      identity.RoleAdmin,
						Scopes:    identity.AllAPIKeyScopes(),
					}, nil
				},
			},
			wantStatus:    http.StatusInternalServerError,
			wantAuthCalls: 1,
		},
		{
			name: "session cookie ignored when session auth unconfigured",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			keys:            allowKey{},
			wantStatus:      http.StatusUnauthorized,
			bearerChallenge: true,
		},
		{
			name: "api key ignores X-Org-ID and session cookie",
			setupReq: func(r *http.Request) {
				authorize(r)
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "not-even-numeric")
			},
			keys: allowKey{},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return userPrincipal, nil
				},
			},
			wantStatus:    http.StatusNoContent,
			wantAuthCalls: 0,
			wantPrincipal: identity.Principal{
				Tenant:    tenant.Tenant{ID: 1},
				ActorKind: identity.ActorAPIKey,
				ActorID:   1,
				Scopes:    identity.AllAPIKeyScopes(),
			},
		},
		{
			name: "invalid Authorization does not fall back to valid session cookie",
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer nope")
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "42")
			},
			keys: allowKey{},
			sessions: &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return userPrincipal, nil
				},
			},
			wantStatus:      http.StatusUnauthorized,
			wantAuthCalls:   0,
			bearerChallenge: true,
		},
		{
			name:       "no authenticators configured fails closed with 500",
			setupReq:   func(*http.Request) {},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got identity.Principal
			var saw bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, saw = principalFrom(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			srv := newServer(serverDeps{
				logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:          memorystore.New(),
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				keys:           tt.keys,
				sessions:       tt.sessions,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/links", nil)
			tt.setupReq(req)
			rec := httptest.NewRecorder()
			srv.authenticate(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if req.Header.Get("Authorization") == "" {
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", got)
				}
			}
			if tt.wantStatus == http.StatusUnauthorized {
				want := ""
				if tt.bearerChallenge {
					want = "Bearer"
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != want {
					t.Errorf("WWW-Authenticate = %q, want %q", got, want)
				}
			}
			if stub, ok := tt.sessions.(*stubSessionAuth); ok && stub != nil {
				if stub.calls != tt.wantAuthCalls {
					t.Errorf("session auth calls = %d, want %d", stub.calls, tt.wantAuthCalls)
				}
			}
			if tt.wantStatus == http.StatusNoContent {
				if !saw || got.Tenant != tt.wantPrincipal.Tenant ||
					got.ActorKind != tt.wantPrincipal.ActorKind ||
					got.ActorID != tt.wantPrincipal.ActorID ||
					got.Role != tt.wantPrincipal.Role ||
					!slices.Equal(got.Scopes, tt.wantPrincipal.Scopes) {
					t.Errorf("principal = %+v, saw = %v, want %+v", got, saw, tt.wantPrincipal)
				}
			}
		})
	}
}

func TestCrossOriginProtection(t *testing.T) {
	validToken := "sls_" + strings.Repeat("cd", 32)
	member := identity.Principal{
		Tenant:    tenant.Tenant{ID: 1},
		ActorKind: identity.ActorUser,
		ActorID:   5,
		Role:      identity.RoleMember,
	}

	tests := []struct {
		name          string
		method        string
		path          string
		body          string
		setupReq      func(*http.Request)
		wantStatus    int
		wantAuthCalls int
		wantCreates   int
	}{
		{
			name:   "cross-site POST with session cookie rejected with 403",
			method: http.MethodPost,
			path:   "/api/v1/links",
			body:   `{"url":"https://example.com/csrf"}`,
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "1")
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			},
			wantStatus:    http.StatusForbidden,
			wantAuthCalls: 0,
			wantCreates:   0,
		},
		{
			name:   "same-site PATCH with session cookie rejected with 403",
			method: http.MethodPatch,
			path:   "/api/v1/links/AbC123xy",
			body:   `{"disabled":true}`,
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "1")
				r.Header.Set("Sec-Fetch-Site", "same-site")
			},
			wantStatus:    http.StatusForbidden,
			wantAuthCalls: 0,
		},
		{
			name:   "cross-origin Origin header DELETE with session cookie rejected with 403",
			method: http.MethodDelete,
			path:   "/api/v1/links/AbC123xy",
			setupReq: func(r *http.Request) {
				r.Host = "console.example.com"
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "1")
				r.Header.Set("Origin", "https://evil.example")
			},
			wantStatus:    http.StatusForbidden,
			wantAuthCalls: 0,
		},
		{
			name:   "same-origin POST with session cookie allowed",
			method: http.MethodPost,
			path:   "/api/v1/links",
			body:   `{"url":"https://example.com/same-origin"}`,
			setupReq: func(r *http.Request) {
				r.Host = "console.example.com"
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "1")
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Origin", "https://console.example.com")
			},
			wantStatus:    http.StatusCreated,
			wantAuthCalls: 1,
			wantCreates:   1,
		},
		{
			name:   "cross-site safe GET with session cookie allowed",
			method: http.MethodGet,
			path:   "/api/v1/links",
			setupReq: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				r.Header.Set(orgIDHeader, "1")
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			},
			wantStatus:    http.StatusOK,
			wantAuthCalls: 1,
		},
		{
			name:   "cross-site POST with API key bypasses CSRF check",
			method: http.MethodPost,
			path:   "/api/v1/links",
			body:   `{"url":"https://example.com/api-key"}`,
			setupReq: func(r *http.Request) {
				authorize(r)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("Origin", "https://evil.example")
			},
			wantStatus:    http.StatusCreated,
			wantAuthCalls: 0,
			wantCreates:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &countingStore{}
			admin := &stubAdmin{
				listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) { return nil, nil },
			}
			sessions := &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return member, nil
				},
			}
			srv := newServer(serverDeps{
				logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:          store,
				admin:          admin,
				stats:          admin,
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				keys:           allowKey{},
				sessions:       sessions,
				authorizer:     identity.NewMapAuthorizer(),
			})

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			tt.setupReq(req)
			rec := httptest.NewRecorder()
			srv.ConsoleRoutes().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if req.Header.Get("Authorization") == "" {
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", got)
				}
			}
			if sessions.calls != tt.wantAuthCalls {
				t.Errorf("session auth calls = %d, want %d", sessions.calls, tt.wantAuthCalls)
			}
			if store.creates != tt.wantCreates {
				t.Errorf("store creates = %d, want %d", store.creates, tt.wantCreates)
			}
		})
	}
}

func TestConsoleRouteUserRolePermissions(t *testing.T) {
	validToken := "sls_" + strings.Repeat("ef", 32)
	routes := []struct {
		name     string
		method   string
		path     string
		body     string
		needPerm identity.Perm
		wantOK   int
	}{
		{name: "create", method: http.MethodPost, path: "/api/v1/links", body: `{"url":"https://example.com/s"}`, needPerm: identity.PermLinksWrite, wantOK: http.StatusCreated},
		{name: "list", method: http.MethodGet, path: "/api/v1/links", needPerm: identity.PermLinksRead, wantOK: http.StatusOK},
		{name: "detail", method: http.MethodGet, path: "/api/v1/links/AbC123xy", needPerm: identity.PermLinksRead, wantOK: http.StatusOK},
		{name: "update", method: http.MethodPatch, path: "/api/v1/links/AbC123xy", body: `{"disabled":true}`, needPerm: identity.PermLinksWrite, wantOK: http.StatusOK},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/links/AbC123xy", needPerm: identity.PermLinksWrite, wantOK: http.StatusNoContent},
		{name: "stats", method: http.MethodGet, path: "/api/v1/links/AbC123xy/stats", needPerm: identity.PermStatsRead, wantOK: http.StatusOK},
	}

	roles := []identity.Role{
		identity.RoleViewer,
		identity.RoleMember,
		identity.RoleAdmin,
		identity.RoleOwner,
	}

	authz := identity.NewMapAuthorizer()
	for _, role := range roles {
		for _, rt := range routes {
			t.Run(string(role)+"/"+rt.name, func(t *testing.T) {
				store := &countingStore{}
				admin := &stubAdmin{
					listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) { return nil, nil },
					getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
						return &link.AdminLink{Link: link.Link{Code: "AbC123xy", URL: "https://example.com"}}, nil
					},
					updFn: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
						return &link.AdminLink{Link: link.Link{Code: "AbC123xy", URL: "https://example.com"}}, nil
					},
					delFn: func(context.Context, int64, string) error { return nil },
					statsFn: func(_ context.Context, _ int64, code string, from, to time.Time) (*link.Stats, error) {
						return &link.Stats{Code: code, From: from, To: to}, nil
					},
				}
				sessions := &stubSessionAuth{
					fn: func(context.Context, string, int64) (identity.Principal, error) {
						return identity.Principal{
							Tenant:    tenant.Tenant{ID: 1},
							ActorKind: identity.ActorUser,
							ActorID:   10,
							Role:      role,
						}, nil
					},
				}
				srv := newServer(serverDeps{
					logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
					store:          store,
					admin:          admin,
					stats:          admin,
					requestTimeout: 5 * time.Second,
					registry:       newTestRegistry(),
					keys:           allowKey{},
					sessions:       sessions,
					authorizer:     authz,
				})

				req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
				req.Header.Set(orgIDHeader, "1")
				rec := httptest.NewRecorder()
				srv.ConsoleRoutes().ServeHTTP(rec, req)

				want := http.StatusForbidden
				if authz.Can(identity.Principal{ActorKind: identity.ActorUser, Role: role}, rt.needPerm) {
					want = rt.wantOK
				}
				if rec.Code != want {
					t.Fatalf("status = %d, want %d, body = %s", rec.Code, want, rec.Body.String())
				}
			})
		}
	}
}

func TestSessionRateLimit(t *testing.T) {
	validToken := "sls_" + strings.Repeat("99", 32)
	sessions := &stubSessionAuth{
		fn: func(context.Context, string, int64) (identity.Principal, error) {
			return identity.Principal{
				Tenant:    tenant.Tenant{ID: 5, RateLimit: 1},
				ActorKind: identity.ActorUser,
				ActorID:   2,
				Role:      identity.RoleOwner,
			}, nil
		},
	}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
		sessions:       sessions,
		authorizer:     identity.NewMapAuthorizer(),
	})
	handler := srv.ConsoleRoutes()

	for i, want := range []int{http.StatusCreated, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/rl"}`))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: validToken})
		req.Header.Set(orgIDHeader, "5")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("req %d status = %d, want %d, body = %s", i+1, rec.Code, want, rec.Body.String())
		}
		if want == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Error("expected Retry-After header on 429 response")
		}
	}
}
