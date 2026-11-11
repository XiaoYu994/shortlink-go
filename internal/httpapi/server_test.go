package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer 返回一个不输出日志的 Server，供各个测试复用。
func newTestServer() *Server {
	return NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

/*
这是 httptest 的写法示例：不启动真正的服务器，直接把请求交给 handler。
之后的 handleCreate、handleRedirect 测试可以照这个样子写。
*/
func TestHealthz(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Body.String(), "ok\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
