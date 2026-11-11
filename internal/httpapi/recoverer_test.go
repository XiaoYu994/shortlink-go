package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecovererPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := recoverer(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if buf.Len() != 0 {
		t.Errorf("log = %s, want empty", buf.String())
	}
}

func TestRecovererTurnsPanicIntoInternalError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/abc", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("body = %q, want internal error", rec.Body.String())
	}

	entry := oneLogEntry(t, &buf)
	if entry["msg"] != "panic" {
		t.Errorf("msg = %v, want panic", entry["msg"])
	}
	if entry["err"] != "boom" {
		t.Errorf("err = %v, want boom", entry["err"])
	}
	if entry["path"] != "/r/abc" {
		t.Errorf("path = %v, want /r/abc", entry["path"])
	}
	stack, _ := entry["stack"].(string)
	if !strings.Contains(stack, "recoverer") {
		t.Errorf("stack = %q, want it to include recoverer", stack)
	}
}

func TestRecovererRepanicsAbortHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Fatalf("recover() = %v, want http.ErrAbortHandler", rec)
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r/abc", nil))
	t.Fatal("ServeHTTP returned, want panic")
}
