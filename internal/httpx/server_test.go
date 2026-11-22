package httpx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// newTestRegistry 返回一个独立的 Registry，并注册 Go 运行时指标。
// 每次新建，避免测试之间重复注册同名指标。
func newTestRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	return registry
}

// newTestServer 返回一个不输出日志的底座 Server，供各个测试复用。
func newTestServer() *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, newTestRegistry())
}

/*
这是 httptest 的写法示例：不启动真正的服务器，直接把请求交给 handler。
*/
func TestHealthz(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Body.String(), "ok\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestLoggerIncludesRequestID(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
			srv.Logger().InfoContext(r.Context(), "inside handler")
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(requestIDHeader, "req-test-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	assertLogsShareRequestID(t, &buf, "req-test-01", "inside handler", "request")
}
