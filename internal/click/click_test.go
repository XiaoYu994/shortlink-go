package click

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestReferrerHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty is direct", "", ReferrerDirect},
		{"host only", "https://news.ycombinator.com/item?id=1", "news.ycombinator.com"},
		{"lower case", "https://News.Example.COM/path", "news.example.com"},
		{"strip one www", "https://www.example.com/path", "example.com"},
		{"keep second www", "https://www.www.example.com/path", "www.example.com"},
		{"no scheme", "//example.com/path", "example.com"},
		{"invalid", "://bad", ReferrerOther},
		{"no host", "not a url", ReferrerOther},
		{"too long", "https://example.com/" + string(bytes.Repeat([]byte("a"), maxRefererBytes)), ReferrerOther},
		{"host too long", "https://" + string(bytes.Repeat([]byte("a"), 256)) + ".com", ReferrerOther},
		{"plain www", "https://www.", ReferrerOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := referrerHost(tt.raw); got != tt.want {
				t.Errorf("referrerHost(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDeviceAndClient(t *testing.T) {
	const (
		chromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
		safariIPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
		chromeAndroid = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36"
		androidTablet = "Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
		iPad          = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/604.1"
		googlebot     = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
		curl          = "curl/8.4.0"
		goHTTP        = "Go-http-client/2.0"
		empty         = ""
	)
	tests := []struct {
		name       string
		ua         string
		wantDevice string
		wantClient string
	}{
		{"chrome windows", chromeWindows, DeviceDesktop, ClientBrowser},
		{"safari iphone", safariIPhone, DeviceMobile, ClientBrowser},
		{"chrome android", chromeAndroid, DeviceMobile, ClientBrowser},
		{"android tablet", androidTablet, DeviceTablet, ClientBrowser},
		{"ipad", iPad, DeviceTablet, ClientBrowser},
		{"googlebot", googlebot, DeviceBot, ClientBot},
		{"curl", curl, DeviceOther, ClientCLI},
		{"go http client", goHTTP, DeviceOther, ClientCLI},
		{"empty", empty, DeviceOther, ClientOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lower := strings.ToLower(tt.ua)
			bot := isBot(lower)
			if got := deviceOf(lower, bot); got != tt.wantDevice {
				t.Errorf("deviceOf(%q) = %q, want %q", tt.ua, got, tt.wantDevice)
			}
			if got := clientOf(lower, bot); got != tt.wantClient {
				t.Errorf("clientOf(%q) = %q, want %q", tt.ua, got, tt.wantClient)
			}
		})
	}
}

func TestVisitorHashChangesByDayAndMetadata(t *testing.T) {
	salt := []byte("test-salt")
	day := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	base := visitorHash(salt, day, "203.0.113.7", "Mozilla/5.0")
	if len(base) != VisitorHashBytes {
		t.Fatalf("hash length = %d, want %d", len(base), VisitorHashBytes)
	}
	if !bytes.Equal(base, visitorHash(salt, day.Add(time.Hour), "203.0.113.7", "Mozilla/5.0")) {
		t.Error("hash changed within the same UTC day")
	}
	if bytes.Equal(base, visitorHash(salt, day.AddDate(0, 0, 1), "203.0.113.7", "Mozilla/5.0")) {
		t.Error("hash did not change across UTC days")
	}
	if bytes.Equal(base, visitorHash(salt, day, "203.0.113.8", "Mozilla/5.0")) {
		t.Error("hash did not change with IP")
	}
	if bytes.Equal(base, visitorHash(salt, day, "203.0.113.7", "Mozilla/5.0 (X11)")) {
		t.Error("hash did not change with User-Agent")
	}
	if bytes.Equal(base, visitorHash([]byte("other-salt"), day, "203.0.113.7", "Mozilla/5.0")) {
		t.Error("hash did not change with salt")
	}
	if got := visitorHash(nil, day, "203.0.113.7", "Mozilla/5.0"); got != nil {
		t.Errorf("hash without salt = %v, want nil", got)
	}
	if got := visitorHash(salt, day, "", "Mozilla/5.0"); got != nil {
		t.Errorf("hash without IP = %v, want nil", got)
	}
}

func TestNewClickTruncatesUserAgent(t *testing.T) {
	longUA := string(bytes.Repeat([]byte("a"), maxUserAgentBytes+100))
	base := NewClick(1, time.Now(), "", longUA, "203.0.113.7", []byte("salt"))
	truncated := NewClick(1, time.Now(), "", longUA[:maxUserAgentBytes], "203.0.113.7", []byte("salt"))
	if !bytes.Equal(base.VisitorHash, truncated.VisitorHash) {
		t.Error("User-Agent was not truncated before hashing")
	}
}
