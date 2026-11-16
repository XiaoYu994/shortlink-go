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

	"shortlink/internal/memorystore"
)

func TestReadyz(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		srv := newTestServer()
		for _, routes := range []http.Handler{srv.RedirectRoutes(), srv.ConsoleRoutes()} {
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			assertReady(t, rec, http.StatusServiceUnavailable, "unavailable\n")
		}
	})

	// redirector 不探依赖：接了线、一项检查都没有，就是就绪。
	t.Run("wired without checks", func(t *testing.T) {
		srv := newTestServer()
		srv.SetReadiness()
		rec := httptest.NewRecorder()
		srv.RedirectRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assertReady(t, rec, http.StatusOK, "ok\n")
	})

	t.Run("dependencies up", func(t *testing.T) {
		srv := newTestServer()
		srv.SetReadiness(
			ReadyCheck{Name: "postgres", Ping: func(context.Context) error { return nil }},
			ReadyCheck{Name: "redis", Ping: func(context.Context) error { return nil }},
		)
		rec := httptest.NewRecorder()
		srv.RedirectRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assertReady(t, rec, http.StatusOK, "ok\n")
	})

	t.Run("stops at first failure", func(t *testing.T) {
		srv := newTestServer()
		redisCalled := false
		srv.SetReadiness(
			ReadyCheck{Name: "postgres", Ping: func(context.Context) error { return errors.New("db down") }},
			ReadyCheck{Name: "redis", Ping: func(context.Context) error {
				redisCalled = true
				return nil
			}},
		)
		rec := httptest.NewRecorder()
		srv.ConsoleRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assertReady(t, rec, http.StatusServiceUnavailable, "unavailable\n")
		if redisCalled {
			t.Error("redis was checked after postgres failed")
		}

		health := httptest.NewRecorder()
		srv.ConsoleRoutes().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		assertReady(t, health, http.StatusOK, "ok\n")
	})

	t.Run("nil ping", func(t *testing.T) {
		srv := newTestServer()
		srv.SetReadiness(ReadyCheck{Name: "postgres"})
		rec := httptest.NewRecorder()
		srv.RedirectRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assertReady(t, rec, http.StatusServiceUnavailable, "unavailable\n")
	})

	t.Run("canceled probe is not logged", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
		srv := testServer(logger, memorystore.New(), 5*time.Second, newTestRegistry(), nil)
		srv.SetReadiness(ReadyCheck{Name: "postgres", Ping: func(ctx context.Context) error {
			return ctx.Err()
		}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.RedirectRoutes().ServeHTTP(rec, req)
		assertReady(t, rec, http.StatusServiceUnavailable, "unavailable\n")
		if logs.Len() != 0 {
			t.Errorf("log = %q, want empty", logs.String())
		}
	})
}

func assertReady(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d", rec.Code, status)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}
