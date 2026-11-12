package httpapi

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

	"shortlink/internal/link"
)

func TestRequestIDKeepsValidInboundHeader(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(requestIDHeader, "gw.7_abc-01")

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if got := rec.Header().Get(requestIDHeader); got != "gw.7_abc-01" {
		t.Errorf("%s = %q, want %q", requestIDHeader, got, "gw.7_abc-01")
	}
}

func TestRequestIDReplacesUnsafeInboundHeader(t *testing.T) {
	tests := []struct {
		name    string
		inbound string
	}{
		{"space", "has space"},
		{"newline", "line\nbreak"},
		{"slash", "bad/id"},
		{"too long", strings.Repeat("a", maxRequestIDLen+1)},
	}
	srv := newTestServer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Header.Set(requestIDHeader, tt.inbound)

			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

			got := rec.Header().Get(requestIDHeader)
			if got == tt.inbound || !validRequestID(got) {
				t.Errorf("%s = %q, want a generated id", requestIDHeader, got)
			}
		})
	}
}

func TestRequestIDDiffersAcrossRequests(t *testing.T) {
	srv := newTestServer()
	seen := map[string]bool{}
	for range 20 {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		id := rec.Header().Get(requestIDHeader)
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

func TestRequestIDTiesErrorAndAccessLog(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(slog.New(slog.NewJSONHandler(&buf, nil)), failingStore{err: errors.New("db down")}, 5*time.Second, newTestRegistry())

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/abc", nil))

	id := rec.Header().Get(requestIDHeader)
	if !validRequestID(id) {
		t.Fatalf("%s = %q, want a valid id", requestIDHeader, id)
	}
	assertLogsShareRequestID(t, &buf, id, "get link", "request")
	for _, entry := range logEntries(t, &buf) {
		if entry["msg"] == "get link" && entry["code"] != "abc" {
			t.Errorf("code = %v, want abc", entry["code"])
		}
	}
}

func TestRequestIDTiesPanicAndAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(requestIDHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	handler := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), requestID(), logging(logger), recoverer(logger))

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
	logger := slog.New(requestIDHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	logger.Info("listening")

	entry := oneLogEntry(t, &buf)
	if _, ok := entry["request_id"]; ok {
		t.Errorf("startup log has request_id: %+v", entry)
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

type failingStore struct {
	err error
}

func (s failingStore) Create(context.Context, *link.Link) error { return s.err }

func (s failingStore) GetByCode(context.Context, string) (*link.Link, error) {
	return nil, s.err
}
