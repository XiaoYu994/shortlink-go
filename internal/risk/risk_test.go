package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/audit"
	"shortlink/internal/cache"
	"shortlink/internal/job"
	"shortlink/internal/link"
)

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

type stubResolver struct {
	ips map[string][]netip.Addr
}

func (r stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r.ips[host], nil
}

func TestSafeHTTPClientBlocksSSRFAndRedirects(t *testing.T) {
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-loopback":
			http.Redirect(w, r, "http://127.0.0.1:8081/metrics", http.StatusFound)
		case "/redirect-file":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case "/redirect-chain":
			http.Redirect(w, r, "/redirect-chain", http.StatusFound)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer localSrv.Close()

	u, err := url.Parse(localSrv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	t.Run("blocks loopback directly", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{Timeout: time.Second})
		resp, err := client.Get(localSrv.URL)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("Get loopback error = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("blocks DNS resolving to private ip", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{
			Timeout: time.Second,
			Resolver: stubResolver{ips: map[string][]netip.Addr{
				"rebind.example": {netip.MustParseAddr("169.254.169.254")},
			}},
		})
		resp, err := client.Get("http://rebind.example/latest")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("Get private DNS error = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("blocks redirect from allowed host to other loopback", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{
			Timeout:          time.Second,
			AllowedHostPorts: []string{u.Host},
		})
		resp, err := client.Get(localSrv.URL + "/redirect-loopback")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("redirect to loopback error = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("blocks redirect to non-http scheme", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{
			Timeout:          time.Second,
			AllowedHostPorts: []string{u.Host},
		})
		resp, err := client.Get(localSrv.URL + "/redirect-file")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrInvalidScheme) {
			t.Fatalf("redirect to file error = %v, want ErrInvalidScheme", err)
		}
	})

	t.Run("blocks redirect loop over max hops", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{
			Timeout:          time.Second,
			MaxRedirects:     2,
			AllowedHostPorts: []string{u.Host},
		})
		resp, err := client.Get(localSrv.URL + "/redirect-chain")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrTooManyRedirects) {
			t.Fatalf("redirect loop error = %v, want ErrTooManyRedirects", err)
		}
	})

	t.Run("blocks non-standard port on public ip", func(t *testing.T) {
		client := NewSafeHTTPClient(HTTPClientOptions{
			Timeout: time.Second,
			Resolver: stubResolver{ips: map[string][]netip.Addr{
				"public.example": {netip.MustParseAddr("93.184.216.34")},
			}},
		})
		resp, err := client.Get("http://public.example:8080/probe")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedPort) {
			t.Fatalf("Get non-80/443 port error = %v, want ErrBlockedPort", err)
		}
	})
}

func TestRuleScannerBodyLimitAndRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/phish":
			_, _ = w.Write([]byte("<html>Please verify your password immediately!</html>"))
		case "/huge":
			// 前 128 字节安全，后面才出现关键词；MaxBodyBytes=64 截断后不应读到。
			_, _ = w.Write([]byte(strings.Repeat("a", 128) + "malware-payload"))
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte("<html>hello world</html>"))
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	scanner := NewRuleScanner(RuleScannerOptions{
		HTTPClient:   NewSafeHTTPClient(HTTPClientOptions{Timeout: time.Second, AllowedHostPorts: []string{u.Host}}),
		MaxBodyBytes: 64,
		BlockedHosts: map[string]link.BanReason{"evil.test": link.BanReasonMalware},
	})

	ctx := context.Background()
	v, err := scanner.Scan(ctx, srv.URL+"/phish")
	if err != nil {
		t.Fatalf("Scan phish: %v", err)
	}
	if !v.Unsafe || v.Reason != link.BanReasonPhishing {
		t.Errorf("phish verdict = %+v, want unsafe/phishing", v)
	}

	v, err = scanner.Scan(ctx, srv.URL+"/huge")
	if err != nil {
		t.Fatalf("Scan huge: %v", err)
	}
	if v.Unsafe {
		t.Errorf("huge verdict = %+v, want safe (truncated at MaxBodyBytes)", v)
	}

	if _, err := scanner.Scan(ctx, srv.URL+"/500"); err == nil {
		t.Error("500 response should return retryable error")
	}

	v, err = scanner.Scan(ctx, srv.URL+"/404")
	if err != nil || v.Unsafe || !v.Uncacheable {
		t.Errorf("404 verdict = (%+v, %v), want safe+uncacheable/nil", v, err)
	}

	// 命中域名黑名单直接判定，无需出站请求。
	v, err = scanner.Scan(ctx, "https://sub.evil.test/page")
	if err != nil || !v.Unsafe || v.Reason != link.BanReasonMalware {
		t.Errorf("blocked host verdict = (%+v, %v), want unsafe/malware", v, err)
	}
}

func TestParseKeywordRules(t *testing.T) {
	rules, err := ParseKeywordRules("smoke-phish:phishing, bad-ware:malware")
	if err != nil {
		t.Fatalf("ParseKeywordRules: %v", err)
	}
	if rules["smoke-phish"] != link.BanReasonPhishing || rules["bad-ware"] != link.BanReasonMalware {
		t.Errorf("rules = %+v", rules)
	}
	if _, err := ParseKeywordRules("broken"); err == nil {
		t.Error("expected error for missing colon")
	}
	if _, err := ParseKeywordRules("foo:unknown_reason"); err == nil {
		t.Error("expected error for invalid reason")
	}
}

func TestHostLimiter(t *testing.T) {
	lim := NewHostLimiter(1)
	if _, ok := lim.Allow("example.com"); !ok {
		t.Fatal("first request should be allowed")
	}
	delay, ok := lim.Allow("example.com")
	if ok || delay <= 0 {
		t.Errorf("second immediate request = (%v, %v), want (delay>0, false)", delay, ok)
	}
	if _, ok := lim.Allow("other.com"); !ok {
		t.Error("different host should not be affected")
	}
}

func TestVerdictCacheRedisAndFailOpen(t *testing.T) {
	t.Run("fail-open when redis down", func(t *testing.T) {
		down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
		_ = down.Close()
		reg := prometheus.NewRegistry()
		vc := NewVerdictCache(down, reg, VerdictCacheOptions{})

		if _, hit := vc.Get(context.Background(), "https://example.com/a"); hit {
			t.Fatal("expected miss on redis down")
		}
		if got := testutil.ToFloat64(vc.lookups.WithLabelValues(cacheResultFallback)); got != 1 {
			t.Errorf("lookups{fallback} = %v, want 1", got)
		}
	})

	t.Run("hit and normalize url on real redis", func(t *testing.T) {
		redisURL := os.Getenv("TEST_REDIS_URL")
		if redisURL == "" {
			t.Skip("TEST_REDIS_URL is not set")
		}
		ctx := context.Background()
		rdb, err := cache.Open(ctx, redisURL)
		if err != nil {
			t.Fatalf("open redis: %v", err)
		}
		t.Cleanup(func() { _ = rdb.Close() })

		raw := fmt.Sprintf("HTTPS://Example.COM:443/path/%d#frag", time.Now().UnixNano())
		norm, err := NormalizeURL(raw)
		if err != nil {
			t.Fatalf("NormalizeURL: %v", err)
		}
		t.Cleanup(func() { _ = rdb.Del(context.Background(), riskCacheKey(norm)).Err() })

		reg := prometheus.NewRegistry()
		vc := NewVerdictCache(rdb, reg, VerdictCacheOptions{})
		if _, hit := vc.Get(ctx, raw); hit {
			t.Fatal("expected miss before Set")
		}
		want := Verdict{Unsafe: true, Reason: link.BanReasonPhishing, Detail: "test"}
		vc.Set(ctx, raw, want)

		// 相同归一化 URL（去掉默认端口和 fragment）应命中缓存。
		got, hit := vc.Get(ctx, strings.Split(strings.Replace(raw, "HTTPS://Example.COM:443", "https://example.com", 1), "#")[0])
		if !hit || got != want {
			t.Fatalf("Get = (%+v, %v), want (%+v, true)", got, hit, want)
		}
	})
}

type fakeScanner struct {
	calls atomic.Int32
	fn    func(ctx context.Context, rawURL string) (Verdict, error)
}

func (f *fakeScanner) Scan(ctx context.Context, rawURL string) (Verdict, error) {
	f.calls.Add(1)
	return f.fn(ctx, rawURL)
}

type fakeBanner struct {
	calls atomic.Int32
	fn    func(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
}

func (f *fakeBanner) BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	f.calls.Add(1)
	return f.fn(ctx, code, expectedURL, reason, source)
}

func TestRiskHandlerOutcomes(t *testing.T) {
	makeJob := func(p ScanPayload) job.Job {
		b, _ := json.Marshal(p)
		return job.Job{ID: 1, Kind: job.KindLinkRiskScan, Payload: b}
	}

	t.Run("bans unsafe url with BanSourceRisk", func(t *testing.T) {
		scanner := &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
			return Verdict{Unsafe: true, Reason: link.BanReasonPhishing}, nil
		}}
		banner := &fakeBanner{fn: func(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
			if code != "AbC12345" || expectedURL != "https://evil.example/p" || reason != link.BanReasonPhishing || source != link.BanSourceRisk {
				t.Errorf("unexpected BanIfURL args: %q %q %q %q", code, expectedURL, reason, source)
			}
			meta, ok := audit.MetaFrom(ctx)
			if !ok || meta.ActorKind != audit.ActorSystem || meta.ActorID != nil || meta.RequestID != "job:1" {
				t.Errorf("audit meta = (%+v, %v), want system job:1", meta, ok)
			}
			return &link.Link{Code: code}, nil
		}}
		h := NewHandler(HandlerOptions{Scanner: scanner, Banner: banner})
		if err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://evil.example/p"})); err != nil {
			t.Fatalf("handler error = %v", err)
		}
		if banner.calls.Load() != 1 {
			t.Errorf("banner calls = %d, want 1", banner.calls.Load())
		}
	})

	t.Run("skips when url changed or deleted", func(t *testing.T) {
		for _, banErr := range []error{link.ErrURLChanged, link.ErrNotFound} {
			h := NewHandler(HandlerOptions{
				Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
					return Verdict{Unsafe: true, Reason: link.BanReasonSpam}, nil
				}},
				Banner: &fakeBanner{fn: func(context.Context, string, string, link.BanReason, link.BanSource) (*link.Link, error) {
					return nil, banErr
				}},
			})
			err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://old.example"}))
			if !job.IsSkipped(err) {
				t.Errorf("banErr %v -> handler err = %v, want job.Skip", banErr, err)
			}
		}
	})

	t.Run("succeeds when ban cache not updated because reconcile job handles sync", func(t *testing.T) {
		h := NewHandler(HandlerOptions{
			Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
				return Verdict{Unsafe: true, Reason: link.BanReasonMalware}, nil
			}},
			Banner: &fakeBanner{fn: func(context.Context, string, string, link.BanReason, link.BanSource) (*link.Link, error) {
				return &link.Link{Code: "AbC12345"}, fmt.Errorf("ban: %w", cache.ErrNotUpdated)
			}},
		})
		if err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://bad.example"})); err != nil {
			t.Fatalf("handler err = %v, want nil (cache sync deferred to cache.reconcile)", err)
		}
	})

	t.Run("permanent on blocked SSRF address and malformed payload", func(t *testing.T) {
		h := NewHandler(HandlerOptions{
			Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
				return Verdict{}, fmt.Errorf("fetch: %w", ErrBlockedAddress)
			}},
			Banner: &fakeBanner{},
		})
		if err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "http://127.0.0.1/"})); !job.IsPermanent(err) {
			t.Errorf("SSRF err = %v, want Permanent", err)
		}
		if err := h(context.Background(), job.Job{Payload: []byte("{bad")}); !job.IsPermanent(err) {
			t.Errorf("bad json err = %v, want Permanent", err)
		}
	})

	t.Run("defers on host rate limit", func(t *testing.T) {
		lim := NewHostLimiter(1)
		lim.Allow("slow.example") // 消耗掉唯一令牌
		h := NewHandler(HandlerOptions{
			Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
				t.Error("scanner should not be called when host limited")
				return Verdict{}, nil
			}},
			Banner:  &fakeBanner{},
			Limiter: lim,
		})
		err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://slow.example/1"}))
		if delay, ok := job.DeferDelay(err); !ok || delay <= 0 {
			t.Errorf("handler err = %v, want Defer(delay>0)", err)
		}
	})
}
