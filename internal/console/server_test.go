package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/identity"
	"shortlink/internal/idgen"
	"shortlink/internal/link"
	"shortlink/internal/memorystore"
	"shortlink/internal/tenant"
)

// testAPIKey 是测试用的固定密钥，符合 slk_ 加 64 位小写十六进制。
const testAPIKey = "slk_00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// allowKey 只接受 testAPIKey，并把它映射成租户 1 的全权限 API Key。
type allowKey struct{}

func (allowKey) Authenticate(_ context.Context, token string) (identity.Principal, error) {
	if token == testAPIKey {
		return identity.Principal{
			Tenant:    tenant.Tenant{ID: 1},
			ActorKind: identity.ActorAPIKey,
			ActorID:   1,
			Scopes:    identity.AllAPIKeyScopes(),
		}, nil
	}
	return identity.Principal{}, identity.ErrUnauthenticated
}

func authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
}

// testServer 用测试密钥组装 Server，避免每个用例都传一遍 authenticator。
// 只注入 LinkCreator；需要管理路由的用例用 newAdminTestServer 或 New。
func testServer(logger *slog.Logger, store LinkCreator, requestTimeout time.Duration, registry *prometheus.Registry) *Server {
	return newServer(serverDeps{
		logger:         logger,
		store:          store,
		requestTimeout: requestTimeout,
		registry:       registry,
		keys:           allowKey{},
		authorizer:     identity.NewMapAuthorizer(),
	})
}

// newTestRegistry 返回一个独立的 Registry，并注册 Go 运行时指标。
// 每次新建，避免测试之间重复注册同名指标。
func newTestRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	return registry
}

// newTestServer 返回一个不输出日志、使用内存存储的 Server，供各个测试复用。
// 请求超时给得足够宽，只有专门测超时的用例才用短时限。
func newTestServer() *Server {
	return testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), memorystore.New(), 5*time.Second, newTestRegistry())
}

func httpRequestCount(t *testing.T, reg *prometheus.Registry, route, method string, status int) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	wantStatus := fmt.Sprintf("%d", status)
	for _, mf := range families {
		if mf.GetName() != "shortlink_http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["route"] == route && labels["method"] == method && labels["status"] == wantStatus {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
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

// stubStore 只满足 New 的 Store 接口，供不访问链接存储的成员路由或隔离测试使用。
type stubStore struct{}

func (*stubStore) Create(context.Context, *link.Link) error { return nil }
func (*stubStore) List(context.Context, link.ListOptions) ([]link.AdminLink, error) {
	return nil, nil
}
func (*stubStore) GetForTenant(context.Context, int64, string) (*link.AdminLink, error) {
	return nil, link.ErrNotFound
}
func (*stubStore) Update(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
	return nil, link.ErrNotFound
}
func (*stubStore) Delete(context.Context, int64, string) error { return link.ErrNotFound }
func (*stubStore) Stats(context.Context, int64, string, time.Time, time.Time) (*link.Stats, error) {
	return nil, link.ErrNotFound
}

/*
TestConsoleRouteIsolation 验证 console 进程的路由边界：
只挂载管理 API 与运维端点，公开跳转与剖析端点一律 404。
*/
func TestConsoleRouteIsolation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	console := New(logger, &stubStore{}, 5*time.Second, newTestRegistry(), Options{
		Keys:       allowKey{},
		Authorizer: identity.NewMapAuthorizer(),
	})
	handler := console.Routes()

	t.Run("console creates", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/new"}`))
		authorize(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
	})

	t.Run("console has no redirect or pprof route", func(t *testing.T) {
		for _, path := range []string{"/r/AbC123xy", "/debug/pprof/"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want %d", path, rec.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("console exposes operational endpoints", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
		}

		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /metrics = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), "go_goroutines") {
			t.Error("GET /metrics body does not contain go_goroutines")
		}
	})
}

func TestCreateLink(t *testing.T) {
	mem := memorystore.New()
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), mem, 5*time.Second, newTestRegistry())
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/path?q=1"}`))
	req.Header.Set("Content-Type", "application/json")
	authorize(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Code) != idgen.CodeLength {
		t.Errorf("code length = %d, want %d", len(resp.Code), idgen.CodeLength)
	}
	if resp.URL != "https://example.com/path?q=1" {
		t.Errorf("url = %q, want %q", resp.URL, "https://example.com/path?q=1")
	}
	if resp.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", resp.ExpiresAt)
	}
	if got, want := rec.Header().Get("Location"), "/r/"+resp.Code; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	saved, err := mem.GetByCode(context.Background(), resp.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if saved.URL != "https://example.com/path?q=1" {
		t.Errorf("stored url = %q, want %q", saved.URL, "https://example.com/path?q=1")
	}
}

func TestCreateWithExpiresAt(t *testing.T) {
	mem := memorystore.New()
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), mem, 5*time.Second, newTestRegistry())
	handler := srv.Routes()

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := `{"url":"https://example.com/exp","expires_at":"` + expires.Format(time.RFC3339) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(body))
	authorize(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(expires) {
		t.Fatalf("expires_at = %v, want %v", resp.ExpiresAt, expires)
	}

	saved, err := mem.GetByCode(context.Background(), resp.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if saved.ExpiresAt == nil || !saved.ExpiresAt.Equal(expires) {
		t.Fatalf("stored expires_at = %v, want %v", saved.ExpiresAt, expires)
	}
}

func TestCreateBadRequest(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	tests := []struct {
		name   string
		body   string
		want   int
		errSub string
	}{
		{"empty body", "", http.StatusBadRequest, "invalid JSON body"},
		{"invalid json", `{`, http.StatusBadRequest, "invalid JSON body"},
		{"missing url", `{}`, http.StatusBadRequest, ""},
		{"invalid url", `{"url":"ftp://example.com"}`, http.StatusBadRequest, ""},
		{"expired", `{"url":"https://example.com","expires_at":"2000-01-01T00:00:00Z"}`, http.StatusBadRequest, "must be in the future"},
		{"expires_at not a time", `{"url":"https://example.com","expires_at":"tomorrow"}`, http.StatusBadRequest, "invalid expires_at"},
		{"expires_at number", `{"url":"https://example.com","expires_at":1}`, http.StatusBadRequest, "invalid expires_at"},
		{"unknown field", `{"url":"https://example.com","expire_at":"2030-01-01T00:00:00Z"}`, http.StatusBadRequest, `unknown field`},
		{"tenant in body", `{"url":"https://example.com","tenant_id":1}`, http.StatusBadRequest, "unknown field"},
		{"url with space", `{"url":"https://example.com/a b"}`, http.StatusBadRequest, ""},
		{"trailing garbage", `{"url":"https://example.com"}x`, http.StatusBadRequest, "invalid JSON body"},
		{"second value", `{"url":"https://example.com"}{"url":"https://example.com"}`, http.StatusBadRequest, "invalid JSON body"},
		{"too large", `{"url":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, ""},
		{"valid prefix + oversized tail", `{"url":"https://example.com/ok"}` + strings.Repeat(" ", maxBodyBytes), http.StatusRequestEntityTooLarge, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(tt.body))
			authorize(req)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.errSub != "" && !strings.Contains(rec.Body.String(), tt.errSub) {
				t.Errorf("body = %s, want substring %q", rec.Body.String(), tt.errSub)
			}
		})
	}
}

func TestCreateFullyEscapedURLWithExpiresAt(t *testing.T) {
	const prefix = "https://example.com/"
	raw := prefix + strings.Repeat("a", link.MaxURLLength-len(prefix))
	var b strings.Builder
	b.WriteString(`{"url":"`)
	for i := 0; i < len(raw); i++ {
		fmt.Fprintf(&b, `\u%04x`, raw[i])
	}
	b.WriteString(`","expires_at":"2030-01-01T00:00:00.000000000+00:00"}`)
	b.WriteString(strings.Repeat(" ", 32))

	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(b.String()))
	authorize(req)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body bytes = %d, limit = %d, resp = %s",
			rec.Code, http.StatusCreated, b.Len(), maxBodyBytes, rec.Body.String())
	}
}

// TestCreateMaxLengthURLAccepted 验证 URL 上限内的合法 URL 不会因为 JSON 转义后请求体变大而被 413 误杀。
func TestCreateMaxLengthURLAccepted(t *testing.T) {
	const prefix = "https://example.com/?q="
	u := prefix + strings.Repeat("&", link.MaxURLLength-len(prefix))
	if len(u) != link.MaxURLLength {
		t.Fatalf("url length = %d, want %d", len(u), link.MaxURLLength)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"`+u+`"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	newTestServer().Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestConsoleClientCanceled(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`))
	authorize(req)
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

type timeoutCreatorStore struct{}

func (timeoutCreatorStore) Create(ctx context.Context, _ *link.Link) error {
	<-ctx.Done()
	return fmt.Errorf("create link: %w", ctx.Err())
}

func TestConsoleGatewayTimeout(t *testing.T) {
	var buf bytes.Buffer
	srv := testServer(slog.New(slog.NewJSONHandler(&buf, nil)), timeoutCreatorStore{}, 20*time.Millisecond, newTestRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

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
		if entry["msg"] == "request" {
			access = entry
		}
	}
	if access == nil {
		t.Fatalf("missing access log\n%s", buf.String())
	}
	if got := logStatus(t, access); got != http.StatusGatewayTimeout {
		t.Errorf("logged status = %d, want %d", got, http.StatusGatewayTimeout)
	}
}

/*
TestConsoleMetricsRouteLabel 验证 console 的创建路由用 ServeMux 模式做 route label。
*/
func TestConsoleMetricsRouteLabel(t *testing.T) {
	reg := newTestRegistry()
	srv := testServer(slog.New(slog.NewTextHandler(io.Discard, nil)), memorystore.New(), 5*time.Second, reg)
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`))
	authorize(req)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := httpRequestCount(t, reg, "POST /api/v1/links", http.MethodPost, http.StatusCreated); got != 1 {
		t.Errorf("requests{route=%q, method=%q, status=%d} = %v, want 1", "POST /api/v1/links", http.MethodPost, http.StatusCreated, got)
	}

	// 没有密钥也走同一个路由，route label 不应该是 unmatched。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := httpRequestCount(t, reg, "POST /api/v1/links", http.MethodPost, http.StatusUnauthorized); got != 1 {
		t.Errorf("requests{route=%q, method=%q, status=%d} = %v, want 1", "POST /api/v1/links", http.MethodPost, http.StatusUnauthorized, got)
	}
}
