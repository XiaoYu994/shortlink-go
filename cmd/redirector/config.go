package main

import (
	"errors"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/logx"
)

const (
	defaultRedirectorAddr         = ":8080"
	defaultAccessLogSampleRate    = 1.0
	defaultAccessLogSlowThreshold = 200 * time.Millisecond
)

// RedirectorConfig 是公开跳转进程（cmd/redirector）的完整启动配置。
type RedirectorConfig struct {
	config.BaseConfig

	/*
		VisitorSalt 是按天访客哈希的 HMAC 密钥。redirector 启动时要求非空；
		为空时不记访客会导致 UV 一直是 0，与其静默降级不如启动失败。
		盐不能提交到仓库，生产从密钥管理注入。
	*/
	VisitorSalt string
	/*
		TrustProxyHeaders 为 true 时按 X-Forwarded-For 与 TrustedProxyCIDRS 解析客户端 IP。
		最左一段可由客户端伪造，只能开在可信反向代理后面。
	*/
	TrustProxyHeaders bool
	// TrustedProxyCIDRS 是可信代理网段（如 CDN/网关 IP）；非空时必须开启 TRUST_PROXY_HEADERS=true。
	TrustedProxyCIDRs []netip.Prefix
	/*
		PprofAddr 是剖析端点的监听地址，为空时不开（默认）。
		只接受回环地址：剖析数据里有堆内容，端点也没有鉴权。见 ADR 0013。
	*/
	PprofAddr string
	// CacheMissConcurrency 是跳转读路径普通缓存未命中的进程内回源并发上限（ADR 0008）。
	CacheMissConcurrency int
	// AccessLogSampleRate 是 redirector 正常 INFO 访问日志的采样率（[0, 1]，默认 1 全量；5xx 与慢请求全保）。
	AccessLogSampleRate float64
	// AccessLogSlowThreshold 是判定慢请求并升为 Warn 全量保留的耗时门槛（默认 200ms）。
	AccessLogSlowThreshold time.Duration
}

// LogValue 掩码 VisitorSalt 与基础连接串。
func (cfg RedirectorConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.String("visitor_salt", logx.RedactSecret(cfg.VisitorSalt)),
		slog.Bool("trust_proxy_headers", cfg.TrustProxyHeaders),
		slog.Int("trusted_proxy_cidrs_count", len(cfg.TrustedProxyCIDRs)),
		slog.String("pprof_addr", cfg.PprofAddr),
		slog.Int("cache_miss_concurrency", cfg.CacheMissConcurrency),
		slog.Float64("access_log_sample_rate", cfg.AccessLogSampleRate),
		slog.Duration("access_log_slow_threshold", cfg.AccessLogSlowThreshold),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出访客盐或连接串密码。
func (cfg RedirectorConfig) String() string {
	return cfg.LogValue().String()
}

/*
loadRedirectorConfig 加载并校验跳转进程配置。
主监听地址只读 ADDR（默认 :8080），独立运维端口读 OPS_ADDR；VISITOR_SALT 必填；
PPROF_ADDR 若设置必须是回环地址。不读取 OIDC_* 或 GITHUB_*。
*/
func loadRedirectorConfig() (RedirectorConfig, error) {
	base, err := config.LoadBaseConfig(config.BaseConfigDefaults{
		Addr:              config.EnvString("ADDR", defaultRedirectorAddr),
		OpsAddr:           os.Getenv("OPS_ADDR"),
		CacheFetchTimeout: cache.DefaultFetchTimeout,
	})
	if err != nil {
		return RedirectorConfig{}, err
	}

	cfg := RedirectorConfig{
		BaseConfig:             base,
		VisitorSalt:            os.Getenv("VISITOR_SALT"),
		PprofAddr:              os.Getenv("PPROF_ADDR"),
		CacheMissConcurrency:   cache.DefaultMissConcurrency,
		AccessLogSampleRate:    defaultAccessLogSampleRate,
		AccessLogSlowThreshold: defaultAccessLogSlowThreshold,
	}
	if cfg.OpsAddr != "" && cfg.OpsAddr == cfg.Addr {
		return RedirectorConfig{}, errors.New("OPS_ADDR must differ from ADDR")
	}
	if cfg.VisitorSalt == "" {
		return RedirectorConfig{}, errors.New("VISITOR_SALT is required")
	}
	if cfg.TrustProxyHeaders, err = config.EnvBool("TRUST_PROXY_HEADERS", false); err != nil {
		return RedirectorConfig{}, err
	}
	if raw := os.Getenv("TRUSTED_PROXY_CIDRS"); raw != "" {
		cidrs, err := config.ParseCIDRs(raw)
		if err != nil {
			return RedirectorConfig{}, err
		}
		if !cfg.TrustProxyHeaders {
			return RedirectorConfig{}, errors.New("TRUSTED_PROXY_CIDRS requires TRUST_PROXY_HEADERS=true")
		}
		cfg.TrustedProxyCIDRs = cidrs
	}
	if cfg.PprofAddr != "" && !config.IsLoopbackAddr(cfg.PprofAddr) {
		return RedirectorConfig{}, errors.New("PPROF_ADDR must be a loopback address such as 127.0.0.1:6060")
	}
	if cfg.CacheMissConcurrency, err = config.EnvInt("CACHE_MISS_CONCURRENCY", cfg.CacheMissConcurrency); err != nil {
		return RedirectorConfig{}, err
	}
	if cfg.CacheMissConcurrency <= 0 {
		return RedirectorConfig{}, errors.New("CACHE_MISS_CONCURRENCY must be positive")
	}
	if cfg.AccessLogSampleRate, err = config.EnvFloat("ACCESS_LOG_SAMPLE_RATE", cfg.AccessLogSampleRate); err != nil {
		return RedirectorConfig{}, err
	}
	if math.IsNaN(cfg.AccessLogSampleRate) || cfg.AccessLogSampleRate < 0 || cfg.AccessLogSampleRate > 1 {
		return RedirectorConfig{}, errors.New("ACCESS_LOG_SAMPLE_RATE must be between 0 and 1")
	}
	if cfg.AccessLogSlowThreshold, err = config.EnvDuration("ACCESS_LOG_SLOW_THRESHOLD", cfg.AccessLogSlowThreshold); err != nil {
		return RedirectorConfig{}, err
	}
	if cfg.AccessLogSlowThreshold <= 0 {
		return RedirectorConfig{}, errors.New("ACCESS_LOG_SLOW_THRESHOLD must be positive")
	}
	return cfg, nil
}
