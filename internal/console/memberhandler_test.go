package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/memorystore"
	"shortlink/internal/tenant"
)

type stubMemberStore struct {
	listCalls   int
	listTenant  int64
	updateCalls int
	removeCalls int

	lastTenantID int64
	lastActorID  int64
	lastTargetID int64
	lastRole     identity.Role

	listFn   func(ctx context.Context, tenantID int64) ([]identity.Member, error)
	updateFn func(ctx context.Context, p identity.UpdateMemberRoleParams) (identity.Member, error)
	removeFn func(ctx context.Context, p identity.RemoveMemberParams) error
}

func (s *stubMemberStore) ListMembers(ctx context.Context, tenantID int64) ([]identity.Member, error) {
	s.listCalls++
	s.listTenant = tenantID
	if s.listFn != nil {
		return s.listFn(ctx, tenantID)
	}
	return nil, nil
}

func (s *stubMemberStore) UpdateMemberRole(ctx context.Context, p identity.UpdateMemberRoleParams) (identity.Member, error) {
	s.updateCalls++
	s.lastTenantID = p.TenantID
	s.lastActorID = p.ActorUserID
	s.lastTargetID = p.TargetUserID
	s.lastRole = p.NewRole
	if s.updateFn != nil {
		return s.updateFn(ctx, p)
	}
	return identity.Member{
		UserID:   p.TargetUserID,
		Email:    "target@example.com",
		Name:     "Target",
		Role:     p.NewRole,
		JoinedAt: time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC),
	}, nil
}

func (s *stubMemberStore) RemoveMember(ctx context.Context, p identity.RemoveMemberParams) error {
	s.removeCalls++
	s.lastTenantID = p.TenantID
	s.lastActorID = p.ActorUserID
	s.lastTargetID = p.TargetUserID
	if s.removeFn != nil {
		return s.removeFn(ctx, p)
	}
	return nil
}

func newMemberTestServer(role identity.Role, members MemberStore, sessions SessionAuthenticator) *Server {
	if sessions == nil {
		sessions = &stubSessionAuth{
			fn: func(context.Context, string, int64) (identity.Principal, error) {
				return identity.Principal{
					Tenant:    tenant.Tenant{ID: 1},
					ActorKind: identity.ActorUser,
					ActorID:   10,
					Role:      role,
				}, nil
			},
		}
	}
	return newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
		sessions:       sessions,
		members:        members,
		authorizer:     identity.NewMapAuthorizer(),
	})
}

func TestMemberRoutesDisabledWhenNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	console := New(logger, &stubStore{}, 5*time.Second, newTestRegistry(), Options{
		Keys:       allowKey{},
		Authorizer: identity.NewMapAuthorizer(),
	})

	handler := console.Routes()
	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/v1/members"},
		{method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`},
		{method: http.MethodDelete, path: "/api/v1/members/2"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", route.method, route.path, rec.Code)
		}
	}
}

func TestMemberRoutesAuthAndPermissionMatrix(t *testing.T) {
	sessionToken := "sls_" + strings.Repeat("aa", 32)
	routes := []struct {
		name     string
		method   string
		path     string
		body     string
		needPerm identity.Perm
		wantOK   int
	}{
		{name: "list", method: http.MethodGet, path: "/api/v1/members", needPerm: identity.PermMembersRead, wantOK: http.StatusOK},
		{name: "update role", method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`, needPerm: identity.PermMembersWrite, wantOK: http.StatusOK},
		{name: "remove", method: http.MethodDelete, path: "/api/v1/members/2", needPerm: identity.PermMembersWrite, wantOK: http.StatusNoContent},
	}

	authz := identity.NewMapAuthorizer()
	for _, role := range []identity.Role{identity.RoleOwner, identity.RoleAdmin, identity.RoleMember, identity.RoleViewer} {
		for _, rt := range routes {
			t.Run(string(role)+"/"+rt.name, func(t *testing.T) {
				members := &stubMemberStore{}
				handler := newMemberTestServer(role, members, nil).Routes()

				req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
				req.Header.Set(orgIDHeader, "1")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				want := http.StatusForbidden
				if authz.Can(identity.Principal{ActorKind: identity.ActorUser, Role: role}, rt.needPerm) {
					want = rt.wantOK
				}
				if rec.Code != want {
					t.Fatalf("status = %d, want %d, body = %s", rec.Code, want, rec.Body.String())
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", got)
				}
			})
		}
	}

	// 全权限 API Key 对成员管理三个端点一律返回 403，且不调用存储。
	for _, rt := range routes {
		t.Run("api_key/"+rt.name, func(t *testing.T) {
			members := &stubMemberStore{}
			handler := newMemberTestServer(identity.RoleOwner, members, nil).Routes()

			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden") {
				t.Fatalf("api key status = %d, body = %s, want 403 forbidden", rec.Code, rec.Body.String())
			}
			if members.listCalls+members.updateCalls+members.removeCalls != 0 {
				t.Fatalf("api key unexpectedly called member store")
			}
		})
	}

	// 纵深防御：即使外层 Authorizer 误放行 API Key 或 member 角色，handler 仍拒绝写操作且不调存储。
	for _, tc := range []struct {
		name   string
		apiKey bool
		role   identity.Role
		method string
		path   string
		body   string
	}{
		{name: "api key patch", apiKey: true, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`},
		{name: "api key delete", apiKey: true, method: http.MethodDelete, path: "/api/v1/members/2"},
		{name: "member patch", role: identity.RoleMember, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`},
		{name: "member delete", role: identity.RoleMember, method: http.MethodDelete, path: "/api/v1/members/2"},
	} {
		t.Run("defense/"+tc.name, func(t *testing.T) {
			members := &stubMemberStore{}
			sessions := &stubSessionAuth{
				fn: func(context.Context, string, int64) (identity.Principal, error) {
					return identity.Principal{
						Tenant:    tenant.Tenant{ID: 1},
						ActorKind: identity.ActorUser,
						ActorID:   10,
						Role:      tc.role,
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
				members:        members,
				authorizer:     &stubAuthorizer{allow: true},
			})

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.apiKey {
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
			} else {
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
				req.Header.Set(orgIDHeader, "1")
			}
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden") {
				t.Fatalf("status = %d, body = %s, want 403 forbidden", rec.Code, rec.Body.String())
			}
			if members.listCalls+members.updateCalls+members.removeCalls != 0 {
				t.Fatalf("defense check unexpectedly called store")
			}
		})
	}

	// 非成员、缺 X-Org-ID、无效会话与跨站写请求。
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		body       string
		orgID      string
		cookie     string
		crossSite  bool
		authFn     func(context.Context, string, int64) (identity.Principal, error)
		wantStatus int
	}{
		{
			name:   "non-member returns 404",
			method: http.MethodGet, path: "/api/v1/members",
			orgID: "9", cookie: sessionToken,
			authFn: func(context.Context, string, int64) (identity.Principal, error) {
				return identity.Principal{}, identity.ErrOrgNotFound
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "missing X-Org-ID returns 400",
			method: http.MethodGet, path: "/api/v1/members",
			cookie:     sessionToken,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:   "invalid session returns 401",
			method: http.MethodGet, path: "/api/v1/members",
			orgID: "1", cookie: sessionToken,
			authFn: func(context.Context, string, int64) (identity.Principal, error) {
				return identity.Principal{}, identity.ErrUnauthenticated
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "cross-site PATCH rejected",
			method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`,
			orgID: "1", cookie: sessionToken, crossSite: true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "cross-site DELETE rejected",
			method: http.MethodDelete, path: "/api/v1/members/2",
			orgID: "1", cookie: sessionToken, crossSite: true,
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := &stubMemberStore{}
			handler := newMemberTestServer(identity.RoleOwner, members, &stubSessionAuth{fn: tc.authFn}).Routes()

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tc.cookie})
			}
			if tc.orgID != "" {
				req.Header.Set(orgIDHeader, tc.orgID)
			}
			if tc.crossSite {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.crossSite && !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
				t.Errorf("body = %s, want cross-origin request rejected", rec.Body.String())
			}
			if members.listCalls+members.updateCalls+members.removeCalls != 0 {
				t.Fatalf("store called unexpectedly")
			}
		})
	}
}

func TestMemberUpdateAndRemoveValidation(t *testing.T) {
	sessionToken := "sls_" + strings.Repeat("bb", 32)
	for _, tc := range []struct {
		name       string
		role       identity.Role
		method     string
		path       string
		body       string
		wantStatus int
		wantErr    string
	}{
		{name: "patch zero user_id", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/0", body: `{"role":"member"}`, wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "patch negative user_id", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/-1", body: `{"role":"member"}`, wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "patch alpha user_id", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/abc", body: `{"role":"member"}`, wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "patch plus user_id", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/+2", body: `{"role":"member"}`, wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "delete zero user_id", role: identity.RoleOwner, method: http.MethodDelete, path: "/api/v1/members/0", wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "delete alpha user_id", role: identity.RoleOwner, method: http.MethodDelete, path: "/api/v1/members/abc", wantStatus: http.StatusBadRequest, wantErr: "invalid user_id"},
		{name: "bad json", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{`, wantStatus: http.StatusBadRequest, wantErr: "invalid JSON body"},
		{name: "unknown field", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member","extra":1}`, wantStatus: http.StatusBadRequest, wantErr: `unknown field \"extra\"`},
		{name: "extra trailing json", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}{}`, wantStatus: http.StatusBadRequest, wantErr: "invalid JSON body"},
		{name: "empty object", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{}`, wantStatus: http.StatusBadRequest, wantErr: "invalid role"},
		{name: "null role", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":null}`, wantStatus: http.StatusBadRequest, wantErr: "invalid role"},
		{name: "number role", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":123}`, wantStatus: http.StatusBadRequest, wantErr: "invalid role"},
		{name: "unknown role", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"guest"}`, wantStatus: http.StatusBadRequest, wantErr: "invalid role"},
		{name: "body too large", role: identity.RoleOwner, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"` + strings.Repeat("a", maxBodyBytes+10) + `"}`, wantStatus: http.StatusRequestEntityTooLarge, wantErr: "request body too large"},
		{name: "admin cannot promote to owner", role: identity.RoleAdmin, method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"owner"}`, wantStatus: http.StatusForbidden, wantErr: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := &stubMemberStore{}
			handler := newMemberTestServer(tc.role, members, nil).Routes()

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
			req.Header.Set(orgIDHeader, "1")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Fatalf("status = %d, body = %s, want (%d, %q)", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantErr)
			}
			if members.updateCalls+members.removeCalls != 0 {
				t.Fatalf("invalid request unexpectedly called store")
			}
		})
	}
}

func TestMemberStoreErrorMappingsAndSuccessResponses(t *testing.T) {
	sessionToken := "sls_" + strings.Repeat("cc", 32)

	t.Run("error mappings", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			method     string
			path       string
			body       string
			err        error
			wantStatus int
			wantErr    string
		}{
			{name: "member not found", method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`, err: identity.ErrMemberNotFound, wantStatus: http.StatusNotFound, wantErr: "member not found"},
			{name: "org not found", method: http.MethodDelete, path: "/api/v1/members/2", err: identity.ErrOrgNotFound, wantStatus: http.StatusNotFound, wantErr: "organization not found"},
			{name: "member forbidden", method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`, err: identity.ErrMemberForbidden, wantStatus: http.StatusForbidden, wantErr: "forbidden"},
			{name: "last owner on update", method: http.MethodPatch, path: "/api/v1/members/10", body: `{"role":"member"}`, err: identity.ErrLastOwner, wantStatus: http.StatusConflict, wantErr: "organization must have at least one owner"},
			{name: "last owner on remove", method: http.MethodDelete, path: "/api/v1/members/10", err: identity.ErrLastOwner, wantStatus: http.StatusConflict, wantErr: "organization must have at least one owner"},
			{name: "invalid role from store", method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`, err: identity.ErrInvalidRole, wantStatus: http.StatusBadRequest, wantErr: "invalid role"},
			{name: "timeout", method: http.MethodGet, path: "/api/v1/members", err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantErr: "request timed out"},
			{name: "internal error", method: http.MethodDelete, path: "/api/v1/members/2", err: errors.New("db down"), wantStatus: http.StatusInternalServerError, wantErr: "internal error"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				members := &stubMemberStore{
					listFn: func(context.Context, int64) ([]identity.Member, error) {
						return nil, tc.err
					},
					updateFn: func(context.Context, identity.UpdateMemberRoleParams) (identity.Member, error) {
						return identity.Member{}, tc.err
					},
					removeFn: func(context.Context, identity.RemoveMemberParams) error {
						return tc.err
					},
				}
				handler := newMemberTestServer(identity.RoleOwner, members, nil).Routes()

				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
				req.Header.Set(orgIDHeader, "1")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantErr) {
					t.Fatalf("status = %d, body = %s, want (%d, %q)", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantErr)
				}
			})
		}
	})

	t.Run("list non-empty and empty", func(t *testing.T) {
		joined := time.Date(2026, 11, 22, 6, 30, 0, 0, time.FixedZone("CST", 8*3600))
		members := &stubMemberStore{
			listFn: func(context.Context, int64) ([]identity.Member, error) {
				return []identity.Member{{
					UserID:   10,
					Email:    "alice@example.com",
					Name:     "Alice",
					Role:     identity.RoleOwner,
					JoinedAt: joined,
				}}, nil
			},
		}
		handler := newMemberTestServer(identity.RoleViewer, members, nil).Routes()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/members", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
		req.Header.Set(orgIDHeader, "1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || members.listTenant != 1 {
			t.Fatalf("list status = %d, tenant = %d, body = %s", rec.Code, members.listTenant, rec.Body.String())
		}
		var body listMembersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list body: %v", err)
		}
		if len(body.Items) != 1 || body.Items[0].UserID != 10 || body.Items[0].Email != "alice@example.com" ||
			body.Items[0].Role != "owner" || !body.Items[0].JoinedAt.Equal(joined.UTC()) {
			t.Fatalf("items = %+v", body.Items)
		}

		members.listFn = func(context.Context, int64) ([]identity.Member, error) { return nil, nil }
		reqEmpty := httptest.NewRequest(http.MethodGet, "/api/v1/members", nil)
		reqEmpty.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
		reqEmpty.Header.Set(orgIDHeader, "1")
		recEmpty := httptest.NewRecorder()
		handler.ServeHTTP(recEmpty, reqEmpty)
		if recEmpty.Code != http.StatusOK || strings.TrimSpace(recEmpty.Body.String()) != `{"items":[]}` {
			t.Fatalf("empty list = (%d, %q), want 200 {\"items\":[]}", recEmpty.Code, recEmpty.Body.String())
		}
	})

	t.Run("update and remove success", func(t *testing.T) {
		members := &stubMemberStore{}
		handler := newMemberTestServer(identity.RoleOwner, members, nil).Routes()

		updReq := httptest.NewRequest(http.MethodPatch, "/api/v1/members/2", strings.NewReader(`{"role":"admin"}`))
		updReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
		updReq.Header.Set(orgIDHeader, "1")
		updRec := httptest.NewRecorder()
		handler.ServeHTTP(updRec, updReq)

		if updRec.Code != http.StatusOK {
			t.Fatalf("update status = %d, body = %s", updRec.Code, updRec.Body.String())
		}
		if members.lastTenantID != 1 || members.lastActorID != 10 || members.lastTargetID != 2 || members.lastRole != identity.RoleAdmin {
			t.Fatalf("update args = tenant:%d actor:%d target:%d role:%s", members.lastTenantID, members.lastActorID, members.lastTargetID, members.lastRole)
		}
		var updated memberResponse
		if err := json.Unmarshal(updRec.Body.Bytes(), &updated); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		if updated.UserID != 2 || updated.Role != "admin" {
			t.Fatalf("updated = %+v", updated)
		}

		delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/members/2", nil)
		delReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
		delReq.Header.Set(orgIDHeader, "1")
		delRec := httptest.NewRecorder()
		handler.ServeHTTP(delRec, delReq)

		if delRec.Code != http.StatusNoContent || delRec.Body.Len() != 0 {
			t.Fatalf("remove = (%d, %q), want (204, empty)", delRec.Code, delRec.Body.String())
		}
		if members.lastTenantID != 1 || members.lastActorID != 10 || members.lastTargetID != 2 {
			t.Fatalf("remove args = tenant:%d actor:%d target:%d", members.lastTenantID, members.lastActorID, members.lastTargetID)
		}
	})
}
