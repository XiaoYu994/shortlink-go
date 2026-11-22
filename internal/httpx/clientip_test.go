package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
