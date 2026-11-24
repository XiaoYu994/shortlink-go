/*
Package config 从环境变量加载进程配置，并在启动时校验。

两个 HTTP 进程共用 BaseConfig（数据库、Redis、连接池、超时、日志与缓存回源保护），
再分别通过 LoadRedirectorConfig 和 LoadConsoleConfig 读取并校验各自的专属配置。
缺少必填项或任一参数不合法时直接返回错误，由 main 终止启动。
*/
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/postgres"
	"shortlink/internal/risk"
)

const (
	defaultRedirectorAddr = ":8080"
	defaultConsoleAddr    = ":8081"
	// defaultShutdownTimeout 与 .env.example 里的 SHUTDOWN_TIMEOUT 一致。
	defaultShutdownTimeout = 10 * time.Second
	// defaultDrainDelay 与 docker-compose.yml 里健康检查的 interval 一致，盖住一个探测周期。
	defaultDrainDelay = 5 * time.Second
	/*
		defaultRequestTimeout 是单个请求的处理时限。httpx.Serve 让读写超时比它宽松，
		保证读完 body，并在超时后还有时间把 504 写回客户端。
	*/
	defaultRequestTimeout = 5 * time.Second
	// 回源保护的默认值和 cache 包共用，避免两处漂移。
	defaultCacheMissConcurrency = cache.DefaultMissConcurrency
	defaultCacheFetchTimeout    = cache.DefaultFetchTimeout

	// 本地默认 text，人眼能直接读。json 一行一条，给生产采集。
	logFormatText = "text"
	logFormatJSON = "json"

	defaultWorkerCleanupInterval = time.Hour
	defaultWorkerRetention       = 7 * 24 * time.Hour
)

/*
BaseConfig 是 redirector 和 console 两个 HTTP 进程共用的启动配置。
Pool 的默认值来自 postgres.DefaultPoolConfig，环境变量只覆盖显式设置的项。
*/
type BaseConfig struct {
	Addr            string // HTTP 监听地址，例如 :8080
	DatabaseURL     string // PostgreSQL 连接串
	RedisURL        string // Redis 连接串，支持密码、TLS（rediss://）和 DB 编号
	Pool            postgres.PoolConfig
	RequestTimeout  time.Duration // 单个请求的处理时限，到点后 handler 的 context 被取消
	ShutdownTimeout time.Duration // 收到中断信号后，等待正在处理的请求结束的上限
	/*
		DrainDelay 是关闭监听之前的等待。这段时间 /readyz 返回 503，
		给主动探测的编排器一个周期把副本摘掉。0 表示不等。
		容器的停止宽限要盖住它，再加上 SHUTDOWN_TIMEOUT。
	*/
	DrainDelay time.Duration
	LogLevel   slog.Level // 日志级别，默认 info
	LogFormat  string     // text 或 json，默认 text

	// CacheMissConcurrency 是普通缓存未命中的回源并发上限。
	CacheMissConcurrency int
	// CacheFetchTimeout 是单次回源超时，独立于请求时限，不能超过 REQUEST_TIMEOUT。
	CacheFetchTimeout time.Duration
}

// RedirectorConfig 是公开跳转进程（cmd/redirector）的完整启动配置。
type RedirectorConfig struct {
	BaseConfig

	/*
		VisitorSalt 是按天访客哈希的 HMAC 密钥。redirector 启动时要求非空；
		为空时不记访客会导致 UV 一直是 0，与其静默降级不如启动失败。
		盐不能提交到仓库，生产从密钥管理注入。
	*/
	VisitorSalt string
	/*
		TrustProxyHeaders 为 true 时从 X-Forwarded-For 的最右一段取客户端 IP。
		最左一段在代理追加头时可以由客户端伪造。只能开在可信反向代理后面。
	*/
	TrustProxyHeaders bool
	/*
		PprofAddr 是剖析端点的监听地址，为空时不开（默认）。
		只接受回环地址：剖析数据里有堆内容，端点也没有鉴权。见 ADR 0013。
	*/
	PprofAddr string
}

// ConsoleConfig 是管理进程 HTTP 模式（cmd/console serve）的完整启动配置。
type ConsoleConfig struct {
	BaseConfig

	/*
		OIDC 是管理进程的 OIDC 登录配置（阶段 11、ADR 0018）。
		四个字段要么全配，要么全空；全空时不挂 OIDC 登录路由。
	*/
	OIDC OIDCConfig
	/*
		GitHub 是管理进程的 GitHub OAuth 2.0 登录配置（阶段 11、ADR 0018）。
		三个字段要么全配，要么全空；可与 OIDC 独立启用或同时启用。
	*/
	GitHub GitHubConfig
	// WebDistDir 非空时，console serve 在 / 托管前端构建产物（阶段 13、ADR 0021）。
	WebDistDir string
	// PublicBaseURL 是公开短链跳转的外部基地址（如 https://s.example.com 或本地 http://127.0.0.1:8080），供前端拼装可点击与可复制的短链地址。
	PublicBaseURL string
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

/*
WorkerConfig 是后台任务进程（cmd/console worker）的完整启动配置（阶段 12、ADR 0020）。
Addr 来自 WORKER_ADDR，默认空表示不开启运维 HTTP 端口；设置后暴露 /healthz、/readyz 和 /metrics。
*/
type WorkerConfig struct {
	BaseConfig

	Concurrency         int
	MaxPerTenant        int
	PollInterval        time.Duration
	LeaseDuration       time.Duration
	JobTimeout          time.Duration
	ReapInterval        time.Duration
	CleanupInterval     time.Duration
	SessionRetention    time.Duration
	DoneJobRetention    time.Duration
	RiskFetchTimeout    time.Duration
	RiskHostRateLimit   int
	RiskBlockedKeywords map[string]link.BanReason
}

/*
LoadRedirectorConfig 加载并校验跳转进程配置。
监听地址只读 ADDR（默认 :8080）；VISITOR_SALT 必填；PPROF_ADDR 若设置必须是回环地址。
不读取 OIDC_* 或 GITHUB_*。
*/
func LoadRedirectorConfig() (RedirectorConfig, error) {
	base, err := loadBaseConfig(envString("ADDR", defaultRedirectorAddr))
	if err != nil {
		return RedirectorConfig{}, err
	}

	cfg := RedirectorConfig{
		BaseConfig:  base,
		VisitorSalt: os.Getenv("VISITOR_SALT"),
		PprofAddr:   os.Getenv("PPROF_ADDR"),
	}
	if cfg.VisitorSalt == "" {
		return RedirectorConfig{}, errors.New("VISITOR_SALT is required")
	}
	if raw := os.Getenv("TRUST_PROXY_HEADERS"); raw != "" {
		trust, err := strconv.ParseBool(raw)
		if err != nil {
			return RedirectorConfig{}, fmt.Errorf("TRUST_PROXY_HEADERS: %w", err)
		}
		cfg.TrustProxyHeaders = trust
	}
	if cfg.PprofAddr != "" && !isLoopbackAddr(cfg.PprofAddr) {
		return RedirectorConfig{}, errors.New("PPROF_ADDR must be a loopback address such as 127.0.0.1:6060")
	}
	return cfg, nil
}

/*
LoadConsoleConfig 加载并校验管理进程 HTTP 模式配置。
监听地址只读 CONSOLE_ADDR（默认 :8081），不读 ADDR，避免和跳转进程抢同一个端口。
不要求 VISITOR_SALT，也不读取 PPROF_ADDR。
*/
func LoadConsoleConfig() (ConsoleConfig, error) {
	base, err := loadBaseConfig(envString("CONSOLE_ADDR", defaultConsoleAddr))
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
	}
	if err := validateOIDC(cfg.OIDC); err != nil {
		return ConsoleConfig{}, err
	}
	if err := validateGitHub(cfg.GitHub); err != nil {
		return ConsoleConfig{}, err
	}
	if cfg.PublicBaseURL != "" {
		if err := validateIssuerURL("PUBLIC_BASE_URL", cfg.PublicBaseURL); err != nil {
			return ConsoleConfig{}, err
		}
	}
	return cfg, nil
}

/*
LoadWorkerConfig 加载并校验后台任务进程（cmd/console worker）的配置。
监听地址只读 WORKER_ADDR（默认空不开），不要求 VISITOR_SALT，不读取 OIDC_* / GITHUB_*。
*/
func LoadWorkerConfig() (WorkerConfig, error) {
	base, err := loadBaseConfig(os.Getenv("WORKER_ADDR"))
	if err != nil {
		return WorkerConfig{}, err
	}

	cfg := WorkerConfig{
		BaseConfig:        base,
		Concurrency:       job.DefaultConcurrency,
		MaxPerTenant:      job.DefaultMaxPerTenant,
		PollInterval:      job.DefaultPollInterval,
		LeaseDuration:     job.DefaultLeaseDuration,
		JobTimeout:        job.DefaultJobTimeout,
		ReapInterval:      job.DefaultReapInterval,
		CleanupInterval:   defaultWorkerCleanupInterval,
		SessionRetention:  defaultWorkerRetention,
		DoneJobRetention:  defaultWorkerRetention,
		RiskFetchTimeout:  risk.DefaultFetchTimeout,
		RiskHostRateLimit: risk.DefaultHostRateLimit,
	}

	if cfg.Concurrency, err = envInt("WORKER_CONCURRENCY", cfg.Concurrency); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.MaxPerTenant, err = envInt("WORKER_MAX_PER_TENANT", cfg.MaxPerTenant); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.PollInterval, err = envDuration("WORKER_POLL_INTERVAL", cfg.PollInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.LeaseDuration, err = envDuration("WORKER_LEASE_DURATION", cfg.LeaseDuration); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.JobTimeout, err = envDuration("WORKER_JOB_TIMEOUT", cfg.JobTimeout); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.ReapInterval, err = envDuration("WORKER_REAP_INTERVAL", cfg.ReapInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.CleanupInterval, err = envDuration("WORKER_CLEANUP_INTERVAL", cfg.CleanupInterval); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.SessionRetention, err = envDuration("WORKER_SESSION_RETENTION", cfg.SessionRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.DoneJobRetention, err = envDuration("WORKER_DONE_JOB_RETENTION", cfg.DoneJobRetention); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.RiskFetchTimeout, err = envDuration("RISK_FETCH_TIMEOUT", cfg.RiskFetchTimeout); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.RiskHostRateLimit, err = envInt("RISK_HOST_RATE_LIMIT", cfg.RiskHostRateLimit); err != nil {
		return WorkerConfig{}, err
	}
	if raw := os.Getenv("RISK_BLOCKED_KEYWORDS"); raw != "" {
		rules, err := risk.ParseKeywordRules(raw)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("RISK_BLOCKED_KEYWORDS: %w", err)
		}
		cfg.RiskBlockedKeywords = rules
	}

	if err := validateWorker(cfg); err != nil {
		return WorkerConfig{}, err
	}
	return cfg, nil
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
	if cfg.JobTimeout > cfg.LeaseDuration {
		return errors.New("WORKER_JOB_TIMEOUT must not exceed WORKER_LEASE_DURATION")
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
	if cfg.RiskFetchTimeout <= 0 {
		return errors.New("RISK_FETCH_TIMEOUT must be positive")
	}
	if cfg.RiskHostRateLimit < 0 {
		return errors.New("RISK_HOST_RATE_LIMIT must not be negative")
	}
	return nil
}

func loadBaseConfig(addr string) (BaseConfig, error) {
	cfg := BaseConfig{
		Addr:                 addr,
		Pool:                 postgres.DefaultPoolConfig(),
		RequestTimeout:       defaultRequestTimeout,
		ShutdownTimeout:      defaultShutdownTimeout,
		DrainDelay:           defaultDrainDelay,
		LogLevel:             slog.LevelInfo,
		LogFormat:            logFormatText,
		CacheMissConcurrency: defaultCacheMissConcurrency,
		CacheFetchTimeout:    defaultCacheFetchTimeout,
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return BaseConfig{}, errors.New("DATABASE_URL is required")
	}
	cfg.RedisURL = os.Getenv("REDIS_URL")
	if cfg.RedisURL == "" {
		return BaseConfig{}, errors.New("REDIS_URL is required")
	}
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return BaseConfig{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	format, err := parseLogFormat(os.Getenv("LOG_FORMAT"))
	if err != nil {
		return BaseConfig{}, err
	}
	cfg.LogFormat = format

	if cfg.Pool.MaxOpenConns, err = envInt("DB_MAX_OPEN_CONNS", cfg.Pool.MaxOpenConns); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.MaxIdleConns, err = envInt("DB_MAX_IDLE_CONNS", cfg.Pool.MaxIdleConns); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.ConnMaxLifetime, err = envDuration("DB_CONN_MAX_LIFETIME", cfg.Pool.ConnMaxLifetime); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.ConnMaxIdleTime, err = envDuration("DB_CONN_MAX_IDLE_TIME", cfg.Pool.ConnMaxIdleTime); err != nil {
		return BaseConfig{}, err
	}
	if cfg.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", cfg.RequestTimeout); err != nil {
		return BaseConfig{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return BaseConfig{}, err
	}
	if cfg.DrainDelay, err = envDuration("DRAIN_DELAY", cfg.DrainDelay); err != nil {
		return BaseConfig{}, err
	}
	if cfg.CacheMissConcurrency, err = envInt("CACHE_MISS_CONCURRENCY", cfg.CacheMissConcurrency); err != nil {
		return BaseConfig{}, err
	}
	if cfg.CacheFetchTimeout, err = envDuration("CACHE_FETCH_TIMEOUT", cfg.CacheFetchTimeout); err != nil {
		return BaseConfig{}, err
	}

	if err := validateBase(cfg); err != nil {
		return BaseConfig{}, err
	}
	return cfg, nil
}

/*
NewLogger 按 LOG_FORMAT 构造日志。调用前格式已经校验过。
text 方便在终端里读；json 给生产采集。这里只认 json，其余按 text，
避免配置加载之外的调用方因为拼写把进程打成没日志。
*/
func (cfg BaseConfig) NewLogger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler
	if cfg.LogFormat == logFormatJSON {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

func parseLogFormat(raw string) (string, error) {
	switch raw {
	case "", logFormatText:
		return logFormatText, nil
	case logFormatJSON:
		return logFormatJSON, nil
	default:
		return "", fmt.Errorf("LOG_FORMAT must be text or json, got %q", raw)
	}
}

func validateBase(cfg BaseConfig) error {
	if cfg.Pool.MaxOpenConns <= 0 {
		return errors.New("DB_MAX_OPEN_CONNS must be positive")
	}
	if cfg.Pool.MaxIdleConns < 0 || cfg.Pool.MaxIdleConns > cfg.Pool.MaxOpenConns {
		return errors.New("DB_MAX_IDLE_CONNS must be between 0 and DB_MAX_OPEN_CONNS")
	}
	if cfg.Pool.ConnMaxLifetime < 0 || cfg.Pool.ConnMaxIdleTime < 0 {
		return errors.New("connection lifetimes must not be negative")
	}
	if cfg.RequestTimeout <= 0 {
		return errors.New("REQUEST_TIMEOUT must be positive")
	}
	if cfg.ShutdownTimeout <= 0 {
		return errors.New("SHUTDOWN_TIMEOUT must be positive")
	}
	if cfg.DrainDelay < 0 {
		return errors.New("DRAIN_DELAY must not be negative")
	}
	if cfg.CacheMissConcurrency <= 0 {
		return errors.New("CACHE_MISS_CONCURRENCY must be positive")
	}
	if cfg.CacheFetchTimeout <= 0 {
		return errors.New("CACHE_FETCH_TIMEOUT must be positive")
	}
	if cfg.CacheFetchTimeout > cfg.RequestTimeout {
		return errors.New("CACHE_FETCH_TIMEOUT must not exceed REQUEST_TIMEOUT")
	}
	return nil
}

func validateOIDC(c OIDCConfig) error {
	set := countNonEmpty(c.Issuer, c.ClientID, c.ClientSecret, c.RedirectURL)
	if set == 0 {
		return nil
	}
	if set != 4 {
		return errors.New("OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, and OIDC_REDIRECT_URL must all be set or all be empty")
	}
	if err := validateIssuerURL("OIDC_ISSUER", c.Issuer); err != nil {
		return err
	}
	return validateRedirectURL("OIDC_REDIRECT_URL", c.RedirectURL)
}

func validateGitHub(c GitHubConfig) error {
	set := countNonEmpty(c.ClientID, c.ClientSecret, c.RedirectURL)
	if set == 0 {
		return nil
	}
	if set != 3 {
		return errors.New("GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET, and GITHUB_REDIRECT_URL must all be set or all be empty")
	}
	return validateRedirectURL("GITHUB_REDIRECT_URL", c.RedirectURL)
}

func countNonEmpty(values ...string) int {
	n := 0
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	return n
}

func validateIssuerURL(envName, raw string) error {
	u, err := parseHTTPURL(raw)
	if err != nil || u.RawQuery != "" {
		return fmt.Errorf("%s must be a valid http or https URL without query or fragment, got %q", envName, raw)
	}
	return nil
}

func validateRedirectURL(envName, raw string) error {
	if _, err := parseHTTPURL(raw); err != nil {
		return fmt.Errorf("%s must be a valid http or https URL, got %q", envName, raw)
	}
	return nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" {
		return nil, errors.New("invalid http url")
	}
	return u, nil
}

/*
isLoopbackAddr 报告 host:port 的 host 是不是回环地址。
":6060" 这种省略 host 的写法会监听所有网卡，不算；
localhost 以外的主机名要解析才知道指向哪里，也不算。
*/
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envInt 读取整数环境变量。变量未设置时返回 def，调用方可以先放上默认值再覆盖。
func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// envDuration 读取 time.ParseDuration 能识别的时长。变量未设置时返回 def。
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}
