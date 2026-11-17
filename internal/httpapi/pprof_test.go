package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPprofHandler(t *testing.T) {
	h := PprofHandler()
	tests := []struct {
		name string
		path string
		want int
	}{
		{"index", "/debug/pprof/", http.StatusOK},
		{"named profile", "/debug/pprof/goroutine?debug=1", http.StatusOK},
		{"cmdline", "/debug/pprof/cmdline", http.StatusOK},
		{"unknown profile", "/debug/pprof/nope", http.StatusNotFound},
		{"business route is absent", "/healthz", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("GET %s = %d, want %d", tt.path, rec.Code, tt.want)
			}
		})
	}
}

// 剖析端点只存在于 PprofHandler，业务端口上的两组路由都不能有。
func TestBusinessRoutesDoNotServePprof(t *testing.T) {
	srv := newTestServer()
	for name, h := range map[string]http.Handler{
		"redirector": srv.RedirectRoutes(),
		"console":    srv.ConsoleRoutes(),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET /debug/pprof/ = %d, want 404", rec.Code)
			}
		})
	}
}
