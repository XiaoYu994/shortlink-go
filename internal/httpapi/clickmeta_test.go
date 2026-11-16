package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/link"
)

func TestRedirectRecordsClickMetadata(t *testing.T) {
	store := &stubStore{links: map[string]*link.Link{
		"AbC123xy": {ID: 42, Code: "AbC123xy", URL: "https://example.com/target"},
	}}
	sink := &recordSink{}
	srv := newServer(serverDeps{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:             store,
		requestTimeout:    5 * time.Second,
		registry:          newTestRegistry(),
		clicks:            sink,
		visitorSalt:       []byte("test-salt"),
		trustProxyHeaders: true,
		keys:              allowKey{},
	})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	req.Header.Set("Referer", "https://www.News.Example.com/story")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	rec := httptest.NewRecorder()
	srv.RedirectRoutes().ServeHTTP(rec, req)
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

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        []string // 每个元素是一行 X-Forwarded-For
		trustProxy bool
		want       string
	}{
		{"remote addr", "203.0.113.7:4567", nil, false, "203.0.113.7"},
		{"remote addr without port", "203.0.113.7", nil, false, "203.0.113.7"},
		{"proxy appended hop", "10.0.0.5:4567", []string{"203.0.113.9, 10.0.0.1"}, true, "10.0.0.1"},
		{"single forwarded hop", "10.0.0.5:4567", []string{"203.0.113.9"}, true, "203.0.113.9"},
		{"proxy ignored without trust", "10.0.0.5:4567", []string{"203.0.113.9"}, false, "10.0.0.5"},
		{"invalid rightmost falls back", "10.0.0.5:4567", []string{"203.0.113.9, not-an-ip"}, true, "10.0.0.5"},
		{"invalid forwarded falls back", "10.0.0.5:4567", []string{"not-an-ip"}, true, "10.0.0.5"},
		{"trailing comma", "10.0.0.5:4567", []string{"203.0.113.9,"}, true, "203.0.113.9"},
		// 代理另起一行追加时，第一行是客户端带来的，不能用。
		{"proxy adds a second line", "10.0.0.5:4567", []string{"1.2.3.4", "203.0.113.9"}, true, "203.0.113.9"},
		{"invalid last line falls back", "10.0.0.5:4567", []string{"203.0.113.9", "not-an-ip"}, true, "10.0.0.5"},
		{"ipv6", "[2001:db8::1]:4567", nil, false, "2001:db8::1"},
		{"invalid remote", "unix", nil, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for _, line := range tt.xff {
				r.Header.Add("X-Forwarded-For", line)
			}
			if got := clientIP(r, tt.trustProxy); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}
