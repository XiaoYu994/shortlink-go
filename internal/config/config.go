/*
Package config 从环境变量加载进程配置，并在启动时校验。

未设置的可选项使用默认值。缺少 DATABASE_URL、REDIS_URL、数值或时长无法解析、
连接池参数、请求超时、关机超时或回源保护参数不合法时，LoadConfig 返回错误，
由 main 终止启动。
*/
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/postgres"
)

const (
	defaultAddr = ":8080"
	// defaultShutdownTimeout 与 .env.example 里的 SHUTDOWN_TIMEOUT 一致。
	defaultShutdownTimeout = 10 * time.Second
	// defaultRequestTimeout 是单个请求的处理时限。main 让读写超时比它宽松，
	// 保证读完 body，并在超时后还有时间把 504 写回客户端。
	defaultRequestTimeout = 5 * time.Second
	// 回源保护的默认值和 cache 包共用，避免两处漂移。
	defaultCacheMissConcurrency = cache.DefaultMissConcurrency
	defaultCacheFetchTimeout    = cache.DefaultFetchTimeout
)

/*
Config 是进程启动需要的配置。
Pool 的默认值来自 postgres.DefaultPoolConfig，环境变量只覆盖显式设置的项。
*/
type Config struct {
	Addr            string // HTTP 监听地址，例如 :8080
	DatabaseURL     string // PostgreSQL 连接串
	RedisURL        string // Redis 连接串，支持密码、TLS（rediss://）和 DB 编号
	Pool            postgres.PoolConfig
	RequestTimeout  time.Duration // 单个请求的处理时限，到点后 handler 的 context 被取消
	ShutdownTimeout time.Duration // 收到中断信号后，等待正在处理的请求结束的上限
	LogLevel        slog.Level    // 日志级别，默认 info

	// CacheMissConcurrency 是普通缓存未命中的回源并发上限。
	CacheMissConcurrency int
	// CacheFetchTimeout 是单次回源超时，独立于请求时限，不能超过 REQUEST_TIMEOUT。
	CacheFetchTimeout time.Duration
}

/*
LoadConfig 从环境变量读取进程配置。
未设置的可选项使用默认值；DATABASE_URL、REDIS_URL、数值或时长无法解析、
连接池参数、请求超时、关机超时或回源保护参数不合法时返回错误。

REDIS_URL 是完整的连接串：redis://localhost:6379/0，没有密码也可以；
需要 TLS 用 rediss://，需要密码和 DB 编号就写进 URL。
*/
func LoadConfig() (Config, error) {
	cfg := Config{
		Addr:                 defaultAddr,
		Pool:                 postgres.DefaultPoolConfig(),
		RequestTimeout:       defaultRequestTimeout,
		ShutdownTimeout:      defaultShutdownTimeout,
		LogLevel:             slog.LevelInfo,
		CacheMissConcurrency: defaultCacheMissConcurrency,
		CacheFetchTimeout:    defaultCacheFetchTimeout,
	}

	if addr := os.Getenv("ADDR"); addr != "" {
		cfg.Addr = addr
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	cfg.RedisURL = os.Getenv("REDIS_URL")
	if cfg.RedisURL == "" {
		return Config{}, errors.New("REDIS_URL is required")
	}
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}

	var err error
	if cfg.Pool.MaxOpenConns, err = envInt("DB_MAX_OPEN_CONNS", cfg.Pool.MaxOpenConns); err != nil {
		return Config{}, err
	}
	if cfg.Pool.MaxIdleConns, err = envInt("DB_MAX_IDLE_CONNS", cfg.Pool.MaxIdleConns); err != nil {
		return Config{}, err
	}
	if cfg.Pool.ConnMaxLifetime, err = envDuration("DB_CONN_MAX_LIFETIME", cfg.Pool.ConnMaxLifetime); err != nil {
		return Config{}, err
	}
	if cfg.Pool.ConnMaxIdleTime, err = envDuration("DB_CONN_MAX_IDLE_TIME", cfg.Pool.ConnMaxIdleTime); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.CacheMissConcurrency, err = envInt("CACHE_MISS_CONCURRENCY", cfg.CacheMissConcurrency); err != nil {
		return Config{}, err
	}
	if cfg.CacheFetchTimeout, err = envDuration("CACHE_FETCH_TIMEOUT", cfg.CacheFetchTimeout); err != nil {
		return Config{}, err
	}

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg Config) error {
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
