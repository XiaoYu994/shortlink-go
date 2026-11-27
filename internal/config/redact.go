package config

import (
	"log/slog"

	"shortlink/internal/logx"
)

// LogValue 掩码 DatabaseURL 与 RedisURL 中的凭据，避免配置直接进日志时泄漏密码。
func (cfg BaseConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", cfg.Addr),
		slog.String("ops_addr", cfg.OpsAddr),
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
		slog.Duration("cache_fetch_timeout", cfg.CacheFetchTimeout),
		slog.String("otel_endpoint", logx.RedactURL(cfg.Tracing.Endpoint)),
		slog.Float64("otel_sample_rate", cfg.Tracing.SampleRate),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出连接串密码。
func (cfg BaseConfig) String() string {
	return cfg.LogValue().String()
}

// LogValue 输出可信代理开关与解析后的 CIDR 数量。
func (p ProxyConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("trust_headers", p.TrustHeaders),
		slog.Int("trusted_cidrs_count", len(p.TrustedCIDRs)),
	)
}

// String 实现 fmt.Stringer。
func (p ProxyConfig) String() string {
	return p.LogValue().String()
}
