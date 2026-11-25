package console

import (
	"context"
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
				modules:        []Module{NewStatsModule(admin)},
				requestTimeout: 5 * time.Second,
				registry:       newTestRegistry(),
				keys:           allowKey{},
				sessions:       sessions,
				authorizer:     identity.NewMapAuthorizer(),
			})

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			tt.setupReq(req)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

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
					modules:        []Module{NewStatsModule(admin)},
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
				srv.Routes().ServeHTTP(rec, req)

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
	handler := srv.Routes()

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
