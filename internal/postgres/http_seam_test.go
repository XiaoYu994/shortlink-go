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

	"shortlink/internal/httpapi"
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

	issued, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID})
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

	expired, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.Key.ID); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	assertUnauthorizedCreate(t, handler, expired.Plaintext)

	revoked, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID})
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

	valid, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID})
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

func newConsoleHandler(store *Store) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpapi.NewConsole(logger, store, 5*time.Second, prometheus.NewRegistry(), store)
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
