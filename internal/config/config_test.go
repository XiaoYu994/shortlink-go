package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"shortlink/internal/postgres"
)

func baseTestEnv() map[string]string {
	// make test 会导出 .env。每个子测试都要把读取的变量设一遍，空字符串表示未配置。
	return map[string]string{
		"ADDR":                   "",
		"CONSOLE_ADDR":           "",
		"DATABASE_URL":           "postgres://localhost/shortlink",
		"DB_MAX_OPEN_CONNS":      "",
		"DB_MAX_IDLE_CONNS":      "",
		"DB_CONN_MAX_LIFETIME":   "",
		"DB_CONN_MAX_IDLE_TIME":  "",
		"REQUEST_TIMEOUT":        "",
		"SHUTDOWN_TIMEOUT":       "",
		"DRAIN_DELAY":            "",
		"CACHE_MISS_CONCURRENCY": "",
		"CACHE_FETCH_TIMEOUT":    "",
		"REDIS_URL":              "redis://localhost:6379/0",
		"VISITOR_SALT":           "test-salt",
		"TRUST_PROXY_HEADERS":    "",
		"LOG_LEVEL":              "",
		"LOG_FORMAT":             "",
		"PPROF_ADDR":             "",
		"OIDC_ISSUER":            "",
		"OIDC_CLIENT_ID":         "",
		"OIDC_CLIENT_SECRET":     "",
		"OIDC_REDIRECT_URL":      "",
		"GITHUB_CLIENT_ID":       "",
		"GITHUB_CLIENT_SECRET":   "",
		"GITHUB_REDIRECT_URL":    "",
	}
}

func defaultBaseConfig(addr string) BaseConfig {
	return BaseConfig{
		Addr:                 addr,
		DatabaseURL:          "postgres://localhost/shortlink",
		RedisURL:             "redis://localhost:6379/0",
		Pool:                 postgres.DefaultPoolConfig(),
		RequestTimeout:       defaultRequestTimeout,
		ShutdownTimeout:      defaultShutdownTimeout,
		DrainDelay:           defaultDrainDelay,
		LogLevel:             slog.LevelInfo,
		LogFormat:            logFormatText,
		CacheMissConcurrency: defaultCacheMissConcurrency,
		CacheFetchTimeout:    defaultCacheFetchTimeout,
	}
}

func TestLoadRedirectorConfig(t *testing.T) {
	base := baseTestEnv()
	defaults := RedirectorConfig{
		BaseConfig:  defaultBaseConfig(defaultRedirectorAddr),
		VisitorSalt: "test-salt",
	}

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
				"ADDR":                   ":9090",
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
				"VISITOR_SALT":           "prod-salt",
				"TRUST_PROXY_HEADERS":    "true",
				"LOG_FORMAT":             "json",
			},
			want: RedirectorConfig{
				BaseConfig: BaseConfig{
					Addr:        ":9090",
					DatabaseURL: "postgres://user:pass@db:5432/shortlink?sslmode=disable",
					RedisURL:    "rediss://user:pass@redis:6380/1",
					Pool: postgres.PoolConfig{
						MaxOpenConns:    20,
						MaxIdleConns:    4,
						ConnMaxLifetime: 90 * time.Minute,
						ConnMaxIdleTime: 5 * time.Minute,
					},
					RequestTimeout:       2 * time.Second,
					ShutdownTimeout:      15 * time.Second,
					DrainDelay:           3 * time.Second,
					LogFormat:            logFormatJSON,
					CacheMissConcurrency: 4,
					CacheFetchTimeout:    500 * time.Millisecond,
				},
				VisitorSalt:       "prod-salt",
				TrustProxyHeaders: true,
			},
		},
		{
			name: "redis url and debug log",
			env: map[string]string{
				"REDIS_URL": "rediss://redis.example.com:6380/2",
				"LOG_LEVEL": "debug",
			},
			want: func() RedirectorConfig {
				c := defaults
				c.RedisURL = "rediss://redis.example.com:6380/2"
				c.LogLevel = slog.LevelDebug
				return c
			}(),
		},
		{
			name: "idle conns equal open conns",
			env: map[string]string{
				"DB_MAX_OPEN_CONNS": "10",
				"DB_MAX_IDLE_CONNS": "10",
			},
			want: func() RedirectorConfig {
				c := defaults
				c.Pool.MaxOpenConns = 10
				c.Pool.MaxIdleConns = 10
				return c
			}(),
		},
		{
			name: "zero drain delay",
			env:  map[string]string{"DRAIN_DELAY": "0s"},
			want: func() RedirectorConfig {
				c := defaults
				c.DrainDelay = 0
				return c
			}(),
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
			name:    "missing visitor salt",
			env:     map[string]string{"VISITOR_SALT": ""},
			wantErr: "VISITOR_SALT is required",
		},
		{
			name:    "pprof on all interfaces",
			env:     map[string]string{"PPROF_ADDR": ":6060"},
			wantErr: "PPROF_ADDR",
		},
		{
			name:    "pprof on public address",
			env:     map[string]string{"PPROF_ADDR": "0.0.0.0:6060"},
			wantErr: "PPROF_ADDR",
		},
		{
			name:    "pprof without port",
			env:     map[string]string{"PPROF_ADDR": "127.0.0.1"},
			wantErr: "PPROF_ADDR",
		},
		{
			name:    "invalid log format",
			env:     map[string]string{"LOG_FORMAT": "yaml"},
			wantErr: "LOG_FORMAT",
		},
		{
			name:    "missing database url",
			env:     map[string]string{"DATABASE_URL": ""},
			wantErr: "DATABASE_URL is required",
		},
		{
			name:    "missing redis url",
			env:     map[string]string{"REDIS_URL": ""},
			wantErr: "REDIS_URL is required",
		},
		{
			name:    "invalid log level",
			env:     map[string]string{"LOG_LEVEL": "verbose"},
			wantErr: "LOG_LEVEL",
		},
		{
			name:    "invalid trust proxy headers",
			env:     map[string]string{"TRUST_PROXY_HEADERS": "sometimes"},
			wantErr: "TRUST_PROXY_HEADERS",
		},
		{
			name:    "invalid open conns",
			env:     map[string]string{"DB_MAX_OPEN_CONNS": "abc"},
			wantErr: "DB_MAX_OPEN_CONNS",
		},
		{
			name:    "open conns not positive",
			env:     map[string]string{"DB_MAX_OPEN_CONNS": "0"},
			wantErr: "DB_MAX_OPEN_CONNS must be positive",
		},
		{
			name: "idle conns above open conns",
			env: map[string]string{
				"DB_MAX_OPEN_CONNS": "10",
				"DB_MAX_IDLE_CONNS": "11",
			},
			wantErr: "DB_MAX_IDLE_CONNS must be between 0 and DB_MAX_OPEN_CONNS",
		},
		{
			name:    "negative conn lifetime",
			env:     map[string]string{"DB_CONN_MAX_LIFETIME": "-1s"},
			wantErr: "connection lifetimes must not be negative",
		},
		{
			name:    "invalid drain delay",
			env:     map[string]string{"DRAIN_DELAY": "abc"},
			wantErr: "DRAIN_DELAY",
		},
		{
			name:    "negative drain delay",
			env:     map[string]string{"DRAIN_DELAY": "-1s"},
			wantErr: "DRAIN_DELAY must not be negative",
		},
		{
			name:    "invalid shutdown timeout",
			env:     map[string]string{"SHUTDOWN_TIMEOUT": "abc"},
			wantErr: "SHUTDOWN_TIMEOUT",
		},
		{
			name:    "invalid request timeout",
			env:     map[string]string{"REQUEST_TIMEOUT": "abc"},
			wantErr: "REQUEST_TIMEOUT",
		},
		{
			name:    "zero request timeout",
			env:     map[string]string{"REQUEST_TIMEOUT": "0s"},
			wantErr: "REQUEST_TIMEOUT must be positive",
		},
		{
			name:    "zero shutdown timeout",
			env:     map[string]string{"SHUTDOWN_TIMEOUT": "0s"},
			wantErr: "SHUTDOWN_TIMEOUT must be positive",
		},
		{
			name:    "invalid cache miss concurrency",
			env:     map[string]string{"CACHE_MISS_CONCURRENCY": "abc"},
			wantErr: "CACHE_MISS_CONCURRENCY",
		},
		{
			name:    "cache miss concurrency not positive",
			env:     map[string]string{"CACHE_MISS_CONCURRENCY": "0"},
			wantErr: "CACHE_MISS_CONCURRENCY must be positive",
		},
		{
			name:    "invalid cache fetch timeout",
			env:     map[string]string{"CACHE_FETCH_TIMEOUT": "abc"},
			wantErr: "CACHE_FETCH_TIMEOUT",
		},
		{
			name:    "cache fetch timeout not positive",
			env:     map[string]string{"CACHE_FETCH_TIMEOUT": "0s"},
			wantErr: "CACHE_FETCH_TIMEOUT must be positive",
		},
		{
			name: "cache fetch timeout exceeds request timeout",
			env: map[string]string{
				"REQUEST_TIMEOUT":     "1s",
				"CACHE_FETCH_TIMEOUT": "2s",
			},
			wantErr: "CACHE_FETCH_TIMEOUT must not exceed REQUEST_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)

			got, err := LoadRedirectorConfig()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadRedirectorConfig() error = nil, want substring %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("LoadRedirectorConfig() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadRedirectorConfig() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadRedirectorConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConsoleConfig(t *testing.T) {
	base := baseTestEnv()
	base["VISITOR_SALT"] = ""
	defaults := ConsoleConfig{
		BaseConfig: defaultBaseConfig(defaultConsoleAddr),
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    ConsoleConfig
		wantErr string
	}{
		{
			name: "defaults ignore ADDR and do not require VISITOR_SALT",
			env: map[string]string{
				"ADDR":       ":8080",
				"PPROF_ADDR": "0.0.0.0:6060",
			},
			want: defaults,
		},
		{
			name: "prefers CONSOLE_ADDR",
			env: map[string]string{
				"CONSOLE_ADDR": ":18081",
				"ADDR":         ":8080",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.Addr = ":18081"
				return c
			}(),
		},
		{
			name: "oidc all set",
			env: map[string]string{
				"OIDC_ISSUER":        "http://127.0.0.1:5556/dex",
				"OIDC_CLIENT_ID":     "shortlink-console",
				"OIDC_CLIENT_SECRET": "shortlink-console-secret",
				"OIDC_REDIRECT_URL":  "http://127.0.0.1:8081/api/v1/auth/callback",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.OIDC = OIDCConfig{
					Issuer:       "http://127.0.0.1:5556/dex",
					ClientID:     "shortlink-console",
					ClientSecret: "shortlink-console-secret",
					RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
				}
				return c
			}(),
		},
		{
			name: "oidc partially set",
			env: map[string]string{
				"OIDC_ISSUER":    "http://127.0.0.1:5556/dex",
				"OIDC_CLIENT_ID": "shortlink-console",
			},
			wantErr: "OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, and OIDC_REDIRECT_URL",
		},
		{
			name: "oidc invalid issuer scheme",
			env: map[string]string{
				"OIDC_ISSUER":        "ftp://127.0.0.1:5556/dex",
				"OIDC_CLIENT_ID":     "shortlink-console",
				"OIDC_CLIENT_SECRET": "shortlink-console-secret",
				"OIDC_REDIRECT_URL":  "http://127.0.0.1:8081/api/v1/auth/callback",
			},
			wantErr: "OIDC_ISSUER",
		},
		{
			name: "oidc issuer with query rejected",
			env: map[string]string{
				"OIDC_ISSUER":        "http://127.0.0.1:5556/dex?foo=1",
				"OIDC_CLIENT_ID":     "shortlink-console",
				"OIDC_CLIENT_SECRET": "shortlink-console-secret",
				"OIDC_REDIRECT_URL":  "http://127.0.0.1:8081/api/v1/auth/callback",
			},
			wantErr: "OIDC_ISSUER",
		},
		{
			name: "oidc invalid redirect url",
			env: map[string]string{
				"OIDC_ISSUER":        "http://127.0.0.1:5556/dex",
				"OIDC_CLIENT_ID":     "shortlink-console",
				"OIDC_CLIENT_SECRET": "shortlink-console-secret",
				"OIDC_REDIRECT_URL":  "/api/v1/auth/callback",
			},
			wantErr: "OIDC_REDIRECT_URL",
		},
		{
			name: "github all set",
			env: map[string]string{
				"GITHUB_CLIENT_ID":     "gh-client",
				"GITHUB_CLIENT_SECRET": "gh-secret",
				"GITHUB_REDIRECT_URL":  "http://127.0.0.1:8081/api/v1/auth/github/callback",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.GitHub = GitHubConfig{
					ClientID:     "gh-client",
					ClientSecret: "gh-secret",
					RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/github/callback",
				}
				return c
			}(),
		},
		{
			name: "github partially set",
			env: map[string]string{
				"GITHUB_CLIENT_ID": "gh-client",
			},
			wantErr: "GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET, and GITHUB_REDIRECT_URL",
		},
		{
			name: "github invalid redirect url",
			env: map[string]string{
				"GITHUB_CLIENT_ID":     "gh-client",
				"GITHUB_CLIENT_SECRET": "gh-secret",
				"GITHUB_REDIRECT_URL":  "/api/v1/auth/github/callback",
			},
			wantErr: "GITHUB_REDIRECT_URL",
		},
		{
			name: "oidc and github both set",
			env: map[string]string{
				"OIDC_ISSUER":          "http://127.0.0.1:5556/dex",
				"OIDC_CLIENT_ID":       "shortlink-console",
				"OIDC_CLIENT_SECRET":   "shortlink-console-secret",
				"OIDC_REDIRECT_URL":    "http://127.0.0.1:8081/api/v1/auth/callback",
				"GITHUB_CLIENT_ID":     "gh-client",
				"GITHUB_CLIENT_SECRET": "gh-secret",
				"GITHUB_REDIRECT_URL":  "http://127.0.0.1:8081/api/v1/auth/github/callback",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.OIDC = OIDCConfig{
					Issuer:       "http://127.0.0.1:5556/dex",
					ClientID:     "shortlink-console",
					ClientSecret: "shortlink-console-secret",
					RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
				}
				c.GitHub = GitHubConfig{
					ClientID:     "gh-client",
					ClientSecret: "gh-secret",
					RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/github/callback",
				}
				return c
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)

			got, err := LoadConsoleConfig()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConsoleConfig() error = nil, want substring %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("LoadConsoleConfig() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConsoleConfig() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadConsoleConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNewLoggerFormat(t *testing.T) {
	var buf bytes.Buffer
	cfg := BaseConfig{LogLevel: slog.LevelInfo, LogFormat: logFormatJSON}
	cfg.NewLogger(&buf).Info("listening", "addr", ":8080")
	if !strings.Contains(buf.String(), `"msg":"listening"`) {
		t.Fatalf("json log = %q", buf.String())
	}

	buf.Reset()
	cfg.LogFormat = logFormatText
	cfg.NewLogger(&buf).Info("listening", "addr", ":8080")
	if !strings.Contains(buf.String(), "msg=listening") || strings.Contains(buf.String(), `"msg"`) {
		t.Fatalf("text log = %q", buf.String())
	}
}

// applyEnv 先铺上 base，再用 override 覆盖。子测试结束时 t.Setenv 会恢复原值。
func applyEnv(t *testing.T, base, override map[string]string) {
	t.Helper()
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range override {
		t.Setenv(k, v)
	}
}
