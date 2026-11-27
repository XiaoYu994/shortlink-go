package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
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

	Features config.FeatureFlags
	/*
		VisitorSalt 是按天访客哈希的 HMAC 密钥（仅 Env：VISITOR_SALT）。
		analytics 开启时启动必填；为空时不记访客会导致 UV 一直是 0，与其静默降级不如启动失败；
		关闭点击统计时允许留空（ADR 0029）。盐不能提交到仓库，生产从密钥管理注入。
	*/
	VisitorSalt string
	// Proxy 控制可信反向代理头解析策略（ADR 0024、ADR 0035）。
	Proxy config.ProxyConfig
	/*
		PprofAddr 是剖析端点的监听地址，为空时不开（默认）。
		只接受回环地址：剖析数据里有堆内容，端点没有鉴权。见 ADR 0013。
	*/
	PprofAddr string
	// CacheMissConcurrency 是跳转读路径普通缓存未命中的进程内回源并发上限（ADR 0008）。
	CacheMissConcurrency int
	// AccessLogSampleRate 是 redirector 正常 INFO 访问日志的采样率（[0, 1]，默认 1 全量；5xx 与慢请求全保）。
	AccessLogSampleRate float64
	// AccessLogSlowThreshold 是判定慢请求并升为 Warn 全量保留的耗时门槛（默认 200ms）。
	AccessLogSlowThreshold time.Duration
}

type redirectorSection struct {
	Addr                   string           `yaml:"addr"`
	OpsAddr                string           `yaml:"ops_addr"`
	PprofAddr              string           `yaml:"pprof_addr"`
	CacheMissConcurrency   *int             `yaml:"cache_miss_concurrency"`
	AccessLogSampleRate    *float64         `yaml:"access_log_sample_rate"`
	AccessLogSlowThreshold *config.Duration `yaml:"access_log_slow_threshold"`
}

// LogValue 掩码 VisitorSalt 与基础连接串。
func (cfg RedirectorConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("base", cfg.BaseConfig.LogValue()),
		slog.Any("features", cfg.Features.LogValue()),
		slog.String("visitor_salt", logx.RedactSecret(cfg.VisitorSalt)),
		slog.Any("proxy", cfg.Proxy.LogValue()),
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
loadRedirectorConfig 加载并校验跳转进程配置（ADR 0035）：
按 代码默认 < 可选共享 YAML < 环境变量 合并；
只解码公共节与 redirector 节，不引入 console/worker/smtp/outbound 依赖，保持 ADR 0024 边界。
*/
func loadRedirectorConfig(configPath string) (RedirectorConfig, error) {
	file, err := config.LoadFile(config.ResolvePath(configPath))
	if err != nil {
		return RedirectorConfig{}, err
	}

	var sec redirectorSection
	if err := config.DecodeSection(file.Redirector, &sec); err != nil {
		return RedirectorConfig{}, fmt.Errorf("redirector: %w", err)
	}

	addr := defaultRedirectorAddr
	if v := strings.TrimSpace(sec.Addr); v != "" {
		addr = v
	}
	opsAddr := strings.TrimSpace(sec.OpsAddr)
	addr = config.EnvString("ADDR", addr)
	opsAddr = config.EnvString("OPS_ADDR", opsAddr)

	base, err := config.LoadBaseConfig(file, config.BaseConfigDefaults{
		Addr:              addr,
		OpsAddr:           opsAddr,
		CacheFetchTimeout: cache.DefaultFetchTimeout,
	})
	if err != nil {
		return RedirectorConfig{}, err
	}

	features, err := config.LoadFeatureFlags(file)
	if err != nil {
		return RedirectorConfig{}, err
	}
	proxy, err := config.LoadProxyConfig(file)
	if err != nil {
		return RedirectorConfig{}, err
	}

	cfg := RedirectorConfig{
		BaseConfig:             base,
		Features:               features,
		VisitorSalt:            os.Getenv("VISITOR_SALT"),
		Proxy:                  proxy,
		PprofAddr:              strings.TrimSpace(sec.PprofAddr),
		CacheMissConcurrency:   cache.DefaultMissConcurrency,
		AccessLogSampleRate:    defaultAccessLogSampleRate,
		AccessLogSlowThreshold: defaultAccessLogSlowThreshold,
	}
	if sec.CacheMissConcurrency != nil {
		cfg.CacheMissConcurrency = *sec.CacheMissConcurrency
	}
	if sec.AccessLogSampleRate != nil {
		cfg.AccessLogSampleRate = *sec.AccessLogSampleRate
	}
	if sec.AccessLogSlowThreshold != nil {
		cfg.AccessLogSlowThreshold = sec.AccessLogSlowThreshold.Std()
	}

	if cfg.OpsAddr != "" && cfg.OpsAddr == cfg.Addr {
		return RedirectorConfig{}, errors.New("redirector.ops_addr must differ from addr")
	}
	if cfg.Features.Analytics && cfg.VisitorSalt == "" {
		return RedirectorConfig{}, errors.New("VISITOR_SALT is required when analytics is enabled")
	}
	if cfg.PprofAddr != "" && !config.IsLoopbackAddr(cfg.PprofAddr) {
		return RedirectorConfig{}, errors.New("redirector.pprof_addr must be a loopback address such as 127.0.0.1:6060")
	}
	if cfg.CacheMissConcurrency <= 0 {
		return RedirectorConfig{}, errors.New("redirector.cache_miss_concurrency must be positive")
	}
	if math.IsNaN(cfg.AccessLogSampleRate) || math.IsInf(cfg.AccessLogSampleRate, 0) || cfg.AccessLogSampleRate < 0 || cfg.AccessLogSampleRate > 1 {
		return RedirectorConfig{}, errors.New("redirector.access_log_sample_rate must be between 0 and 1")
	}
	if cfg.AccessLogSlowThreshold <= 0 {
		return RedirectorConfig{}, errors.New("redirector.access_log_slow_threshold must be positive")
	}
	return cfg, nil
}
