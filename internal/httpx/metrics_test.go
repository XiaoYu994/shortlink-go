package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

/*
TestMetricsEndpoint 验证 /metrics 从注入的 Registry 收集指标。
先打一次 /healthz：HTTP 计数写在响应完成之后，没观测过的 label 也不会输出，
刚启动后的第一次抓取正文里还没有 shortlink_http_*。
*/
func TestMetricsEndpoint(t *testing.T) {
	srv := newTestServer()
	handler := srv.Handler(nil)

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
	if strings.Contains(body, "shortlink_rate_limited_total") {
		t.Error("httpx registered console-only metric shortlink_rate_limited_total")
	}
}

/*
抓取的 route 是 ServeMux 模式 "GET /metrics"，不是 URL 路径。
再打一次 /healthz，确认只有抓取被跳过。
*/
func TestHTTPMetricsSkipsMetricsPath(t *testing.T) {
	srv := newTestServer()
	handler := srv.Handler(nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", metricsPath, rec.Code, http.StatusOK)
	}

	metricsRoute := "GET " + metricsPath
	statusOK := strconv.Itoa(http.StatusOK)
	if got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues(metricsRoute, http.MethodGet, statusOK)); got != 0 {
		t.Errorf("requests{route=%q} = %v, want 0", metricsRoute, got)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues("GET /healthz", http.MethodGet, statusOK)); got != 1 {
		t.Errorf("requests{route=%q} = %v, want 1", "GET /healthz", got)
	}
	if got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues(metricsRoute, http.MethodGet, statusOK)); got != 0 {
		t.Errorf("requests{route=%q} = %v, want 0", metricsRoute, got)
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
		wantStatus int
		wantRoute  string
		wantMethod string
	}{
		{
			name:       "healthz",
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantRoute:  "GET /healthz",
		},
		{
			name:       "readyz not configured",
			method:     http.MethodGet,
			path:       "/readyz",
			wantStatus: http.StatusServiceUnavailable,
			wantRoute:  "GET /readyz",
		},
		{
			name:       "head",
			method:     http.MethodHead,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantRoute:  "GET /healthz",
		},
		{
			name:       "parameterized route ok",
			method:     http.MethodGet,
			path:       "/items/AbC123xy",
			wantStatus: http.StatusFound,
			wantRoute:  "GET /items/{id}",
		},
		{
			name:       "parameterized route 404",
			method:     http.MethodGet,
			path:       "/items/missing",
			wantStatus: http.StatusNotFound,
			wantRoute:  "GET /items/{id}",
		},
		{
			name:       "not found",
			method:     http.MethodGet,
			path:       "/nope",
			wantStatus: http.StatusNotFound,
			wantRoute:  routeUnmatched,
		},
		{
			name:       "method not allowed",
			method:     http.MethodPost,
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
			wantRoute:  routeUnmatched,
		},
		{
			name:       "unknown method",
			method:     "MADEUP",
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
			wantRoute:  routeUnmatched,
			wantMethod: methodOther,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, newTestRegistry())
			handler := srv.Handler(func(mux *http.ServeMux) {
				mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
					if r.PathValue("id") == "missing" {
						WriteError(srv.Logger(), r.Context(), w, http.StatusNotFound, "not found")
						return
					}
					http.Redirect(w, r, "https://example.com", http.StatusFound)
				})
			})

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

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

// TestHTTPMetricsRecordsPanicAs500 验证指标中间件在 recoverer 外面，panic 后的 500 也能记到。
func TestHTTPMetricsRecordsPanicAs500(t *testing.T) {
	srv := newTestServer()
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /boom = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	status := strconv.Itoa(http.StatusInternalServerError)
	got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues("GET /boom", http.MethodGet, status))
	if got != 1 {
		t.Errorf("requests{route=%q, method=%q, status=%q} = %v, want 1", "GET /boom", http.MethodGet, status, got)
	}
}
