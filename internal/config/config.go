/*
Package config 从可选 YAML 配置文件与环境变量加载各进程共用的基础配置，并在启动时校验（ADR 0024、ADR 0035）。

本包仅依赖标准库、logx、pgdb 与 go.yaml.in/yaml/v3，不引入任何业务域或存储业务实现；
redirector、console serve、console worker 各自的专属配置与加载器位于对应 cmd 包内。
*/
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
)

const (
	// DefaultShutdownTimeout 是收到中断信号后等待在途请求结束的默认上限。
	DefaultShutdownTimeout = 10 * time.Second
	// DefaultDrainDelay 与 docker-compose.yml 里健康检查的 interval 一致，盖住一个探测周期。
	DefaultDrainDelay = 5 * time.Second
	/*
		DefaultRequestTimeout 是单个请求的处理时限。httpx.Serve 让读写超时比它宽松，
		保证读完 body，并在超时后还有时间把 504 写回客户端。
	*/
	DefaultRequestTimeout = 5 * time.Second
	// DefaultCacheFetchTimeout 为进程未显式覆写时的默认缓存回源/复查超时。
	DefaultCacheFetchTimeout = time.Second

	// DefaultTraceSampleRate 为开启 OTLP 导出时的默认采样率（1.0 全采，经 ParentBased 传播）。
	DefaultTraceSampleRate = 1.0

	// LogFormatText / LogFormatJSON 区分本地可读日志与生产结构化采集日志。
	LogFormatText = "text"
	LogFormatJSON = "json"
)

/*
TracingConfig 控制进程级 OpenTelemetry 追踪导出（ADR 0032）。
Endpoint 为空时追踪保持默认 Noop（零网络与协程开销）。
*/
type TracingConfig struct {
	Endpoint   string  // tracing.endpoint / OTEL_EXPORTER_OTLP_ENDPOINT，为空时关闭追踪
	SampleRate float64 // tracing.sample_rate，[0, 1]，默认 1.0
}

// Enabled 报告是否配置了 OTLP 追踪导出端点。
func (c TracingConfig) Enabled() bool {
	return c.Endpoint != ""
}

// DefaultTracingConfig 返回追踪配置的默认值（关闭导出，默认采样率 1.0）。
func DefaultTracingConfig() TracingConfig {
	return TracingConfig{SampleRate: DefaultTraceSampleRate}
}

/*
ProxyConfig 统一承载可信反向代理配置（ADR 0024、ADR 0035）。
  - TrustHeaders=false：忽略转发头，只用 RemoteAddr；
  - TrustHeaders=true 且 TrustedCIDRs 为空（来自 "*"）：单跳模式，取 X-Forwarded-For 最右合法 IP；
  - TrustHeaders=true 且 TrustedCIDRs 非空：多跳可信代理链右向剥离模式。
*/
type ProxyConfig struct {
	TrustHeaders bool
	TrustedCIDRs []netip.Prefix
}

/*
BaseConfigDefaults 指定进程级别的默认监听与缓存参数。
由各 cmd 包传入，避免多个进程在共享配置文件与 .env 中争夺同一个监听地址。
*/
type BaseConfigDefaults struct {
	Addr              string
	OpsAddr           string
	CacheFetchTimeout time.Duration
}

/*
BaseConfig 是 redirector、console 与 worker 共用的启动配置。
Pool 的默认值来自 pgdb.DefaultPoolConfig，YAML 与环境变量按优先级逐层覆盖。
*/
type BaseConfig struct {
	Addr            string // 主 HTTP 监听地址（worker 为可选运维端口）
	OpsAddr         string // 可选独立运维端口（/healthz、/readyz、/metrics），为空时不单独开启
	DatabaseURL     string // PostgreSQL 连接串（仅 Env：DATABASE_URL）
	RedisURL        string // 可选 Redis 连接串（仅 Env：REDIS_URL），留空时跳过缓存层直连数据库（ADR 0029）
	Pool            pgdb.PoolConfig
	RequestTimeout  time.Duration // 单个请求的处理时限，到点后 handler 的 context 被取消
	ShutdownTimeout time.Duration // 收到中断信号后，等待正在处理的请求结束的上限
	/*
		DrainDelay 是关闭监听之前的等待。这段时间 /readyz 返回 503，
		给主动探测的编排器一个周期把副本摘掉。0 表示不等。
		容器的停止宽限要盖住它，再加上 shutdown_timeout。
	*/
	DrainDelay time.Duration
	LogLevel   slog.Level // 日志级别，默认 info
	LogFormat  string     // text 或 json，默认 text

	// CacheFetchTimeout 是单次回源或写后复查的超时，独立于请求时限，不能超过 RequestTimeout。
	CacheFetchTimeout time.Duration
	// Tracing 是 OpenTelemetry OTLP/HTTP 追踪导出配置；Endpoint 为空时保持 Noop。
	Tracing TracingConfig
}

type databaseSection struct {
	MaxOpenConns    *int      `yaml:"max_open_conns"`
	MaxIdleConns    *int      `yaml:"max_idle_conns"`
	ConnMaxLifetime *Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime *Duration `yaml:"conn_max_idle_time"`
}

type httpSection struct {
	RequestTimeout  *Duration `yaml:"request_timeout"`
	ShutdownTimeout *Duration `yaml:"shutdown_timeout"`
	DrainDelay      *Duration `yaml:"drain_delay"`
}

type cacheSection struct {
	FetchTimeout *Duration `yaml:"fetch_timeout"`
}

type logSection struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type tracingSection struct {
	Endpoint   string   `yaml:"endpoint"`
	SampleRate *float64 `yaml:"sample_rate"`
}

type outboundSection struct {
	AllowedPorts []int `yaml:"allowed_ports"`
}

/*
LoadBaseConfig 按优先级加载并校验共用基础配置：
代码默认值 < YAML 公共节（database / http / cache / log / tracing） < 环境变量。
*/
func LoadBaseConfig(file *File, defaults BaseConfigDefaults) (BaseConfig, error) {
	if defaults.CacheFetchTimeout <= 0 {
		defaults.CacheFetchTimeout = DefaultCacheFetchTimeout
	}

	cfg := BaseConfig{
		Addr:              defaults.Addr,
		OpsAddr:           defaults.OpsAddr,
		Pool:              pgdb.DefaultPoolConfig(),
		RequestTimeout:    DefaultRequestTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		DrainDelay:        DefaultDrainDelay,
		LogLevel:          slog.LevelInfo,
		LogFormat:         LogFormatText,
		CacheFetchTimeout: defaults.CacheFetchTimeout,
		Tracing:           DefaultTracingConfig(),
	}

	if file != nil {
		if err := applyBaseYAML(&cfg, file); err != nil {
			return BaseConfig{}, err
		}
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return BaseConfig{}, errors.New("DATABASE_URL is required")
	}
	cfg.RedisURL = strings.TrimSpace(os.Getenv("REDIS_URL"))

	if level := strings.TrimSpace(os.Getenv("LOG_LEVEL")); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return BaseConfig{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	if rawFmt := strings.TrimSpace(os.Getenv("LOG_FORMAT")); rawFmt != "" {
		format, err := parseLogFormat("LOG_FORMAT", rawFmt)
		if err != nil {
			return BaseConfig{}, err
		}
		cfg.LogFormat = format
	}
	var err error
	if cfg.DrainDelay, err = EnvDuration("DRAIN_DELAY", cfg.DrainDelay); err != nil {
		return BaseConfig{}, err
	}
	if ep := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); ep != "" {
		cfg.Tracing.Endpoint = ep
	}

	if err := validateBase(cfg); err != nil {
		return BaseConfig{}, err
	}
	return cfg, nil
}

func applyBaseYAML(cfg *BaseConfig, file *File) error {
	var dbSec databaseSection
	if err := DecodeSection(file.Database, &dbSec); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if dbSec.MaxOpenConns != nil {
		cfg.Pool.MaxOpenConns = *dbSec.MaxOpenConns
	}
	if dbSec.MaxIdleConns != nil {
		cfg.Pool.MaxIdleConns = *dbSec.MaxIdleConns
	}
	if dbSec.ConnMaxLifetime != nil {
		cfg.Pool.ConnMaxLifetime = dbSec.ConnMaxLifetime.Std()
	}
	if dbSec.ConnMaxIdleTime != nil {
		cfg.Pool.ConnMaxIdleTime = dbSec.ConnMaxIdleTime.Std()
	}

	var httpSec httpSection
	if err := DecodeSection(file.HTTP, &httpSec); err != nil {
		return fmt.Errorf("http: %w", err)
	}
	if httpSec.RequestTimeout != nil {
		cfg.RequestTimeout = httpSec.RequestTimeout.Std()
	}
	if httpSec.ShutdownTimeout != nil {
		cfg.ShutdownTimeout = httpSec.ShutdownTimeout.Std()
	}
	if httpSec.DrainDelay != nil {
		cfg.DrainDelay = httpSec.DrainDelay.Std()
	}

	var cacheSec cacheSection
	if err := DecodeSection(file.Cache, &cacheSec); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if cacheSec.FetchTimeout != nil {
		cfg.CacheFetchTimeout = cacheSec.FetchTimeout.Std()
	}

	var logSec logSection
	if err := DecodeSection(file.Log, &logSec); err != nil {
		return fmt.Errorf("log: %w", err)
	}
	if level := strings.TrimSpace(logSec.Level); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return fmt.Errorf("log.level: %w", err)
		}
	}
	if rawFmt := strings.TrimSpace(logSec.Format); rawFmt != "" {
		format, err := parseLogFormat("log.format", rawFmt)
		if err != nil {
			return err
		}
		cfg.LogFormat = format
	}

	var trSec tracingSection
	if err := DecodeSection(file.Tracing, &trSec); err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	cfg.Tracing.Endpoint = strings.TrimSpace(trSec.Endpoint)
	if trSec.SampleRate != nil {
		cfg.Tracing.SampleRate = *trSec.SampleRate
	}
	return nil
}

/*
LoadProxyConfig 从 YAML trusted_proxies 与环境变量 TRUSTED_PROXIES 加载可信代理配置（ADR 0035）。
两者均为空时不信任转发头；"*" 启用单跳模式；CIDR 列表启用多跳右向剥离模式。
*/
func LoadProxyConfig(file *File) (ProxyConfig, error) {
	var cfg ProxyConfig
	if file != nil {
		var entries []string
		if err := DecodeSection(file.TrustedProxies, &entries); err != nil {
			return ProxyConfig{}, fmt.Errorf("trusted_proxies: %w", err)
		}
		var err error
		cfg, err = ParseTrustedProxies(entries)
		if err != nil {
			return ProxyConfig{}, fmt.Errorf("trusted_proxies: %w", err)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); raw != "" {
		var entries []string
		for part := range strings.SplitSeq(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				entries = append(entries, part)
			}
		}
		if len(entries) == 0 {
			return ProxyConfig{}, errors.New("TRUSTED_PROXIES must contain * or at least one CIDR")
		}
		parsed, err := ParseTrustedProxies(entries)
		if err != nil {
			return ProxyConfig{}, fmt.Errorf("TRUSTED_PROXIES: %w", err)
		}
		cfg = parsed
	}
	return cfg, nil
}

/*
ParseTrustedProxies 解析统一的可信代理条目切片（ADR 0035）。
空切片返回不信任代理；["*"] 返回单跳模式；CIDR 或单 IP 返回多跳右向剥离模式；禁止混写 "*"。
*/
func ParseTrustedProxies(entries []string) (ProxyConfig, error) {
	if len(entries) == 0 {
		return ProxyConfig{}, nil
	}
	cleaned := make([]string, 0, len(entries))
	hasWildcard := false
	for _, entry := range entries {
		s := strings.TrimSpace(entry)
		if s == "" {
			return ProxyConfig{}, errors.New("entry must not be empty")
		}
		if s == "*" {
			hasWildcard = true
		}
		cleaned = append(cleaned, s)
	}
	if hasWildcard {
		if len(cleaned) > 1 {
			return ProxyConfig{}, errors.New("cannot mix \"*\" with other entries")
		}
		return ProxyConfig{TrustHeaders: true}, nil
	}

	cidrs := make([]netip.Prefix, 0, len(cleaned))
	for _, part := range cleaned {
		var prefix netip.Prefix
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil || !p.IsValid() {
				return ProxyConfig{}, fmt.Errorf("invalid cidr %q", part)
			}
			addr := p.Addr()
			bits := p.Bits()
			if addr.Is4In6() {
				addr = addr.Unmap()
				bits = max(bits-96, 0)
			}
			prefix = netip.PrefixFrom(addr, bits).Masked()
		} else {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return ProxyConfig{}, fmt.Errorf("invalid ip or cidr %q", part)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		cidrs = append(cidrs, prefix)
	}
	return ProxyConfig{TrustHeaders: true, TrustedCIDRs: cidrs}, nil
}

// ValidatePorts 校验出站端口白名单切片（空切片表示交由 safehttp 默认 80/443）。
func ValidatePorts(ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d: must be in 1..65535", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadOutboundAllowedPorts 从 YAML outbound.allowed_ports 加载并校验出站端口白名单。
func LoadOutboundAllowedPorts(file *File) ([]int, error) {
	if file == nil {
		return nil, nil
	}
	var sec outboundSection
	if err := DecodeSection(file.Outbound, &sec); err != nil {
		return nil, fmt.Errorf("outbound: %w", err)
	}
	ports, err := ValidatePorts(sec.AllowedPorts)
	if err != nil {
		return nil, fmt.Errorf("outbound.allowed_ports: %w", err)
	}
	return ports, nil
}

/*
NewLogger 按 LOG_FORMAT 构造日志，并装配统一 context 提取与敏感字段脱敏。调用前格式已经校验过。
text 方便在终端里读；json 给生产采集。这里只认 json，其余按 text，
避免配置加载之外的调用方因为拼写把进程打成没日志。
*/
func (cfg BaseConfig) NewLogger(w io.Writer) *slog.Logger {
	logger, _ := cfg.NewLoggerWithService(w, "")
	return logger
}

// NewLoggerWithService 构造绑定固定 service 标签的根日志器，并返回支持运行时调级的 *slog.LevelVar。
func (cfg BaseConfig) NewLoggerWithService(w io.Writer, service string) (*slog.Logger, *slog.LevelVar) {
	return logx.New(w, logx.Options{
		Format:  cfg.LogFormat,
		Level:   cfg.LogLevel,
		Service: service,
	})
}

func parseLogFormat(field, raw string) (string, error) {
	switch raw {
	case "", LogFormatText:
		return LogFormatText, nil
	case LogFormatJSON:
		return LogFormatJSON, nil
	default:
		return "", fmt.Errorf("%s must be text or json, got %q", field, raw)
	}
}

func validateBase(cfg BaseConfig) error {
	if cfg.Pool.MaxOpenConns <= 0 {
		return errors.New("database.max_open_conns must be positive")
	}
	if cfg.Pool.MaxIdleConns < 0 || cfg.Pool.MaxIdleConns > cfg.Pool.MaxOpenConns {
		return errors.New("database.max_idle_conns must be between 0 and database.max_open_conns")
	}
	if cfg.Pool.ConnMaxLifetime < 0 || cfg.Pool.ConnMaxIdleTime < 0 {
		return errors.New("connection lifetimes must not be negative")
	}
	if cfg.RequestTimeout <= 0 {
		return errors.New("http.request_timeout must be positive")
	}
	if cfg.ShutdownTimeout <= 0 {
		return errors.New("http.shutdown_timeout must be positive")
	}
	if cfg.DrainDelay < 0 {
		return errors.New("drain delay must not be negative")
	}
	if cfg.CacheFetchTimeout <= 0 {
		return errors.New("cache.fetch_timeout must be positive")
	}
	if cfg.CacheFetchTimeout > cfg.RequestTimeout {
		return errors.New("cache.fetch_timeout must not exceed http.request_timeout")
	}
	if cfg.Tracing.Endpoint != "" {
		if err := ValidateIssuerURL("tracing.endpoint", cfg.Tracing.Endpoint); err != nil {
			return err
		}
	}
	if math.IsNaN(cfg.Tracing.SampleRate) || math.IsInf(cfg.Tracing.SampleRate, 0) || cfg.Tracing.SampleRate < 0 || cfg.Tracing.SampleRate > 1 {
		return errors.New("tracing.sample_rate must be between 0 and 1")
	}
	return nil
}

// CountNonEmpty 统计非空字符串个数，用于校验整组凭证同开或同空。
func CountNonEmpty(values ...string) int {
	n := 0
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	return n
}

// ValidateIssuerURL 校验不带 query 和 fragment 的 http/https URL。
func ValidateIssuerURL(field, raw string) error {
	u, err := parseHTTPURL(raw)
	if err != nil || u.RawQuery != "" {
		return fmt.Errorf("%s must be a valid http or https URL without query or fragment, got %q", field, raw)
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
IsLoopbackAddr 报告 host:port 的 host 是不是回环地址。
":6060" 这种省略 host 的写法会监听所有网卡，不算；
localhost 以外的主机名要解析才知道指向哪里，也不算。
*/
func IsLoopbackAddr(addr string) bool {
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

// EnvString 读取字符串环境变量；未设置或为空时返回 fallback。
func EnvString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// EnvBool 读取布尔环境变量。变量未设置时返回 def。
func EnvBool(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}

// EnvDuration 读取 time.ParseDuration 能识别的时长。变量未设置时返回 def。
func EnvDuration(name string, def time.Duration) (time.Duration, error) {
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
