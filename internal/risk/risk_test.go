package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"shortlink/internal/safehttp"
)

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
		HTTPClient:   NewSafeHTTPClient(safehttp.ClientOptions{Timeout: time.Second, AllowedHostPorts: []string{u.Host}}),
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
				return Verdict{}, fmt.Errorf("fetch: %w", safehttp.ErrBlockedAddress)
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
		lim := safehttp.NewHostLimiter(1)
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
