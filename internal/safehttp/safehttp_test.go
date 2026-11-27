package safehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

type stubResolver struct {
	ips map[string][]netip.Addr
}

func (r stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r.ips[host], nil
}

func TestIsBlockedAddr(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"127.0.1.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"2001:db8::1", true},
		{"64:ff9b::10.0.0.1", true},
		{"64:ff9b:1::10.0.0.1", true},
		{"2002:0a00:0001::1", true},
		{"2001::1", true},
		{"fec0::1", true},
		{"100::1", true},
		{"93.184.216.34", false},
		{"1.1.1.1", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			addr, err := netip.ParseAddr(tt.ip)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", tt.ip, err)
			}
			if got := IsBlockedAddr(addr); got != tt.blocked {
				t.Errorf("IsBlockedAddr(%q) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		opts    URLValidationOptions
		wantErr error
	}{
		{name: "valid https", rawURL: "https://hooks.example.com/events"},
		{name: "valid http with 80", rawURL: "http://hooks.example.com:80/events"},
		{name: "empty", rawURL: "   ", wantErr: ErrInvalidURL},
		{name: "ftp scheme", rawURL: "ftp://hooks.example.com/events", wantErr: ErrInvalidScheme},
		{name: "userinfo rejected", rawURL: "https://user:pass@hooks.example.com/events", wantErr: ErrInvalidURL},
		{name: "fragment rejected", rawURL: "https://hooks.example.com/events#frag", wantErr: ErrInvalidURL},
		{name: "control char rejected", rawURL: "https://hooks.example.com/ev\nents", wantErr: ErrInvalidURL},
		{name: "internal whitespace rejected", rawURL: "https://hooks.example.com/my hook", wantErr: ErrInvalidURL},
		{name: "localhost blocked", rawURL: "http://localhost/hook", wantErr: ErrBlockedAddress},
		{name: "localhost trailing dot blocked", rawURL: "http://localhost./hook", wantErr: ErrBlockedAddress},
		{name: "sub-localhost blocked", rawURL: "https://api.localhost/hook", wantErr: ErrBlockedAddress},
		{name: "sub-localhost trailing dot blocked", rawURL: "https://api.localhost./hook", wantErr: ErrBlockedAddress},
		{name: "loopback ip blocked", rawURL: "http://127.0.0.1/hook", wantErr: ErrBlockedAddress},
		{name: "ipv4 mapped ipv6 blocked", rawURL: "http://[::ffff:127.0.0.1]/hook", wantErr: ErrBlockedAddress},
		{name: "private ip blocked", rawURL: "https://10.0.0.5/hook", wantErr: ErrBlockedAddress},
		{name: "disallowed port blocked", rawURL: "https://hooks.example.com:8443/hook", wantErr: ErrBlockedPort},
		{
			name:   "allowed test host bypasses in devtest",
			rawURL: "http://127.0.0.1:18090/hook",
			opts:   URLValidationOptions{AllowedHostPorts: []string{"127.0.0.1:18090"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateURL(tt.rawURL, tt.opts)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateURL(%q) = %v, want nil", tt.rawURL, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateURL(%q) = %v, want errors.Is(_, %v)", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestNewClientDisableRedirectsAndDialChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redir":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/redirect-loopback":
			http.Redirect(w, r, "http://127.0.0.1:8081/metrics", http.StatusFound)
		case "/redirect-file":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case "/redirect-chain":
			http.Redirect(w, r, "/redirect-chain", http.StatusFound)
		case "/final":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	client := NewClient(ClientOptions{
		Timeout:          time.Second,
		DisableRedirects: true,
		AllowedHostPorts: []string{u.Host},
	})
	resp, err := client.Post(srv.URL+"/redir", "application/json", nil)
	if err != nil {
		t.Fatalf("Post /redir error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirects must not be followed)", resp.StatusCode)
	}

	// 允许重定向上限内按协议和 SSRF 规则拦下回环、非 HTTP scheme 和死循环跳转。
	redirClient := NewClient(ClientOptions{
		Timeout:          time.Second,
		MaxRedirects:     2,
		AllowedHostPorts: []string{u.Host},
	})
	for _, tc := range []struct {
		path    string
		wantErr error
	}{
		{"/redirect-loopback", ErrBlockedAddress},
		{"/redirect-file", ErrInvalidScheme},
		{"/redirect-chain", ErrTooManyRedirects},
	} {
		r, rerr := redirClient.Get(srv.URL + tc.path)
		if r != nil {
			_ = r.Body.Close()
		}
		if !errors.Is(rerr, tc.wantErr) {
			t.Fatalf("Get %s error = %v, want %v", tc.path, rerr, tc.wantErr)
		}
	}

	rebindClient := NewClient(ClientOptions{
		Timeout: time.Second,
		Resolver: stubResolver{ips: map[string][]netip.Addr{
			"rebind.example": {netip.MustParseAddr("169.254.169.254")},
		}},
	})
	resp2, err := rebindClient.Get("http://rebind.example/hook")
	if resp2 != nil {
		_ = resp2.Body.Close()
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("Get rebind error = %v, want ErrBlockedAddress", err)
	}

	portClient := NewClient(ClientOptions{
		Timeout: time.Second,
		Resolver: stubResolver{ips: map[string][]netip.Addr{
			"public.example": {netip.MustParseAddr("93.184.216.34")},
		}},
	})
	resp3, err := portClient.Get("http://public.example:8080/probe")
	if resp3 != nil {
		_ = resp3.Body.Close()
	}
	if !errors.Is(err, ErrBlockedPort) {
		t.Fatalf("Get non-80/443 port error = %v, want ErrBlockedPort", err)
	}
}

func TestHostLimiter(t *testing.T) {
	lim := NewHostLimiter(1)
	if _, ok := lim.Allow("hooks.example.com"); !ok {
		t.Fatal("first request should be allowed")
	}
	delay, ok := lim.Allow("  HOOKS.EXAMPLE.COM  ")
	if ok || delay <= 0 {
		t.Fatalf("normalized duplicate host = (%v, %v), want (positive delay, false)", delay, ok)
	}
	delayDot, okDot := lim.Allow("HOOKS.EXAMPLE.COM.")
	if okDot || delayDot <= 0 {
		t.Fatalf("trailing-dot duplicate host = (%v, %v), want (positive delay, false)", delayDot, okDot)
	}
	if _, ok := lim.Allow("other.com"); !ok {
		t.Fatal("different host should not be affected")
	}
	if _, ok := lim.Allow("   "); !ok {
		t.Fatal("empty host should be allowed without error")
	}

	disabled := NewHostLimiter(0)
	for range 5 {
		if _, ok := disabled.Allow("hooks.example.com"); !ok {
			t.Fatal("disabled limiter should always allow")
		}
	}
}
