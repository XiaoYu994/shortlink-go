package httpx

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
		{"operational route is absent", "/healthz", http.StatusNotFound},
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

// 剖析端点只存在于 PprofHandler，底座 Handler 默认不注册。
func TestOperationalHandlerDoesNotServePprof(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().Handler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /debug/pprof/ = %d, want 404", rec.Code)
	}
}
