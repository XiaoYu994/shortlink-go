package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
)

func baseTestEnv() map[string]string {
	return map[string]string{
		EnvConfigPath:                 "",
		"DATABASE_URL":                "postgres://localhost/shortlink",
		"REDIS_URL":                   "redis://localhost:6379/0",
		"LOG_LEVEL":                   "",
		"LOG_FORMAT":                  "",
		"DRAIN_DELAY":                 "",
		"TRUSTED_PROXIES":             "",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "",
	}
}

func loadFileFromYAML(t *testing.T, content string) *File {
	t.Helper()
	if strings.TrimSpace(content) == "" {
		return &File{}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return f
}

func TestLoadBaseConfig(t *testing.T) {
	base := baseTestEnv()
	defaults := BaseConfig{
		Addr:              ":8080",
		DatabaseURL:       "postgres://localhost/shortlink",
		RedisURL:          "redis://localhost:6379/0",
		Pool:              pgdb.DefaultPoolConfig(),
		RequestTimeout:    DefaultRequestTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		DrainDelay:        DefaultDrainDelay,
		LogLevel:          slog.LevelInfo,
		LogFormat:         LogFormatText,
		CacheFetchTimeout: DefaultCacheFetchTimeout,
		Tracing:           DefaultTracingConfig(),
	}

	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		want    BaseConfig
		wantErr string
	}{
		{name: "defaults without yaml", want: defaults},
		{
			name: "yaml values with env overrides",
			yaml: `
database:
  max_open_conns: 20
  max_idle_conns: 0
  conn_max_lifetime: 1h30m
  conn_max_idle_time: 5m
http:
  request_timeout: 2s
  shutdown_timeout: 15s
  drain_delay: 9s
cache:
  fetch_timeout: 500ms
log:
  level: warn
  format: text
tracing:
  endpoint: http://yaml-otel:4318
  sample_rate: 0
`,
			env: map[string]string{
				"DATABASE_URL":                "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				"REDIS_URL":                   "rediss://user:pass@redis:6380/1",
				"LOG_FORMAT":                  "json",
				"LOG_LEVEL":                   "debug",
				"DRAIN_DELAY":                 "3s",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			},
			want: BaseConfig{
				Addr:        ":8080",
				DatabaseURL: "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				RedisURL:    "rediss://user:pass@redis:6380/1",
				Pool: pgdb.PoolConfig{
					MaxOpenConns:    20,
					MaxIdleConns:    0,
					ConnMaxLifetime: 90 * time.Minute,
					ConnMaxIdleTime: 5 * time.Minute,
				},
				RequestTimeout:    2 * time.Second,
				ShutdownTimeout:   15 * time.Second,
				DrainDelay:        3 * time.Second,
				LogLevel:          slog.LevelDebug,
				LogFormat:         LogFormatJSON,
				CacheFetchTimeout: 500 * time.Millisecond,
				Tracing: TracingConfig{
					Endpoint:   "http://127.0.0.1:4318",
					SampleRate: 0,
				},
			},
		},
		{name: "missing database url", env: map[string]string{"DATABASE_URL": ""}, wantErr: "DATABASE_URL is required"},
		{
			name: "empty redis url disables cache",
			env:  map[string]string{"REDIS_URL": ""},
			want: func() BaseConfig { c := defaults; c.RedisURL = ""; return c }(),
		},
		{name: "rejects database url in yaml", yaml: "database:\n  url: postgres://bad/db\n", wantErr: "database"},
		{name: "rejects redis url in yaml", yaml: "cache:\n  redis_url: redis://bad\n", wantErr: "cache"},
		{name: "invalid log format", env: map[string]string{"LOG_FORMAT": "yaml"}, wantErr: "LOG_FORMAT"},
		{name: "invalid yaml log format", yaml: "log:\n  format: xml\n", wantErr: "log.format"},
		{name: "invalid log level", env: map[string]string{"LOG_LEVEL": "verbose"}, wantErr: "LOG_LEVEL"},
		{name: "open conns not positive", yaml: "database:\n  max_open_conns: 0\n", wantErr: "database.max_open_conns must be positive"},
		{name: "idle conns above open conns", yaml: "database:\n  max_open_conns: 10\n  max_idle_conns: 11\n", wantErr: "database.max_idle_conns must be between 0 and database.max_open_conns"},
		{name: "negative conn lifetime", yaml: "database:\n  conn_max_lifetime: -1s\n", wantErr: "connection lifetimes must not be negative"},
		{name: "negative drain delay env", env: map[string]string{"DRAIN_DELAY": "-1s"}, wantErr: "drain delay must not be negative"},
		{name: "zero request timeout", yaml: "http:\n  request_timeout: 0s\n", wantErr: "http.request_timeout must be positive"},
		{name: "zero shutdown timeout", yaml: "http:\n  shutdown_timeout: 0s\n", wantErr: "http.shutdown_timeout must be positive"},
		{name: "cache fetch timeout exceeds request timeout", yaml: "http:\n  request_timeout: 1s\ncache:\n  fetch_timeout: 2s\n", wantErr: "cache.fetch_timeout must not exceed http.request_timeout"},
		{name: "invalid otel endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "grpc://127.0.0.1:4317"}, wantErr: "tracing.endpoint"},
		{name: "otel endpoint with query", yaml: "tracing:\n  endpoint: http://127.0.0.1:4318?foo=1\n", wantErr: "tracing.endpoint"},
		{name: "negative otel sample rate", yaml: "tracing:\n  sample_rate: -0.1\n", wantErr: "tracing.sample_rate must be between 0 and 1"},
		{name: "otel sample rate above 1", yaml: "tracing:\n  sample_rate: 1.5\n", wantErr: "tracing.sample_rate must be between 0 and 1"},
		{name: "nan otel sample rate", yaml: "tracing:\n  sample_rate: .nan\n", wantErr: "tracing.sample_rate must be between 0 and 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			f := loadFileFromYAML(t, tt.yaml)
			got, err := LoadBaseConfig(f, BaseConfigDefaults{Addr: ":8080"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadBaseConfig() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadBaseConfig() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadBaseConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadProxyConfig(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		t.Setenv("TRUSTED_PROXIES", "")
		got, err := LoadProxyConfig(&File{})
		if err != nil || got.TrustHeaders || len(got.TrustedCIDRs) != 0 {
			t.Fatalf("got = %+v, err = %v", got, err)
		}
	})

	t.Run("wildcard from yaml", func(t *testing.T) {
		t.Setenv("TRUSTED_PROXIES", "")
		f := loadFileFromYAML(t, "trusted_proxies:\n  - \"*\"\n")
		got, err := LoadProxyConfig(f)
		if err != nil || !got.TrustHeaders || len(got.TrustedCIDRs) != 0 {
			t.Fatalf("got = %+v, err = %v", got, err)
		}
	})

	t.Run("cidrs from yaml overridden by env", func(t *testing.T) {
		f := loadFileFromYAML(t, "trusted_proxies:\n  - 10.0.0.0/8\n")
		t.Setenv("TRUSTED_PROXIES", "192.168.1.5, ::ffff:172.16.0.0/108, 2001:db8::/32")
		got, err := LoadProxyConfig(f)
		if err != nil {
			t.Fatalf("LoadProxyConfig: %v", err)
		}
		want := []netip.Prefix{
			netip.MustParsePrefix("192.168.1.5/32"),
			netip.MustParsePrefix("172.16.0.0/12"),
			netip.MustParsePrefix("2001:db8::/32"),
		}
		if !got.TrustHeaders || !slices.Equal(got.TrustedCIDRs, want) {
			t.Fatalf("got = %+v, want cidrs %v", got, want)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			yaml string
			env  string
		}{
			{name: "mix wildcard and cidr in yaml", yaml: "trusted_proxies:\n  - \"*\"\n  - 10.0.0.0/8\n"},
			{name: "blank entry in yaml", yaml: "trusted_proxies:\n  - \"\"\n"},
			{name: "bad cidr in yaml", yaml: "trusted_proxies:\n  - 10.0.0.0/99\n"},
			{name: "mix wildcard in env", env: "*,10.0.0.0/8"},
			{name: "only comma in env", env: " , "},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("TRUSTED_PROXIES", tc.env)
				f := loadFileFromYAML(t, tc.yaml)
				if _, err := LoadProxyConfig(f); err == nil {
					t.Fatal("expected error, got nil")
				}
			})
		}
	})
}

func TestLoadOutboundAllowedPorts(t *testing.T) {
	f := loadFileFromYAML(t, "outbound:\n  allowed_ports: [80, 443, 8443]\n")
	got, err := LoadOutboundAllowedPorts(f)
	if err != nil || !slices.Equal(got, []int{80, 443, 8443}) {
		t.Fatalf("got = %v, err = %v", got, err)
	}

	fBad := loadFileFromYAML(t, "outbound:\n  allowed_ports: [80, 70000]\n")
	if _, err := LoadOutboundAllowedPorts(fBad); err == nil || !strings.Contains(err.Error(), "outbound.allowed_ports") {
		t.Fatalf("err = %v, want outbound.allowed_ports", err)
	}
}

func TestNewLoggerFormat(t *testing.T) {
	var buf bytes.Buffer
	cfg := BaseConfig{LogLevel: slog.LevelInfo, LogFormat: LogFormatJSON}
	cfg.NewLogger(&buf).Info("listening", "addr", ":8080")
	if !strings.Contains(buf.String(), `"msg":"listening"`) {
		t.Fatalf("json log = %q", buf.String())
	}

	buf.Reset()
	cfg.LogFormat = LogFormatText
	cfg.NewLogger(&buf).Info("listening", "addr", ":8080")
	out := buf.String()
	if !strings.Contains(out, "listening") || !strings.Contains(out, "addr=:8080") || strings.Contains(out, `"msg"`) {
		t.Fatalf("text log = %q", out)
	}
}

func TestNewLoggerWithServiceAndDynamicLevel(t *testing.T) {
	var buf bytes.Buffer
	cfg := BaseConfig{LogLevel: slog.LevelInfo, LogFormat: LogFormatJSON}
	logger, levelVar := cfg.NewLoggerWithService(&buf, "console-worker")

	logger.Debug("skipped")
	if buf.Len() != 0 {
		t.Fatalf("unexpected debug log: %s", buf.String())
	}

	levelVar.Set(slog.LevelDebug)
	ctx := logx.WithRequestID(context.Background(), "req-99")
	ctx = logx.WithJob(ctx, 17, "link.risk_scan", 5)
	logger.DebugContext(ctx, "running")

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decode log: %v\n%s", err, buf.String())
	}
	if entry["service"] != "console-worker" || entry["request_id"] != "req-99" ||
		entry["job_id"] != float64(17) || entry["job_kind"] != "link.risk_scan" || entry["tenant_id"] != float64(5) {
		t.Errorf("entry = %+v", entry)
	}
}

func TestBaseConfigRedaction(t *testing.T) {
	base := BaseConfig{
		Addr:        ":8080",
		OpsAddr:     ":9090",
		DatabaseURL: "postgres://shortlink:pg-secret-pass@db:5432/shortlink?sslmode=disable",
		RedisURL:    "rediss://:redis-secret-pass@redis:6380/0",
		LogFormat:   LogFormatJSON,
		Tracing: TracingConfig{
			Endpoint:   "https://user:otel-secret-pass@otel.example.com:4318",
			SampleRate: 0.5,
		},
	}
	var buf bytes.Buffer
	logger, _ := base.NewLoggerWithService(&buf, "config-test")
	logger.Info("dump", "base", base)

	combined := buf.String() + "\n" + fmt.Sprintf("%+v", base)
	for _, secret := range []string{"pg-secret-pass", "redis-secret-pass", "otel-secret-pass"} {
		if strings.Contains(combined, secret) {
			t.Errorf("secret %q leaked in config output: %s", secret, combined)
		}
	}
	if !strings.Contains(buf.String(), "postgres://shortlink:xxxxx@db:5432/shortlink?sslmode=disable") {
		t.Errorf("expected url-redacted placeholder in log output: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "sslmode=disable") {
		t.Errorf("safe DSN parameter lost in log output: %s", buf.String())
	}
}
