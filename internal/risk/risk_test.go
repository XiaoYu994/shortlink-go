package risk

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestRuleScannerRedirectRules(t *testing.T) {
	ctx := context.Background()

	t.Run("302 to blocked host stops before dialing", func(t *testing.T) {
		entrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://sub.evil.test/landing", http.StatusFound)
		}))
		defer entrySrv.Close()

		entryURL, _ := url.Parse(entrySrv.URL)
		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: NewSafeHTTPClient(safehttp.ClientOptions{
				Timeout:          time.Second,
				AllowedHostPorts: []string{entryURL.Host},
			}),
			BlockedHosts: map[string]link.BanReason{"evil.test": link.BanReasonMalware},
		})

		v, err := scanner.Scan(ctx, entrySrv.URL+"/go")
		if err != nil {
			t.Fatalf("Scan error = %v", err)
		}
		if !v.Unsafe || v.Reason != link.BanReasonMalware || v.Detail != "blocked redirect host: sub.evil.test" {
			t.Fatalf("verdict = %+v, want unsafe/malware/blocked redirect host: sub.evil.test", v)
		}
	})

	t.Run("multi-hop intermediate blocked host stops before hitting blocked or clean landing", func(t *testing.T) {
		var entryHits, bounceHits, landingHits atomic.Int32
		rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Hostname() {
			case "entry.example":
				entryHits.Add(1)
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://bounce.evil.test/hop"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			case "bounce.evil.test":
				bounceHits.Add(1)
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://landing.example/ok"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			case "landing.example":
				landingHits.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
					Request:    req,
				}, nil
			default:
				return nil, fmt.Errorf("unexpected host %q", req.URL.Hostname())
			}
		})

		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient:   &http.Client{Transport: rt},
			BlockedHosts: map[string]link.BanReason{"evil.test": link.BanReasonMalware},
		})

		v, err := scanner.Scan(ctx, "https://entry.example/go")
		if err != nil {
			t.Fatalf("Scan error = %v", err)
		}
		if !v.Unsafe || v.Reason != link.BanReasonMalware || v.Detail != "blocked redirect host: bounce.evil.test" {
			t.Fatalf("verdict = %+v, want unsafe/malware/blocked redirect host: bounce.evil.test", v)
		}
		if entryHits.Load() != 1 || bounceHits.Load() != 0 || landingHits.Load() != 0 {
			t.Fatalf("hits = (entry:%d, bounce:%d, landing:%d), want (1, 0, 0)",
				entryHits.Load(), bounceHits.Load(), landingHits.Load())
		}
	})

	t.Run("302 to url with blocked keyword stops before dialing landing", func(t *testing.T) {
		var landingHits atomic.Int32
		landingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			landingHits.Add(1)
			_, _ = w.Write([]byte("ok"))
		}))
		defer landingSrv.Close()

		entrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landingSrv.URL+"/download?f=malware-payload", http.StatusFound)
		}))
		defer entrySrv.Close()

		entryURL, _ := url.Parse(entrySrv.URL)
		landingURL, _ := url.Parse(landingSrv.URL)
		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: NewSafeHTTPClient(safehttp.ClientOptions{
				Timeout:          time.Second,
				AllowedHostPorts: []string{entryURL.Host, landingURL.Host},
			}),
		})

		v, err := scanner.Scan(ctx, entrySrv.URL+"/go")
		if err != nil {
			t.Fatalf("Scan error = %v", err)
		}
		if !v.Unsafe || v.Reason != link.BanReasonMalware || v.Detail != "matched redirect url keyword: malware-payload" {
			t.Fatalf("verdict = %+v, want unsafe/malware/matched redirect url keyword: malware-payload", v)
		}
		if landingHits.Load() != 0 {
			t.Fatalf("landingHits = %d, want 0", landingHits.Load())
		}
	})

	t.Run("trailing dot fqdn matched on direct and redirect urls", func(t *testing.T) {
		entrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://sub.evil.test./landing", http.StatusFound)
		}))
		defer entrySrv.Close()

		entryURL, _ := url.Parse(entrySrv.URL)
		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: NewSafeHTTPClient(safehttp.ClientOptions{
				Timeout:          time.Second,
				AllowedHostPorts: []string{entryURL.Host},
			}),
			BlockedHosts: map[string]link.BanReason{"Evil.Test.": link.BanReasonMalware},
		})

		cases := []struct {
			name       string
			url        string
			wantDetail string
		}{
			{name: "direct apex", url: "https://evil.test./page", wantDetail: "blocked host: evil.test"},
			{name: "direct sub", url: "https://sub.evil.test./page", wantDetail: "blocked host: sub.evil.test"},
			{name: "redirect sub", url: entrySrv.URL + "/hop", wantDetail: "blocked redirect host: sub.evil.test"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v, err := scanner.Scan(ctx, tc.url)
				if err != nil {
					t.Fatalf("Scan(%q) error = %v", tc.url, err)
				}
				if !v.Unsafe || v.Reason != link.BanReasonMalware || v.Detail != tc.wantDetail {
					t.Fatalf("Scan(%q) verdict = %+v, want unsafe/malware/%q", tc.url, v, tc.wantDetail)
				}
			})
		}
	})

	t.Run("preserves safehttp redirect limit and scheme checks", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/loop":
				http.Redirect(w, r, "/loop", http.StatusFound)
			case "/bad-scheme":
				http.Redirect(w, r, "ftp://example.com/a", http.StatusFound)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer srv.Close()

		u, _ := url.Parse(srv.URL)
		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: NewSafeHTTPClient(safehttp.ClientOptions{
				Timeout:          time.Second,
				MaxRedirects:     2,
				AllowedHostPorts: []string{u.Host},
			}),
		})

		if _, err := scanner.Scan(ctx, srv.URL+"/loop"); !errors.Is(err, safehttp.ErrTooManyRedirects) {
			t.Errorf("/loop err = %v, want ErrTooManyRedirects", err)
		}
		if _, err := scanner.Scan(ctx, srv.URL+"/bad-scheme"); !errors.Is(err, safehttp.ErrInvalidScheme) {
			t.Errorf("/bad-scheme err = %v, want ErrInvalidScheme", err)
		}

		// CheckRedirect 为 nil 的客户端也保留 Go 标准库 10 跳上限，不无限循环。
		var hits atomic.Int32
		loopRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			n := hits.Add(1)
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{fmt.Sprintf("https://loop.example/hop/%d", n)}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})
		defaultLimitScanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: &http.Client{Transport: loopRT},
		})
		if _, err := defaultLimitScanner.Scan(ctx, "https://loop.example/start"); err == nil {
			t.Error("expected default 10-redirect error, got nil")
		}
		if hits.Load() != 10 {
			t.Errorf("default redirect loop hits = %d, want 10", hits.Load())
		}
	})

	t.Run("does not mutate caller HTTPClient", func(t *testing.T) {
		var landingHits atomic.Int32
		landingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			landingHits.Add(1)
			_, _ = w.Write([]byte("ok"))
		}))
		defer landingSrv.Close()

		entrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landingSrv.URL+"/download?f=malware-payload", http.StatusFound)
		}))
		defer entrySrv.Close()

		entryURL, _ := url.Parse(entrySrv.URL)
		landingURL, _ := url.Parse(landingSrv.URL)
		baseClient := NewSafeHTTPClient(safehttp.ClientOptions{
			Timeout:          time.Second,
			AllowedHostPorts: []string{entryURL.Host, landingURL.Host},
		})
		scanner := NewRuleScanner(RuleScannerOptions{HTTPClient: baseClient})

		v, err := scanner.Scan(ctx, entrySrv.URL+"/go")
		if err != nil || !v.Unsafe {
			t.Fatalf("Scan = (%+v, %v), want unsafe/nil", v, err)
		}
		if landingHits.Load() != 0 {
			t.Fatalf("landingHits after Scan = %d, want 0", landingHits.Load())
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entrySrv.URL+"/go", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := baseClient.Do(req)
		if err != nil {
			t.Fatalf("caller baseClient.Do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || landingHits.Load() != 1 {
			t.Fatalf("caller client status=%d landingHits=%d, want 200 and 1", resp.StatusCode, landingHits.Load())
		}
	})
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
		if _, err := NormalizeURL(raw); err != nil {
			t.Fatalf("NormalizeURL: %v", err)
		}

		reg := prometheus.NewRegistry()
		vc := NewVerdictCache(rdb, reg, VerdictCacheOptions{})
		if norm, err := NormalizeURL(raw); err == nil {
			t.Cleanup(func() { _ = rdb.Del(context.Background(), vc.cacheKey(norm)).Err() })
		}
		if _, hit := vc.Get(ctx, raw); hit {
			t.Fatal("expected miss before Set")
		}
		want := Verdict{Unsafe: true, Reason: link.BanReasonPhishing, Detail: "test"}
		vc.Set(ctx, raw, want)

		// 相同归一化 URL（去掉默认端口、尾点和 fragment）应命中缓存。
		got, hit := vc.Get(ctx, strings.Split(strings.Replace(raw, "HTTPS://Example.COM:443", "https://example.com.", 1), "#")[0])
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

	t.Run("succeeds when ban cache not updated and lets old cache expire within ttl", func(t *testing.T) {
		h := NewHandler(HandlerOptions{
			Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
				return Verdict{Unsafe: true, Reason: link.BanReasonMalware}, nil
			}},
			Banner: &fakeBanner{fn: func(context.Context, string, string, link.BanReason, link.BanSource) (*link.Link, error) {
				return &link.Link{Code: "AbC12345"}, fmt.Errorf("ban: %w", cache.ErrNotUpdated)
			}},
		})
		if err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://bad.example"})); err != nil {
			t.Fatalf("handler err = %v, want nil (cache expires within ttl without refetch)", err)
		}
	})

	t.Run("permanent on malformed payload and skips unscannable targets with metrics", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		metrics := NewMetrics(reg)

		badPayloadH := NewHandler(HandlerOptions{
			Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
				t.Error("scanner must not run on invalid payload")
				return Verdict{}, nil
			}},
			Banner:  &fakeBanner{},
			Metrics: metrics,
		})
		if err := badPayloadH(context.Background(), job.Job{Payload: []byte("{bad")}); !job.IsPermanent(err) {
			t.Errorf("bad json err = %v, want Permanent", err)
		}
		if err := badPayloadH(context.Background(), makeJob(ScanPayload{Code: "AbC12345"})); !job.IsPermanent(err) {
			t.Errorf("missing url err = %v, want Permanent", err)
		}

		cases := []struct {
			name        string
			scanErr     error
			wantOutcome string
		}{
			{name: "blocked address", scanErr: safehttp.ErrBlockedAddress, wantOutcome: OutcomeBlockedAddress},
			{name: "blocked port", scanErr: safehttp.ErrBlockedPort, wantOutcome: OutcomeBlockedPort},
			{name: "invalid scheme", scanErr: safehttp.ErrInvalidScheme, wantOutcome: OutcomeInvalidScheme},
			{name: "too many redirects", scanErr: safehttp.ErrTooManyRedirects, wantOutcome: OutcomeTooManyRedirects},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				banner := &fakeBanner{}
				h := NewHandler(HandlerOptions{
					Scanner: &fakeScanner{fn: func(context.Context, string) (Verdict, error) {
						return Verdict{}, fmt.Errorf("fetch: %w", tc.scanErr)
					}},
					Banner:  banner,
					Metrics: metrics,
				})
				err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://example.com/a"}))
				if !job.IsSkipped(err) {
					t.Fatalf("err = %v, want job.Skip", err)
				}
				if job.IsPermanent(err) {
					t.Fatalf("err = %v must not be job.Permanent", err)
				}
				if banner.calls.Load() != 0 {
					t.Fatalf("banner calls = %d, want 0", banner.calls.Load())
				}
				if got := testutil.ToFloat64(metrics.scans.WithLabelValues(StageFetch, tc.wantOutcome)); got != 1 {
					t.Errorf("scans{fetch,%s} = %v, want 1", tc.wantOutcome, got)
				}
			})
		}
	})

	t.Run("defers on host rate limit without hitting target server", func(t *testing.T) {
		var targetHits atomic.Int32
		rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
			targetHits.Add(1)
			return nil, errors.New("should not dial")
		})
		lim := safehttp.NewHostLimiter(1)
		lim.Allow("slow.example") // 消耗掉唯一令牌

		scanner := NewRuleScanner(RuleScannerOptions{
			HTTPClient: &http.Client{Transport: rt},
			Limiter:    lim,
		})
		h := NewHandler(HandlerOptions{
			Scanner: scanner,
			Banner:  &fakeBanner{},
		})
		err := h(context.Background(), makeJob(ScanPayload{Code: "AbC12345", URL: "https://slow.example/1"}))
		if delay, ok := job.DeferDelay(err); !ok || delay <= 0 {
			t.Errorf("handler err = %v, want Defer(delay>0)", err)
		}
		if targetHits.Load() != 0 {
			t.Errorf("targetHits = %d, want 0", targetHits.Load())
		}
	})
}

func TestRiskTracingScanSpanDoesNotLeakTraceparent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello clean page"))
	}))
	defer srv.Close()

	client := NewSafeHTTPClient(safehttp.ClientOptions{
		Timeout:          time.Second,
		AllowedHostPorts: []string{srv.Listener.Addr().String()},
	})
	h := NewHandler(HandlerOptions{
		Scanner: NewRuleScanner(RuleScannerOptions{HTTPClient: client}),
		Banner:  &fakeBanner{},
	})

	b, _ := json.Marshal(ScanPayload{Code: "AbC12345", URL: srv.URL})
	ctx, root := otel.Tracer("test").Start(context.Background(), "job.process link.risk_scan")
	if err := h(ctx, job.Job{ID: 1, Kind: job.KindLinkRiskScan, Payload: b}); err != nil {
		t.Fatalf("handler err = %v", err)
	}
	root.End()

	if gotTraceparent != "" {
		t.Errorf("risk scan outbound request leaked traceparent header %q", gotTraceparent)
	}

	var foundScan bool
	for _, sp := range exp.GetSpans() {
		if sp.Name != "risk.scan" {
			continue
		}
		foundScan = true
		if sp.SpanKind != trace.SpanKindClient {
			t.Errorf("span kind = %v, want client", sp.SpanKind)
		}
		var sawCacheMiss, sawSafe bool
		for _, a := range sp.Attributes {
			if a.Key == attribute.Key("risk.cache.hit") && !a.Value.AsBool() {
				sawCacheMiss = true
			}
			if a.Key == attribute.Key("risk.verdict.unsafe") && !a.Value.AsBool() {
				sawSafe = true
			}
		}
		if !sawCacheMiss || !sawSafe {
			t.Errorf("unexpected risk.scan attrs: %+v", sp.Attributes)
		}
	}
	if !foundScan {
		t.Error("expected risk.scan span")
	}
}
