package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/logx"
	"shortlink/internal/mailer"
	"shortlink/internal/risk"
	"shortlink/internal/webhook"
)

const (
	defaultWorkerCleanupInterval = time.Hour
	defaultWorkerShortRetention  = 7 * 24 * time.Hour
	defaultWorkerLongRetention   = 30 * 24 * time.Hour
	defaultStatsRetentionDays    = 90
	defaultAuditRetentionDays    = 180
)

// RiskGSBConfig 配置可选的 Google Safe Browsing v4 威胁情报源。APIKey 为空时关闭。
type RiskGSBConfig struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

// Enabled 报告是否启用 Google Safe Browsing 查验。
func (c RiskGSBConfig) Enabled() bool {
	return c.APIKey != ""
}

// LogValue 掩码 APIKey。
func (c RiskGSBConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("api_key", logx.RedactSecret(c.APIKey)),
		slog.String("base_url", c.BaseURL),
		slog.Duration("timeout", c.Timeout),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出 GSB API Key。
func (c RiskGSBConfig) String() string {
	return c.LogValue().String()
}

// RiskLLMConfig 配置可选的 OpenAI 兼容 LLM 语义风控器（L3）。BaseURL 与 Model 均非空时启用。
type RiskLLMConfig struct {
	BaseURL       string
	APIKey        string
	Model         string
	Mode          string
	MinConfidence float64
	Timeout       time.Duration
	MaxPerMin     int
}

// Enabled 报告是否启用 LLM 语义风控。
func (c RiskLLMConfig) Enabled() bool {
	return c.BaseURL != "" && c.Model != ""
}

// LogValue 掩码 APIKey。
func (c RiskLLMConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.BaseURL),
		slog.String("api_key", logx.RedactSecret(c.APIKey)),
		slog.String("model", c.Model),
		slog.String("mode", c.Mode),
		slog.Float64("min_confidence", c.MinConfidence),
		slog.Duration("timeout", c.Timeout),
		slog.Int("max_per_min", c.MaxPerMin),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出 LLM API Key。
func (c RiskLLMConfig) String() string {
	return c.LogValue().String()
}

/*
WorkerConfig 是后台任务进程（cmd/console worker）的完整启动配置（阶段 12、ADR 0020、ADR 0035）。
Addr 来自 WORKER_ADDR / worker.addr，默认空表示不开启运维 HTTP 端口；设置后暴露 /healthz、/readyz 和 /metrics。
*/
type WorkerConfig struct {
	config.BaseConfig

	Features                      config.FeatureFlags
	Concurrency                   int
	MaxPerTenant                  int
	PollInterval                  time.Duration
	LeaseDuration                 time.Duration
	JobTimeout                    time.Duration
	ReapInterval                  time.Duration
	CleanupInterval               time.Duration
	RetentionShort                time.Duration // sessions、done_jobs、webhook_deliveries 清理保留期
	RetentionLong                 time.Duration // dead_jobs、invitations 清理保留期
	StatsRetentionDays            int
	AuditRetentionDays            int
	RiskFetchTimeout              time.Duration
	RiskHostRateLimit             int
	RiskAllowedHosts              []string
	RiskBlockedHosts              map[string]link.BanReason
	RiskBlockedKeywords           map[string]link.BanReason
	OutboundAllowedPorts          []int
	RiskGSB                       RiskGSBConfig
	RiskLLM                       RiskLLMConfig
	WebhookSecretKey              string
	WebhookTimeout                time.Duration
	WebhookHostRateLimit          int
	WebhookRetryBaseDelay         time.Duration
	WebhookRetryMaxDelay          time.Duration
	WebhookMaxConsecutiveFailures int
	ConsolePublicURL              string
	SMTP                          mailer.SMTPConfig
}

// StoreFeatures 推导后台任务进程写路径（如风控 BanIfURL）需要挂载的事务钩子特性集。
func (cfg WorkerConfig) StoreFeatures() StoreFeatures {
	return StoreFeatures{
		Analytics: cfg.Features.Analytics,
		RiskScan:  cfg.Features.RiskScan,
		Webhooks:  cfg.WebhookSecretKey != "",
		Email:     cfg.SMTP.Enabled(),
	}
}

// LogValue 掩码基础连接串、Webhook 主密钥、SMTP 密码与风控外部 API 密钥，保留非敏感调度与风控参数。
func (cfg WorkerConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.Any("features", cfg.Features.LogValue()),
		slog.Int("concurrency", cfg.Concurrency),
		slog.Int("max_per_tenant", cfg.MaxPerTenant),
		slog.Duration("poll_interval", cfg.PollInterval),
		slog.Duration("lease_duration", cfg.LeaseDuration),
		slog.Duration("job_timeout", cfg.JobTimeout),
		slog.Duration("reap_interval", cfg.ReapInterval),
		slog.Duration("cleanup_interval", cfg.CleanupInterval),
		slog.Duration("retention_short", cfg.RetentionShort),
		slog.Duration("retention_long", cfg.RetentionLong),
		slog.Int("stats_retention_days", cfg.StatsRetentionDays),
		slog.Int("audit_retention_days", cfg.AuditRetentionDays),
		slog.Duration("risk_fetch_timeout", cfg.RiskFetchTimeout),
		slog.Int("risk_host_rate_limit", cfg.RiskHostRateLimit),
		slog.Int("risk_allowed_hosts_count", len(cfg.RiskAllowedHosts)),
		slog.Int("risk_blocked_hosts_count", len(cfg.RiskBlockedHosts)),
		slog.Int("risk_blocked_keywords_count", len(cfg.RiskBlockedKeywords)),
		slog.Int("outbound_allowed_ports_count", len(cfg.OutboundAllowedPorts)),
		slog.Any("risk_gsb", cfg.RiskGSB.LogValue()),
		slog.Any("risk_llm", cfg.RiskLLM.LogValue()),
		slog.String("webhook_secret_key", logx.RedactSecret(cfg.WebhookSecretKey)),
		slog.Duration("webhook_timeout", cfg.WebhookTimeout),
		slog.Int("webhook_host_rate_limit", cfg.WebhookHostRateLimit),
		slog.Duration("webhook_retry_base_delay", cfg.WebhookRetryBaseDelay),
		slog.Duration("webhook_retry_max_delay", cfg.WebhookRetryMaxDelay),
		slog.Int("webhook_max_consecutive_failures", cfg.WebhookMaxConsecutiveFailures),
		slog.String("console_public_url", cfg.ConsolePublicURL),
		slog.Any("smtp", cfg.SMTP.LogValue()),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出基础连接串凭据。
func (cfg WorkerConfig) String() string {
	return cfg.LogValue().String()
}

type workerSection struct {
	Addr            string              `yaml:"addr"`
	Concurrency     *int                `yaml:"concurrency"`
	MaxPerTenant    *int                `yaml:"max_per_tenant"`
	PollInterval    *config.Duration    `yaml:"poll_interval"`
	LeaseDuration   *config.Duration    `yaml:"lease_duration"`
	JobTimeout      *config.Duration    `yaml:"job_timeout"`
	ReapInterval    *config.Duration    `yaml:"reap_interval"`
	CleanupInterval *config.Duration    `yaml:"cleanup_interval"`
	Retention       workerRetentionYAML `yaml:"retention"`
	Risk            workerRiskYAML      `yaml:"risk"`
	Webhook         workerWebhookYAML   `yaml:"webhook"`
}

type workerRetentionYAML struct {
	Short     *config.Duration `yaml:"short"`
	Long      *config.Duration `yaml:"long"`
	StatsDays *int             `yaml:"stats_days"`
	AuditDays *int             `yaml:"audit_days"`
}

type workerRiskYAML struct {
	FetchTimeout    *config.Duration  `yaml:"fetch_timeout"`
	HostRateLimit   *int              `yaml:"host_rate_limit"`
	AllowedHosts    []string          `yaml:"allowed_hosts"`
	BlockedHosts    map[string]string `yaml:"blocked_hosts"`
	BlockedKeywords map[string]string `yaml:"blocked_keywords"`
	GSB             workerRiskGSBYAML `yaml:"gsb"`
	LLM             workerRiskLLMYAML `yaml:"llm"`
}

type workerRiskGSBYAML struct {
	BaseURL string           `yaml:"base_url"`
	Timeout *config.Duration `yaml:"timeout"`
	// api_key 仅从 RISK_GSB_API_KEY 环境变量读取，不定义 yaml tag。
}

type workerRiskLLMYAML struct {
	BaseURL       string           `yaml:"base_url"`
	Model         string           `yaml:"model"`
	Mode          string           `yaml:"mode"`
	MinConfidence *float64         `yaml:"min_confidence"`
	Timeout       *config.Duration `yaml:"timeout"`
	MaxPerMin     *int             `yaml:"max_per_min"`
	// api_key 仅从 RISK_LLM_API_KEY 环境变量读取，不定义 yaml tag。
}

type workerWebhookYAML struct {
	Timeout                *config.Duration `yaml:"timeout"`
	HostRateLimit          *int             `yaml:"host_rate_limit"`
	RetryBaseDelay         *config.Duration `yaml:"retry_base_delay"`
	RetryMaxDelay          *config.Duration `yaml:"retry_max_delay"`
	MaxConsecutiveFailures *int             `yaml:"max_consecutive_failures"`
	// secret_key 仅从 WEBHOOK_SECRET_KEY 环境变量读取，不定义 yaml tag。
}

/*
loadWorkerConfig 加载并校验后台任务进程（cmd/console worker）的配置（ADR 0035）。
监听地址只读 WORKER_ADDR / worker.addr（默认空不开），ConsolePublicURL 复用 console.public_url。
*/
func loadWorkerConfig(configPath string) (WorkerConfig, error) {
	file, err := config.LoadFile(config.ResolvePath(configPath))
	if err != nil {
		return WorkerConfig{}, err
	}

	var sec workerSection
	if err := config.DecodeSection(file.Worker, &sec); err != nil {
		return WorkerConfig{}, fmt.Errorf("worker: %w", err)
	}

	addr := strings.TrimSpace(sec.Addr)
	addr = config.EnvString("WORKER_ADDR", addr)

	base, err := config.LoadBaseConfig(file, config.BaseConfigDefaults{
		Addr:              addr,
		CacheFetchTimeout: cache.DefaultFetchTimeout,
	})
	if err != nil {
		return WorkerConfig{}, err
	}
	features, err := config.LoadFeatureFlags(file)
	if err != nil {
		return WorkerConfig{}, err
	}
	outboundPorts, err := config.LoadOutboundAllowedPorts(file)
	if err != nil {
		return WorkerConfig{}, err
	}
	smtpCfg, err := loadSMTPFromFile(file)
	if err != nil {
		return WorkerConfig{}, err
	}

	consolePublicURL, _, err := config.LookupMappingString(file.Console, "public_url")
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("console.public_url: %w", err)
	}
	consolePublicURL = strings.TrimRight(strings.TrimSpace(consolePublicURL), "/")

	blockedHosts, err := risk.NormalizeHostRules(sec.Risk.BlockedHosts)
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("worker.risk.blocked_hosts: %w", err)
	}
	blockedKeywords, err := risk.NormalizeKeywordRules(sec.Risk.BlockedKeywords)
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("worker.risk.blocked_keywords: %w", err)
	}

	cfg := WorkerConfig{
		BaseConfig:           base,
		Features:             features,
		Concurrency:          job.DefaultConcurrency,
		MaxPerTenant:         job.DefaultMaxPerTenant,
		PollInterval:         job.DefaultPollInterval,
		LeaseDuration:        job.DefaultLeaseDuration,
		JobTimeout:           job.DefaultJobTimeout,
		ReapInterval:         job.DefaultReapInterval,
		CleanupInterval:      defaultWorkerCleanupInterval,
		RetentionShort:       defaultWorkerShortRetention,
		RetentionLong:        defaultWorkerLongRetention,
		StatsRetentionDays:   defaultStatsRetentionDays,
		AuditRetentionDays:   defaultAuditRetentionDays,
		RiskFetchTimeout:     risk.DefaultFetchTimeout,
		RiskHostRateLimit:    risk.DefaultHostRateLimit,
		RiskAllowedHosts:     risk.NormalizeHostList(sec.Risk.AllowedHosts),
		RiskBlockedHosts:     blockedHosts,
		RiskBlockedKeywords:  blockedKeywords,
		OutboundAllowedPorts: outboundPorts,
		RiskGSB: RiskGSBConfig{
			APIKey:  strings.TrimSpace(os.Getenv("RISK_GSB_API_KEY")),
			BaseURL: risk.DefaultGSBBaseURL,
			Timeout: risk.DefaultGSBTimeout,
		},
		RiskLLM: RiskLLMConfig{
			BaseURL:       strings.TrimRight(strings.TrimSpace(sec.Risk.LLM.BaseURL), "/"),
			APIKey:        strings.TrimSpace(os.Getenv("RISK_LLM_API_KEY")),
			Model:         strings.TrimSpace(sec.Risk.LLM.Model),
			Mode:          risk.LLMModeBan,
			MinConfidence: risk.DefaultLLMMinConfidence,
			Timeout:       risk.DefaultLLMTimeout,
			MaxPerMin:     risk.DefaultLLMMaxPerMin,
		},
		WebhookSecretKey:              strings.TrimSpace(os.Getenv("WEBHOOK_SECRET_KEY")),
		WebhookTimeout:                webhook.DefaultTimeout,
		WebhookHostRateLimit:          webhook.DefaultHostRateLimit,
		WebhookRetryBaseDelay:         webhook.DefaultRetryBaseDelay,
		WebhookRetryMaxDelay:          webhook.DefaultRetryMaxDelay,
		WebhookMaxConsecutiveFailures: webhook.DefaultMaxConsecutiveFailures,
		ConsolePublicURL:              consolePublicURL,
		SMTP:                          smtpCfg,
	}

	applyWorkerYAML(&cfg, sec)

	if err := validateWorker(cfg); err != nil {
		return WorkerConfig{}, err
	}
	return cfg, nil
}

func applyWorkerYAML(cfg *WorkerConfig, sec workerSection) {
	if sec.Concurrency != nil {
		cfg.Concurrency = *sec.Concurrency
	}
	if sec.MaxPerTenant != nil {
		cfg.MaxPerTenant = *sec.MaxPerTenant
	}
	if sec.PollInterval != nil {
		cfg.PollInterval = sec.PollInterval.Std()
	}
	if sec.LeaseDuration != nil {
		cfg.LeaseDuration = sec.LeaseDuration.Std()
	}
	if sec.JobTimeout != nil {
		cfg.JobTimeout = sec.JobTimeout.Std()
	}
	if sec.ReapInterval != nil {
		cfg.ReapInterval = sec.ReapInterval.Std()
	}
	if sec.CleanupInterval != nil {
		cfg.CleanupInterval = sec.CleanupInterval.Std()
	}
	if sec.Retention.Short != nil {
		cfg.RetentionShort = sec.Retention.Short.Std()
	}
	if sec.Retention.Long != nil {
		cfg.RetentionLong = sec.Retention.Long.Std()
	}
	if sec.Retention.StatsDays != nil {
		cfg.StatsRetentionDays = *sec.Retention.StatsDays
	}
	if sec.Retention.AuditDays != nil {
		cfg.AuditRetentionDays = *sec.Retention.AuditDays
	}
	if sec.Risk.FetchTimeout != nil {
		cfg.RiskFetchTimeout = sec.Risk.FetchTimeout.Std()
	}
	if sec.Risk.HostRateLimit != nil {
		cfg.RiskHostRateLimit = *sec.Risk.HostRateLimit
	}
	if u := strings.TrimRight(strings.TrimSpace(sec.Risk.GSB.BaseURL), "/"); u != "" {
		cfg.RiskGSB.BaseURL = u
	}
	if sec.Risk.GSB.Timeout != nil {
		cfg.RiskGSB.Timeout = sec.Risk.GSB.Timeout.Std()
	}
	if mode := strings.ToLower(strings.TrimSpace(sec.Risk.LLM.Mode)); mode != "" {
		cfg.RiskLLM.Mode = mode
	}
	if sec.Risk.LLM.MinConfidence != nil {
		cfg.RiskLLM.MinConfidence = *sec.Risk.LLM.MinConfidence
	}
	if sec.Risk.LLM.Timeout != nil {
		cfg.RiskLLM.Timeout = sec.Risk.LLM.Timeout.Std()
	}
	if sec.Risk.LLM.MaxPerMin != nil {
		cfg.RiskLLM.MaxPerMin = *sec.Risk.LLM.MaxPerMin
	}
	if sec.Webhook.Timeout != nil {
		cfg.WebhookTimeout = sec.Webhook.Timeout.Std()
	}
	if sec.Webhook.HostRateLimit != nil {
		cfg.WebhookHostRateLimit = *sec.Webhook.HostRateLimit
	}
	if sec.Webhook.RetryBaseDelay != nil {
		cfg.WebhookRetryBaseDelay = sec.Webhook.RetryBaseDelay.Std()
	}
	if sec.Webhook.RetryMaxDelay != nil {
		cfg.WebhookRetryMaxDelay = sec.Webhook.RetryMaxDelay.Std()
	}
	if sec.Webhook.MaxConsecutiveFailures != nil {
		cfg.WebhookMaxConsecutiveFailures = *sec.Webhook.MaxConsecutiveFailures
	}
}

func validateWorker(cfg WorkerConfig) error {
	if cfg.Concurrency <= 0 {
		return errors.New("worker.concurrency must be positive")
	}
	if cfg.MaxPerTenant < 0 {
		return errors.New("worker.max_per_tenant must not be negative")
	}
	if cfg.PollInterval <= 0 {
		return errors.New("worker.poll_interval must be positive")
	}
	if cfg.LeaseDuration <= 0 {
		return errors.New("worker.lease_duration must be positive")
	}
	if cfg.JobTimeout <= 0 {
		return errors.New("worker.job_timeout must be positive")
	}
	if cfg.JobTimeout+job.CommitTimeout > cfg.LeaseDuration {
		return fmt.Errorf("worker.job_timeout + %s commit margin must not exceed worker.lease_duration", job.CommitTimeout)
	}
	if cfg.ReapInterval <= 0 {
		return errors.New("worker.reap_interval must be positive")
	}
	if cfg.CleanupInterval <= 0 {
		return errors.New("worker.cleanup_interval must be positive")
	}
	if cfg.RetentionShort < 0 {
		return errors.New("worker.retention.short must not be negative")
	}
	if cfg.RetentionLong < 0 {
		return errors.New("worker.retention.long must not be negative")
	}
	if cfg.StatsRetentionDays <= 0 {
		return errors.New("worker.retention.stats_days must be positive")
	}
	if cfg.AuditRetentionDays <= 0 {
		return errors.New("worker.retention.audit_days must be positive")
	}
	if cfg.RiskFetchTimeout <= 0 {
		return errors.New("worker.risk.fetch_timeout must be positive")
	}
	if cfg.RiskFetchTimeout >= cfg.JobTimeout {
		return errors.New("worker.risk.fetch_timeout must be less than worker.job_timeout")
	}
	if cfg.RiskHostRateLimit < 0 {
		return errors.New("worker.risk.host_rate_limit must not be negative")
	}
	if err := validateRiskGSB(cfg.RiskGSB, cfg.JobTimeout); err != nil {
		return err
	}
	if err := validateRiskLLM(cfg.RiskLLM, cfg.JobTimeout); err != nil {
		return err
	}
	if cfg.WebhookSecretKey != "" {
		if _, err := webhook.ParseMasterKeyHex(cfg.WebhookSecretKey); err != nil {
			return err
		}
	}
	if cfg.WebhookTimeout <= 0 {
		return errors.New("worker.webhook.timeout must be positive")
	}
	if cfg.WebhookTimeout >= cfg.JobTimeout {
		return errors.New("worker.webhook.timeout must be less than worker.job_timeout")
	}
	if cfg.WebhookHostRateLimit < 0 {
		return errors.New("worker.webhook.host_rate_limit must not be negative")
	}
	if cfg.WebhookRetryBaseDelay <= 0 {
		return errors.New("worker.webhook.retry_base_delay must be positive")
	}
	if cfg.WebhookRetryMaxDelay < cfg.WebhookRetryBaseDelay {
		return errors.New("worker.webhook.retry_max_delay must not be less than worker.webhook.retry_base_delay")
	}
	if cfg.WebhookMaxConsecutiveFailures < 0 {
		return errors.New("worker.webhook.max_consecutive_failures must not be negative")
	}
	if cfg.ConsolePublicURL != "" {
		if err := config.ValidateIssuerURL("console.public_url", cfg.ConsolePublicURL); err != nil {
			return err
		}
	}
	if cfg.SMTP.Port < 1 || cfg.SMTP.Port > 65535 {
		return errors.New("smtp.port must be in 1..65535")
	}
	if !mailer.ValidTLSMode(cfg.SMTP.TLSMode) {
		return fmt.Errorf("smtp.tls_mode must be starttls, smtps, or none, got %q", cfg.SMTP.TLSMode)
	}
	if cfg.SMTP.Timeout <= 0 {
		return errors.New("smtp.timeout must be positive")
	}
	if cfg.SMTP.Timeout >= cfg.JobTimeout {
		return errors.New("smtp.timeout must be less than worker.job_timeout")
	}
	if (cfg.SMTP.Username == "") != (cfg.SMTP.Password == "") {
		return errors.New("smtp.username and SMTP_PASSWORD must both be set or both be empty")
	}
	if cfg.SMTP.Enabled() {
		if _, err := mailer.NewSMTPSender(cfg.SMTP); err != nil {
			return err
		}
	}
	return nil
}

func validateRiskGSB(g RiskGSBConfig, jobTimeout time.Duration) error {
	if g.Timeout <= 0 {
		return errors.New("worker.risk.gsb.timeout must be positive")
	}
	if !g.Enabled() {
		return nil
	}
	if err := validateRiskEndpointURL("worker.risk.gsb.base_url", g.BaseURL); err != nil {
		return err
	}
	if g.Timeout >= jobTimeout {
		return errors.New("worker.risk.gsb.timeout must be less than worker.job_timeout")
	}
	return nil
}

func validateRiskLLM(l RiskLLMConfig, jobTimeout time.Duration) error {
	if l.BaseURL != "" || l.Model != "" || l.APIKey != "" {
		if l.BaseURL == "" || l.Model == "" {
			return errors.New("worker.risk.llm.base_url and worker.risk.llm.model must both be set when LLM risk scan is configured")
		}
		if err := validateRiskEndpointURL("worker.risk.llm.base_url", l.BaseURL); err != nil {
			return err
		}
	}
	if !risk.ValidLLMMode(l.Mode) {
		return fmt.Errorf("worker.risk.llm.mode must be %q or %q, got %q", risk.LLMModeBan, risk.LLMModeShadow, l.Mode)
	}
	if math.IsNaN(l.MinConfidence) || math.IsInf(l.MinConfidence, 0) || l.MinConfidence <= 0 || l.MinConfidence > 1 {
		return errors.New("worker.risk.llm.min_confidence must be in (0, 1]")
	}
	if l.Timeout <= 0 {
		return errors.New("worker.risk.llm.timeout must be positive")
	}
	if l.Enabled() && l.Timeout >= jobTimeout {
		return errors.New("worker.risk.llm.timeout must be less than worker.job_timeout")
	}
	if l.MaxPerMin < 0 {
		return errors.New("worker.risk.llm.max_per_min must not be negative")
	}
	return nil
}

func validateRiskEndpointURL(field, raw string) error {
	if err := config.ValidateIssuerURL(field, raw); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return fmt.Errorf("%s must not contain userinfo", field)
	}
	return nil
}
