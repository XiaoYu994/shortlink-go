package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shortlink/internal/memorystore"
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
			if _, ok := entry["duration"]; !ok {
				t.Errorf("log missing duration: %+v", entry)
			}
		})
	}
}

func TestRoutesLogRequest(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(slog.New(slog.NewJSONHandler(&buf, nil)), memorystore.New())

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	entry := oneLogEntry(t, &buf)
	if entry["path"] != "/healthz" {
		t.Errorf("path = %v, want /healthz", entry["path"])
	}
	if got := logStatus(t, entry); got != http.StatusOK {
		t.Errorf("logged status = %d, want %d", got, http.StatusOK)
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
		// 真实 handler 在 clientGone 为 true 时直接 return，什么都不写。
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
}
