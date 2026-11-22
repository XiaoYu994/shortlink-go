package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTimeoutCancelsHandlerContext 验证到点后 handler 拿到的 context 变成 DeadlineExceeded。
func TestTimeoutCancelsHandlerContext(t *testing.T) {
	var ctxErr error
	handler := timeout(20 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		ctxErr = r.Context().Err()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", ctxErr)
	}
}

type overloadedError struct{}

func (overloadedError) Error() string   { return "overloaded" }
func (overloadedError) Temporary() bool { return true }

/*
TestHandleStoreErrorResponses 验证 HandleStoreError 把超时、临时过载和普通错误映射为一致的 HTTP 响应与日志，
并确认客户端提前断开时不写出错误响应。
*/
func TestHandleStoreErrorResponses(t *testing.T) {
	t.Run("gateway timeout returns 504 and access log", func(t *testing.T) {
		var buf bytes.Buffer
		srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 20*time.Millisecond, newTestRegistry())
		handler := srv.Handler(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				err := fmt.Errorf("slow query: %w", r.Context().Err())
				HandleStoreError(srv.Logger(), w, r, "load item", err, "code", "abc12345")
			})
		})

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))

		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if !strings.Contains(rec.Body.String(), "request timed out") {
			t.Errorf("body = %q, want it to mention the timeout", rec.Body.String())
		}

		var access map[string]any
		for _, entry := range logEntries(t, &buf) {
			switch entry["msg"] {
			case "request":
				access = entry
			case "request timed out":
				if entry["code"] != "abc12345" {
					t.Errorf("code = %v, want abc12345", entry["code"])
				}
			}
		}
		if access == nil {
			t.Fatalf("missing access log\n%s", buf.String())
		}
		if got := logStatus(t, access); got != http.StatusGatewayTimeout {
			t.Errorf("logged status = %d, want %d", got, http.StatusGatewayTimeout)
		}
	})

	t.Run("temporary overload returns 503", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := New(logger, 5*time.Second, newTestRegistry())
		handler := srv.Handler(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /busy", func(w http.ResponseWriter, r *http.Request) {
				HandleStoreError(srv.Logger(), w, r, "load item", overloadedError{})
			})
		})

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/busy", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if got, want := rec.Body.String(), `{"error":"service temporarily overloaded"}`; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
	})

	t.Run("client canceled writes nothing", func(t *testing.T) {
		var buf bytes.Buffer
		srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
		handler := srv.Handler(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /canceled", func(w http.ResponseWriter, r *http.Request) {
				HandleStoreError(srv.Logger(), w, r, "load item", context.Canceled)
			})
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/canceled", nil).WithContext(ctx))

		if rec.Code == http.StatusInternalServerError {
			t.Errorf("status = %d, want not 500", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
		entry := oneLogEntry(t, &buf)
		if got := logStatus(t, entry); got != statusClientClosedRequest {
			t.Errorf("logged status = %d, want %d", got, statusClientClosedRequest)
		}
	})
}
