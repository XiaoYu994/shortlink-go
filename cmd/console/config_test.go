package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
	"shortlink/internal/risk"
)

func baseConsoleTestEnv() map[string]string {
	return map[string]string{
		"ADDR":                        "",
		"CONSOLE_ADDR":                "",
		"CONSOLE_OPS_ADDR":            "",
		"DATABASE_URL":                "postgres://localhost/shortlink",
		"REDIS_URL":                   "redis://localhost:6379/0",
		"LOG_LEVEL":                   "",
		"LOG_FORMAT":                  "",
		"PPROF_ADDR":                  "",
		"TRUST_PROXY_HEADERS":         "",
		"TRUSTED_PROXY_CIDRS":         "",
		"CONSOLE_IP_RATE_LIMIT":       "",
		"CONSOLE_IP_RATE_BURST":       "",
		"OIDC_ISSUER":                 "",
		"OIDC_CLIENT_ID":              "",
		"OIDC_CLIENT_SECRET":          "",
		"OIDC_REDIRECT_URL":           "",
		"GITHUB_CLIENT_ID":            "",
		"GITHUB_CLIENT_SECRET":        "",
		"GITHUB_REDIRECT_URL":         "",
		"WEB_DIST_DIR":                "",
		"PUBLIC_BASE_URL":             "",
		"WORKER_ADDR":                 "",
		"WORKER_CONCURRENCY":          "",
		"WORKER_MAX_PER_TENANT":       "",
		"WORKER_POLL_INTERVAL":        "",
		"WORKER_LEASE_DURATION":       "",
		"WORKER_JOB_TIMEOUT":          "",
		"WORKER_REAP_INTERVAL":        "",
		"WORKER_CLEANUP_INTERVAL":     "",
		"WORKER_SESSION_RETENTION":    "",
		"WORKER_DONE_JOB_RETENTION":   "",
		"WORKER_DEAD_JOB_RETENTION":   "",
		"WORKER_INVITATION_RETENTION": "",
		"RISK_FETCH_TIMEOUT":          "",
		"RISK_HOST_RATE_LIMIT":        "",
		"RISK_BLOCKED_HOSTS":          "",
		"RISK_BLOCKED_KEYWORDS":       "",
		"RISK_ALLOWED_PORTS":          "",
	}
}

func defaultBaseConfig(addr string) config.BaseConfig {
	return config.BaseConfig{
		Addr:                 addr,
		DatabaseURL:          "postgres://localhost/shortlink",
		RedisURL:             "redis://localhost:6379/0",
		Pool:                 pgdb.DefaultPoolConfig(),
		RequestTimeout:       config.DefaultRequestTimeout,
		ShutdownTimeout:      config.DefaultShutdownTimeout,
		DrainDelay:           config.DefaultDrainDelay,
		LogLevel:             slog.LevelInfo,
		LogFormat:            config.LogFormatText,
		CacheMissConcurrency: cache.DefaultMissConcurrency,
		CacheFetchTimeout:    cache.DefaultFetchTimeout,
	}
}

func TestLoadConsoleConfig(t *testing.T) {
	base := baseConsoleTestEnv()
	defaults := ConsoleConfig{
		BaseConfig:  defaultBaseConfig(defaultConsoleAddr),
		IPRateLimit: defaultConsoleIPRateLimit,
		IPRateBurst: defaultConsoleIPRateBurst,
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
			name: "prefers CONSOLE_ADDR and CONSOLE_OPS_ADDR",
			env: map[string]string{
				"CONSOLE_ADDR":     ":18081",
				"CONSOLE_OPS_ADDR": "127.0.0.1:9091",
				"ADDR":             ":8080",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.Addr = ":18081"
				c.OpsAddr = "127.0.0.1:9091"
				return c
			}(),
		},
		{
			name: "trusted proxies and custom ip limit",
			env: map[string]string{
				"TRUST_PROXY_HEADERS":   "true",
				"TRUSTED_PROXY_CIDRS":   "10.0.0.0/8",
				"CONSOLE_IP_RATE_LIMIT": "25",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.TrustProxyHeaders = true
				c.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
				c.IPRateLimit = 25
				c.IPRateBurst = 50
				return c
			}(),
		},
		{
			name: "disable ip rate limit",
			env:  map[string]string{"CONSOLE_IP_RATE_LIMIT": "0"},
			want: func() ConsoleConfig {
				c := defaults
				c.IPRateLimit = 0
				return c
			}(),
		},
		{
			name:    "ops addr equals console addr",
			env:     map[string]string{"CONSOLE_ADDR": ":8081", "CONSOLE_OPS_ADDR": ":8081"},
			wantErr: "CONSOLE_OPS_ADDR must differ from CONSOLE_ADDR",
		},
		{
			name:    "trusted cidrs without trust header",
			env:     map[string]string{"TRUSTED_PROXY_CIDRS": "10.0.0.0/8"},
			wantErr: "TRUSTED_PROXY_CIDRS requires TRUST_PROXY_HEADERS=true",
		},
		{
			name:    "negative ip rate limit",
			env:     map[string]string{"CONSOLE_IP_RATE_LIMIT": "-1"},
			wantErr: "CONSOLE_IP_RATE_LIMIT must not be negative",
		},
		{
			name:    "zero ip burst when limit enabled",
			env:     map[string]string{"CONSOLE_IP_RATE_LIMIT": "10", "CONSOLE_IP_RATE_BURST": "0"},
			wantErr: "CONSOLE_IP_RATE_BURST must be positive",
		},
		{
			name: "web dist dir trimmed",
			env: map[string]string{
				"WEB_DIST_DIR":    "  web/dist  ",
				"PUBLIC_BASE_URL": " http://127.0.0.1:8080/ ",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.WebDistDir = "web/dist"
				c.PublicBaseURL = "http://127.0.0.1:8080"
				return c
			}(),
		},
		{
			name:    "invalid public base url",
			env:     map[string]string{"PUBLIC_BASE_URL": "ftp://example.com"},
			wantErr: "PUBLIC_BASE_URL",
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
			name:    "github partially set",
			env:     map[string]string{"GITHUB_CLIENT_ID": "gh-client"},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)

			got, err := loadConsoleConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadConsoleConfig() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConsoleConfig() returned error: %v", err)
			}
			if !consoleConfigEqual(got, tt.want) {
				t.Errorf("loadConsoleConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadWorkerConfig(t *testing.T) {
	base := baseConsoleTestEnv()

	defaults := WorkerConfig{
		BaseConfig:          defaultBaseConfig(""),
		Concurrency:         job.DefaultConcurrency,
		MaxPerTenant:        job.DefaultMaxPerTenant,
		PollInterval:        job.DefaultPollInterval,
		LeaseDuration:       job.DefaultLeaseDuration,
		JobTimeout:          job.DefaultJobTimeout,
		ReapInterval:        job.DefaultReapInterval,
		CleanupInterval:     defaultWorkerCleanupInterval,
		SessionRetention:    defaultWorkerRetention,
		DoneJobRetention:    defaultWorkerRetention,
		DeadJobRetention:    defaultWorkerLongRetention,
		InvitationRetention: defaultWorkerLongRetention,
		StatsRetentionDays:  defaultStatsRetentionDays,
		AuditRetentionDays:  defaultAuditRetentionDays,
		RiskFetchTimeout:    risk.DefaultFetchTimeout,
		RiskHostRateLimit:   risk.DefaultHostRateLimit,
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    WorkerConfig
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{
			name: "explicit worker values",
			env: map[string]string{
				"WORKER_ADDR":                 "127.0.0.1:8082",
				"WORKER_CONCURRENCY":          "16",
				"WORKER_MAX_PER_TENANT":       "2",
				"WORKER_POLL_INTERVAL":        "100ms",
				"WORKER_LEASE_DURATION":       "45s",
				"WORKER_JOB_TIMEOUT":          "15s",
				"WORKER_REAP_INTERVAL":        "10s",
				"WORKER_CLEANUP_INTERVAL":     "30m",
				"WORKER_SESSION_RETENTION":    "24h",
				"WORKER_DONE_JOB_RETENTION":   "48h",
				"WORKER_DEAD_JOB_RETENTION":   "168h",
				"WORKER_INVITATION_RETENTION": "168h",
				"WORKER_STATS_RETENTION_DAYS": "45",
				"WORKER_AUDIT_RETENTION_DAYS": "365",
				"RISK_FETCH_TIMEOUT":          "3s",
				"RISK_HOST_RATE_LIMIT":        "5",
				"RISK_BLOCKED_HOSTS":          "bad.example:malware",
				"RISK_BLOCKED_KEYWORDS":       "smoke-phish:phishing",
				"RISK_ALLOWED_PORTS":          "80, 443, 8443",
			},
			want: WorkerConfig{
				BaseConfig:          defaultBaseConfig("127.0.0.1:8082"),
				Concurrency:         16,
				MaxPerTenant:        2,
				PollInterval:        100 * time.Millisecond,
				LeaseDuration:       45 * time.Second,
				JobTimeout:          15 * time.Second,
				ReapInterval:        10 * time.Second,
				CleanupInterval:     30 * time.Minute,
				SessionRetention:    24 * time.Hour,
				DoneJobRetention:    48 * time.Hour,
				DeadJobRetention:    168 * time.Hour,
				InvitationRetention: 168 * time.Hour,
				StatsRetentionDays:  45,
				AuditRetentionDays:  365,
				RiskFetchTimeout:    3 * time.Second,
				RiskHostRateLimit:   5,
				RiskBlockedHosts:    map[string]link.BanReason{"bad.example": link.BanReasonMalware},
				RiskBlockedKeywords: map[string]link.BanReason{"smoke-phish": link.BanReasonPhishing},
				RiskAllowedPorts:    []int{80, 443, 8443},
			},
		},
		{
			name:    "job timeout without commit margin exceeds lease",
			env:     map[string]string{"WORKER_LEASE_DURATION": "30s", "WORKER_JOB_TIMEOUT": "28s"},
			wantErr: "WORKER_JOB_TIMEOUT + 5s commit margin must not exceed WORKER_LEASE_DURATION",
		},
		{
			name:    "zero concurrency",
			env:     map[string]string{"WORKER_CONCURRENCY": "0"},
			wantErr: "WORKER_CONCURRENCY must be positive",
		},
		{
			name:    "negative max per tenant",
			env:     map[string]string{"WORKER_MAX_PER_TENANT": "-1"},
			wantErr: "WORKER_MAX_PER_TENANT must not be negative",
		},
		{
			name:    "invalid risk blocked hosts",
			env:     map[string]string{"RISK_BLOCKED_HOSTS": "badrule"},
			wantErr: "RISK_BLOCKED_HOSTS",
		},
		{
			name:    "invalid risk blocked keywords",
			env:     map[string]string{"RISK_BLOCKED_KEYWORDS": "badrule"},
			wantErr: "RISK_BLOCKED_KEYWORDS",
		},
		{
			name:    "invalid risk allowed ports",
			env:     map[string]string{"RISK_ALLOWED_PORTS": "80,99999"},
			wantErr: "RISK_ALLOWED_PORTS",
		},
		{
			name:    "non-positive stats retention days",
			env:     map[string]string{"WORKER_STATS_RETENTION_DAYS": "0"},
			wantErr: "WORKER_STATS_RETENTION_DAYS must be positive",
		},
		{
			name:    "non-positive audit retention days",
			env:     map[string]string{"WORKER_AUDIT_RETENTION_DAYS": "-5"},
			wantErr: "WORKER_AUDIT_RETENTION_DAYS must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)
			got, err := loadWorkerConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadWorkerConfig() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadWorkerConfig() returned error: %v", err)
			}
			if !workerConfigEqual(got, tt.want) ||
				!maps.Equal(got.RiskBlockedHosts, tt.want.RiskBlockedHosts) ||
				!maps.Equal(got.RiskBlockedKeywords, tt.want.RiskBlockedKeywords) ||
				!slices.Equal(got.RiskAllowedPorts, tt.want.RiskAllowedPorts) {
				t.Errorf("loadWorkerConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func consoleConfigEqual(a, b ConsoleConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.OIDC == b.OIDC &&
		a.GitHub == b.GitHub &&
		a.WebDistDir == b.WebDistDir &&
		a.PublicBaseURL == b.PublicBaseURL &&
		a.TrustProxyHeaders == b.TrustProxyHeaders &&
		slices.Equal(a.TrustedProxyCIDRs, b.TrustedProxyCIDRs) &&
		a.IPRateLimit == b.IPRateLimit &&
		a.IPRateBurst == b.IPRateBurst
}

func workerConfigEqual(a, b WorkerConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.Concurrency == b.Concurrency &&
		a.MaxPerTenant == b.MaxPerTenant &&
		a.PollInterval == b.PollInterval &&
		a.LeaseDuration == b.LeaseDuration &&
		a.JobTimeout == b.JobTimeout &&
		a.ReapInterval == b.ReapInterval &&
		a.CleanupInterval == b.CleanupInterval &&
		a.SessionRetention == b.SessionRetention &&
		a.DoneJobRetention == b.DoneJobRetention &&
		a.DeadJobRetention == b.DeadJobRetention &&
		a.InvitationRetention == b.InvitationRetention &&
		a.StatsRetentionDays == b.StatsRetentionDays &&
		a.AuditRetentionDays == b.AuditRetentionDays &&
		a.RiskFetchTimeout == b.RiskFetchTimeout &&
		a.RiskHostRateLimit == b.RiskHostRateLimit
}

func TestConsoleConfigRedaction(t *testing.T) {
	base := defaultBaseConfig(":8081")
	base.DatabaseURL = "postgres://shortlink:pg-secret@db:5432/shortlink?sslmode=disable"
	base.RedisURL = "rediss://:redis-secret@redis:6380/0"

	oidcCfg := OIDCConfig{
		Issuer:       "https://dex.example.com",
		ClientID:     "shortlink-console",
		ClientSecret: "oidc-top-secret",
		RedirectURL:  "https://console.example.com/api/v1/auth/callback",
	}
	ghCfg := GitHubConfig{
		ClientID:     "gh-client",
		ClientSecret: "github-top-secret",
		RedirectURL:  "https://console.example.com/api/v1/auth/github/callback",
	}
	conCfg := ConsoleConfig{BaseConfig: base, OIDC: oidcCfg, GitHub: ghCfg}
	workerCfg := WorkerConfig{BaseConfig: base, Concurrency: 8, RiskBlockedKeywords: map[string]link.BanReason{"evil": link.BanReasonPhishing}}

	var buf bytes.Buffer
	logger, _ := base.NewLoggerWithService(&buf, "console-config-test")
	logger.Info("dump", "console", conCfg, "worker", workerCfg, "oidc", oidcCfg, "github", ghCfg)

	combined := buf.String() + "\n" + fmt.Sprintf("%+v %+v %+v %+v", conCfg, workerCfg, oidcCfg, ghCfg)
	for _, secret := range []string{"pg-secret", "redis-secret", "oidc-top-secret", "github-top-secret"} {
		if strings.Contains(combined, secret) {
			t.Errorf("secret %q leaked in config output: %s", secret, combined)
		}
	}
	if !strings.Contains(buf.String(), logx.Redacted) {
		t.Errorf("expected redacted placeholder: %s", buf.String())
	}
}

func applyEnv(t *testing.T, base, override map[string]string) {
	t.Helper()
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range override {
		t.Setenv(k, v)
	}
}
