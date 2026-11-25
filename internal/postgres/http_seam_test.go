package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/console"
	"shortlink/internal/identity"
)

/*
TestHTTPListCursorRoundTrip 把列表游标从 HTTP 响应送回下一次查询。
编码在 console，排序在 SQL。两边分开测时，游标格式对不上也会全绿。
*/
func TestHTTPListCursorRoundTrip(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())
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
	ctx := testAuditCtx(context.Background())
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
	ctx := testAuditCtx(context.Background())
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
TestHTTPOIDCLoginPersistsSession 端到端串联 fakeoidc、oauth.OIDCProvider、
console 登录/回调路由和真实 PostgreSQL 存储，验证首次与重复登录落库和会话 Cookie 哈希一致。
*/

func newConsoleHandler(store *Store) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := console.New(logger, store, 5*time.Second, prometheus.NewRegistry(), console.Options{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
	})
	return srv.Routes()
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
TestHTTPGitHubLoginIsolationAndInvitationRedemption 端到端验证第二个身份提供方 GitHub：
1. 与 OIDC 挂载在同一台 console 上时，相同已验证邮箱不自动合并为同一平台用户；
2. GitHub 仅在 /user/emails 返回 primary 且 verified 的邮箱时才兑现邀请并放行组织管理接口。
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
