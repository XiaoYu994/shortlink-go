package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
)

func baseRedirectorTestEnv() map[string]string {
	return map[string]string{
		"ADDR":                      "",
		"OPS_ADDR":                  "",
		"DATABASE_URL":              "postgres://localhost/shortlink",
		"DB_MAX_OPEN_CONNS":         "",
		"DB_MAX_IDLE_CONNS":         "",
		"DB_CONN_MAX_LIFETIME":      "",
		"DB_CONN_MAX_IDLE_TIME":     "",
		"REQUEST_TIMEOUT":           "",
		"SHUTDOWN_TIMEOUT":          "",
		"DRAIN_DELAY":               "",
		"CACHE_MISS_CONCURRENCY":    "",
		"CACHE_FETCH_TIMEOUT":       "",
		"REDIS_URL":                 "redis://localhost:6379/0",
		"VISITOR_SALT":              "test-salt",
		"TRUST_PROXY_HEADERS":       "",
		"TRUSTED_PROXY_CIDRS":       "",
		"LOG_LEVEL":                 "",
		"LOG_FORMAT":                "",
		"PPROF_ADDR":                "",
		"ACCESS_LOG_SAMPLE_RATE":    "",
		"ACCESS_LOG_SLOW_THRESHOLD": "",
		"OIDC_ISSUER":               "",
		"GITHUB_CLIENT_ID":          "",
		"FEATURE_ANALYTICS":         "",
		"FEATURE_RISK_SCAN":         "",
	}
}

func defaultRedirectorConfig() RedirectorConfig {
	return RedirectorConfig{
		BaseConfig: config.BaseConfig{
			Addr:              defaultRedirectorAddr,
			DatabaseURL:       "postgres://localhost/shortlink",
			RedisURL:          "redis://localhost:6379/0",
			Pool:              pgdb.DefaultPoolConfig(),
			RequestTimeout:    config.DefaultRequestTimeout,
			ShutdownTimeout:   config.DefaultShutdownTimeout,
			DrainDelay:        config.DefaultDrainDelay,
			LogLevel:          slog.LevelInfo,
			LogFormat:         config.LogFormatText,
			CacheFetchTimeout: cache.DefaultFetchTimeout,
		},
		Features:               config.FeatureFlags{Analytics: true, RiskScan: true},
		VisitorSalt:            "test-salt",
		CacheMissConcurrency:   cache.DefaultMissConcurrency,
		AccessLogSampleRate:    defaultAccessLogSampleRate,
		AccessLogSlowThreshold: defaultAccessLogSlowThreshold,
	}
}

func TestLoadRedirectorConfig(t *testing.T) {
	base := baseRedirectorTestEnv()
	defaults := defaultRedirectorConfig()

	tests := []struct {
		name    string
		env     map[string]string
		want    RedirectorConfig
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{
			name: "explicit values",
			env: map[string]string{
				"ADDR":                      ":9090",
				"OPS_ADDR":                  "127.0.0.1:9091",
				"DATABASE_URL":              "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				"DB_MAX_OPEN_CONNS":         "20",
				"DB_MAX_IDLE_CONNS":         "4",
				"DB_CONN_MAX_LIFETIME":      "1h30m",
				"DB_CONN_MAX_IDLE_TIME":     "5m",
				"REQUEST_TIMEOUT":           "2s",
				"SHUTDOWN_TIMEOUT":          "15s",
				"DRAIN_DELAY":               "3s",
				"CACHE_MISS_CONCURRENCY":    "4",
				"CACHE_FETCH_TIMEOUT":       "500ms",
				"REDIS_URL":                 "rediss://user:pass@redis:6380/1",
				"VISITOR_SALT":              "prod-salt",
				"TRUST_PROXY_HEADERS":       "true",
				"TRUSTED_PROXY_CIDRS":       "10.0.0.0/8, 172.16.0.0/12",
				"LOG_FORMAT":                "json",
				"ACCESS_LOG_SAMPLE_RATE":    "0.1",
				"ACCESS_LOG_SLOW_THRESHOLD": "150ms",
			},
			want: RedirectorConfig{
				BaseConfig: config.BaseConfig{
					Addr:        ":9090",
					OpsAddr:     "127.0.0.1:9091",
					DatabaseURL: "postgres://user:pass@db:5432/shortlink?sslmode=disable",
					RedisURL:    "rediss://user:pass@redis:6380/1",
					Pool: pgdb.PoolConfig{
						MaxOpenConns:    20,
						MaxIdleConns:    4,
						ConnMaxLifetime: 90 * time.Minute,
						ConnMaxIdleTime: 5 * time.Minute,
					},
					RequestTimeout:    2 * time.Second,
					ShutdownTimeout:   15 * time.Second,
					DrainDelay:        3 * time.Second,
					LogFormat:         config.LogFormatJSON,
					CacheFetchTimeout: 500 * time.Millisecond,
				},
				Features:               config.FeatureFlags{Analytics: true, RiskScan: true},
				VisitorSalt:            "prod-salt",
				TrustProxyHeaders:      true,
				TrustedProxyCIDRs:      []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12")},
				CacheMissConcurrency:   4,
				AccessLogSampleRate:    0.1,
				AccessLogSlowThreshold: 150 * time.Millisecond,
			},
		},
		{
			name: "ignores invalid console oauth env",
			env: map[string]string{
				"OIDC_ISSUER":      "not-a-url",
				"GITHUB_CLIENT_ID": "partial-only",
			},
			want: defaults,
		},
		{
			name: "pprof on loopback",
			env:  map[string]string{"PPROF_ADDR": "127.0.0.1:6060"},
			want: func() RedirectorConfig { c := defaults; c.PprofAddr = "127.0.0.1:6060"; return c }(),
		},
		{
			name: "pprof on localhost",
			env:  map[string]string{"PPROF_ADDR": "localhost:6060"},
			want: func() RedirectorConfig { c := defaults; c.PprofAddr = "localhost:6060"; return c }(),
		},
		{
			name: "minimal mode allows empty redis and empty visitor salt when analytics disabled",
			env: map[string]string{
				"REDIS_URL":         "",
				"FEATURE_ANALYTICS": "false",
				"VISITOR_SALT":      "",
			},
			want: func() RedirectorConfig {
				c := defaults
				c.RedisURL = ""
				c.Features.Analytics = false
				c.VisitorSalt = ""
				return c
			}(),
		},
		{name: "ops addr collides with addr", env: map[string]string{"ADDR": ":8080", "OPS_ADDR": ":8080"}, wantErr: "OPS_ADDR must differ from ADDR"},
		{name: "trusted cidrs without trust header", env: map[string]string{"TRUSTED_PROXY_CIDRS": "10.0.0.0/8"}, wantErr: "TRUSTED_PROXY_CIDRS requires TRUST_PROXY_HEADERS=true"},
		{name: "invalid trusted cidrs", env: map[string]string{"TRUST_PROXY_HEADERS": "true", "TRUSTED_PROXY_CIDRS": "bad-cidr"}, wantErr: "invalid"},
		{name: "missing visitor salt", env: map[string]string{"VISITOR_SALT": ""}, wantErr: "VISITOR_SALT is required"},
		{name: "pprof on all interfaces", env: map[string]string{"PPROF_ADDR": ":6060"}, wantErr: "PPROF_ADDR"},
		{name: "pprof on public address", env: map[string]string{"PPROF_ADDR": "0.0.0.0:6060"}, wantErr: "PPROF_ADDR"},
		{name: "pprof without port", env: map[string]string{"PPROF_ADDR": "127.0.0.1"}, wantErr: "PPROF_ADDR"},
		{name: "invalid cache miss concurrency", env: map[string]string{"CACHE_MISS_CONCURRENCY": "abc"}, wantErr: "CACHE_MISS_CONCURRENCY"},
		{name: "cache miss concurrency not positive", env: map[string]string{"CACHE_MISS_CONCURRENCY": "0"}, wantErr: "CACHE_MISS_CONCURRENCY must be positive"},
		{name: "invalid access log sample rate", env: map[string]string{"ACCESS_LOG_SAMPLE_RATE": "abc"}, wantErr: "ACCESS_LOG_SAMPLE_RATE"},
		{name: "negative access log sample rate", env: map[string]string{"ACCESS_LOG_SAMPLE_RATE": "-0.1"}, wantErr: "ACCESS_LOG_SAMPLE_RATE must be between 0 and 1"},
		{name: "access log sample rate above 1", env: map[string]string{"ACCESS_LOG_SAMPLE_RATE": "1.5"}, wantErr: "ACCESS_LOG_SAMPLE_RATE must be between 0 and 1"},
		{name: "nan access log sample rate", env: map[string]string{"ACCESS_LOG_SAMPLE_RATE": "NaN"}, wantErr: "ACCESS_LOG_SAMPLE_RATE must be between 0 and 1"},
		{name: "invalid access log slow threshold", env: map[string]string{"ACCESS_LOG_SLOW_THRESHOLD": "abc"}, wantErr: "ACCESS_LOG_SLOW_THRESHOLD"},
		{name: "non-positive access log slow threshold", env: map[string]string{"ACCESS_LOG_SLOW_THRESHOLD": "0s"}, wantErr: "ACCESS_LOG_SLOW_THRESHOLD must be positive"},
		{name: "invalid trust proxy headers", env: map[string]string{"TRUST_PROXY_HEADERS": "sometimes"}, wantErr: "TRUST_PROXY_HEADERS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := loadRedirectorConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadRedirectorConfig() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadRedirectorConfig() returned error: %v", err)
			}
			if !redirectorConfigEqual(got, tt.want) {
				t.Errorf("loadRedirectorConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func redirectorConfigEqual(a, b RedirectorConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.Features == b.Features &&
		a.VisitorSalt == b.VisitorSalt &&
		a.TrustProxyHeaders == b.TrustProxyHeaders &&
		slices.Equal(a.TrustedProxyCIDRs, b.TrustedProxyCIDRs) &&
		a.PprofAddr == b.PprofAddr &&
		a.CacheMissConcurrency == b.CacheMissConcurrency &&
		a.AccessLogSampleRate == b.AccessLogSampleRate &&
		a.AccessLogSlowThreshold == b.AccessLogSlowThreshold
}

func TestRedirectorConfigRedaction(t *testing.T) {
	cfg := defaultRedirectorConfig()
	cfg.DatabaseURL = "postgres://shortlink:pg-secret@db:5432/shortlink?sslmode=disable"
	cfg.RedisURL = "rediss://:redis-secret@redis:6380/0"
	cfg.VisitorSalt = "top-secret-salt"

	var buf bytes.Buffer
	logger, _ := cfg.NewLoggerWithService(&buf, "redirector")
	logger.Info("dump", "cfg", cfg)
	combined := buf.String() + "\n" + fmt.Sprintf("%+v", cfg)
	for _, secret := range []string{"pg-secret", "redis-secret", "top-secret-salt"} {
		if strings.Contains(combined, secret) {
			t.Errorf("secret %q leaked in redirector config: %s", secret, combined)
		}
	}
	if !strings.Contains(buf.String(), logx.Redacted) {
		t.Errorf("expected redacted placeholder: %s", buf.String())
	}
}
