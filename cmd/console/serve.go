/*
serve 是 console 的 HTTP 模式：提供短链增删改查、统计、/healthz、/readyz 和 /metrics。
创建、修改、删除后维护 Redis 缓存，公开跳转在 redirector 进程里读同一份缓存。
密钥命令仍然走 admin.go 的路径，不加载 Redis。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/httpapi"
	"shortlink/internal/postgres"
	"shortlink/internal/storemetrics"
)

func runServe() error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	cfg.Addr = config.ConsoleAddr()
	logger := cfg.NewLogger(os.Stdout)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.Pool)
	cancel()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger.Info("connected to postgres")

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	rdb, err := cache.Open(ctx, cfg.RedisURL)
	cancel()
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() { _ = rdb.Close() }()
	logger.Info("connected to redis")

	// 每个进程各自建 Registry，/metrics 只反映这个进程的指标。
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	/*
		创建必须走 cache.Store：成功后写穿正缓存，盖掉 redirector 可能已经写入的负缓存。
		否则新短码会在负缓存 TTL 内继续 404。
		console 不跳转，不启动 ClickRecorder。
	*/
	pgStore := postgres.NewStore(db)
	store := cache.New(storemetrics.New(pgStore, registry), rdb, registry, cache.Options{
		MissConcurrency: cfg.CacheMissConcurrency,
		FetchTimeout:    cfg.CacheFetchTimeout,
		Logger:          logger,
	})
	// 鉴权直接用 pgStore，缓存不参与 API Key 校验。
	srv := httpapi.NewConsole(logger, store, cfg.RequestTimeout, registry, pgStore)
	/*
		管理进程的 /readyz 只探 PostgreSQL：每个管理请求都要查库鉴权，库不通就一个也处理不了。
		Redis 不探：写缓存失败只记日志，管理请求照常成功（ADR 0006 的 fail-open）。
	*/
	srv.SetReadiness(httpapi.ReadyCheck{Name: "postgres", Ping: db.PingContext})

	readWriteTimeout := cfg.RequestTimeout + 5*time.Second
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.ConsoleRoutes(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readWriteTimeout,
		WriteTimeout:      readWriteTimeout,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", cfg.Addr)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-signalCtx.Done():
		logger.Info("shutting down", "cause", context.Cause(signalCtx))
	}

	stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
