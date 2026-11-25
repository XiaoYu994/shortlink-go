package httpx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shortlink/internal/logx"
)

func TestRequestIDGeneratesServerIDAndTracksValidClientHeader(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
	var gotServerID, gotClientID string
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
			gotServerID = RequestIDFrom(r.Context())
			gotClientID = ClientRequestIDFrom(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	req.Header.Set(requestIDHeader, "gw.7_abc-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	respID := rec.Header().Get(requestIDHeader)
	if !validRequestID(respID) || respID == "gw.7_abc-01" {
		t.Fatalf("%s = %q, want newly generated server id != inbound header", requestIDHeader, respID)
	}
	if gotServerID != respID {
		t.Errorf("RequestIDFrom(ctx) = %q, want %q", gotServerID, respID)
	}
	if gotClientID != "gw.7_abc-01" {
		t.Errorf("ClientRequestIDFrom(ctx) = %q, want gw.7_abc-01", gotClientID)
	}
	entry := oneLogEntry(t, &buf)
	if entry["request_id"] != respID || entry["client_request_id"] != "gw.7_abc-01" {
		t.Errorf("access log = %+v, want request_id=%q client_request_id=gw.7_abc-01", entry, respID)
	}
}

func TestRequestIDRejectsUnsafeInboundClientHeader(t *testing.T) {
	tests := []struct {
		name    string
		inbound string
	}{
		{"space", "has space"},
		{"newline", "line\nbreak"},
		{"slash", "bad/id"},
		{"too long", strings.Repeat("a", maxRequestIDLen+1)},
	}
	var gotClientID string
	srv := newTestServer()
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
			gotClientID = ClientRequestIDFrom(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClientID = "stale"
			req := httptest.NewRequest(http.MethodGet, "/check", nil)
			req.Header.Set(requestIDHeader, tt.inbound)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get(requestIDHeader)
			if got == tt.inbound || !validRequestID(got) {
				t.Errorf("%s = %q, want a generated id", requestIDHeader, got)
			}
			if gotClientID != "" {
				t.Errorf("ClientRequestIDFrom = %q, want empty for invalid header", gotClientID)
			}
		})
	}
}

func TestRequestIDDiffersAcrossRequests(t *testing.T) {
	srv := newTestServer()
	handler := srv.Handler(nil)
	seen := map[string]bool{}
	for range 20 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		id := rec.Header().Get(requestIDHeader)
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

func TestRequestIDTiesErrorAndAccessLog(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
	var ctxRequestID string
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
			ctxRequestID = RequestIDFrom(r.Context())
			HandleStoreError(srv.Logger(), w, r, "load item", errors.New("db down"), "code", "AbC123xy")
		})
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

	id := rec.Header().Get(requestIDHeader)
	if !validRequestID(id) {
		t.Fatalf("%s = %q, want a valid id", requestIDHeader, id)
	}
	if ctxRequestID != id {
		t.Errorf("RequestIDFrom(ctx) = %q, want %q", ctxRequestID, id)
	}
	assertLogsShareRequestID(t, &buf, id, "load item", "request")
	for _, entry := range logEntries(t, &buf) {
		if entry["msg"] == "load item" && entry["code"] != "AbC123xy" {
			t.Errorf("code = %v, want AbC123xy", entry["code"])
		}
	}
}

func TestRequestIDTiesPanicAndAccessLog(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /r/abc", func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/abc", nil))

	id := rec.Header().Get(requestIDHeader)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertLogsShareRequestID(t, &buf, id, "panic", "request")
}

func TestRequestIDAbsentWithoutContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(logx.WrapContext(slog.NewJSONHandler(&buf, nil)))
	logger.Info("listening")

	if got := RequestIDFrom(context.Background()); got != "" {
		t.Errorf("RequestIDFrom(Background) = %q, want empty", got)
	}
	entry := oneLogEntry(t, &buf)
	if _, ok := entry["request_id"]; ok {
		t.Errorf("startup log has request_id: %+v", entry)
	}
}

func TestNewWithLogxLoggerDoesNotDuplicateRequestID(t *testing.T) {
	var buf bytes.Buffer
	baseLogger, _ := logx.New(&buf, logx.Options{Format: logx.FormatJSON, Level: slog.LevelInfo, Service: "redirector"})
	srv := New(baseLogger, 5*time.Second, newTestRegistry())
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
			srv.Logger().InfoContext(r.Context(), "inside handler")
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(requestIDHeader, "req-dedup-01")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	for _, line := range lines {
		if count := strings.Count(string(line), `"request_id":`); count != 1 {
			t.Errorf(`"request_id": appeared %d times in line: %s`, count, line)
		}
	}
}

func assertLogsShareRequestID(t *testing.T, buf *bytes.Buffer, id string, msgs ...string) {
	t.Helper()
	found := map[string]bool{}
	for _, entry := range logEntries(t, buf) {
		msg, _ := entry["msg"].(string)
		found[msg] = true
		if entry["request_id"] != id {
			t.Errorf("%s request_id = %v, want %s", msg, entry["request_id"], id)
		}
	}
	for _, msg := range msgs {
		if !found[msg] {
			t.Errorf("missing log %q\n%s", msg, buf.String())
		}
	}
}
