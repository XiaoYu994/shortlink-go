package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// 剖析与动态调级端点只存在于 DebugHandler，底座 Handler 默认不注册。
func TestOperationalHandlerDoesNotServePprof(t *testing.T) {
	handler := newTestServer().Handler(nil)
	for _, path := range []string{"/debug/pprof/", "/debug/log/level"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestDebugHandlerLogLevel(t *testing.T) {
	t.Run("PprofHandler without levelVar returns 404 on log level endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		PprofHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/log/level", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /debug/log/level = %d, want 404", rec.Code)
		}
	})

	t.Run("GET and PUT /debug/log/level", func(t *testing.T) {
		var level slog.LevelVar
		level.Set(slog.LevelInfo)
		h := DebugHandler(&level)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/log/level", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", rec.Code)
		}
		var body logLevelPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Level != "INFO" {
			t.Fatalf("GET body = %q, err = %v", rec.Body.String(), err)
		}

		// 查询参数更新为 debug。
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/debug/log/level?level=debug", nil))
		if rec.Code != http.StatusOK || level.Level() != slog.LevelDebug {
			t.Fatalf("PUT query status=%d level=%v", rec.Code, level.Level())
		}

		// JSON 请求体更新为 warn。
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/debug/log/level", strings.NewReader(`{"level":"warn"}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || level.Level() != slog.LevelWarn {
			t.Fatalf("PUT json status=%d level=%v", rec.Code, level.Level())
		}

		// 非法级别返回 400 且不改变现有级别。
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPut, "/debug/log/level", strings.NewReader(`{"level":"verbose"}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || level.Level() != slog.LevelWarn {
			t.Fatalf("PUT invalid status=%d level=%v", rec.Code, level.Level())
		}
	})
}
