/*
Package config 从环境变量加载各进程共用的基础配置，并在启动时校验（ADR 0024）。

本包仅依赖标准库、logx 与 pgdb，不引入任何业务域或存储业务实现；
redirector、console serve、console worker 各自的专属配置与加载器位于对应 cmd 包内。
*/
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// DefaultShutdownTimeout 与 .env.example 里的 SHUTDOWN_TIMEOUT 一致。
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

	// LogFormatText / LogFormatJSON 区分本地可读日志与生产结构化采集日志。
	LogFormatText = "text"
	LogFormatJSON = "json"
)

/*
BaseConfigDefaults 指定进程级别的默认监听与缓存参数。
由各 cmd 包传入，避免多个进程在共享 .env 中争夺同一个环境变量。
*/
type BaseConfigDefaults struct {
	Addr              string
	OpsAddr           string
	CacheFetchTimeout time.Duration
}

/*
BaseConfig 是 redirector、console 与 worker 共用的启动配置。
Pool 的默认值来自 pgdb.DefaultPoolConfig，环境变量只覆盖显式设置的项。
*/
type BaseConfig struct {
	Addr            string // 主 HTTP 监听地址（worker 为可选运维端口）
	OpsAddr         string // 可选独立运维端口（/healthz、/readyz、/metrics），为空时不单独开启
	DatabaseURL     string // PostgreSQL 连接串
	RedisURL        string // Redis 连接串，支持密码、TLS（rediss://）和 DB 编号
	Pool            pgdb.PoolConfig
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

	// CacheFetchTimeout 是单次回源或写后复查的超时，独立于请求时限，不能超过 REQUEST_TIMEOUT。
	CacheFetchTimeout time.Duration
}

// LoadBaseConfig 加载并校验各进程共用的基础环境变量。
func LoadBaseConfig(defaults BaseConfigDefaults) (BaseConfig, error) {
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

	if cfg.Pool.MaxOpenConns, err = EnvInt("DB_MAX_OPEN_CONNS", cfg.Pool.MaxOpenConns); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.MaxIdleConns, err = EnvInt("DB_MAX_IDLE_CONNS", cfg.Pool.MaxIdleConns); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.ConnMaxLifetime, err = EnvDuration("DB_CONN_MAX_LIFETIME", cfg.Pool.ConnMaxLifetime); err != nil {
		return BaseConfig{}, err
	}
	if cfg.Pool.ConnMaxIdleTime, err = EnvDuration("DB_CONN_MAX_IDLE_TIME", cfg.Pool.ConnMaxIdleTime); err != nil {
		return BaseConfig{}, err
	}
	if cfg.RequestTimeout, err = EnvDuration("REQUEST_TIMEOUT", cfg.RequestTimeout); err != nil {
		return BaseConfig{}, err
	}
	if cfg.ShutdownTimeout, err = EnvDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return BaseConfig{}, err
	}
	if cfg.DrainDelay, err = EnvDuration("DRAIN_DELAY", cfg.DrainDelay); err != nil {
		return BaseConfig{}, err
	}
	if cfg.CacheFetchTimeout, err = EnvDuration("CACHE_FETCH_TIMEOUT", cfg.CacheFetchTimeout); err != nil {
		return BaseConfig{}, err
	}

	if err := validateBase(cfg); err != nil {
		return BaseConfig{}, err
	}
	return cfg, nil
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

func parseLogFormat(raw string) (string, error) {
	switch raw {
	case "", LogFormatText:
		return LogFormatText, nil
	case LogFormatJSON:
		return LogFormatJSON, nil
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
	if cfg.CacheFetchTimeout <= 0 {
		return errors.New("CACHE_FETCH_TIMEOUT must be positive")
	}
	if cfg.CacheFetchTimeout > cfg.RequestTimeout {
		return errors.New("CACHE_FETCH_TIMEOUT must not exceed REQUEST_TIMEOUT")
	}
	return nil
}

// ParsePorts 解析逗号分隔的 TCP 端口号列表（1..65535）。
func ParsePorts(raw string) ([]int, error) {
	var ports []int
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %q: must be in 1..65535", part)
		}
		ports = append(ports, p)
	}
	if len(ports) == 0 {
		return nil, errors.New("must contain at least one port")
	}
	return ports, nil
}

/*
ParseCIDRs 解析逗号分隔的可信代理网段（CIDR 或单个 IP）。
单个 IP 自动归一化为 /32 或 /128 前缀，IPv4 映射的 IPv6 自动还原为标准 IPv4 前缀。
*/
func ParseCIDRs(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var prefix netip.Prefix
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil || !p.IsValid() {
				return nil, fmt.Errorf("invalid cidr %q", part)
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
				return nil, fmt.Errorf("invalid ip or cidr %q", part)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, prefix)
	}
	if len(out) == 0 {
		return nil, errors.New("must contain at least one CIDR")
	}
	return out, nil
}

// CountNonEmpty 统计非空字符串个数，用于校验整组环境变量同开或同空。
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
func ValidateIssuerURL(envName, raw string) error {
	u, err := parseHTTPURL(raw)
	if err != nil || u.RawQuery != "" {
		return fmt.Errorf("%s must be a valid http or https URL without query or fragment, got %q", envName, raw)
	}
	return nil
}

// ValidateRedirectURL 校验回调 http/https URL（允许 query，禁止 fragment）。
func ValidateRedirectURL(envName, raw string) error {
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

// EnvInt 读取整数环境变量。变量未设置时返回 def。
func EnvInt(name string, def int) (int, error) {
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

// EnvFloat 读取浮点数环境变量。变量未设置时返回 def。
func EnvFloat(name string, def float64) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}
