package httpapi

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

	"shortlink/internal/link"
	"shortlink/internal/memorystore"
)

/*
stubAdmin 是管理 handler 的存储替身。
handler 测试只关心参数解析、租户传递和响应映射；排序和游标的 SQL 行为
在 postgres 集成测试里验证，这里按调用方给的 Limit 返回预设结果。
*/
type stubAdmin struct {
	listFn  func(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error)
	getFn   func(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error)
	updFn   func(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error)
	delFn   func(ctx context.Context, tenantID int64, code string) error
	statsFn func(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error)
}

func (s *stubAdmin) List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	if s.listFn != nil {
		return s.listFn(ctx, opts)
	}
	return nil, nil
}

func (s *stubAdmin) GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error) {
	if s.getFn != nil {
		return s.getFn(ctx, tenantID, code)
	}
	return nil, link.ErrNotFound
}

func (s *stubAdmin) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	if s.updFn != nil {
		return s.updFn(ctx, tenantID, code, u)
	}
	return nil, link.ErrNotFound
}

func (s *stubAdmin) Delete(ctx context.Context, tenantID int64, code string) error {
	if s.delFn != nil {
		return s.delFn(ctx, tenantID, code)
	}
	return nil
}

func (s *stubAdmin) Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error) {
	if s.statsFn != nil {
		return s.statsFn(ctx, tenantID, code, from, to)
	}
	return nil, link.ErrNotFound
}

// newAdminTestServer 组装带管理路由的 Server；基础 LinkStore 用内存存储。
func newAdminTestServer(admin *stubAdmin) *Server {
	return newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		admin:          admin,
		stats:          admin,
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
	})
}

func doAdminRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	authorize(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestListLinks(t *testing.T) {
	var gotOpts link.ListOptions
	created := time.Now().UTC().Truncate(time.Second)
	admin := &stubAdmin{listFn: func(_ context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
		gotOpts = opts
		return []link.AdminLink{
			{Link: link.Link{ID: 3, Code: "AbC123xy", URL: "https://example.com/3", CreatedAt: created}, Clicks: 9, ClicksToday: 2},
			{Link: link.Link{ID: 2, Code: "AbC123xz", URL: "https://example.com/2", CreatedAt: created}, Clicks: 7, ClicksToday: 1},
			// 第三条是多要的那一条，只用来判断有下一页。
			{Link: link.Link{ID: 1, Code: "AbC123xw", URL: "https://example.com/1", CreatedAt: created}},
		}, nil
	}}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	rec := doAdminRequest(t, handler, http.MethodGet, "/api/v1/links?limit=2&sort=clicks&cursor=8:3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if gotOpts.TenantID != 1 || gotOpts.Limit != 3 || gotOpts.Sort != link.SortClicks {
		t.Errorf("opts = %+v, want tenant 1 limit 3 sort clicks", gotOpts)
	}
	if gotOpts.Cursor == nil || gotOpts.Cursor.ID != 3 || gotOpts.Cursor.Clicks != 8 {
		t.Errorf("cursor = %+v, want clicks 8 id 3", gotOpts.Cursor)
	}

	var resp listLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Clicks != 9 || resp.Items[0].ClicksToday != 2 || resp.Items[0].CreatedAt.IsZero() {
		t.Errorf("first item = %+v, want counts", resp.Items[0])
	}
	if got, want := resp.NextCursor, "7:2"; got != want {
		t.Errorf("next_cursor = %q, want %q", got, want)
	}
}

func TestListLinksWithoutNextPage(t *testing.T) {
	admin := &stubAdmin{listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) {
		return []link.AdminLink{{Link: link.Link{ID: 1, Code: "AbC123xy", URL: "https://example.com"}}}, nil
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodGet, "/api/v1/links", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "next_cursor") {
		t.Errorf("body = %s, want no next_cursor", rec.Body.String())
	}
}

func TestListLinksBadQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"limit zero", "?limit=0"},
		{"limit too large", "?limit=101"},
		{"limit not a number", "?limit=abc"},
		{"invalid sort", "?sort=url"},
		{"cursor for id sort", "?cursor=1:2"},
		{"cursor missing id", "?sort=clicks&cursor=5"},
		{"cursor negative", "?sort=clicks&cursor=-1:5"},
		{"cursor zero id", "?sort=clicks&cursor=1:0"},
		{"cursor text", "?sort=id&cursor=abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doAdminRequest(t, newAdminTestServer(&stubAdmin{}).ConsoleRoutes(), http.MethodGet, "/api/v1/links"+tt.query, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGetLink(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	admin := &stubAdmin{getFn: func(_ context.Context, tenantID int64, code string) (*link.AdminLink, error) {
		if tenantID != 1 {
			t.Errorf("tenant = %d, want 1", tenantID)
		}
		if code != "AbC123xy" {
			return nil, link.ErrNotFound
		}
		return &link.AdminLink{
			Link:        link.Link{ID: 7, Code: code, URL: "https://example.com/7", ExpiresAt: &expires},
			Clicks:      12,
			ClicksToday: 3,
		}, nil
	}}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	rec := doAdminRequest(t, handler, http.MethodGet, "/api/v1/links/AbC123xy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != "AbC123xy" || resp.Clicks != 12 || resp.ClicksToday != 3 || resp.ExpiresAt == nil {
		t.Errorf("response = %+v, want full admin link", resp)
	}

	rec = doAdminRequest(t, handler, http.MethodGet, "/api/v1/links/missing1", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing status = %d, want 404", rec.Code)
	}
}

func TestGetLinkRejectsInvalidCode(t *testing.T) {
	admin := &stubAdmin{getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
		t.Fatal("store was called for an invalid code")
		return nil, nil
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodGet, "/api/v1/links/short", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestUpdateLink(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var gotCode string
	var gotUpdate link.Update
	admin := &stubAdmin{updFn: func(_ context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
		if tenantID != 1 {
			t.Errorf("tenant = %d, want 1", tenantID)
		}
		gotCode = code
		gotUpdate = u
		result := link.AdminLink{Link: link.Link{ID: 7, Code: code, URL: "https://example.com/new"}}
		if u.URL != nil {
			result.URL = *u.URL
		}
		if u.ExpiresAt != nil {
			result.ExpiresAt = u.ExpiresAt
		}
		return &result, nil
	}}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	rec := doAdminRequest(t, handler, http.MethodPatch, "/api/v1/links/AbC123xy",
		`{"url":"https://example.com/new","expires_at":"`+future.Format(time.RFC3339)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if gotCode != "AbC123xy" {
		t.Errorf("code = %q, want AbC123xy", gotCode)
	}
	if gotUpdate.URL == nil || *gotUpdate.URL != "https://example.com/new" {
		t.Errorf("update URL = %v, want new URL", gotUpdate.URL)
	}
	if gotUpdate.ExpiresAt == nil || !gotUpdate.ExpiresAt.Equal(future) {
		t.Errorf("update ExpiresAt = %v, want %v", gotUpdate.ExpiresAt, future)
	}
	if gotUpdate.ClearExpiresAt {
		t.Error("ClearExpiresAt = true, want false")
	}

	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != "https://example.com/new" {
		t.Errorf("response URL = %q, want new URL", resp.URL)
	}
}

func TestUpdateLinkClearExpiresAt(t *testing.T) {
	var got link.Update
	admin := &stubAdmin{updFn: func(_ context.Context, _ int64, code string, u link.Update) (*link.AdminLink, error) {
		got = u
		return &link.AdminLink{Link: link.Link{ID: 7, Code: code, URL: "https://example.com"}}, nil
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodPatch,
		"/api/v1/links/AbC123xy", `{"expires_at": null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !got.ClearExpiresAt || got.ExpiresAt != nil || got.URL != nil {
		t.Errorf("update = %+v, want only ClearExpiresAt", got)
	}
}

func TestUpdateLinkBadRequest(t *testing.T) {
	admin := &stubAdmin{updFn: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
		t.Fatal("store was called for an invalid update")
		return nil, nil
	}}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	tests := []struct {
		name   string
		body   string
		errSub string
	}{
		{"empty body", "", "invalid JSON body"},
		{"empty object", `{}`, "at least one"},
		{"unknown field", `{"code":"AbC123xy"}`, "unknown field"},
		{"url not a string", `{"url":1}`, "invalid url"},
		{"url null", `{"url":null}`, "invalid url"},
		{"url invalid", `{"url":"ftp://example.com"}`, "only http and https"},
		{"expires_at number", `{"expires_at":1}`, "invalid expires_at"},
		{"expires_at not a time", `{"expires_at":"tomorrow"}`, "invalid expires_at"},
		{"expires_at in the past", `{"expires_at":"2000-01-01T00:00:00Z"}`, "must be in the future"},
		{"second value", `{"url":"https://example.com"}{"url":"https://example.com"}`, "invalid JSON body"},
		{"too large", `{"url":"https://example.com","x":"` + strings.Repeat("a", maxBodyBytes) + `"}`, "request body too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doAdminRequest(t, handler, http.MethodPatch, "/api/v1/links/AbC123xy", tt.body)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 400/413, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.errSub) {
				t.Errorf("body = %s, want substring %q", rec.Body.String(), tt.errSub)
			}
		})
	}
}

func TestUpdateLinkNotFound(t *testing.T) {
	admin := &stubAdmin{updFn: func(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
		return nil, link.ErrNotFound
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodPatch,
		"/api/v1/links/AbC123xy", `{"url":"https://example.com"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteLink(t *testing.T) {
	var gotCode string
	admin := &stubAdmin{delFn: func(_ context.Context, tenantID int64, code string) error {
		if tenantID != 1 {
			t.Errorf("tenant = %d, want 1", tenantID)
		}
		gotCode = code
		return nil
	}}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	rec := doAdminRequest(t, handler, http.MethodDelete, "/api/v1/links/AbC123xy", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotCode != "AbC123xy" {
		t.Errorf("code = %q, want AbC123xy", gotCode)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func TestDeleteLinkNotFound(t *testing.T) {
	admin := &stubAdmin{delFn: func(context.Context, int64, string) error { return link.ErrNotFound }}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodDelete, "/api/v1/links/AbC123xy", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAdminRoutesRequireAPIKey(t *testing.T) {
	admin := &stubAdmin{}
	handler := newAdminTestServer(admin).ConsoleRoutes()

	tests := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/links"},
		{http.MethodGet, "/api/v1/links/AbC123xy"},
		{http.MethodPatch, "/api/v1/links/AbC123xy"},
		{http.MethodDelete, "/api/v1/links/AbC123xy"},
		{http.MethodGet, "/api/v1/links/AbC123xy/stats"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`)))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestRedirectorHasNoAdminRoutes(t *testing.T) {
	admin := &stubAdmin{}
	srv := newServer(serverDeps{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          memorystore.New(),
		admin:          admin,
		stats:          admin,
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
	})
	handler := srv.RedirectRoutes()

	for _, path := range []string{
		"/api/v1/links", "/api/v1/links/AbC123xy", "/api/v1/links/AbC123xy/stats",
	} {
		rec := doAdminRequest(t, handler, http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestAdminStoreErrorIs500(t *testing.T) {
	admin := &stubAdmin{getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
		return nil, errors.New("db down")
	}}
	rec := doAdminRequest(t, newAdminTestServer(admin).ConsoleRoutes(), http.MethodGet, "/api/v1/links/AbC123xy", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// slices 用于比较类别顺序辅助函数的输出；放在这里避免测试文件之间重复导入。
