package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/identity"
	"shortlink/internal/logx"
	"shortlink/internal/mailer"
	"shortlink/internal/sms"
	"shortlink/internal/webhook"
)

const (
	defaultConsoleAddr        = ":8081"
	defaultConsoleIPRateLimit = 60
	defaultConsoleIPRateBurst = 120
)

// ConsoleConfig 是管理进程 HTTP 模式（cmd/console serve）的完整启动配置。
type ConsoleConfig struct {
	config.BaseConfig

	Features             config.FeatureFlags
	EmailEnabled         bool
	OIDC                 OIDCConfig
	GitHub               GitHubConfig
	PhoneOTP             PhoneOTPConfig
	PublicURL            string
	PublicBaseURL        string
	WebDistDir           string
	WebhookSecretKey     string
	OutboundAllowedPorts []int
	Proxy                config.ProxyConfig
	// IPRateLimit 与 IPRateBurst 控制管理 API 鉴权前按源 IP 限流（0 表示关闭）。
	IPRateLimit int
	IPRateBurst int
}

// StoreFeatures 推导管理进程写路径需要挂载的事务钩子特性集。
func (cfg ConsoleConfig) StoreFeatures() StoreFeatures {
	return StoreFeatures{
		Analytics: cfg.Features.Analytics,
		RiskScan:  cfg.Features.RiskScan,
		Webhooks:  cfg.WebhookSecretKey != "",
		Email:     cfg.EmailEnabled,
	}
}

// OIDCConfig 存放标准 OIDC 身份提供方的接入参数。
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Enabled 报告是否配置了 OIDC 登录。validate 已保证所需参数全配或全空。
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

// Enabled 报告是否配置了 GitHub 登录。validate 已保证所需参数全配或全空。
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

// PhoneOTPConfig 存放手机号短信验证码登录的通道与防刷参数（ADR 0030）。Provider 为空时关闭该登录方式。
type PhoneOTPConfig struct {
	Provider      string
	WebhookURL    string
	WebhookToken  string
	AllowedPorts  []int
	Timeout       time.Duration
	TTL           time.Duration
	Cooldown      time.Duration
	MaxPerHour    int
	MaxAttempts   int
	SecureCookies bool
}

// Enabled 报告是否配置了手机号验证码登录。
func (c PhoneOTPConfig) Enabled() bool {
	return c.Provider != ""
}

// LogValue 掩码 WebhookToken 与 WebhookURL 潜在凭据，避免日志泄露短信网关凭据。
func (c PhoneOTPConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", c.Provider),
		slog.String("webhook_url", logx.RedactURL(c.WebhookURL)),
		slog.String("webhook_token", logx.RedactSecret(c.WebhookToken)),
		slog.Int("allowed_ports_count", len(c.AllowedPorts)),
		slog.Duration("timeout", c.Timeout),
		slog.Duration("ttl", c.TTL),
		slog.Duration("cooldown", c.Cooldown),
		slog.Int("max_per_hour", c.MaxPerHour),
		slog.Int("max_attempts", c.MaxAttempts),
		slog.Bool("secure_cookies", c.SecureCookies),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出短信网关凭据。
func (c PhoneOTPConfig) String() string {
	return c.LogValue().String()
}

// LogValue 掩码基础连接串及 OIDC/GitHub/SMS/Webhook 客户端密钥。
func (cfg ConsoleConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.Any("features", cfg.Features.LogValue()),
		slog.Bool("email_enabled", cfg.EmailEnabled),
		slog.String("public_url", cfg.PublicURL),
		slog.Any("oidc", cfg.OIDC.LogValue()),
		slog.Any("github", cfg.GitHub.LogValue()),
		slog.Any("phone_otp", cfg.PhoneOTP.LogValue()),
		slog.String("web_dist_dir", cfg.WebDistDir),
		slog.String("public_base_url", cfg.PublicBaseURL),
		slog.String("webhook_secret_key", logx.RedactSecret(cfg.WebhookSecretKey)),
		slog.Int("outbound_allowed_ports_count", len(cfg.OutboundAllowedPorts)),
		slog.Any("proxy", cfg.Proxy.LogValue()),
		slog.Int("ip_rate_limit", cfg.IPRateLimit),
		slog.Int("ip_rate_burst", cfg.IPRateBurst),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出凭据。
func (cfg ConsoleConfig) String() string {
	return cfg.LogValue().String()
}

type consoleSection struct {
	Addr          string          `yaml:"addr"`
	OpsAddr       string          `yaml:"ops_addr"`
	PublicURL     string          `yaml:"public_url"`
	PublicBaseURL string          `yaml:"public_base_url"`
	WebDistDir    string          `yaml:"web_dist_dir"`
	IPRateLimit   *int            `yaml:"ip_rate_limit"`
	IPRateBurst   *int            `yaml:"ip_rate_burst"`
	Auth          consoleAuthYAML `yaml:"auth"`
}

type consoleAuthYAML struct {
	OIDC     consoleOIDCYAML     `yaml:"oidc"`
	GitHub   consoleGitHubYAML   `yaml:"github"`
	PhoneOTP consolePhoneOTPYAML `yaml:"phone_otp"`
}

type consoleOIDCYAML struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// client_secret 仅从 OIDC_CLIENT_SECRET 环境变量读取，不定义 yaml tag。
}

type consoleGitHubYAML struct {
	ClientID string `yaml:"client_id"`
	// client_secret 仅从 GITHUB_CLIENT_SECRET 环境变量读取，不定义 yaml tag。
}

type consolePhoneOTPYAML struct {
	Provider    string           `yaml:"provider"`
	WebhookURL  string           `yaml:"webhook_url"`
	Timeout     *config.Duration `yaml:"timeout"`
	TTL         *config.Duration `yaml:"ttl"`
	Cooldown    *config.Duration `yaml:"cooldown"`
	MaxPerHour  *int             `yaml:"max_per_hour"`
	MaxAttempts *int             `yaml:"max_attempts"`
	// webhook_token 仅从 SMS_WEBHOOK_TOKEN 环境变量读取，不定义 yaml tag。
}

type smtpSection struct {
	Host     string           `yaml:"host"`
	Port     *int             `yaml:"port"`
	Username string           `yaml:"username"`
	From     string           `yaml:"from"`
	TLSMode  string           `yaml:"tls_mode"`
	Timeout  *config.Duration `yaml:"timeout"`
	// password 仅从 SMTP_PASSWORD 环境变量读取，不定义 yaml tag。
}

/*
loadConsoleConfig 加载并校验管理进程 HTTP 模式配置（ADR 0035）。
主监听地址优先读 CONSOLE_ADDR，其次 console.addr（默认 :8081）；独立运维端口读 CONSOLE_OPS_ADDR / console.ops_addr。
OIDC/GitHub 回调与 PhoneOTP SecureCookies 均由 console.public_url 自动推导（单一事实来源）。
*/
func loadConsoleConfig(configPath string) (ConsoleConfig, error) {
	file, err := config.LoadFile(config.ResolvePath(configPath))
	if err != nil {
		return ConsoleConfig{}, err
	}

	var sec consoleSection
	if err := config.DecodeSection(file.Console, &sec); err != nil {
		return ConsoleConfig{}, fmt.Errorf("console: %w", err)
	}

	addr := defaultConsoleAddr
	if v := strings.TrimSpace(sec.Addr); v != "" {
		addr = v
	}
	opsAddr := strings.TrimSpace(sec.OpsAddr)
	addr = config.EnvString("CONSOLE_ADDR", addr)
	opsAddr = config.EnvString("CONSOLE_OPS_ADDR", opsAddr)

	base, err := config.LoadBaseConfig(file, config.BaseConfigDefaults{
		Addr:              addr,
		OpsAddr:           opsAddr,
		CacheFetchTimeout: cache.DefaultFetchTimeout,
	})
	if err != nil {
		return ConsoleConfig{}, err
	}
	features, err := config.LoadFeatureFlags(file)
	if err != nil {
		return ConsoleConfig{}, err
	}
	proxy, err := config.LoadProxyConfig(file)
	if err != nil {
		return ConsoleConfig{}, err
	}
	outboundPorts, err := config.LoadOutboundAllowedPorts(file)
	if err != nil {
		return ConsoleConfig{}, err
	}
	emailEnabled, err := loadEmailEnabled(file)
	if err != nil {
		return ConsoleConfig{}, err
	}

	publicURL := strings.TrimRight(strings.TrimSpace(sec.PublicURL), "/")
	if publicURL != "" {
		if err := config.ValidateIssuerURL("console.public_url", publicURL); err != nil {
			return ConsoleConfig{}, err
		}
	}
	publicBaseURL := strings.TrimRight(strings.TrimSpace(sec.PublicBaseURL), "/")
	if publicBaseURL != "" {
		if err := config.ValidateIssuerURL("console.public_base_url", publicBaseURL); err != nil {
			return ConsoleConfig{}, err
		}
	}

	cfg := ConsoleConfig{
		BaseConfig:   base,
		Features:     features,
		EmailEnabled: emailEnabled,
		OIDC: OIDCConfig{
			Issuer:       strings.TrimSpace(sec.Auth.OIDC.Issuer),
			ClientID:     strings.TrimSpace(sec.Auth.OIDC.ClientID),
			ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		},
		GitHub: GitHubConfig{
			ClientID:     strings.TrimSpace(sec.Auth.GitHub.ClientID),
			ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		},
		PhoneOTP: PhoneOTPConfig{
			Provider:      strings.ToLower(strings.TrimSpace(sec.Auth.PhoneOTP.Provider)),
			WebhookURL:    strings.TrimSpace(sec.Auth.PhoneOTP.WebhookURL),
			WebhookToken:  strings.TrimSpace(os.Getenv("SMS_WEBHOOK_TOKEN")),
			AllowedPorts:  outboundPorts,
			Timeout:       sms.DefaultTimeout,
			TTL:           identity.DefaultOTPTTL,
			Cooldown:      identity.DefaultOTPCooldown,
			MaxPerHour:    identity.DefaultOTPMaxPerHour,
			MaxAttempts:   identity.DefaultOTPMaxAttempts,
			SecureCookies: strings.HasPrefix(strings.ToLower(publicURL), "https://"),
		},
		PublicURL:            publicURL,
		PublicBaseURL:        publicBaseURL,
		WebDistDir:           strings.TrimSpace(sec.WebDistDir),
		WebhookSecretKey:     strings.TrimSpace(os.Getenv("WEBHOOK_SECRET_KEY")),
		OutboundAllowedPorts: outboundPorts,
		Proxy:                proxy,
		IPRateLimit:          defaultConsoleIPRateLimit,
		IPRateBurst:          defaultConsoleIPRateBurst,
	}

	if sec.Auth.PhoneOTP.Timeout != nil {
		cfg.PhoneOTP.Timeout = sec.Auth.PhoneOTP.Timeout.Std()
	}
	if sec.Auth.PhoneOTP.TTL != nil {
		cfg.PhoneOTP.TTL = sec.Auth.PhoneOTP.TTL.Std()
	}
	if sec.Auth.PhoneOTP.Cooldown != nil {
		cfg.PhoneOTP.Cooldown = sec.Auth.PhoneOTP.Cooldown.Std()
	}
	if sec.Auth.PhoneOTP.MaxPerHour != nil {
		cfg.PhoneOTP.MaxPerHour = *sec.Auth.PhoneOTP.MaxPerHour
	}
	if sec.Auth.PhoneOTP.MaxAttempts != nil {
		cfg.PhoneOTP.MaxAttempts = *sec.Auth.PhoneOTP.MaxAttempts
	}

	if sec.IPRateLimit != nil {
		cfg.IPRateLimit = *sec.IPRateLimit
	}
	if sec.IPRateBurst != nil {
		cfg.IPRateBurst = *sec.IPRateBurst
	} else if sec.IPRateLimit != nil && cfg.IPRateLimit > 0 {
		cfg.IPRateBurst = max(cfg.IPRateLimit*2, 1)
	}

	if cfg.OpsAddr != "" && cfg.OpsAddr == cfg.Addr {
		return ConsoleConfig{}, errors.New("console.ops_addr must differ from addr")
	}
	if cfg.WebhookSecretKey != "" {
		if _, err := webhook.ParseMasterKeyHex(cfg.WebhookSecretKey); err != nil {
			return ConsoleConfig{}, err
		}
	}
	if err := validateOIDC(cfg.OIDC, cfg.PublicURL); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.OIDC.Enabled() {
		cfg.OIDC.RedirectURL = cfg.PublicURL + "/api/v1/auth/oidc/callback"
	}
	if err := validateGitHub(cfg.GitHub, cfg.PublicURL); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.GitHub.Enabled() {
		cfg.GitHub.RedirectURL = cfg.PublicURL + "/api/v1/auth/github/callback"
	}
	if err := validatePhoneOTP(cfg.PhoneOTP); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.IPRateLimit < 0 {
		return ConsoleConfig{}, errors.New("console.ip_rate_limit must not be negative")
	}
	if cfg.IPRateLimit > 0 && cfg.IPRateBurst <= 0 {
		return ConsoleConfig{}, errors.New("console.ip_rate_burst must be positive when console.ip_rate_limit is enabled")
	}
	return cfg, nil
}

func loadSMTPFromFile(file *config.File) (mailer.SMTPConfig, error) {
	cfg := mailer.SMTPConfig{
		Port:     mailer.DefaultSMTPPort,
		Password: os.Getenv("SMTP_PASSWORD"),
		TLSMode:  mailer.TLSModeStartTLS,
		Timeout:  mailer.DefaultSMTPTimeout,
	}
	if file == nil {
		return cfg, nil
	}
	var sec smtpSection
	if err := config.DecodeSection(file.SMTP, &sec); err != nil {
		return mailer.SMTPConfig{}, fmt.Errorf("smtp: %w", err)
	}
	cfg.Host = strings.TrimSpace(sec.Host)
	if sec.Port != nil {
		cfg.Port = *sec.Port
	}
	cfg.Username = strings.TrimSpace(sec.Username)
	cfg.From = strings.TrimSpace(sec.From)
	if mode := strings.ToLower(strings.TrimSpace(sec.TLSMode)); mode != "" {
		cfg.TLSMode = mode
	}
	if sec.Timeout != nil {
		cfg.Timeout = sec.Timeout.Std()
	}
	return cfg, nil
}

func validateOIDC(c OIDCConfig, publicURL string) error {
	set := config.CountNonEmpty(c.Issuer, c.ClientID, c.ClientSecret)
	if set == 0 {
		return nil
	}
	if set != 3 {
		return errors.New("console.auth.oidc.issuer, console.auth.oidc.client_id, and OIDC_CLIENT_SECRET must all be set or all be empty")
	}
	if publicURL == "" {
		return errors.New("console.public_url is required when OIDC auth is enabled")
	}
	return config.ValidateIssuerURL("console.auth.oidc.issuer", c.Issuer)
}

func validateGitHub(c GitHubConfig, publicURL string) error {
	set := config.CountNonEmpty(c.ClientID, c.ClientSecret)
	if set == 0 {
		return nil
	}
	if set != 2 {
		return errors.New("console.auth.github.client_id and GITHUB_CLIENT_SECRET must both be set or both be empty")
	}
	if publicURL == "" {
		return errors.New("console.public_url is required when GitHub auth is enabled")
	}
	return nil
}

func validatePhoneOTP(c PhoneOTPConfig) error {
	if c.Provider == "" {
		if c.WebhookURL != "" || c.WebhookToken != "" {
			return errors.New("console.auth.phone_otp.provider is required when webhook_url or SMS_WEBHOOK_TOKEN is set")
		}
		return nil
	}
	if !sms.ValidProvider(c.Provider) {
		return fmt.Errorf("console.auth.phone_otp.provider must be %q or %q, got %q", sms.ProviderWebhook, sms.ProviderLog, c.Provider)
	}
	if c.Provider == sms.ProviderWebhook && c.WebhookURL == "" {
		return errors.New("console.auth.phone_otp.webhook_url is required when provider=webhook")
	}
	if c.Timeout <= 0 {
		return errors.New("console.auth.phone_otp.timeout must be positive")
	}
	if c.TTL <= 0 {
		return errors.New("console.auth.phone_otp.ttl must be positive")
	}
	if c.Cooldown <= 0 {
		return errors.New("console.auth.phone_otp.cooldown must be positive")
	}
	if c.MaxPerHour <= 0 {
		return errors.New("console.auth.phone_otp.max_per_hour must be positive")
	}
	if c.MaxAttempts <= 0 {
		return errors.New("console.auth.phone_otp.max_attempts must be positive")
	}
	return nil
}
