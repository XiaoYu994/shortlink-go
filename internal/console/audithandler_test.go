package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/audit"
	"shortlink/internal/identity"
	"shortlink/internal/link"
)

type fakeAuditLogStore struct {
	listFn   func(ctx context.Context, opts audit.ListOptions) ([]audit.Event, error)
	lastOpts audit.ListOptions
}

func (f *fakeAuditLogStore) ListAuditLogs(ctx context.Context, opts audit.ListOptions) ([]audit.Event, error) {
	f.lastOpts = opts
	return f.listFn(ctx, opts)
}

type permissiveAuthorizer struct{}

func (permissiveAuthorizer) Can(identity.Principal, identity.Perm) bool { return true }

type metaCaptureStore struct {
	stubStore
	createMeta audit.Meta
	hasMeta    bool
}

func (m *metaCaptureStore) Create(ctx context.Context, l *link.Link) error {
	m.createMeta, m.hasMeta = audit.MetaFrom(ctx)
	return m.stubStore.Create(ctx, l)
}

func TestAuditLogsHandlerPermissionsValidationAndPagination(t *testing.T) {
	sessionTok, _, _ := identity.SessionToken.Generate()
	apiKeyTok, _, _ := identity.APIKeyToken.Generate()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	actorID := int64(7)

	auditStore := &fakeAuditLogStore{
		listFn: func(_ context.Context, opts audit.ListOptions) ([]audit.Event, error) {
			if opts.Action == audit.ActionLinkBan {
				return []audit.Event{
					{
						ID:         5,
						TenantID:   opts.TenantID,
						ActorKind:  audit.ActorSystem,
						Action:     audit.ActionLinkBan,
						TargetType: audit.TargetLink,
						TargetID:   "AbC12345",
						RequestID:  "job:42",
						Metadata:   json.RawMessage(`{"reason":"phishing"}`),
						CreatedAt:  now,
					},
				}, nil
			}
			// Limit=3 表示请求方 pageLimit=2，返回 3 条触发 next_cursor。
			return []audit.Event{
				{
					ID:         12,
					TenantID:   opts.TenantID,
					ActorKind:  audit.ActorUser,
					ActorID:    &actorID,
					ActorEmail: "alice@example.com",
					ActorName:  "Alice",
					Action:     audit.ActionAPIKeyIssue,
					TargetType: audit.TargetAPIKey,
					TargetID:   "9",
					RequestID:  "req-12",
					Metadata:   json.RawMessage(`{"scopes":["links:read"]}`),
					CreatedAt:  now,
				},
				{
					ID:         11,
					TenantID:   opts.TenantID,
					ActorKind:  audit.ActorOperator,
					Action:     audit.ActionLinkUnban,
					TargetType: audit.TargetLink,
					TargetID:   "AbC12345",
					Metadata:   json.RawMessage(`{}`),
					CreatedAt:  now.Add(-time.Minute),
				},
				{
					ID:         10,
					TenantID:   opts.TenantID,
					ActorKind:  audit.ActorAPIKey,
					ActorID:    &actorID,
					Action:     audit.ActionLinkCreate,
					TargetType: audit.TargetLink,
					TargetID:   "XyZ98765",
					Metadata:   json.RawMessage(`{}`),
					CreatedAt:  now.Add(-2 * time.Minute),
				},
			}, nil
		},
	}

	makeHandler := func(role identity.Role, authz Authorizer) http.Handler {
		return New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
			Keys:       selfServiceAPIKeyAuth{scopes: identity.AllAPIKeyScopes()},
			Authorizer: authz,
			Sessions:   &selfServiceSessionStore{userID: 7, role: role, orgID: 1},
			Modules:    []Module{NewAuditModule(auditStore)},
		}).Routes()
	}

	doSession := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
		req.Header.Set("X-Org-ID", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. owner / admin 成功读取，带 Cache-Control: no-store 与 next_cursor。
	hOwner := makeHandler(identity.RoleOwner, identity.NewMapAuthorizer())
	rec := doSession(hOwner, "/api/v1/audit-logs?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET /audit-logs = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	var resp listAuditLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 2 || resp.NextCursor != "11" {
		t.Fatalf("resp = %+v, want 2 items and next_cursor=11", resp)
	}
	if resp.Items[0].ActorEmail != "alice@example.com" || resp.Items[0].ActorName != "Alice" || resp.Items[0].RequestID != "req-12" {
		t.Fatalf("first item = %+v", resp.Items[0])
	}

	// admin + action + cursor 传参解析正确。
	hAdmin := makeHandler(identity.RoleAdmin, identity.NewMapAuthorizer())
	rec = doSession(hAdmin, "/api/v1/audit-logs?action=link.ban&cursor=20&limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin filtered GET /audit-logs = %d (%s)", rec.Code, rec.Body.String())
	}
	if auditStore.lastOpts.TenantID != 1 || auditStore.lastOpts.Action != audit.ActionLinkBan ||
		auditStore.lastOpts.Limit != 6 || auditStore.lastOpts.Cursor == nil || auditStore.lastOpts.Cursor.ID != 20 {
		t.Fatalf("lastOpts = %+v", auditStore.lastOpts)
	}

	// 2. member / viewer 无权查看（403）；未登录返回 401。
	for _, role := range []identity.Role{identity.RoleMember, identity.RoleViewer} {
		rec = doSession(makeHandler(role, identity.NewMapAuthorizer()), "/api/v1/audit-logs")
		if rec.Code != http.StatusForbidden {
			t.Errorf("role %s GET /audit-logs = %d, want 403", role, rec.Code)
		}
	}
	unauthRec := httptest.NewRecorder()
	hOwner.ServeHTTP(unauthRec, httptest.NewRequest(http.MethodGet, "/api/v1/audit-logs", nil))
	if unauthRec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /audit-logs = %d, want 401", unauthRec.Code)
	}

	// 3. API Key 默认策略拒绝，且即使放开自定义 Authorizer，handler 纵深防御仍拦截（403）。
	for _, h := range []http.Handler{hOwner, makeHandler(identity.RoleOwner, permissiveAuthorizer{})} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/audit-logs", nil)
		req.Header.Set("Authorization", "Bearer "+apiKeyTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("API Key GET /audit-logs = %d, want 403", rec.Code)
		}
	}

	// 4. 非法查询参数返回 400。
	for _, bad := range []string{
		"/api/v1/audit-logs?limit=0",
		"/api/v1/audit-logs?limit=101",
		"/api/v1/audit-logs?limit=abc",
		"/api/v1/audit-logs?action=unknown.op",
		"/api/v1/audit-logs?cursor=0",
		"/api/v1/audit-logs?cursor=-5",
		"/api/v1/audit-logs?cursor=notnum",
	} {
		rec := doSession(hOwner, bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestAuditMetaPropagationInMiddlewareAndOrgCreate(t *testing.T) {
	sessionTok, _, _ := identity.SessionToken.Generate()
	apiKeyTok, _, _ := identity.APIKeyToken.Generate()

	capture := &metaCaptureStore{}
	var orgMeta audit.Meta
	var hasOrgMeta bool
	orgs := &fakeOrgStore{
		createFn: func(ctx context.Context, _ int64, name string, _ int) (identity.CreatedOrg, error) {
			orgMeta, hasOrgMeta = audit.MetaFrom(ctx)
			return identity.CreatedOrg{ID: 42, Name: name, Role: identity.RoleOwner, CreatedAt: time.Now().UTC()}, nil
		},
	}

	sess := &selfServiceSessionStore{userID: 7, role: identity.RoleOwner, orgID: 1}
	h := New(selfServiceLogger(), capture, 2*time.Second, prometheus.NewRegistry(), Options{
		Keys:       selfServiceAPIKeyAuth{scopes: identity.AllAPIKeyScopes()},
		Authorizer: identity.NewMapAuthorizer(),
		Sessions:   sess,
		Modules:    []Module{NewOrgsModule(sess, orgs)},
	}).Routes()

	// 会话创建短链：ActorUser + ActorID=7 + 服务端签发 RequestID（忽略伪造 X-Request-ID）。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/a"}`))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
	req.Header.Set("X-Org-ID", "1")
	req.Header.Set("X-Request-ID", "req-session-create")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session create = %d (%s)", rec.Code, rec.Body.String())
	}
	sessReqID := rec.Header().Get("X-Request-ID")
	if sessReqID == "" || sessReqID == "req-session-create" {
		t.Fatalf("server X-Request-ID = %q", sessReqID)
	}
	if !capture.hasMeta || capture.createMeta.ActorKind != audit.ActorUser || capture.createMeta.ActorID == nil || *capture.createMeta.ActorID != 7 || capture.createMeta.RequestID != sessReqID {
		t.Fatalf("session create audit meta = (%+v, %v), want RequestID=%q", capture.createMeta, capture.hasMeta, sessReqID)
	}

	// API Key 创建短链：ActorAPIKey + ActorID=99 + 服务端签发 RequestID。
	capture.hasMeta = false
	req = httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/b"}`))
	req.Header.Set("Authorization", "Bearer "+apiKeyTok)
	req.Header.Set("X-Request-ID", "req-key-create")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("api key create = %d (%s)", rec.Code, rec.Body.String())
	}
	keyReqID := rec.Header().Get("X-Request-ID")
	if keyReqID == "" || keyReqID == "req-key-create" {
		t.Fatalf("server X-Request-ID = %q", keyReqID)
	}
	if !capture.hasMeta || capture.createMeta.ActorKind != audit.ActorAPIKey || capture.createMeta.ActorID == nil || *capture.createMeta.ActorID != 99 || capture.createMeta.RequestID != keyReqID {
		t.Fatalf("api key create audit meta = (%+v, %v), want RequestID=%q", capture.createMeta, capture.hasMeta, keyReqID)
	}

	// 自助建组织：ActorUser + ActorID=7 + 服务端签发 RequestID。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/orgs", strings.NewReader(`{"name":"Meta Org"}`))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTok})
	req.Header.Set("X-Request-ID", "req-org-create")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org = %d (%s)", rec.Code, rec.Body.String())
	}
	orgReqID := rec.Header().Get("X-Request-ID")
	if orgReqID == "" || orgReqID == "req-org-create" {
		t.Fatalf("server X-Request-ID = %q", orgReqID)
	}
	if !hasOrgMeta || orgMeta.ActorKind != audit.ActorUser || orgMeta.ActorID == nil || *orgMeta.ActorID != 7 || orgMeta.RequestID != orgReqID {
		t.Fatalf("org create audit meta = (%+v, %v), want RequestID=%q", orgMeta, hasOrgMeta, orgReqID)
	}
}
