package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
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
		config.EnvConfigPath:          "",
		"ADDR":                        "",
		"OPS_ADDR":                    "",
		"DATABASE_URL":                "postgres://localhost/shortlink",
		"REDIS_URL":                   "redis://localhost:6379/0",
		"VISITOR_SALT":                "test-salt",
		"TRUSTED_PROXIES":             "",
		"LOG_LEVEL":                   "",
		"LOG_FORMAT":                  "",
		"DRAIN_DELAY":                 "",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "",
		"FEATURE_ANALYTICS":           "",
		"FEATURE_RISK_SCAN":           "",
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
			Tracing:           config.DefaultTracingConfig(),
		},
		Features:               config.FeatureFlags{Analytics: true, RiskScan: true},
		VisitorSalt:            "test-salt",
		CacheMissConcurrency:   cache.DefaultMissConcurrency,
		AccessLogSampleRate:    defaultAccessLogSampleRate,
		AccessLogSlowThreshold: defaultAccessLogSlowThreshold,
	}
}

func TestParseRedirectorArgs(t *testing.T) {
	var stderr bytes.Buffer
	path, ok, err := parseRedirectorArgs([]string{"--config", "/etc/shortlink.yaml"}, &stderr)
	if !ok || err != nil || path != "/etc/shortlink.yaml" {
		t.Fatalf("parseRedirectorArgs = (%q, %v, %v)", path, ok, err)
	}
	if _, ok, err := parseRedirectorArgs([]string{"-h"}, &stderr); ok || err != nil {
		t.Fatalf("help = (%v, %v), want (false, nil)", ok, err)
	}
	if _, ok, err := parseRedirectorArgs([]string{"extra"}, &stderr); ok || !errors.Is(err, errUsage) {
		t.Fatalf("extra arg = (%v, %v), want errUsage", ok, err)
	}
}

func TestLoadRedirectorConfig(t *testing.T) {
	base := baseRedirectorTestEnv()
	defaults := defaultRedirectorConfig()

	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		flagCfg bool
		want    RedirectorConfig
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{
			name: "yaml values with env overrides",
			yaml: `
database:
  max_open_conns: 20
  max_idle_conns: 4
  conn_max_lifetime: 1h30m
  conn_max_idle_time: 5m
http:
  request_timeout: 2s
  shutdown_timeout: 15s
  drain_delay: 9s
cache:
  fetch_timeout: 500ms
trusted_proxies:
  - 10.0.0.0/8
  - 172.16.0.0/12
log:
  level: warn
  format: json
redirector:
  addr: ":9090"
  ops_addr: "127.0.0.1:9091"
  pprof_addr: "127.0.0.1:6060"
  cache_miss_concurrency: 4
  access_log_sample_rate: 0.1
  access_log_slow_threshold: 150ms
console:
  auth:
    oidc:
      issuer: "not-decoded-by-redirector"
`,
			env: map[string]string{
				"ADDR":         ":19090",
				"DATABASE_URL": "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				"REDIS_URL":    "rediss://user:pass@redis:6380/1",
				"VISITOR_SALT": "prod-salt",
				"DRAIN_DELAY":  "3s",
				"LOG_LEVEL":    "info",
			},
			want: RedirectorConfig{
				BaseConfig: config.BaseConfig{
					Addr:        ":19090",
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
					LogLevel:          slog.LevelInfo,
					LogFormat:         config.LogFormatJSON,
					CacheFetchTimeout: 500 * time.Millisecond,
					Tracing:           config.DefaultTracingConfig(),
				},
				Features:    config.FeatureFlags{Analytics: true, RiskScan: true},
				VisitorSalt: "prod-salt",
				Proxy: config.ProxyConfig{
					TrustHeaders: true,
					TrustedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12")},
				},
				PprofAddr:              "127.0.0.1:6060",
				CacheMissConcurrency:   4,
				AccessLogSampleRate:    0.1,
				AccessLogSlowThreshold: 150 * time.Millisecond,
			},
		},
		{
			name:    "flag config overrides SHORTLINK_CONFIG",
			yaml:    "redirector:\n  cache_miss_concurrency: 12\n",
			env:     map[string]string{config.EnvConfigPath: "/does/not/exist.yaml"},
			flagCfg: true,
			want: func() RedirectorConfig {
				c := defaults
				c.CacheMissConcurrency = 12
				return c
			}(),
		},
		{
			name: "pprof on localhost",
			yaml: "redirector:\n  pprof_addr: localhost:6060\n",
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
		{name: "rejects visitor_salt in yaml", yaml: "redirector:\n  visitor_salt: leaked\n", wantErr: "visitor_salt"},
		{name: "rejects unknown top-level section", yaml: "unknown:\n  foo: 1\n", wantErr: "unknown"},
		{name: "ops addr collides with addr", yaml: "redirector:\n  addr: \":8080\"\n  ops_addr: \":8080\"\n", wantErr: "redirector.ops_addr must differ from addr"},
		{name: "invalid trusted proxies env", env: map[string]string{"TRUSTED_PROXIES": "bad-cidr"}, wantErr: "TRUSTED_PROXIES"},
		{name: "missing visitor salt", env: map[string]string{"VISITOR_SALT": ""}, wantErr: "VISITOR_SALT is required"},
		{name: "pprof on all interfaces", yaml: "redirector:\n  pprof_addr: \":6060\"\n", wantErr: "redirector.pprof_addr"},
		{name: "pprof on public address", yaml: "redirector:\n  pprof_addr: 0.0.0.0:6060\n", wantErr: "redirector.pprof_addr"},
		{name: "pprof without port", yaml: "redirector:\n  pprof_addr: 127.0.0.1\n", wantErr: "redirector.pprof_addr"},
		{name: "cache miss concurrency not positive", yaml: "redirector:\n  cache_miss_concurrency: 0\n", wantErr: "redirector.cache_miss_concurrency must be positive"},
		{name: "negative access log sample rate", yaml: "redirector:\n  access_log_sample_rate: -0.1\n", wantErr: "redirector.access_log_sample_rate must be between 0 and 1"},
		{name: "access log sample rate above 1", yaml: "redirector:\n  access_log_sample_rate: 1.5\n", wantErr: "redirector.access_log_sample_rate must be between 0 and 1"},
		{name: "nan access log sample rate", yaml: "redirector:\n  access_log_sample_rate: .nan\n", wantErr: "redirector.access_log_sample_rate must be between 0 and 1"},
		{name: "non-positive access log slow threshold", yaml: "redirector:\n  access_log_slow_threshold: 0s\n", wantErr: "redirector.access_log_slow_threshold must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var cfgPath string
			if tt.yaml != "" {
				p := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(p, []byte(tt.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
				if tt.flagCfg {
					cfgPath = p
				} else {
					t.Setenv(config.EnvConfigPath, p)
				}
			}

			got, err := loadRedirectorConfig(cfgPath)
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

func TestLoadRedirectorConfigExampleYAMLMatchesDefaults(t *testing.T) {
	for k, v := range baseRedirectorTestEnv() {
		t.Setenv(k, v)
	}
	got, err := loadRedirectorConfig(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("loadRedirectorConfig(config.example.yaml) = %v", err)
	}
	want := defaultRedirectorConfig()
	if !redirectorConfigEqual(got, want) {
		t.Errorf("config.example.yaml drifted from code defaults:\n got:  %+v\n want: %+v", got, want)
	}
}

func redirectorConfigEqual(a, b RedirectorConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.Features == b.Features &&
		a.VisitorSalt == b.VisitorSalt &&
		a.Proxy.TrustHeaders == b.Proxy.TrustHeaders &&
		slices.Equal(a.Proxy.TrustedCIDRs, b.Proxy.TrustedCIDRs) &&
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
