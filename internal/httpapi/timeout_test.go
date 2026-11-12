package httpapi

import (
	"context"
	"errors"
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

// TestTimeoutCancelsHandlerContext 验证到点后 handler 拿到的 context 变成 DeadlineExceeded。
func TestTimeoutCancelsHandlerContext(t *testing.T) {
	var ctxErr error
	handler := timeout(20 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		ctxErr = r.Context().Err()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", ctxErr)
	}
}

// timeoutStore 把存储调用阻塞到请求 context 结束，模拟慢查询。
// 错误照真实 store 的写法用 %w 包装，验证 errors.Is 能穿透。
type timeoutStore struct{}

func (timeoutStore) Create(ctx context.Context, _ *link.Link) error {
	<-ctx.Done()
	return fmt.Errorf("create link: %w", ctx.Err())
}

func (timeoutStore) GetByCode(ctx context.Context, _ string) (*link.Link, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("get link: %w", ctx.Err())
}

/*
TestRoutesGatewayTimeout 验证慢查询到点后 handler 返回 504 和 JSON 错误体，
而不是把连接一直挂到客户端放弃、访问日志还记成成功。
*/
func TestRoutesGatewayTimeout(t *testing.T) {
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), timeoutStore{}, 20*time.Millisecond)
	handler := srv.Routes()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create", http.MethodPost, "/api/v1/links", `{"url":"https://example.com"}`},
		{"redirect", http.MethodGet, "/r/abc12345", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			if rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(rec.Body.String(), "request timed out") {
				t.Errorf("body = %q, want it to mention the timeout", rec.Body.String())
			}
		})
	}
}
