package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shortlink/internal/logx"
)

func TestLoggingRecordsStatus(t *testing.T) {
	tests := []struct {
		name   string
		handle func(http.ResponseWriter, *http.Request)
		want   int
	}{
		{
			name:   "explicit status",
			handle: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
			want:   http.StatusNoContent,
		},
		{
			name: "body without header",
			handle: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			},
			want: http.StatusOK,
		},
		{
			name: "first header wins",
			handle: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: http.StatusCreated,
		},
		{
			name: "handler writes nothing",
			// 没写响应时，服务器最终会补 200。NewRecorder 的 Code 也从 200 起。
			handle: func(http.ResponseWriter, *http.Request) {},
			want:   http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			handler := logging(logger)(http.HandlerFunc(tt.handle))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/links?token=secret", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if strings.Contains(buf.String(), "secret") {
				t.Errorf("log contains query secret: %s", buf.String())
			}
			entry := oneLogEntry(t, &buf)
			if entry["msg"] != "request" {
				t.Errorf("msg = %v, want request", entry["msg"])
			}
			if entry["method"] != http.MethodPost {
				t.Errorf("method = %v, want %s", entry["method"], http.MethodPost)
			}
			if entry["path"] != "/api/v1/links" {
				t.Errorf("path = %v, want /api/v1/links", entry["path"])
			}
			if got := logStatus(t, entry); got != tt.want {
				t.Errorf("logged status = %d, want %d", got, tt.want)
			}
			ms, ok := entry["duration_ms"].(float64)
			if !ok || ms < 0 {
				t.Errorf("log duration_ms = %#v, want non-negative number", entry["duration_ms"])
			}
			if _, ok := entry["bytes"].(float64); !ok {
				t.Errorf("log missing numeric bytes: %+v", entry)
			}
		})
	}
}

func TestRoutesLogRequest(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())

	rec := httptest.NewRecorder()
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("pong"))
		})
	})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.RemoteAddr = "198.51.100.9:12345"
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	entry := oneLogEntry(t, &buf)
	if entry["path"] != "/ping" || entry["route"] != "GET /ping" || entry["ip"] != "198.51.100.9" {
		t.Errorf("access log = %+v", entry)
	}
	if entry["bytes"] != float64(4) {
		t.Errorf("bytes = %v, want 4", entry["bytes"])
	}
	if got := logStatus(t, entry); got != http.StatusOK {
		t.Errorf("logged status = %d, want %d", got, http.StatusOK)
	}
}

func TestLoggingProbeLevels(t *testing.T) {
	t.Run("2xx healthz and readyz suppressed at info and logged at debug", func(t *testing.T) {
		var buf bytes.Buffer
		var level slog.LevelVar
		level.Set(slog.LevelInfo)
		srv := New(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: &level})), 5*time.Second, newTestRegistry())
		srv.SetReadiness()
		handler := srv.Handler(nil)

		for _, path := range []string{"/healthz", "/readyz"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
		}
		if buf.Len() != 0 {
			t.Fatalf("probe 200 logged at info level: %s", buf.String())
		}

		level.Set(slog.LevelDebug)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		entry := oneLogEntry(t, &buf)
		if entry["level"] != "DEBUG" || entry["path"] != "/healthz" {
			t.Errorf("probe debug log = %+v", entry)
		}
	})

	t.Run("non-2xx readyz still logged at info level", func(t *testing.T) {
		var buf bytes.Buffer
		srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
		handler := srv.Handler(nil)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET /readyz = %d, want 503", rec.Code)
		}

		entries := logEntries(t, &buf)
		var access map[string]any
		for _, entry := range entries {
			if entry["msg"] == "request" {
				access = entry
			}
		}
		if access == nil {
			t.Fatalf("missing access log: %s", buf.String())
		}
		if access["level"] != "INFO" || logStatus(t, access) != http.StatusServiceUnavailable {
			t.Errorf("probe non-2xx access log = %+v", access)
		}
	})
}

func TestLoggingSkipsMetrics(t *testing.T) {
	var buf bytes.Buffer
	handler := logging(slog.New(slog.NewJSONHandler(&buf, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, metricsPath, nil))

	if buf.Len() != 0 {
		t.Errorf("metrics request was logged: %s", buf.String())
	}
}

func oneLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	entries := logEntries(t, buf)
	if len(entries) != 1 {
		t.Fatalf("log lines = %d, want 1\n%s", len(entries), buf.String())
	}
	return entries[0]
}

func logEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log: %v\n%s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

func logStatus(t *testing.T, entry map[string]any) int {
	t.Helper()
	status, ok := entry["status"].(float64)
	if !ok {
		t.Fatalf("status = %#v, want number", entry["status"])
	}
	return int(status)
}

// TestLoggingRecordsClientCanceled 验证客户端断开、handler 没写响应时，访问日志记 499 而不是 200。
func TestLoggingRecordsClientCanceled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := logging(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// 真实 handler 在 IsClientGone 为 true 时直接 return，什么都不写。
	}))

	req := httptest.NewRequest(http.MethodGet, "/r/abc", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	entry := oneLogEntry(t, &buf)
	if got := logStatus(t, entry); got != statusClientClosedRequest {
		t.Errorf("logged status = %d, want %d", got, statusClientClosedRequest)
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
}

func TestCanonicalLogLineEnrichmentAndGrading(t *testing.T) {
	tests := []struct {
		name           string
		treat404AsInfo bool
		slowThreshold  time.Duration
		handle         func(srv *Server, w http.ResponseWriter, r *http.Request)
		wantLevel      string
		check          func(t *testing.T, entry map[string]any)
	}{
		{
			name:      "2xx with cache and actor",
			wantLevel: "INFO",
			handle: func(_ *Server, w http.ResponseWriter, r *http.Request) {
				logx.SetActor(r.Context(), 7, "api_key", 10)
				logx.SetCacheResult(r.Context(), "hit")
				w.WriteHeader(http.StatusFound)
			},
			check: func(t *testing.T, entry map[string]any) {
				if entry["tenant_id"] != float64(7) || entry["actor"] != "api_key:10" || entry["cache"] != "hit" {
					t.Errorf("enriched entry = %+v", entry)
				}
			},
		},
		{
			name:      "403 error includes err_msg at warn level",
			wantLevel: "WARN",
			handle: func(srv *Server, w http.ResponseWriter, r *http.Request) {
				logx.SetActor(r.Context(), 9, "user", 3)
				WriteError(srv.Logger(), r.Context(), w, http.StatusForbidden, "forbidden")
			},
			check: func(t *testing.T, entry map[string]any) {
				if entry["err_msg"] != "forbidden" || entry["actor"] != "user:3" || entry["tenant_id"] != float64(9) {
					t.Errorf("403 entry = %+v", entry)
				}
			},
		},
		{
			name:      "429 rate limited includes retry_after",
			wantLevel: "WARN",
			handle: func(srv *Server, w http.ResponseWriter, r *http.Request) {
				logx.SetRateLimited(r.Context(), 5)
				WriteError(srv.Logger(), r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
			},
			check: func(t *testing.T, entry map[string]any) {
				if entry["retry_after"] != float64(5) || entry["err_msg"] != "rate limit exceeded" {
					t.Errorf("429 entry = %+v", entry)
				}
			},
		},
		{
			name:           "404 default warn vs treat404AsInfo",
			treat404AsInfo: false,
			wantLevel:      "WARN",
			handle: func(srv *Server, w http.ResponseWriter, r *http.Request) {
				WriteError(srv.Logger(), r.Context(), w, http.StatusNotFound, "link not found")
			},
		},
		{
			name:           "redirector 404 stays info",
			treat404AsInfo: true,
			wantLevel:      "INFO",
			handle: func(srv *Server, w http.ResponseWriter, r *http.Request) {
				WriteError(srv.Logger(), r.Context(), w, http.StatusNotFound, "link not found")
			},
		},
		{
			name:      "500 error level",
			wantLevel: "ERROR",
			handle: func(srv *Server, w http.ResponseWriter, r *http.Request) {
				WriteError(srv.Logger(), r.Context(), w, http.StatusInternalServerError, "internal error")
			},
		},
		{
			name:          "slow 200 promoted to warn",
			slowThreshold: time.Millisecond,
			wantLevel:     "WARN",
			handle: func(_ *Server, w http.ResponseWriter, _ *http.Request) {
				time.Sleep(5 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
			},
			check: func(t *testing.T, entry map[string]any) {
				if entry["slow"] != true {
					t.Errorf("slow = %v, want true", entry["slow"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
			srv.SetAccessLog(AccessLogOptions{
				SampleRate:     1,
				SlowThreshold:  tt.slowThreshold,
				Treat404AsInfo: tt.treat404AsInfo,
			})
			handler := srv.Handler(func(mux *http.ServeMux) {
				mux.HandleFunc("GET /test", func(w http.ResponseWriter, r *http.Request) {
					tt.handle(srv, w, r)
				})
			})
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/test", nil))

			entry := oneLogEntry(t, &buf)
			if entry["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %s (entry=%+v)", entry["level"], tt.wantLevel, entry)
			}
			if tt.check != nil {
				tt.check(t, entry)
			}
		})
	}
}

func TestAccessLogSamplingRetainsErrorsAndSlowRequests(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
	srv.SetAccessLog(AccessLogOptions{
		SampleRate:     0,
		SlowThreshold:  2 * time.Millisecond,
		Treat404AsInfo: true,
	})
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) {
			WriteError(srv.Logger(), r.Context(), w, http.StatusNotFound, "link not found")
		})
		mux.HandleFunc("GET /forbidden", func(w http.ResponseWriter, r *http.Request) {
			WriteError(srv.Logger(), r.Context(), w, http.StatusForbidden, "forbidden")
		})
		mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
			WriteError(srv.Logger(), r.Context(), w, http.StatusInternalServerError, "internal error")
		})
		mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(5 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		})
	})

	for _, path := range []string{"/ok", "/missing", "/forbidden", "/boom", "/slow"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	entries := logEntries(t, &buf)
	if len(entries) != 3 {
		t.Fatalf("sampled entries = %d, want 3 (/forbidden, /boom, /slow):\n%s", len(entries), buf.String())
	}
	gotPaths := map[string]bool{}
	for _, entry := range entries {
		p, _ := entry["path"].(string)
		gotPaths[p] = true
	}
	for _, want := range []string{"/forbidden", "/boom", "/slow"} {
		if !gotPaths[want] {
			t.Errorf("missing retained path %s in %+v", want, gotPaths)
		}
	}
}
