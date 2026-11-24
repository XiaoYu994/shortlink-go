package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
)

func baseTestEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":           "postgres://localhost/shortlink",
		"REDIS_URL":              "redis://localhost:6379/0",
		"DB_MAX_OPEN_CONNS":      "",
		"DB_MAX_IDLE_CONNS":      "",
		"DB_CONN_MAX_LIFETIME":   "",
		"DB_CONN_MAX_IDLE_TIME":  "",
		"REQUEST_TIMEOUT":        "",
		"SHUTDOWN_TIMEOUT":       "",
		"DRAIN_DELAY":            "",
		"CACHE_MISS_CONCURRENCY": "",
		"CACHE_FETCH_TIMEOUT":    "",
		"LOG_LEVEL":              "",
		"LOG_FORMAT":             "",
	}
}

func TestLoadBaseConfig(t *testing.T) {
	base := baseTestEnv()
	defaults := BaseConfig{
		Addr:                 ":8080",
		DatabaseURL:          "postgres://localhost/shortlink",
		RedisURL:             "redis://localhost:6379/0",
		Pool:                 pgdb.DefaultPoolConfig(),
		RequestTimeout:       DefaultRequestTimeout,
		ShutdownTimeout:      DefaultShutdownTimeout,
		DrainDelay:           DefaultDrainDelay,
		LogLevel:             slog.LevelInfo,
		LogFormat:            LogFormatText,
		CacheMissConcurrency: DefaultCacheMissConcurrency,
		CacheFetchTimeout:    DefaultCacheFetchTimeout,
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    BaseConfig
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{
			name: "explicit values",
			env: map[string]string{
				"DATABASE_URL":           "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				"DB_MAX_OPEN_CONNS":      "20",
				"DB_MAX_IDLE_CONNS":      "4",
				"DB_CONN_MAX_LIFETIME":   "1h30m",
				"DB_CONN_MAX_IDLE_TIME":  "5m",
				"REQUEST_TIMEOUT":        "2s",
				"SHUTDOWN_TIMEOUT":       "15s",
				"DRAIN_DELAY":            "3s",
				"CACHE_MISS_CONCURRENCY": "4",
				"CACHE_FETCH_TIMEOUT":    "500ms",
				"REDIS_URL":              "rediss://user:pass@redis:6380/1",
				"LOG_FORMAT":             "json",
				"LOG_LEVEL":              "debug",
			},
			want: BaseConfig{
				Addr:        ":8080",
				DatabaseURL: "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				RedisURL:    "rediss://user:pass@redis:6380/1",
				Pool: pgdb.PoolConfig{
					MaxOpenConns:    20,
					MaxIdleConns:    4,
					ConnMaxLifetime: 90 * time.Minute,
					ConnMaxIdleTime: 5 * time.Minute,
				},
				RequestTimeout:       2 * time.Second,
				ShutdownTimeout:      15 * time.Second,
				DrainDelay:           3 * time.Second,
				LogLevel:             slog.LevelDebug,
				LogFormat:            LogFormatJSON,
				CacheMissConcurrency: 4,
				CacheFetchTimeout:    500 * time.Millisecond,
			},
		},
		{name: "missing database url", env: map[string]string{"DATABASE_URL": ""}, wantErr: "DATABASE_URL is required"},
		{name: "missing redis url", env: map[string]string{"REDIS_URL": ""}, wantErr: "REDIS_URL is required"},
		{name: "invalid log format", env: map[string]string{"LOG_FORMAT": "yaml"}, wantErr: "LOG_FORMAT"},
		{name: "invalid log level", env: map[string]string{"LOG_LEVEL": "verbose"}, wantErr: "LOG_LEVEL"},
		{name: "invalid open conns", env: map[string]string{"DB_MAX_OPEN_CONNS": "abc"}, wantErr: "DB_MAX_OPEN_CONNS"},
		{name: "open conns not positive", env: map[string]string{"DB_MAX_OPEN_CONNS": "0"}, wantErr: "DB_MAX_OPEN_CONNS must be positive"},
		{name: "idle conns above open conns", env: map[string]string{"DB_MAX_OPEN_CONNS": "10", "DB_MAX_IDLE_CONNS": "11"}, wantErr: "DB_MAX_IDLE_CONNS must be between 0 and DB_MAX_OPEN_CONNS"},
		{name: "negative conn lifetime", env: map[string]string{"DB_CONN_MAX_LIFETIME": "-1s"}, wantErr: "connection lifetimes must not be negative"},
		{name: "negative drain delay", env: map[string]string{"DRAIN_DELAY": "-1s"}, wantErr: "DRAIN_DELAY must not be negative"},
		{name: "zero request timeout", env: map[string]string{"REQUEST_TIMEOUT": "0s"}, wantErr: "REQUEST_TIMEOUT must be positive"},
		{name: "zero shutdown timeout", env: map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, wantErr: "SHUTDOWN_TIMEOUT must be positive"},
		{name: "cache miss concurrency not positive", env: map[string]string{"CACHE_MISS_CONCURRENCY": "0"}, wantErr: "CACHE_MISS_CONCURRENCY must be positive"},
		{name: "cache fetch timeout exceeds request timeout", env: map[string]string{"REQUEST_TIMEOUT": "1s", "CACHE_FETCH_TIMEOUT": "2s"}, wantErr: "CACHE_FETCH_TIMEOUT must not exceed REQUEST_TIMEOUT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := LoadBaseConfig(BaseConfigDefaults{Addr: ":8080"})
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

func TestParseCIDRs(t *testing.T) {
	got, err := ParseCIDRs("10.0.0.0/8, 192.168.1.5, ::ffff:172.16.0.0/108, 2001:db8::/32")
	if err != nil {
		t.Fatalf("ParseCIDRs: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.5/32"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseCIDRs = %v, want %v", got, want)
	}
	for _, bad := range []string{"", " , ", "not-a-cidr", "10.0.0.0/99"} {
		if _, err := ParseCIDRs(bad); err == nil {
			t.Errorf("ParseCIDRs(%q) = nil, want error", bad)
		}
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
	}
	var buf bytes.Buffer
	logger, _ := base.NewLoggerWithService(&buf, "config-test")
	logger.Info("dump", "base", base)

	combined := buf.String() + "\n" + fmt.Sprintf("%+v", base)
	for _, secret := range []string{"pg-secret-pass", "redis-secret-pass"} {
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
