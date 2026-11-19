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
	tests := []struct {
		name       string
		header     string
		keys       APIKeyAuthenticator
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "basic", header: "Basic " + testAPIKey, wantStatus: http.StatusUnauthorized},
		{name: "malformed", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "unknown", header: "Bearer " + otherKey, wantStatus: http.StatusUnauthorized},
		{name: "ok", header: "Bearer " + testAPIKey, wantStatus: http.StatusNoContent},
		{name: "lowercase scheme", header: "bearer " + testAPIKey, wantStatus: http.StatusNoContent},
		{
			name:       "db error",
			header:     "Bearer " + testAPIKey,
			keys:       failingKey{err: errors.New("db down")},
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
				if !saw || got.Tenant.ID != 1 || got.ActorKind != identity.ActorAPIKey || !slices.Equal(got.Scopes, identity.AllAPIKeyScopes()) {
					t.Errorf("principal = %+v, saw = %v, want tenant 1 api_key with all scopes", got, saw)
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

func (f failingKey) Authenticate(context.Context, string) (tenant.Tenant, error) {
	return tenant.Tenant{}, f.err
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
		method   string
		path     string
		body     string
		wantPerm identity.Perm
		wantOK   int
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
			method:   http.MethodPatch,
			path:     "/api/v1/links/AbC123xy",
			body:     `{"disabled":true}`,
			wantPerm: identity.PermLinksWrite,
			wantOK:   http.StatusOK,
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
			if !slices.Equal(allow.perms, []identity.Perm{tt.wantPerm}) {
				t.Errorf("allow asked perms = %v, want [%s]", allow.perms, tt.wantPerm)
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
