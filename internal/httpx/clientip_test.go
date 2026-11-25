package httpx

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

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
			if got := ClientIP(r, tt.trustProxy); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPWithTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
	}
	tests := []struct {
		name       string
		remoteAddr string
		xff        []string
		trustProxy bool
		cidrs      []netip.Prefix
		want       string
	}{
		{
			name:       "untrusted direct peer spoofing xff",
			remoteAddr: "198.51.100.7:1234",
			xff:        []string{"203.0.113.10"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "198.51.100.7",
		},
		{
			name:       "cdn plus nginx two-hop skips both trusted proxies",
			remoteAddr: "10.0.0.5:4567",
			xff:        []string{"1.2.3.4, 203.0.113.9, 172.16.1.20"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "multi-header lines across trusted chain",
			remoteAddr: "10.0.0.5:4567",
			xff:        []string{"9.9.9.9, 198.51.100.42", "172.16.0.9"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "198.51.100.42",
		},
		{
			name:       "ipv4-mapped ipv6 proxy normalized before prefix check",
			remoteAddr: "[::ffff:10.0.0.5]:4567",
			xff:        []string{"203.0.113.8, ::ffff:172.16.2.3"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "203.0.113.8",
		},
		{
			name:       "invalid token mid-chain falls back to remote addr",
			remoteAddr: "10.0.0.5:4567",
			xff:        []string{"203.0.113.8, garbage, 172.16.2.3"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "all xff hops inside trusted cidrs returns leftmost valid",
			remoteAddr: "10.0.0.5:4567",
			xff:        []string{"10.1.1.1, 172.16.0.2"},
			trustProxy: true,
			cidrs:      trusted,
			want:       "10.1.1.1",
		},
		{
			name:       "trustProxy false ignores cidrs and xff",
			remoteAddr: "10.0.0.5:4567",
			xff:        []string{"203.0.113.9"},
			trustProxy: false,
			cidrs:      trusted,
			want:       "10.0.0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for _, line := range tt.xff {
				r.Header.Add("X-Forwarded-For", line)
			}
			if got := ClientIPWithTrustedProxies(r, tt.trustProxy, tt.cidrs); got != tt.want {
				t.Errorf("ClientIPWithTrustedProxies = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPFrom(t *testing.T) {
	if got := ClientIPFrom(context.Background()); got != "" {
		t.Fatalf("ClientIPFrom(Background) = %q, want empty", got)
	}

	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name         string
		trustHeaders bool
		cidrs        []netip.Prefix
		mutateCIDRs  bool
		wantIP       string
	}{
		{
			name:         "trustHeaders false uses remote addr",
			trustHeaders: false,
			wantIP:       "10.0.0.5",
		},
		{
			name:         "single hop without cidrs uses rightmost forwarded ip",
			trustHeaders: true,
			wantIP:       "10.0.0.2",
		},
		{
			name:         "trusted cidrs strips proxy chain and clones input slice",
			trustHeaders: true,
			cidrs:        append([]netip.Prefix(nil), trusted...),
			mutateCIDRs:  true,
			wantIP:       "203.0.113.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			srv := New(slog.New(slog.NewJSONHandler(&buf, nil)), 5*time.Second, newTestRegistry())
			srv.SetTrustedProxies(tt.trustHeaders, tt.cidrs)
			if tt.mutateCIDRs {
				// 覆盖调用方传入的切片，确认 Server 内部持有独立副本。
				tt.cidrs[0] = netip.MustParsePrefix("0.0.0.0/0")
			}

			var handlerIP string
			handler := srv.Handler(func(mux *http.ServeMux) {
				mux.HandleFunc("GET /ip", func(w http.ResponseWriter, r *http.Request) {
					handlerIP = ClientIPFrom(r.Context())
					w.WriteHeader(http.StatusNoContent)
				})
			})

			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = "10.0.0.5:4567"
			req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.2")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
			if handlerIP != tt.wantIP {
				t.Errorf("ClientIPFrom(ctx) = %q, want %q", handlerIP, tt.wantIP)
			}
			entry := oneLogEntry(t, &buf)
			if entry["ip"] != tt.wantIP {
				t.Errorf("access log ip = %v, want %q", entry["ip"], tt.wantIP)
			}
		})
	}
}
