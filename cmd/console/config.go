package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/logx"
	"shortlink/internal/risk"
)

const (
	defaultConsoleAddr        = ":8081"
	defaultConsoleIPRateLimit = 60
	defaultConsoleIPRateBurst = 120

	defaultWorkerCleanupInterval = time.Hour
	defaultWorkerRetention       = 7 * 24 * time.Hour
	defaultWorkerLongRetention   = 30 * 24 * time.Hour
	defaultStatsRetentionDays    = 90
	defaultAuditRetentionDays    = 180
)

// ConsoleConfig 是管理进程 HTTP 模式（cmd/console serve）的完整启动配置。
type ConsoleConfig struct {
	config.BaseConfig

	OIDC              OIDCConfig
	GitHub            GitHubConfig
	WebDistDir        string
	PublicBaseURL     string
	TrustProxyHeaders bool
	TrustedProxyCIDRs []netip.Prefix
	// IPRateLimit 与 IPRateBurst 控制管理 API 鉴权前按源 IP 限流（0 表示关闭）。
	IPRateLimit int
	IPRateBurst int
}

// OIDCConfig 存放标准 OIDC 身份提供方的接入参数。
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Enabled 报告是否配置了 OIDC 登录。validate 已保证四个字段同空或同非空。
func (c OIDCConfig) Enabled() bool {
	return c.Issuer != ""
}

// LogValue 掩码 ClientSecret。
func (c OIDCConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("issuer", c.Issuer),
		slog.String("client_id", c.ClientID),
		slog.String("client_secret", logx.RedactSecret(c.ClientSecret)),
		slog.String("redirect_url", c.RedirectURL),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出 OIDC ClientSecret。
func (c OIDCConfig) String() string {
	return c.LogValue().String()
}

// GitHubConfig 存放 GitHub OAuth 2.0 应用的接入参数。
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Enabled 报告是否配置了 GitHub 登录。validate 已保证三个字段同空或同非空。
func (c GitHubConfig) Enabled() bool {
	return c.ClientID != ""
}

// LogValue 掩码 ClientSecret。
func (c GitHubConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("client_id", c.ClientID),
		slog.String("client_secret", logx.RedactSecret(c.ClientSecret)),
		slog.String("redirect_url", c.RedirectURL),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出 GitHub ClientSecret。
func (c GitHubConfig) String() string {
	return c.LogValue().String()
}

// LogValue 掩码基础连接串及 OIDC/GitHub 客户端密钥。
func (cfg ConsoleConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.Any("oidc", cfg.OIDC.LogValue()),
		slog.Any("github", cfg.GitHub.LogValue()),
		slog.String("web_dist_dir", cfg.WebDistDir),
		slog.String("public_base_url", cfg.PublicBaseURL),
		slog.Bool("trust_proxy_headers", cfg.TrustProxyHeaders),
		slog.Int("trusted_proxy_cidrs_count", len(cfg.TrustedProxyCIDRs)),
		slog.Int("ip_rate_limit", cfg.IPRateLimit),
		slog.Int("ip_rate_burst", cfg.IPRateBurst),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出凭据。
func (cfg ConsoleConfig) String() string {
	return cfg.LogValue().String()
}

/*
WorkerConfig 是后台任务进程（cmd/console worker）的完整启动配置（阶段 12、ADR 0020）。
Addr 来自 WORKER_ADDR，默认空表示不开启运维 HTTP 端口；设置后暴露 /healthz、/readyz 和 /metrics。
*/
type WorkerConfig struct {
	config.BaseConfig

	Concurrency         int
	MaxPerTenant        int
	PollInterval        time.Duration
	LeaseDuration       time.Duration
	JobTimeout          time.Duration
	ReapInterval        time.Duration
	CleanupInterval     time.Duration
	SessionRetention    time.Duration
	DoneJobRetention    time.Duration
	DeadJobRetention    time.Duration
	InvitationRetention time.Duration
	StatsRetentionDays  int
	AuditRetentionDays  int
	RiskFetchTimeout    time.Duration
	RiskHostRateLimit   int
	RiskBlockedHosts    map[string]link.BanReason
	RiskBlockedKeywords map[string]link.BanReason
	RiskAllowedPorts    []int
}

// LogValue 掩码基础连接串，保留后台调度与风控非敏感参数。
func (cfg WorkerConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.Int("concurrency", cfg.Concurrency),
		slog.Int("max_per_tenant", cfg.MaxPerTenant),
		slog.Duration("poll_interval", cfg.PollInterval),
		slog.Duration("lease_duration", cfg.LeaseDuration),
		slog.Duration("job_timeout", cfg.JobTimeout),
		slog.Duration("reap_interval", cfg.ReapInterval),
		slog.Duration("cleanup_interval", cfg.CleanupInterval),
		slog.Duration("session_retention", cfg.SessionRetention),
		slog.Duration("done_job_retention", cfg.DoneJobRetention),
		slog.Duration("dead_job_retention", cfg.DeadJobRetention),
		slog.Duration("invitation_retention", cfg.InvitationRetention),
		slog.Int("stats_retention_days", cfg.StatsRetentionDays),
		slog.Int("audit_retention_days", cfg.AuditRetentionDays),
		slog.Duration("risk_fetch_timeout", cfg.RiskFetchTimeout),
		slog.Int("risk_host_rate_limit", cfg.RiskHostRateLimit),
		slog.Int("risk_blocked_hosts_count", len(cfg.RiskBlockedHosts)),
		slog.Int("risk_blocked_keywords_count", len(cfg.RiskBlockedKeywords)),
		slog.Int("risk_allowed_ports_count", len(cfg.RiskAllowedPorts)),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出基础连接串凭据。
func (cfg WorkerConfig) String() string {
	return cfg.LogValue().String()
}

/*
loadConsoleConfig 加载并校验管理进程 HTTP 模式配置。
监听地址只读 CONSOLE_ADDR（默认 :8081），独立运维端口只读 CONSOLE_OPS_ADDR，
避免在共享 .env 中与跳转进程 ADDR/OPS_ADDR 发生端口碰撞。
*/
func loadConsoleConfig() (ConsoleConfig, error) {
	base, err := config.LoadBaseConfig(config.BaseConfigDefaults{
		Addr:                 config.EnvString("CONSOLE_ADDR", defaultConsoleAddr),
		OpsAddr:              os.Getenv("CONSOLE_OPS_ADDR"),
		CacheMissConcurrency: cache.DefaultMissConcurrency,
		CacheFetchTimeout:    cache.DefaultFetchTimeout,
	})
	if err != nil {
		return ConsoleConfig{}, err
	}

	cfg := ConsoleConfig{
		BaseConfig: base,
		OIDC: OIDCConfig{
			Issuer:       os.Getenv("OIDC_ISSUER"),
			ClientID:     os.Getenv("OIDC_CLIENT_ID"),
			ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
			RedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		},
		GitHub: GitHubConfig{
			ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
			ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			RedirectURL:  os.Getenv("GITHUB_REDIRECT_URL"),
		},
		WebDistDir:    strings.TrimSpace(os.Getenv("WEB_DIST_DIR")),
		PublicBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")), "/"),
		IPRateLimit:   defaultConsoleIPRateLimit,
		IPRateBurst:   defaultConsoleIPRateBurst,
	}
	if cfg.OpsAddr != "" && cfg.OpsAddr == cfg.Addr {
		return ConsoleConfig{}, errors.New("CONSOLE_OPS_ADDR must differ from CONSOLE_ADDR")
	}
	if err := validateOIDC(cfg.OIDC); err != nil {
		return ConsoleConfig{}, err
	}
	if err := validateGitHub(cfg.GitHub); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.PublicBaseURL != "" {
		if err := config.ValidateIssuerURL("PUBLIC_BASE_URL", cfg.PublicBaseURL); err != nil {
			return ConsoleConfig{}, err
		}
	}
	if cfg.TrustProxyHeaders, err = config.EnvBool("TRUST_PROXY_HEADERS", false); err != nil {
		return ConsoleConfig{}, err
	}
	if raw := os.Getenv("TRUSTED_PROXY_CIDRS"); raw != "" {
		cidrs, err := config.ParseCIDRs(raw)
		if err != nil {
			return ConsoleConfig{}, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
		}
		if !cfg.TrustProxyHeaders {
			return ConsoleConfig{}, errors.New("TRUSTED_PROXY_CIDRS requires TRUST_PROXY_HEADERS=true")
		}
		cfg.TrustedProxyCIDRs = cidrs
	}
	if cfg.IPRateLimit, err = config.EnvInt("CONSOLE_IP_RATE_LIMIT", cfg.IPRateLimit); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.IPRateLimit < 0 {
		return ConsoleConfig{}, errors.New("CONSOLE_IP_RATE_LIMIT must not be negative")
	}
	if os.Getenv("CONSOLE_IP_RATE_BURST") != "" {
		if cfg.IPRateBurst, err = config.EnvInt("CONSOLE_IP_RATE_BURST", cfg.IPRateBurst); err != nil {
			return ConsoleConfig{}, err
		}
	} else if cfg.IPRateLimit > 0 && os.Getenv("CONSOLE_IP_RATE_LIMIT") != "" {
		cfg.IPRateBurst = max(cfg.IPRateLimit*2, 1)
	}
	if cfg.IPRateLimit > 0 && cfg.IPRateBurst <= 0 {
		return ConsoleConfig{}, errors.New("CONSOLE_IP_RATE_BURST must be positive when CONSOLE_IP_RATE_LIMIT is enabled")
	}
	return cfg, nil
}

/*
loadWorkerConfig 加载并校验后台任务进程（cmd/console worker）的配置。
监听地址只读 WORKER_ADDR（默认空不开），不要求 VISITOR_SALT，不读取 OIDC_* / GITHUB_*。
*/
func loadWorkerConfig() (WorkerConfig, error) {
	base, err := config.LoadBaseConfig(config.BaseConfigDefaults{
		Addr:                 os.Getenv("WORKER_ADDR"),
		CacheMissConcurrency: cache.DefaultMissConcurrency,
		CacheFetchTimeout:    cache.DefaultFetchTimeout,
	})
	if err != nil {
		return WorkerConfig{}, err
	}

	cfg := WorkerConfig{
		BaseConfig:          base,
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

	if cfg.Concurrency, err = config.EnvInt("WORKER_CONCURRENCY", cfg.Concurrency); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.MaxPerTenant, err = config.EnvInt("WORKER_MAX_PER_TENANT", cfg.MaxPerTenant); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.PollInterval, err = config.EnvDuration("WORKER_POLL_INTERVAL", cfg.PollInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.LeaseDuration, err = config.EnvDuration("WORKER_LEASE_DURATION", cfg.LeaseDuration); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.JobTimeout, err = config.EnvDuration("WORKER_JOB_TIMEOUT", cfg.JobTimeout); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.ReapInterval, err = config.EnvDuration("WORKER_REAP_INTERVAL", cfg.ReapInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.CleanupInterval, err = config.EnvDuration("WORKER_CLEANUP_INTERVAL", cfg.CleanupInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.SessionRetention, err = config.EnvDuration("WORKER_SESSION_RETENTION", cfg.SessionRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.DoneJobRetention, err = config.EnvDuration("WORKER_DONE_JOB_RETENTION", cfg.DoneJobRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.DeadJobRetention, err = config.EnvDuration("WORKER_DEAD_JOB_RETENTION", cfg.DeadJobRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.InvitationRetention, err = config.EnvDuration("WORKER_INVITATION_RETENTION", cfg.InvitationRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.StatsRetentionDays, err = config.EnvInt("WORKER_STATS_RETENTION_DAYS", cfg.StatsRetentionDays); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.AuditRetentionDays, err = config.EnvInt("WORKER_AUDIT_RETENTION_DAYS", cfg.AuditRetentionDays); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.RiskFetchTimeout, err = config.EnvDuration("RISK_FETCH_TIMEOUT", cfg.RiskFetchTimeout); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.RiskHostRateLimit, err = config.EnvInt("RISK_HOST_RATE_LIMIT", cfg.RiskHostRateLimit); err != nil {
		return WorkerConfig{}, err
	}
	if raw := os.Getenv("RISK_BLOCKED_HOSTS"); raw != "" {
		rules, err := risk.ParseKeywordRules(raw)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("RISK_BLOCKED_HOSTS: %w", err)
		}
		cfg.RiskBlockedHosts = rules
	}
	if raw := os.Getenv("RISK_BLOCKED_KEYWORDS"); raw != "" {
		rules, err := risk.ParseKeywordRules(raw)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("RISK_BLOCKED_KEYWORDS: %w", err)
		}
		cfg.RiskBlockedKeywords = rules
	}
	if raw := os.Getenv("RISK_ALLOWED_PORTS"); raw != "" {
		ports, err := config.ParsePorts(raw)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("RISK_ALLOWED_PORTS: %w", err)
		}
		cfg.RiskAllowedPorts = ports
	}

	if err := validateWorker(cfg); err != nil {
		return WorkerConfig{}, err
	}
	return cfg, nil
}

func validateOIDC(c OIDCConfig) error {
	set := config.CountNonEmpty(c.Issuer, c.ClientID, c.ClientSecret, c.RedirectURL)
	if set == 0 {
		return nil
	}
	if set != 4 {
		return errors.New("OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, and OIDC_REDIRECT_URL must all be set or all be empty")
	}
	if err := config.ValidateIssuerURL("OIDC_ISSUER", c.Issuer); err != nil {
		return err
	}
	return config.ValidateRedirectURL("OIDC_REDIRECT_URL", c.RedirectURL)
}

func validateGitHub(c GitHubConfig) error {
	set := config.CountNonEmpty(c.ClientID, c.ClientSecret, c.RedirectURL)
	if set == 0 {
		return nil
	}
	if set != 3 {
		return errors.New("GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET, and GITHUB_REDIRECT_URL must all be set or all be empty")
	}
	return config.ValidateRedirectURL("GITHUB_REDIRECT_URL", c.RedirectURL)
}

func validateWorker(cfg WorkerConfig) error {
	if cfg.Concurrency <= 0 {
		return errors.New("WORKER_CONCURRENCY must be positive")
	}
	if cfg.MaxPerTenant < 0 {
		return errors.New("WORKER_MAX_PER_TENANT must not be negative")
	}
	if cfg.PollInterval <= 0 {
		return errors.New("WORKER_POLL_INTERVAL must be positive")
	}
	if cfg.LeaseDuration <= 0 {
		return errors.New("WORKER_LEASE_DURATION must be positive")
	}
	if cfg.JobTimeout <= 0 {
		return errors.New("WORKER_JOB_TIMEOUT must be positive")
	}
	if cfg.JobTimeout+job.CommitTimeout > cfg.LeaseDuration {
		return fmt.Errorf("WORKER_JOB_TIMEOUT + %s commit margin must not exceed WORKER_LEASE_DURATION", job.CommitTimeout)
	}
	if cfg.ReapInterval <= 0 {
		return errors.New("WORKER_REAP_INTERVAL must be positive")
	}
	if cfg.CleanupInterval <= 0 {
		return errors.New("WORKER_CLEANUP_INTERVAL must be positive")
	}
	if cfg.SessionRetention < 0 {
		return errors.New("WORKER_SESSION_RETENTION must not be negative")
	}
	if cfg.DoneJobRetention < 0 {
		return errors.New("WORKER_DONE_JOB_RETENTION must not be negative")
	}
	if cfg.DeadJobRetention < 0 {
		return errors.New("WORKER_DEAD_JOB_RETENTION must not be negative")
	}
	if cfg.InvitationRetention < 0 {
		return errors.New("WORKER_INVITATION_RETENTION must not be negative")
	}
	if cfg.StatsRetentionDays <= 0 {
		return errors.New("WORKER_STATS_RETENTION_DAYS must be positive")
	}
	if cfg.AuditRetentionDays <= 0 {
		return errors.New("WORKER_AUDIT_RETENTION_DAYS must be positive")
	}
	if cfg.RiskFetchTimeout <= 0 {
		return errors.New("RISK_FETCH_TIMEOUT must be positive")
	}
	if cfg.RiskHostRateLimit < 0 {
		return errors.New("RISK_HOST_RATE_LIMIT must not be negative")
	}
	return nil
}
