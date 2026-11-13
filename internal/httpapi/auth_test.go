package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shortlink/internal/link"
	"shortlink/internal/memorystore"
	"shortlink/internal/tenant"
)

func TestAuthenticate(t *testing.T) {
	const otherKey = "slk_ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	tests := []struct {
		name       string
		header     string
		keys       APIKeyAuthenticator
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "basic", header: "Basic " + testAPIKey, wantStatus: http.StatusUnauthorized},
		{name: "malformed", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "unknown", header: "Bearer " + otherKey, wantStatus: http.StatusUnauthorized},
		{name: "ok", header: "Bearer " + testAPIKey, wantStatus: http.StatusNoContent},
		{name: "lowercase scheme", header: "bearer " + testAPIKey, wantStatus: http.StatusNoContent},
		{
			name:       "db error",
			header:     "Bearer " + testAPIKey,
			keys:       failingKey{err: errors.New("db down")},
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := tt.keys
			if keys == nil {
				keys = allowKey{}
			}
			var got tenant.Tenant
			var saw bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, saw = principalFrom(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), memorystore.New(), 5*time.Second, newTestRegistry(), nil, keys)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/links", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			srv.authenticate(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want Bearer", got)
				}
				if !strings.Contains(rec.Body.String(), "unauthorized") {
					t.Errorf("body = %s, want unauthorized", rec.Body.String())
				}
			}
			if tt.wantStatus == http.StatusNoContent {
				if !saw || got.ID != 1 {
					t.Errorf("principal = %+v, saw = %v, want tenant 1", got, saw)
				}
			}
		})
	}
}

func TestCreateAssignsTenantFromAPIKey(t *testing.T) {
	mem := memorystore.New()
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), mem, 5*time.Second, newTestRegistry(), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/tenant"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp createLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := mem.GetByCode(context.Background(), resp.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.TenantID != 1 {
		t.Errorf("TenantID = %d, want 1", got.TenantID)
	}
}

func TestCreateWithoutAPIKeyDoesNotStore(t *testing.T) {
	store := &countingStore{}
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if store.creates != 0 {
		t.Errorf("creates = %d, want 0", store.creates)
	}
}

type countingStore struct{ creates int }

func (s *countingStore) Create(context.Context, *link.Link) error {
	s.creates++
	return nil
}

func (s *countingStore) GetByCode(context.Context, string) (*link.Link, error) {
	return nil, link.ErrNotFound
}

type failingKey struct{ err error }

func (f failingKey) Authenticate(context.Context, string) (tenant.Tenant, error) {
	return tenant.Tenant{}, f.err
}
