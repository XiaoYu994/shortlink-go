package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shortlink/internal/link"
)

// quotaStore 的 Create 返回包了一层的配额错误，验证 handler 用 errors.Is 判断。
type quotaStore struct{}

func (quotaStore) Create(context.Context, *link.Link) error {
	return fmt.Errorf("insert link: %w", link.ErrQuotaExceeded)
}

/*
TestCreateQuotaExceeded 验证配额不足映射成 403 和 JSON 错误体，
而不是当成存储故障返回 500。
*/
func TestCreateQuotaExceeded(t *testing.T) {
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), quotaStore{}, 5*time.Second, newTestRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/quota"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got, want := rec.Body.String(), `{"error":"link quota exceeded"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
