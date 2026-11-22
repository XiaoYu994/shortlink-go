/*
serve 是 console 的 HTTP 模式：提供可选的 OIDC 登录、短链增删改查、统计、/healthz、/readyz 和 /metrics。
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
	"shortlink/internal/identity"
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

	/*
		四个 OIDC_* 环境变量全空时不挂载登录路由，只支持 API Key（ADR 0018）。
		变量类型保持为接口，只在启用时赋值，避免把带类型的 nil 指针放进 ConsoleOptions.OIDC。
	*/
	var (
		oidcClient httpapi.OIDCClient
		sessions   httpapi.SessionStore
	)
	if cfg.OIDC.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		provider, err := identity.NewOIDCProvider(ctx, identity.OIDCConfig{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			RedirectURL:  cfg.OIDC.RedirectURL,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("init oidc provider: %w", err)
		}
		oidcClient = provider
		sessions = pgStore
		logger.Info("initialized oidc provider", "issuer", cfg.OIDC.Issuer)
	}

	// 鉴权和会话都直接用 pgStore，缓存不参与身份校验。
	srv := httpapi.NewConsole(logger, store, cfg.RequestTimeout, registry, httpapi.ConsoleOptions{
		Keys:       pgStore,
		Authorizer: identity.NewMapAuthorizer(),
		OIDC:       oidcClient,
		Sessions:   sessions,
	})
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

	/*
		先把 /readyz 打成 503，等 DRAIN_DELAY，再关监听。
		监听还开着，这段时间进来的请求照常处理。
		/healthz 仍然是 200，避免把正在退出的进程当成需要重启。
		第二次信号会立刻退出，不等这段延迟。
	*/
	srv.SetDraining()
	logger.Info("draining", "delay", cfg.DrainDelay)
	if cfg.DrainDelay > 0 {
		time.Sleep(cfg.DrainDelay)
	}

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
