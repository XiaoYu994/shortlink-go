package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"shortlink/internal/link"
)

/*
TestMetricsEndpoint 验证 /metrics 从注入的 Registry 收集指标。
先打一次 /healthz：HTTP 计数写在响应完成之后，没观测过的 label 也不会输出，
刚启动后的第一次抓取正文里还没有 shortlink_http_*。
*/
func TestMetricsEndpoint(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Error("GET /metrics body does not contain go_goroutines")
	}
	if !strings.Contains(body, "shortlink_http_requests_total") {
		t.Error("GET /metrics body does not contain shortlink_http_requests_total")
	}
}

/*
TestHTTPMetricsRecordsRouteAndStatus 验证 counter 的 route、method、status label。
每个子测试用新的 Server 和 Registry，计数从 1 开始。
*/
func TestHTTPMetricsRecordsRouteAndStatus(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		store      LinkStore
		wantStatus int
		wantRoute  string
		wantMethod string
	}{
		{
			name:       "healthz",
			method:     http.MethodGet,
			path:       "/healthz",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusOK,
			wantRoute:  "GET /healthz",
		},
		{
			name:       "head",
			method:     http.MethodHead,
			path:       "/healthz",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusOK,
			wantRoute:  "GET /healthz",
		},
		{
			name:   "redirect",
			method: http.MethodGet,
			path:   "/r/AbC123xy",
			store: &stubStore{links: map[string]*link.Link{
				"AbC123xy": {Code: "AbC123xy", URL: "https://example.com"},
			}},
			wantStatus: http.StatusFound,
			wantRoute:  "GET /r/{code}",
		},
		{
			name:       "not found",
			method:     http.MethodGet,
			path:       "/nope",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusNotFound,
			wantRoute:  routeUnmatched,
		},
		{
			name:       "missing short code",
			method:     http.MethodGet,
			path:       "/r/AbC123xy",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusNotFound,
			wantRoute:  "GET /r/{code}",
		},
		{
			name:       "method not allowed",
			method:     http.MethodPost,
			path:       "/healthz",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusMethodNotAllowed,
			wantRoute:  routeUnmatched,
		},
		{
			name:       "unknown method",
			method:     "MADEUP",
			path:       "/healthz",
			store:      &stubStore{links: map[string]*link.Link{}},
			wantStatus: http.StatusMethodNotAllowed,
			wantRoute:  routeUnmatched,
			wantMethod: methodOther,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), tt.store, 5*time.Second, newTestRegistry())

			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}

			status := strconv.Itoa(tt.wantStatus)
			wantMethod := tt.method
			if tt.wantMethod != "" {
				wantMethod = tt.wantMethod
			}
			got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues(tt.wantRoute, wantMethod, status))
			if got != 1 {
				t.Errorf("requests{route=%q, method=%q, status=%q} = %v, want 1", tt.wantRoute, wantMethod, status, got)
			}
			if tt.method != wantMethod {
				got = testutil.ToFloat64(srv.metrics.requests.WithLabelValues(tt.wantRoute, tt.method, status))
				if got != 0 {
					t.Errorf("requests{route=%q, method=%q, status=%q} = %v, want 0", tt.wantRoute, tt.method, status, got)
				}
			}
			if n := testutil.CollectAndCount(srv.metrics.duration); n != 1 {
				t.Errorf("duration metrics = %d, want 1", n)
			}
		})
	}
}

// panicStore 让 handler panic，验证指标中间件能记到 recoverer 写出的 500。
type panicStore struct{}

func (panicStore) Create(context.Context, *link.Link) error { panic("boom") }

func (panicStore) GetByCode(context.Context, string) (*link.Link, error) { panic("boom") }

// TestHTTPMetricsRecordsPanicAs500 验证指标中间件在 recoverer 外面，panic 后的 500 也能记到。
func TestHTTPMetricsRecordsPanicAs500(t *testing.T) {
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), panicStore{}, 5*time.Second, newTestRegistry())

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /r/AbC123xy = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	status := strconv.Itoa(http.StatusInternalServerError)
	got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues("GET /r/{code}", http.MethodGet, status))
	if got != 1 {
		t.Errorf("requests{route=%q, method=%q, status=%q} = %v, want 1", "GET /r/{code}", http.MethodGet, status, got)
	}
}
