package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
	"shortlink/internal/memorystore"
)

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
	return NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), memorystore.New(), 5*time.Second, newTestRegistry(), nil)
}

/*
这是 httptest 的写法示例：不启动真正的服务器，直接把请求交给 handler。
*/
func TestHealthz(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Body.String(), "ok\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestCreateAndRedirect(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/path?q=1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp createLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Code) != idgen.CodeLength {
		t.Errorf("code length = %d, want %d", len(resp.Code), idgen.CodeLength)
	}
	if resp.URL != "https://example.com/path?q=1" {
		t.Errorf("url = %q, want %q", resp.URL, "https://example.com/path?q=1")
	}
	if got, want := rec.Header().Get("Location"), "/r/"+resp.Code; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	req = httptest.NewRequest(http.MethodGet, "/r/"+resp.Code, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "https://example.com/path?q=1" {
		t.Errorf("redirect Location = %q, want %q", got, "https://example.com/path?q=1")
	}
}

func TestCreateBadRequest(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"invalid json", `{`, http.StatusBadRequest},
		{"missing url", `{}`, http.StatusBadRequest},
		{"invalid url", `{"url":"ftp://example.com"}`, http.StatusBadRequest},
		{"url with space", `{"url":"https://example.com/a b"}`, http.StatusBadRequest},
		{"trailing garbage", `{"url":"https://example.com"}x`, http.StatusBadRequest},
		{"second value", `{"url":"https://example.com"}{"url":"https://example.com"}`, http.StatusBadRequest},
		{"too large", `{"url":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"valid prefix + oversized tail", `{"url":"https://example.com/ok"}` + strings.Repeat(" ", maxBodyBytes), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestRedirectNotFound(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

/*
TestRedirectInvalidCode 验证非法短码在进入存储之前就被拒绝。
否则任意字符串都会先查 Redis 和数据库，超长 code 还会变成超长的缓存 key。
*/
func TestRedirectInvalidCode(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{}}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), nil)
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

type recordSink struct {
	mu  sync.Mutex
	ids []int64
}

func (s *recordSink) Record(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
}

func TestRedirectRecordsClick(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/recorded"},
	}}
	sink := &recordSink{}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), sink)
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
	if len(sink.ids) != 1 || sink.ids[0] != 42 {
		t.Errorf("recorded = %v, want [42]", sink.ids)
	}
}

func TestRedirectHeadDoesNotRecordClick(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/recorded"},
	}}
	sink := &recordSink{}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), sink)

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
	if len(sink.ids) != 0 {
		t.Errorf("recorded = %v, want none", sink.ids)
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
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), sink)

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusFound {
		t.Errorf("status = %d, want request to stop before the redirect", rec.Code)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.ids) != 0 {
		t.Errorf("recorded = %v, want none", sink.ids)
	}
}

// cancelOnGetStore 在查出链接的同时取消请求，用来覆盖查询期间客户端断开的窗口。
type cancelOnGetStore struct {
	l      *link.Link
	cancel context.CancelFunc
}

func (s *cancelOnGetStore) Create(context.Context, *link.Link) error { return nil }

func (s *cancelOnGetStore) GetByCode(context.Context, string) (*link.Link, error) {
	s.cancel()
	return s.l, nil
}

// stubStore 只实现测试需要的读取能力，用来构造指定短码的链接。
type stubStore struct {
	links map[string]*link.Link
	calls int
}

func (s *stubStore) Create(context.Context, *link.Link) error { return nil }

func (s *stubStore) GetByCode(_ context.Context, code string) (*link.Link, error) {
	s.calls++
	l, ok := s.links[code]
	if !ok {
		return nil, link.ErrNotFound
	}
	return l, nil
}

/*
TestCodeAndRouteNamespaces 验证短码和路由分属不同命名空间：
/r/ 下的短码不会占用 /healthz，两个路径可以同时工作。
*/
func TestCodeAndRouteNamespaces(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"healthz1": {Code: "healthz1", URL: "https://example.com/healthz"},
	}}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), store, 5*time.Second, newTestRegistry(), nil)
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

func TestClientCanceled(t *testing.T) {
	srv := newTestServer()
	handler := srv.Routes()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create", http.MethodPost, "/api/v1/links", `{"url":"https://example.com"}`},
		{"redirect", http.MethodGet, "/r/AbC123xy", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			ctx, cancel := context.WithCancel(req.Context())
			cancel()
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			/*
				没有写响应时 ResponseRecorder 的 Code 默认是 200；
				真实连接已经断开，这个 200 不会被发送。
			*/
			if rec.Code == http.StatusInternalServerError {
				t.Errorf("status = %d, want not 500", rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
		})
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
	rec := httptest.NewRecorder()
	newTestServer().Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}
