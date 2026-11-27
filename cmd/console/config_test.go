package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/logx"
	"shortlink/internal/mailer"
	"shortlink/internal/pgdb"
	"shortlink/internal/risk"
	"shortlink/internal/sms"
	"shortlink/internal/webhook"
)

func baseConsoleTestEnv() map[string]string {
	return map[string]string{
		config.EnvConfigPath:          "",
		"ADDR":                        "",
		"CONSOLE_ADDR":                "",
		"CONSOLE_OPS_ADDR":            "",
		"WORKER_ADDR":                 "",
		"DATABASE_URL":                "postgres://localhost/shortlink",
		"REDIS_URL":                   "redis://localhost:6379/0",
		"LOG_LEVEL":                   "",
		"LOG_FORMAT":                  "",
		"DRAIN_DELAY":                 "",
		"TRUSTED_PROXIES":             "",
		"OIDC_CLIENT_SECRET":          "",
		"GITHUB_CLIENT_SECRET":        "",
		"SMS_WEBHOOK_TOKEN":           "",
		"WEBHOOK_SECRET_KEY":          "",
		"SMTP_PASSWORD":               "",
		"RISK_GSB_API_KEY":            "",
		"RISK_LLM_API_KEY":            "",
		"FEATURE_ANALYTICS":           "",
		"FEATURE_RISK_SCAN":           "",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "",
	}
}

func defaultBaseConfig(addr string) config.BaseConfig {
	return config.BaseConfig{
		Addr:              addr,
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
	}
}

func defaultConsoleConfig() ConsoleConfig {
	return ConsoleConfig{
		BaseConfig: defaultBaseConfig(defaultConsoleAddr),
		Features:   config.FeatureFlags{Analytics: true, RiskScan: true},
		PhoneOTP: PhoneOTPConfig{
			Timeout:     sms.DefaultTimeout,
			TTL:         identity.DefaultOTPTTL,
			Cooldown:    identity.DefaultOTPCooldown,
			MaxPerHour:  identity.DefaultOTPMaxPerHour,
			MaxAttempts: identity.DefaultOTPMaxAttempts,
		},
		IPRateLimit: defaultConsoleIPRateLimit,
		IPRateBurst: defaultConsoleIPRateBurst,
	}
}

func defaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		BaseConfig:         defaultBaseConfig(""),
		Features:           config.FeatureFlags{Analytics: true, RiskScan: true},
		Concurrency:        job.DefaultConcurrency,
		MaxPerTenant:       job.DefaultMaxPerTenant,
		PollInterval:       job.DefaultPollInterval,
		LeaseDuration:      job.DefaultLeaseDuration,
		JobTimeout:         job.DefaultJobTimeout,
		ReapInterval:       job.DefaultReapInterval,
		CleanupInterval:    defaultWorkerCleanupInterval,
		RetentionShort:     defaultWorkerShortRetention,
		RetentionLong:      defaultWorkerLongRetention,
		StatsRetentionDays: defaultStatsRetentionDays,
		AuditRetentionDays: defaultAuditRetentionDays,
		RiskFetchTimeout:   risk.DefaultFetchTimeout,
		RiskHostRateLimit:  risk.DefaultHostRateLimit,
		RiskGSB: RiskGSBConfig{
			BaseURL: risk.DefaultGSBBaseURL,
			Timeout: risk.DefaultGSBTimeout,
		},
		RiskLLM: RiskLLMConfig{
			Mode:          risk.LLMModeBan,
			MinConfidence: risk.DefaultLLMMinConfidence,
			Timeout:       risk.DefaultLLMTimeout,
			MaxPerMin:     risk.DefaultLLMMaxPerMin,
		},
		WebhookTimeout:                webhook.DefaultTimeout,
		WebhookHostRateLimit:          webhook.DefaultHostRateLimit,
		WebhookRetryBaseDelay:         webhook.DefaultRetryBaseDelay,
		WebhookRetryMaxDelay:          webhook.DefaultRetryMaxDelay,
		WebhookMaxConsecutiveFailures: webhook.DefaultMaxConsecutiveFailures,
		SMTP: mailer.SMTPConfig{
			Port:    mailer.DefaultSMTPPort,
			TLSMode: mailer.TLSModeStartTLS,
			Timeout: mailer.DefaultSMTPTimeout,
		},
	}
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	if strings.TrimSpace(content) == "" {
		return ""
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConsoleConfig(t *testing.T) {
	base := baseConsoleTestEnv()
	defaults := defaultConsoleConfig()

	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		flagCfg bool
		want    ConsoleConfig
		wantErr string
	}{
		{
			name: "defaults ignore ADDR and redirector section",
			yaml: "redirector:\n  addr: \":8080\"\n  pprof_addr: \"0.0.0.0:6060\"\n  cache_miss_concurrency: 0\n",
			env: map[string]string{
				"ADDR": ":8080",
			},
			want: defaults,
		},
		{
			name: "prefers CONSOLE_ADDR and CONSOLE_OPS_ADDR env over yaml",
			yaml: "console:\n  addr: \":8088\"\n  ops_addr: \"127.0.0.1:9088\"\n",
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
			name: "trusted proxies and derived ip burst",
			yaml: `
trusted_proxies:
  - 10.0.0.0/8
console:
  ip_rate_limit: 25
`,
			want: func() ConsoleConfig {
				c := defaults
				c.Proxy = config.ProxyConfig{
					TrustHeaders: true,
					TrustedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
				}
				c.IPRateLimit = 25
				c.IPRateBurst = 50
				return c
			}(),
		},
		{
			name: "disable ip rate limit",
			yaml: "console:\n  ip_rate_limit: 0\n",
			want: func() ConsoleConfig {
				c := defaults
				c.IPRateLimit = 0
				return c
			}(),
		},
		{
			name:    "ops addr equals console addr",
			yaml:    "console:\n  addr: \":8081\"\n  ops_addr: \":8081\"\n",
			wantErr: "console.ops_addr must differ from addr",
		},
		{
			name:    "negative ip rate limit",
			yaml:    "console:\n  ip_rate_limit: -1\n",
			wantErr: "console.ip_rate_limit must not be negative",
		},
		{
			name:    "zero ip burst when limit enabled",
			yaml:    "console:\n  ip_rate_limit: 10\n  ip_rate_burst: 0\n",
			wantErr: "console.ip_rate_burst must be positive",
		},
		{
			name: "web dist dir and public urls trimmed",
			yaml: "console:\n  web_dist_dir: \"  web/dist  \"\n  public_base_url: \" http://127.0.0.1:8080/ \"\n  public_url: \" https://console.example.com/ \"\n",
			want: func() ConsoleConfig {
				c := defaults
				c.WebDistDir = "web/dist"
				c.PublicBaseURL = "http://127.0.0.1:8080"
				c.PublicURL = "https://console.example.com"
				c.PhoneOTP.SecureCookies = true
				return c
			}(),
		},
		{
			name:    "invalid public base url",
			yaml:    "console:\n  public_base_url: ftp://example.com\n",
			wantErr: "console.public_base_url",
		},
		{
			name:    "invalid public url",
			yaml:    "console:\n  public_url: https://console.example.com?foo=1\n",
			wantErr: "console.public_url",
		},
		{
			name: "oidc derives redirect url from public_url",
			yaml: `
console:
  public_url: http://127.0.0.1:8081/
  auth:
    oidc:
      issuer: http://127.0.0.1:5556/dex
      client_id: shortlink-console
`,
			env: map[string]string{
				"OIDC_CLIENT_SECRET": "shortlink-console-secret",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.PublicURL = "http://127.0.0.1:8081"
				c.OIDC = OIDCConfig{
					Issuer:       "http://127.0.0.1:5556/dex",
					ClientID:     "shortlink-console",
					ClientSecret: "shortlink-console-secret",
					RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/oidc/callback",
				}
				return c
			}(),
		},
		{
			name:    "oidc partially set without secret",
			yaml:    "console:\n  public_url: http://127.0.0.1:8081\n  auth:\n    oidc:\n      issuer: http://127.0.0.1:5556/dex\n      client_id: shortlink-console\n",
			wantErr: "OIDC_CLIENT_SECRET",
		},
		{
			name:    "oidc requires console.public_url",
			yaml:    "console:\n  auth:\n    oidc:\n      issuer: http://127.0.0.1:5556/dex\n      client_id: shortlink-console\n",
			env:     map[string]string{"OIDC_CLIENT_SECRET": "shortlink-console-secret"},
			wantErr: "console.public_url is required when OIDC auth is enabled",
		},
		{
			name:    "oidc invalid issuer scheme",
			yaml:    "console:\n  public_url: http://127.0.0.1:8081\n  auth:\n    oidc:\n      issuer: ftp://127.0.0.1:5556/dex\n      client_id: shortlink-console\n",
			env:     map[string]string{"OIDC_CLIENT_SECRET": "shortlink-console-secret"},
			wantErr: "console.auth.oidc.issuer",
		},
		{
			name:    "rejects oidc client_secret in yaml",
			yaml:    "console:\n  auth:\n    oidc:\n      client_secret: leaked\n",
			wantErr: "client_secret",
		},
		{
			name: "github derives redirect url from public_url",
			yaml: "console:\n  public_url: https://console.example.com\n  auth:\n    github:\n      client_id: gh-client\n",
			env:  map[string]string{"GITHUB_CLIENT_SECRET": "gh-secret"},
			want: func() ConsoleConfig {
				c := defaults
				c.PublicURL = "https://console.example.com"
				c.PhoneOTP.SecureCookies = true
				c.GitHub = GitHubConfig{
					ClientID:     "gh-client",
					ClientSecret: "gh-secret",
					RedirectURL:  "https://console.example.com/api/v1/auth/github/callback",
				}
				return c
			}(),
		},
		{
			name:    "github partially set",
			yaml:    "console:\n  public_url: https://console.example.com\n  auth:\n    github:\n      client_id: gh-client\n",
			wantErr: "GITHUB_CLIENT_SECRET",
		},
		{
			name:    "github requires console.public_url",
			yaml:    "console:\n  auth:\n    github:\n      client_id: gh-client\n",
			env:     map[string]string{"GITHUB_CLIENT_SECRET": "gh-secret"},
			wantErr: "console.public_url is required when GitHub auth is enabled",
		},
		{
			name: "minimal single-pg mode with feature flags disabled",
			yaml: "features:\n  analytics: false\n  risk_scan: false\nsmtp:\n  host: smtp.example.com\n  from: noreply@example.com\n",
			env:  map[string]string{"REDIS_URL": ""},
			want: func() ConsoleConfig {
				c := defaults
				c.RedisURL = ""
				c.Features = config.FeatureFlags{Analytics: false, RiskScan: false}
				c.EmailEnabled = true
				return c
			}(),
		},
		{
			name:    "smtp host without smtp from rejected",
			yaml:    "smtp:\n  host: smtp.example.com\n",
			wantErr: "smtp.from is required",
		},
		{
			name: "phone otp webhook configured with https public_url and outbound ports",
			yaml: `
outbound:
  allowed_ports: [443, 8443]
console:
  public_url: https://console.example.com
  auth:
    phone_otp:
      provider: webhook
      webhook_url: https://sms.example.com/otp
      timeout: 3s
      ttl: 10m
      cooldown: 30s
      max_per_hour: 8
      max_attempts: 4
`,
			env: map[string]string{
				"SMS_WEBHOOK_TOKEN": "sms-secret",
			},
			want: func() ConsoleConfig {
				c := defaults
				c.PublicURL = "https://console.example.com"
				c.OutboundAllowedPorts = []int{443, 8443}
				c.PhoneOTP = PhoneOTPConfig{
					Provider:      "webhook",
					WebhookURL:    "https://sms.example.com/otp",
					WebhookToken:  "sms-secret",
					AllowedPorts:  []int{443, 8443},
					Timeout:       3 * time.Second,
					TTL:           10 * time.Minute,
					Cooldown:      30 * time.Second,
					MaxPerHour:    8,
					MaxAttempts:   4,
					SecureCookies: true,
				}
				return c
			}(),
		},
		{
			name:    "rejects sms webhook_token in yaml",
			yaml:    "console:\n  auth:\n    phone_otp:\n      webhook_token: leaked\n",
			wantErr: "webhook_token",
		},
		{
			name:    "sms webhook url without provider rejected",
			yaml:    "console:\n  auth:\n    phone_otp:\n      webhook_url: https://sms.example.com/otp\n",
			wantErr: "console.auth.phone_otp.provider is required",
		},
		{
			name:    "sms webhook provider without url rejected",
			yaml:    "console:\n  auth:\n    phone_otp:\n      provider: webhook\n",
			wantErr: "console.auth.phone_otp.webhook_url is required",
		},
		{
			name:    "sms unsupported provider rejected",
			yaml:    "console:\n  auth:\n    phone_otp:\n      provider: aliyun\n",
			wantErr: "console.auth.phone_otp.provider must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)
			cfgPath := writeTempYAML(t, tt.yaml)
			if cfgPath != "" && !tt.flagCfg {
				t.Setenv(config.EnvConfigPath, cfgPath)
				cfgPath = ""
			}

			got, err := loadConsoleConfig(cfgPath)
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
	defaults := defaultWorkerConfig()

	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		want    WorkerConfig
		wantErr string
	}{
		{
			name: "defaults ignore redirector section",
			yaml: "redirector:\n  cache_miss_concurrency: 0\n",
			want: defaults,
		},
		{
			name: "explicit worker values from yaml and env",
			yaml: `
outbound:
  allowed_ports: [80, 443, 8443]
worker:
  addr: 127.0.0.1:9082
  concurrency: 16
  max_per_tenant: 2
  poll_interval: 100ms
  lease_duration: 45s
  job_timeout: 15s
  reap_interval: 10s
  cleanup_interval: 30m
  retention:
    short: 24h
    long: 168h
    stats_days: 45
    audit_days: 365
  risk:
    fetch_timeout: 3s
    host_rate_limit: 5
    allowed_hosts:
      - github.com.
      - docs.go.dev
    blocked_hosts:
      "Bad.Example.": malware
      "[2001:db8::1]": phishing
    blocked_keywords:
      smoke-phish: phishing
    gsb:
      base_url: https://safebrowsing.example.com/
      timeout: 4s
    llm:
      base_url: https://llm.example.com/v1/
      model: deepseek-chat
      mode: shadow
      min_confidence: 0.9
      timeout: 6s
      max_per_min: 60
`,
			env: map[string]string{
				"WORKER_ADDR":      "127.0.0.1:8082",
				"RISK_GSB_API_KEY": "gsb-key",
				"RISK_LLM_API_KEY": "llm-key",
			},
			want: WorkerConfig{
				BaseConfig:                    defaultBaseConfig("127.0.0.1:8082"),
				Features:                      config.FeatureFlags{Analytics: true, RiskScan: true},
				Concurrency:                   16,
				MaxPerTenant:                  2,
				PollInterval:                  100 * time.Millisecond,
				LeaseDuration:                 45 * time.Second,
				JobTimeout:                    15 * time.Second,
				ReapInterval:                  10 * time.Second,
				CleanupInterval:               30 * time.Minute,
				RetentionShort:                24 * time.Hour,
				RetentionLong:                 168 * time.Hour,
				StatsRetentionDays:            45,
				AuditRetentionDays:            365,
				RiskFetchTimeout:              3 * time.Second,
				RiskHostRateLimit:             5,
				RiskAllowedHosts:              []string{"docs.go.dev", "github.com"},
				RiskBlockedHosts:              map[string]link.BanReason{"bad.example": link.BanReasonMalware, "2001:db8::1": link.BanReasonPhishing},
				RiskBlockedKeywords:           map[string]link.BanReason{"smoke-phish": link.BanReasonPhishing},
				OutboundAllowedPorts:          []int{80, 443, 8443},
				RiskGSB:                       RiskGSBConfig{APIKey: "gsb-key", BaseURL: "https://safebrowsing.example.com", Timeout: 4 * time.Second},
				RiskLLM:                       RiskLLMConfig{BaseURL: "https://llm.example.com/v1", APIKey: "llm-key", Model: "deepseek-chat", Mode: "shadow", MinConfidence: 0.9, Timeout: 6 * time.Second, MaxPerMin: 60},
				WebhookTimeout:                webhook.DefaultTimeout,
				WebhookHostRateLimit:          webhook.DefaultHostRateLimit,
				WebhookRetryBaseDelay:         webhook.DefaultRetryBaseDelay,
				WebhookRetryMaxDelay:          webhook.DefaultRetryMaxDelay,
				WebhookMaxConsecutiveFailures: webhook.DefaultMaxConsecutiveFailures,
				SMTP: mailer.SMTPConfig{
					Port:    mailer.DefaultSMTPPort,
					TLSMode: mailer.TLSModeStartTLS,
					Timeout: mailer.DefaultSMTPTimeout,
				},
			},
		},
		{
			name: "webhook, smtp, and console public_url shared",
			yaml: `
outbound:
  allowed_ports: [443]
console:
  public_url: https://console.example.com/
worker:
  webhook:
    timeout: 4s
    host_rate_limit: 10
    retry_base_delay: 5s
    retry_max_delay: 2m
    max_consecutive_failures: 0
smtp:
  host: smtp.example.com
  port: 2525
  username: mailer
  from: "Shortlink <noreply@example.com>"
  tls_mode: NONE
  timeout: 8s
`,
			env: map[string]string{
				"WEBHOOK_SECRET_KEY": strings.Repeat("ab", 32),
				"SMTP_PASSWORD":      "mail-pass",
			},
			want: func() WorkerConfig {
				c := defaults
				c.WebhookSecretKey = strings.Repeat("ab", 32)
				c.OutboundAllowedPorts = []int{443}
				c.WebhookTimeout = 4 * time.Second
				c.WebhookHostRateLimit = 10
				c.WebhookRetryBaseDelay = 5 * time.Second
				c.WebhookRetryMaxDelay = 2 * time.Minute
				c.WebhookMaxConsecutiveFailures = 0
				c.ConsolePublicURL = "https://console.example.com"
				c.SMTP = mailer.SMTPConfig{
					Host:     "smtp.example.com",
					Port:     2525,
					Username: "mailer",
					Password: "mail-pass",
					From:     "Shortlink <noreply@example.com>",
					TLSMode:  mailer.TLSModeNone,
					Timeout:  8 * time.Second,
				}
				return c
			}(),
		},
		{
			name:    "rejects webhook secret_key in yaml",
			yaml:    "worker:\n  webhook:\n    secret_key: abcd\n",
			wantErr: "secret_key",
		},
		{
			name:    "rejects smtp password in yaml",
			yaml:    "smtp:\n  password: secret\n",
			wantErr: "password",
		},
		{
			name:    "rejects gsb api_key in yaml",
			yaml:    "worker:\n  risk:\n    gsb:\n      api_key: secret\n",
			wantErr: "api_key",
		},
		{
			name:    "rejects llm api_key in yaml",
			yaml:    "worker:\n  risk:\n    llm:\n      api_key: secret\n",
			wantErr: "api_key",
		},
		{
			name:    "invalid webhook secret key",
			env:     map[string]string{"WEBHOOK_SECRET_KEY": "not-hex"},
			wantErr: "WEBHOOK_SECRET_KEY",
		},
		{
			name:    "webhook timeout must be less than job timeout",
			yaml:    "worker:\n  job_timeout: 20s\n  webhook:\n    timeout: 20s\n",
			wantErr: "worker.webhook.timeout must be less than worker.job_timeout",
		},
		{
			name:    "smtp host requires smtp from",
			yaml:    "smtp:\n  host: smtp.example.com\n",
			wantErr: "from is required",
		},
		{
			name:    "smtp invalid tls mode",
			yaml:    "smtp:\n  tls_mode: plain\n",
			wantErr: "smtp.tls_mode",
		},
		{
			name:    "job timeout without commit margin exceeds lease",
			yaml:    "worker:\n  lease_duration: 30s\n  job_timeout: 28s\n",
			wantErr: "worker.job_timeout + 5s commit margin must not exceed worker.lease_duration",
		},
		{
			name:    "zero concurrency",
			yaml:    "worker:\n  concurrency: 0\n",
			wantErr: "worker.concurrency must be positive",
		},
		{
			name:    "negative max per tenant",
			yaml:    "worker:\n  max_per_tenant: -1\n",
			wantErr: "worker.max_per_tenant must not be negative",
		},
		{
			name:    "invalid risk blocked hosts",
			yaml:    "worker:\n  risk:\n    blocked_hosts:\n      evil.test: bad_reason\n",
			wantErr: "worker.risk.blocked_hosts",
		},
		{
			name:    "invalid risk blocked keywords",
			yaml:    "worker:\n  risk:\n    blocked_keywords:\n      foo: bad_reason\n",
			wantErr: "worker.risk.blocked_keywords",
		},
		{
			name:    "invalid outbound allowed ports",
			yaml:    "outbound:\n  allowed_ports: [80, 99999]\n",
			wantErr: "outbound.allowed_ports",
		},
		{
			name:    "risk fetch timeout exceeds job timeout",
			yaml:    "worker:\n  job_timeout: 5s\n  risk:\n    fetch_timeout: 5s\n",
			wantErr: "worker.risk.fetch_timeout must be less than worker.job_timeout",
		},
		{
			name:    "gsb timeout exceeds job timeout when enabled",
			yaml:    "worker:\n  risk:\n    gsb:\n      timeout: 20s\n",
			env:     map[string]string{"RISK_GSB_API_KEY": "k"},
			wantErr: "worker.risk.gsb.timeout must be less than worker.job_timeout",
		},
		{
			name:    "llm base url without model",
			yaml:    "worker:\n  risk:\n    llm:\n      base_url: https://api.deepseek.com\n",
			wantErr: "worker.risk.llm.base_url and worker.risk.llm.model must both be set",
		},
		{
			name:    "llm invalid mode",
			yaml:    "worker:\n  risk:\n    llm:\n      mode: audit\n",
			wantErr: "worker.risk.llm.mode",
		},
		{
			name:    "llm invalid confidence",
			yaml:    "worker:\n  risk:\n    llm:\n      min_confidence: 1.2\n",
			wantErr: "worker.risk.llm.min_confidence",
		},
		{
			name:    "llm timeout exceeds job timeout when enabled",
			yaml:    "worker:\n  risk:\n    llm:\n      base_url: http://127.0.0.1:11434/v1\n      model: qwen2.5\n      timeout: 20s\n",
			wantErr: "worker.risk.llm.timeout must be less than worker.job_timeout",
		},
		{
			name:    "llm negative max per min",
			yaml:    "worker:\n  risk:\n    llm:\n      max_per_min: -1\n",
			wantErr: "worker.risk.llm.max_per_min must not be negative",
		},
		{
			name:    "non-positive stats retention days",
			yaml:    "worker:\n  retention:\n    stats_days: 0\n",
			wantErr: "worker.retention.stats_days must be positive",
		},
		{
			name:    "non-positive audit retention days",
			yaml:    "worker:\n  retention:\n    audit_days: -5\n",
			wantErr: "worker.retention.audit_days must be positive",
		},
		{
			name: "minimal worker mode without redis and with features disabled",
			env: map[string]string{
				"REDIS_URL":         "",
				"FEATURE_ANALYTICS": "false",
				"FEATURE_RISK_SCAN": "false",
			},
			want: func() WorkerConfig {
				c := defaults
				c.RedisURL = ""
				c.Features = config.FeatureFlags{Analytics: false, RiskScan: false}
				return c
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)
			cfgPath := writeTempYAML(t, tt.yaml)
			if cfgPath != "" {
				t.Setenv(config.EnvConfigPath, cfgPath)
			}
			got, err := loadWorkerConfig("")
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
				!slices.Equal(got.RiskAllowedHosts, tt.want.RiskAllowedHosts) ||
				!maps.Equal(got.RiskBlockedHosts, tt.want.RiskBlockedHosts) ||
				!maps.Equal(got.RiskBlockedKeywords, tt.want.RiskBlockedKeywords) ||
				!slices.Equal(got.OutboundAllowedPorts, tt.want.OutboundAllowedPorts) {
				t.Errorf("loadWorkerConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConsoleAndWorkerExampleYAMLMatchesDefaults(t *testing.T) {
	applyEnv(t, baseConsoleTestEnv(), nil)
	examplePath := filepath.Join("..", "..", "config.example.yaml")

	gotConsole, err := loadConsoleConfig(examplePath)
	if err != nil {
		t.Fatalf("loadConsoleConfig(config.example.yaml) = %v", err)
	}
	if !consoleConfigEqual(gotConsole, defaultConsoleConfig()) {
		t.Errorf("console config.example.yaml drifted from defaults:\n got:  %+v\n want: %+v", gotConsole, defaultConsoleConfig())
	}

	gotWorker, err := loadWorkerConfig(examplePath)
	if err != nil {
		t.Fatalf("loadWorkerConfig(config.example.yaml) = %v", err)
	}
	wantWorker := defaultWorkerConfig()
	if !workerConfigEqual(gotWorker, wantWorker) ||
		!slices.Equal(gotWorker.RiskAllowedHosts, wantWorker.RiskAllowedHosts) ||
		!maps.Equal(gotWorker.RiskBlockedHosts, wantWorker.RiskBlockedHosts) ||
		!maps.Equal(gotWorker.RiskBlockedKeywords, wantWorker.RiskBlockedKeywords) ||
		!slices.Equal(gotWorker.OutboundAllowedPorts, wantWorker.OutboundAllowedPorts) {
		t.Errorf("worker config.example.yaml drifted from defaults:\n got:  %+v\n want: %+v", gotWorker, wantWorker)
	}
}

func TestParseConfigCommand(t *testing.T) {
	t.Run("valid dump flags", func(t *testing.T) {
		var stderr bytes.Buffer
		opts, ok, err := parseConfigCommand([]string{"dump", "--config", " /tmp/c.yaml ", "--format", "TEXT"}, &stderr)
		if err != nil || !ok {
			t.Fatalf("parseConfigCommand = (%+v, %v, %v), want ok", opts, ok, err)
		}
		if opts.configPath != "/tmp/c.yaml" || opts.format != logx.FormatText {
			t.Fatalf("opts = %+v", opts)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %s", stderr.String())
		}
	})

	t.Run("invalid format writes to stderr", func(t *testing.T) {
		var stderr bytes.Buffer
		_, ok, err := parseConfigCommand([]string{"dump", "--format", "xml"}, &stderr)
		if ok || !errors.Is(err, errUsage) {
			t.Fatalf("parseConfigCommand = (%v, %v), want false and errUsage", ok, err)
		}
		if !strings.Contains(stderr.String(), "--format must be json or text") {
			t.Fatalf("stderr = %q, want format error hint", stderr.String())
		}
	})

	t.Run("missing subcommand writes usage", func(t *testing.T) {
		var stderr bytes.Buffer
		_, ok, err := parseConfigCommand(nil, &stderr)
		if ok || !errors.Is(err, errUsage) {
			t.Fatalf("parseConfigCommand = (%v, %v), want false and errUsage", ok, err)
		}
		if stderr.Len() == 0 {
			t.Fatal("expected usage on stderr")
		}
	})
}

func TestRunConfigDump(t *testing.T) {
	t.Run("outputs valid redacted json", func(t *testing.T) {
		webhookKey := strings.Repeat("cd", 32)
		applyEnv(t, baseConsoleTestEnv(), map[string]string{
			"DATABASE_URL":         "postgres://shortlink:pg-secret@db:5432/shortlink?sslmode=disable",
			"REDIS_URL":            "rediss://:redis-secret@redis:6380/0",
			"OIDC_CLIENT_SECRET":   "oidc-top-secret",
			"WEBHOOK_SECRET_KEY":   webhookKey,
			"SMTP_PASSWORD":        "smtp-top-secret",
			"RISK_GSB_API_KEY":     "gsb-top-secret",
			"RISK_LLM_API_KEY":     "llm-top-secret",
			"GITHUB_CLIENT_SECRET": "gh-top-secret",
			"SMS_WEBHOOK_TOKEN":    "sms-top-secret",
		})
		cfgPath := writeTempYAML(t, `
console:
  public_url: https://console.example.com
  auth:
    oidc:
      issuer: https://dex.example.com
      client_id: shortlink-console
    github:
      client_id: gh-id
    phone_otp:
      provider: webhook
      webhook_url: https://sms.example.com/send
worker:
  risk:
    llm:
      base_url: https://llm.example.com/v1
      model: deepseek-chat
smtp:
  host: smtp.example.com
  username: mailer
  from: noreply@example.com
`)
		var out bytes.Buffer
		if err := runConfigDump(configDumpOptions{configPath: cfgPath, format: logx.FormatJSON}, &out); err != nil {
			t.Fatalf("runConfigDump error = %v", err)
		}
		text := out.String()
		for _, secret := range []string{
			"pg-secret", "redis-secret", "oidc-top-secret", webhookKey,
			"smtp-top-secret", "gsb-top-secret", "llm-top-secret", "gh-top-secret", "sms-top-secret",
		} {
			if strings.Contains(text, secret) {
				t.Fatalf("secret %q leaked in config dump: %s", secret, text)
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &decoded); err != nil {
			t.Fatalf("config dump not valid json: %v\n%s", err, text)
		}
		if decoded["console"] == nil || decoded["worker"] == nil {
			t.Fatalf("expected console and worker in dump: %+v", decoded)
		}
	})

	t.Run("fails on invalid worker or console config", func(t *testing.T) {
		applyEnv(t, baseConsoleTestEnv(), nil)
		cfgPath := writeTempYAML(t, "worker:\n  concurrency: 0\n")
		var out bytes.Buffer
		if err := runConfigDump(configDumpOptions{configPath: cfgPath, format: logx.FormatJSON}, &out); err == nil {
			t.Fatal("expected error for invalid worker config")
		}
		if out.Len() != 0 {
			t.Fatalf("unexpected stdout on error: %s", out.String())
		}
	})
}

func phoneOTPConfigEqual(a, b PhoneOTPConfig) bool {
	return a.Provider == b.Provider &&
		a.WebhookURL == b.WebhookURL &&
		a.WebhookToken == b.WebhookToken &&
		slices.Equal(a.AllowedPorts, b.AllowedPorts) &&
		a.Timeout == b.Timeout &&
		a.TTL == b.TTL &&
		a.Cooldown == b.Cooldown &&
		a.MaxPerHour == b.MaxPerHour &&
		a.MaxAttempts == b.MaxAttempts &&
		a.SecureCookies == b.SecureCookies
}

func consoleConfigEqual(a, b ConsoleConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.Features == b.Features &&
		a.EmailEnabled == b.EmailEnabled &&
		a.OIDC == b.OIDC &&
		a.GitHub == b.GitHub &&
		phoneOTPConfigEqual(a.PhoneOTP, b.PhoneOTP) &&
		a.PublicURL == b.PublicURL &&
		a.WebDistDir == b.WebDistDir &&
		a.PublicBaseURL == b.PublicBaseURL &&
		a.WebhookSecretKey == b.WebhookSecretKey &&
		slices.Equal(a.OutboundAllowedPorts, b.OutboundAllowedPorts) &&
		a.Proxy.TrustHeaders == b.Proxy.TrustHeaders &&
		slices.Equal(a.Proxy.TrustedCIDRs, b.Proxy.TrustedCIDRs) &&
		a.IPRateLimit == b.IPRateLimit &&
		a.IPRateBurst == b.IPRateBurst
}

func workerConfigEqual(a, b WorkerConfig) bool {
	return a.BaseConfig == b.BaseConfig &&
		a.Features == b.Features &&
		a.Concurrency == b.Concurrency &&
		a.MaxPerTenant == b.MaxPerTenant &&
		a.PollInterval == b.PollInterval &&
		a.LeaseDuration == b.LeaseDuration &&
		a.JobTimeout == b.JobTimeout &&
		a.ReapInterval == b.ReapInterval &&
		a.CleanupInterval == b.CleanupInterval &&
		a.RetentionShort == b.RetentionShort &&
		a.RetentionLong == b.RetentionLong &&
		a.StatsRetentionDays == b.StatsRetentionDays &&
		a.AuditRetentionDays == b.AuditRetentionDays &&
		a.RiskFetchTimeout == b.RiskFetchTimeout &&
		a.RiskHostRateLimit == b.RiskHostRateLimit &&
		a.RiskGSB == b.RiskGSB &&
		a.RiskLLM == b.RiskLLM &&
		a.WebhookSecretKey == b.WebhookSecretKey &&
		a.WebhookTimeout == b.WebhookTimeout &&
		a.WebhookHostRateLimit == b.WebhookHostRateLimit &&
		a.WebhookRetryBaseDelay == b.WebhookRetryBaseDelay &&
		a.WebhookRetryMaxDelay == b.WebhookRetryMaxDelay &&
		a.WebhookMaxConsecutiveFailures == b.WebhookMaxConsecutiveFailures &&
		a.ConsolePublicURL == b.ConsolePublicURL &&
		a.SMTP == b.SMTP
}

func TestConsoleConfigRedaction(t *testing.T) {
	base := defaultBaseConfig(":8081")
	base.DatabaseURL = "postgres://shortlink:pg-secret@db:5432/shortlink?sslmode=disable"
	base.RedisURL = "rediss://:redis-secret@redis:6380/0"

	oidcCfg := OIDCConfig{
		Issuer:       "https://dex.example.com",
		ClientID:     "shortlink-console",
		ClientSecret: "oidc-top-secret",
		RedirectURL:  "https://console.example.com/api/v1/auth/oidc/callback",
	}
	ghCfg := GitHubConfig{
		ClientID:     "gh-client",
		ClientSecret: "github-top-secret",
		RedirectURL:  "https://console.example.com/api/v1/auth/github/callback",
	}
	webhookMasterKey := strings.Repeat("ef", 32)
	phoneCfg := PhoneOTPConfig{
		Provider:     "webhook",
		WebhookURL:   "https://sms.example.com/otp",
		WebhookToken: "sms-top-secret",
	}
	conCfg := ConsoleConfig{BaseConfig: base, OIDC: oidcCfg, GitHub: ghCfg, PhoneOTP: phoneCfg, WebhookSecretKey: webhookMasterKey}
	workerCfg := WorkerConfig{
		BaseConfig:          base,
		Concurrency:         8,
		RiskBlockedKeywords: map[string]link.BanReason{"evil": link.BanReasonPhishing},
		RiskGSB:             RiskGSBConfig{APIKey: "gsb-top-secret", BaseURL: risk.DefaultGSBBaseURL, Timeout: time.Second},
		RiskLLM:             RiskLLMConfig{BaseURL: "https://api.deepseek.com", APIKey: "llm-top-secret", Model: "deepseek-chat", Mode: risk.LLMModeBan},
		WebhookSecretKey:    webhookMasterKey,
		SMTP: mailer.SMTPConfig{
			Host:     "smtp.example.com",
			Username: "mailer",
			Password: "smtp-top-secret",
		},
	}

	var buf bytes.Buffer
	logger, _ := base.NewLoggerWithService(&buf, "console-config-test")
	logger.Info("dump", "console", conCfg, "worker", workerCfg, "oidc", oidcCfg, "github", ghCfg, "phone", phoneCfg)

	combined := buf.String() + "\n" + fmt.Sprintf("%+v %+v %+v %+v %+v", conCfg, workerCfg, oidcCfg, ghCfg, phoneCfg)
	for _, secret := range []string{"pg-secret", "redis-secret", "oidc-top-secret", "github-top-secret", "sms-top-secret", "gsb-top-secret", "llm-top-secret", webhookMasterKey, "smtp-top-secret"} {
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
