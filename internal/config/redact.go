package config

import (
	"log/slog"

	"shortlink/internal/logx"
)

// LogValue 掩码 DatabaseURL 与 RedisURL 中的凭据，避免配置直接进日志时泄漏密码。
func (cfg BaseConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", cfg.Addr),
		slog.String("database_url", logx.RedactURL(cfg.DatabaseURL)),
		slog.String("redis_url", logx.RedactURL(cfg.RedisURL)),
		slog.Int("db_max_open_conns", cfg.Pool.MaxOpenConns),
		slog.Int("db_max_idle_conns", cfg.Pool.MaxIdleConns),
		slog.Duration("db_conn_max_lifetime", cfg.Pool.ConnMaxLifetime),
		slog.Duration("db_conn_max_idle_time", cfg.Pool.ConnMaxIdleTime),
		slog.Duration("request_timeout", cfg.RequestTimeout),
		slog.Duration("shutdown_timeout", cfg.ShutdownTimeout),
		slog.Duration("drain_delay", cfg.DrainDelay),
		slog.String("log_level", cfg.LogLevel.String()),
		slog.String("log_format", cfg.LogFormat),
		slog.Int("cache_miss_concurrency", cfg.CacheMissConcurrency),
		slog.Duration("cache_fetch_timeout", cfg.CacheFetchTimeout),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出连接串密码。
func (cfg BaseConfig) String() string {
	return cfg.LogValue().String()
}

// LogValue 掩码 VisitorSalt 与基础连接串。
func (cfg RedirectorConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.String("visitor_salt", logx.RedactSecret(cfg.VisitorSalt)),
		slog.Bool("trust_proxy_headers", cfg.TrustProxyHeaders),
		slog.String("pprof_addr", cfg.PprofAddr),
		slog.Float64("access_log_sample_rate", cfg.AccessLogSampleRate),
		slog.Duration("access_log_slow_threshold", cfg.AccessLogSlowThreshold),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出访客盐或连接串密码。
func (cfg RedirectorConfig) String() string {
	return cfg.LogValue().String()
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
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出凭据。
func (cfg ConsoleConfig) String() string {
	return cfg.LogValue().String()
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
		slog.Duration("risk_fetch_timeout", cfg.RiskFetchTimeout),
		slog.Int("risk_host_rate_limit", cfg.RiskHostRateLimit),
		slog.Int("risk_blocked_keywords_count", len(cfg.RiskBlockedKeywords)),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出基础连接串凭据。
func (cfg WorkerConfig) String() string {
	return cfg.LogValue().String()
}
