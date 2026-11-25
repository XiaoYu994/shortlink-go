package redirector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

func newTestRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	return registry
}

type stubStore struct {
	links map[string]*link.Link
	calls int
}

func (s *stubStore) GetByCode(_ context.Context, code string) (*link.Link, error) {
	s.calls++
	l, ok := s.links[code]
	if !ok {
		return nil, link.ErrNotFound
	}
	return l, nil
}

type recordSink struct {
	mu     sync.Mutex
	clicks []click.Click
}

func (s *recordSink) Record(c click.Click) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicks = append(s.clicks, c)
}

func TestRedirectSuccess(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC()
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 1, Code: "AbC123xy", URL: "https://example.com/path?q=1"},
		"ExP123xy": {ID: 2, Code: "ExP123xy", URL: "https://example.com/exp", ExpiresAt: &future},
	}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})
	handler := srv.Routes()

	for _, tt := range []struct {
		code string
		want string
	}{
		{code: "AbC123xy", want: "https://example.com/path?q=1"},
		{code: "ExP123xy", want: "https://example.com/exp"},
	} {
		t.Run(tt.code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/"+tt.code, nil))
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
			}
			if got := rec.Header().Get("Location"); got != tt.want {
				t.Errorf("Location = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedirectNotFound(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got, want := rec.Body.String(), `{"error":"link not found"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

/*
TestRedirectInvalidCode 验证非法短码在进入存储之前就被拒绝。
否则任意字符串都会先查 Redis 和数据库，超长 code 还会变成超长的缓存 key。
*/
func TestRedirectInvalidCode(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})
	handler := srv.Routes()

	tests := []struct {
		name string
		code string
	}{
		{"too short", "missing"},
		{"invalid character", "AbC123-y"},
		{"too long", strings.Repeat("A", 1024)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/"+tt.code, nil))

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
		})
	}
	if store.calls != 0 {
		t.Errorf("store GetByCode calls = %d, want 0", store.calls)
	}
}

func TestRedirectRecordsClick(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/recorded"},
	}}
	sink := &recordSink{}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{Clicks: sink})
	handler := srv.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/missing1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.clicks) != 1 || sink.clicks[0].LinkID != 42 {
		t.Errorf("recorded = %v, want link 42", sink.clicks)
	}
}

func TestRedirectHeadDoesNotRecordClick(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/recorded"},
	}}
	sink := &recordSink{}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{Clicks: sink})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/r/AbC123xy", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/recorded" {
		t.Errorf("Location = %q, want %q", loc, "https://example.com/recorded")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.clicks) != 0 {
		t.Errorf("recorded = %v, want none", sink.clicks)
	}
}

/*
TestRedirectCanceledDuringGetDoesNotRecordClick 模拟查询返回时客户端已经断开。
singleflight 的等待者会遇到这个窗口：存储仍返回链接，但响应已经没人接收。
*/
func TestRedirectCanceledDuringGetDoesNotRecordClick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancelOnGetStore{
		l:      &link.Link{ID: 7, Code: "AbC123xy", URL: "https://example.com/x"},
		cancel: cancel,
	}
	sink := &recordSink{}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{Clicks: sink})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusFound {
		t.Errorf("status = %d, want request to stop before the redirect", rec.Code)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.clicks) != 0 {
		t.Errorf("recorded = %v, want none", sink.clicks)
	}
}

type cancelOnGetStore struct {
	l      *link.Link
	cancel context.CancelFunc
}

func (s *cancelOnGetStore) GetByCode(context.Context, string) (*link.Link, error) {
	s.cancel()
	return s.l, nil
}

func TestRedirectRecordsClickMetadata(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/target"},
	}}
	sink := &recordSink{}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{
		Clicks:            sink,
		VisitorSalt:       []byte("test-salt"),
		TrustProxyHeaders: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	req.Header.Set("Referer", "https://www.News.Example.com/story")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.clicks) != 1 {
		t.Fatalf("clicks = %d, want 1", len(sink.clicks))
	}
	got := sink.clicks[0]
	if got.LinkID != 42 || got.Referrer != "news.example.com" {
		t.Errorf("click = %+v, want link 42 and referrer news.example.com", got)
	}
	if got.Device != click.DeviceMobile || got.Client != click.ClientBrowser {
		t.Errorf("device/client = %q/%q, want mobile/browser", got.Device, got.Client)
	}
	if len(got.VisitorHash) != 16 {
		t.Errorf("VisitorHash length = %d, want 16", len(got.VisitorHash))
	}
	if got.At.IsZero() {
		t.Error("At is zero")
	}
}

/*
TestCodeAndRouteNamespaces 验证短码和路由分属不同命名空间：
/r/ 下的短码不会占用 /healthz，两个路径可以同时工作。
*/
func TestCodeAndRouteNamespaces(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"healthz1": {Code: "healthz1", URL: "https://example.com/healthz"},
	}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})
	handler := srv.Routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", rec.Code, rec.Body.String(), "ok\n")
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/healthz1", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/healthz" {
		t.Errorf("GET /r/healthz1 = %d Location=%q, want 302 %q", rec.Code, rec.Header().Get("Location"), "https://example.com/healthz")
	}
}

func TestRedirectClientCanceled(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Errorf("status = %d, want not 500", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestStoreOverloadedReturns503 验证存储层的临时过载错误映射成 503，而不是 500。
func TestStoreOverloadedReturns503(t *testing.T) {
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), overloadedStore{}, 5*time.Second, newTestRegistry(), Options{})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

type overloadedStore struct{}

func (overloadedStore) GetByCode(context.Context, string) (*link.Link, error) {
	return nil, overloadedError{}
}

type overloadedError struct{}

func (overloadedError) Error() string   { return "overloaded" }
func (overloadedError) Temporary() bool { return true }

type timeoutStore struct{}

func (timeoutStore) GetByCode(ctx context.Context, _ string) (*link.Link, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("get link: %w", ctx.Err())
}

func TestRedirectGatewayTimeout(t *testing.T) {
	var buf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), timeoutStore{}, 20*time.Millisecond, newTestRegistry(), Options{})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/abc12345", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "request timed out") {
		t.Errorf("body = %q, want it to mention the timeout", rec.Body.String())
	}

	var accessStatus float64
	var timeoutCode any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log: %v\n%s", err, line)
		}
		switch entry["msg"] {
		case "request":
			accessStatus, _ = entry["status"].(float64)
		case "request timed out":
			timeoutCode = entry["code"]
		}
	}
	if int(accessStatus) != http.StatusGatewayTimeout {
		t.Errorf("logged status = %v, want %d", accessStatus, http.StatusGatewayTimeout)
	}
	if timeoutCode != "abc12345" {
		t.Errorf("timeout log code = %v, want abc12345", timeoutCode)
	}
}

func TestRedirectMetricsRouteLabel(t *testing.T) {
	reg := newTestRegistry()
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com"},
	}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, reg, Options{})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var gotCount float64
	var found bool
	for _, mf := range families {
		if mf.GetName() == "shortlink_rate_limited_total" {
			t.Fatal("redirector registered console-only metric shortlink_rate_limited_total")
		}
		if mf.GetName() != "shortlink_http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["route"] == "GET /r/{code}" && labels["method"] == http.MethodGet && labels["status"] == "302" {
				found = true
				gotCount = m.GetCounter().GetValue()
			}
		}
	}
	if !found || gotCount != 1 {
		t.Errorf("requests{route=GET /r/{code}, method=GET, status=302} found=%v count=%v, want true/1", found, gotCount)
	}
}

/*
TestRedirectorRouteIsolation 验证 redirector 进程只提供公开跳转与运维端点，
管理、认证、成员和 pprof 路径一律返回 404。
*/
func TestRedirectorRouteIsolation(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{}}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), Options{})
	handler := srv.Routes()

	for _, rt := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/api/v1/links", body: `{"url":"https://example.com"}`},
		{method: http.MethodGet, path: "/api/v1/links"},
		{method: http.MethodGet, path: "/api/v1/links/AbC123xy"},
		{method: http.MethodGet, path: "/api/v1/links/AbC123xy/stats"},
		{method: http.MethodGet, path: "/api/v1/capabilities"},
		{method: http.MethodGet, path: "/api/v1/auth/oidc/login"},
		{method: http.MethodGet, path: "/api/v1/auth/oidc/callback"},
		{method: http.MethodGet, path: "/api/v1/auth/github/login"},
		{method: http.MethodGet, path: "/api/v1/auth/github/callback"},
		{method: http.MethodPost, path: "/api/v1/auth/logout"},
		{method: http.MethodGet, path: "/api/v1/me"},
		{method: http.MethodGet, path: "/api/v1/members"},
		{method: http.MethodPatch, path: "/api/v1/members/2", body: `{"role":"member"}`},
		{method: http.MethodDelete, path: "/api/v1/members/2"},
		{method: http.MethodGet, path: "/debug/pprof/"},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("redirector %s %s = %d, want 404", rt.method, rt.path, rec.Code)
		}
	}

	for _, path := range []string{"/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("redirector GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestRedirectVisitorHashAndAccessLogShareClientIP(t *testing.T) {
	var buf bytes.Buffer
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/target"},
	}}
	sink := &recordSink{}
	salt := []byte("test-salt")
	srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), store, 5*time.Second, newTestRegistry(), Options{
		Clicks:            sink,
		VisitorSalt:       salt,
		TrustProxyHeaders: true,
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	req.RemoteAddr = "10.0.0.5:4567"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.2")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decode access log: %v\n%s", err, buf.String())
	}
	if entry["ip"] != "203.0.113.9" {
		t.Errorf("access log ip = %v, want 203.0.113.9", entry["ip"])
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.clicks) != 1 {
		t.Fatalf("clicks = %d, want 1", len(sink.clicks))
	}
	got := sink.clicks[0]
	want := click.NewClick(42, got.At, "", req.UserAgent(), "203.0.113.9", salt)
	if !bytes.Equal(got.VisitorHash, want.VisitorHash) {
		t.Errorf("VisitorHash = %x, want %x (for client IP 203.0.113.9)", got.VisitorHash, want.VisitorHash)
	}
}
