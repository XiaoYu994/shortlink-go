/*
Command server 是短链接服务的入口。

目前创建短链和跳转都在这一个程序里。做到 SaaS 层时，
再按笔记第 1 节拆成 cmd/redirector、cmd/console、cmd/worker。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"shortlink/internal/httpapi"
	"shortlink/internal/postgres"
)

func main() {
	// 逻辑都放在 run 里并返回 error，这样 run 里的 defer 才会执行（第 8 课）。
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// TODO(第 8 课): 换成 internal/config 里的 LoadConfig，启动时校验必需配置。
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	pool, err := loadPoolConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := postgres.Open(ctx, dsn, pool)
	cancel()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger.Info("connected to postgres")

	srv := httpapi.NewServer(logger, postgres.NewStore(db))

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", addr)

	// TODO(第 8 课): 用 signal.NotifyContext 加 httpSrv.Shutdown 实现优雅退出。
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadPoolConfig 从环境变量读取连接池参数，未配置时使用默认值。
func loadPoolConfig() (postgres.PoolConfig, error) {
	cfg := postgres.DefaultPoolConfig()
	var err error
	if cfg.MaxOpenConns, err = envInt("DB_MAX_OPEN_CONNS", cfg.MaxOpenConns); err != nil {
		return cfg, err
	}
	if cfg.MaxIdleConns, err = envInt("DB_MAX_IDLE_CONNS", cfg.MaxIdleConns); err != nil {
		return cfg, err
	}
	if cfg.ConnMaxLifetime, err = envDuration("DB_CONN_MAX_LIFETIME", cfg.ConnMaxLifetime); err != nil {
		return cfg, err
	}
	if cfg.ConnMaxIdleTime, err = envDuration("DB_CONN_MAX_IDLE_TIME", cfg.ConnMaxIdleTime); err != nil {
		return cfg, err
	}
	if cfg.MaxOpenConns <= 0 {
		return cfg, errors.New("DB_MAX_OPEN_CONNS must be positive")
	}
	if cfg.MaxIdleConns < 0 || cfg.MaxIdleConns > cfg.MaxOpenConns {
		return cfg, errors.New("DB_MAX_IDLE_CONNS must be between 0 and DB_MAX_OPEN_CONNS")
	}
	if cfg.ConnMaxLifetime < 0 || cfg.ConnMaxIdleTime < 0 {
		return cfg, errors.New("connection lifetimes must not be negative")
	}
	return cfg, nil
}

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
