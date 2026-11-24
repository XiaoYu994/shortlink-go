/*
worker 是 console 的后台任务模式（阶段 12、ADR 0020）：
运行 PostgreSQL Outbox Worker Pool，处理异步 URL 风控（link.risk_scan）、
回收过期租约，并定期循环分批清理过期会话、已完成/死信任务与过期/失效邀请。
配置了 WORKER_ADDR 时同时暴露独立的 /healthz、/readyz 和 /metrics 运维端口。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/httpx"
	"shortlink/internal/job"
	"shortlink/internal/postgres"
	"shortlink/internal/risk"
	"shortlink/internal/storemetrics"
)

const (
	cleanupBatchLimit        = 500
	maxCleanupBatchesPerTick = 40
)

type workerCLIOptions struct {
	allowTestHosts []string
}

func parseWorkerArgs(args []string, stderr io.Writer) (workerCLIOptions, bool, error) {
	fs := newFlagSet("worker", stderr)
	var opts workerCLIOptions
	registerWorkerTestFlags(fs, &opts)
	ok, err := parseFlags(fs, args, stderr)
	if !ok {
		return workerCLIOptions{}, false, err
	}
	if err := validateWorkerTestFlags(opts); err != nil {
		return workerCLIOptions{}, false, err
	}
	return opts, true, nil
}

// drainCleanupBatches 在单次周期 tick 内连续分批清理，直到返回不足 batchLimit 行或达到批数上限。
func drainCleanupBatches(ctx context.Context, batchLimit, maxBatches int, fn func(context.Context, int) (int64, error)) (int64, error) {
	var total int64
	for range maxBatches {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := fn(ctx, batchLimit)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(batchLimit) {
			break
		}
	}
	return total, nil
}

func runWorker(cliOpts workerCLIOptions) error {
	cfg, err := config.LoadWorkerConfig()
	if err != nil {
		return err
	}
	logger, _ := cfg.NewLoggerWithService(os.Stdout, "console-worker")

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

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	pgStore := postgres.NewStore(db)
	linkStore := cache.New(storemetrics.New(pgStore, registry), rdb, registry, cache.Options{
		MissConcurrency: cfg.CacheMissConcurrency,
		FetchTimeout:    cfg.CacheFetchTimeout,
		Logger:          logger,
	})

	httpClient := risk.NewSafeHTTPClient(risk.HTTPClientOptions{
		Timeout:          cfg.RiskFetchTimeout,
		AllowedPorts:     cfg.RiskAllowedPorts,
		AllowedHostPorts: cliOpts.allowTestHosts,
	})
	scanner := risk.NewRuleScanner(risk.RuleScannerOptions{
		HTTPClient:      httpClient,
		BlockedHosts:    cfg.RiskBlockedHosts,
		BlockedKeywords: cfg.RiskBlockedKeywords,
	})
	verdictCache := risk.NewVerdictCache(rdb, registry, risk.VerdictCacheOptions{Logger: logger})
	hostLimiter := risk.NewHostLimiter(cfg.RiskHostRateLimit)
	riskHandler := risk.NewHandler(risk.HandlerOptions{
		Scanner: scanner,
		Banner:  linkStore,
		Cache:   verdictCache,
		Limiter: hostLimiter,
		Logger:  logger,
	})

	runner := job.New(pgStore, map[string]job.Handler{
		job.KindLinkRiskScan: riskHandler,
	}, logger, registry, job.Options{
		Concurrency:     cfg.Concurrency,
		MaxPerTenant:    cfg.MaxPerTenant,
		PollInterval:    cfg.PollInterval,
		LeaseDuration:   cfg.LeaseDuration,
		JobTimeout:      cfg.JobTimeout,
		ReapInterval:    cfg.ReapInterval,
		ShutdownTimeout: cfg.ShutdownTimeout,
		PeriodicTasks: []job.PeriodicTask{
			{
				Name:     "sessions.cleanup_expired",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupExpiredSessionsByExpiry(ctx, cfg.SessionRetention, limit)
					})
				},
			},
			{
				Name:     "sessions.cleanup_revoked",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupRevokedSessions(ctx, cfg.SessionRetention, limit)
					})
				},
			},
			{
				Name:     "jobs.cleanup_done",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupDoneJobs(ctx, cfg.DoneJobRetention, limit)
					})
				},
			},
			{
				Name:     "jobs.cleanup_dead",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupDeadJobs(ctx, cfg.DeadJobRetention, limit)
					})
				},
			},
			{
				Name:     "invitations.cleanup",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					n1, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupExpiredInvitationsByExpiry(ctx, cfg.InvitationRetention, limit)
					})
					if err != nil {
						return n1, err
					}
					n2, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupRevokedInvitations(ctx, cfg.InvitationRetention, limit)
					})
					if err != nil {
						return n1 + n2, err
					}
					n3, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
						return pgStore.CleanupSupersededInvitations(ctx, cfg.InvitationRetention, limit)
					})
					return n1 + n2 + n3, err
				},
			},
		},
	})

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 收到第一次信号后立即恢复默认信号处理，支持再按一次信号立即退出（ADR 0020）。
	go func() {
		<-signalCtx.Done()
		stop()
	}()

	var (
		opsErrCh chan error
		opsDone  bool
	)
	if cfg.Addr != "" {
		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return err
		}
		opsSrv := httpx.New(logger, cfg.RequestTimeout, registry)
		opsSrv.SetReadiness(httpx.ReadyCheck{Name: "postgres", Ping: db.PingContext})
		opsErrCh = make(chan error, 1)
		opsDone = true
		go func() {
			opsErrCh <- httpx.Serve(signalCtx, ln, opsSrv.Handler(nil), logger, httpx.ServeConfig{
				Addr:            cfg.Addr,
				RequestTimeout:  cfg.RequestTimeout,
				DrainDelay:      0, // worker 运维端口只给探活和抓取指标，随信号尽快收敛
				ShutdownTimeout: cfg.ShutdownTimeout,
				OnShutdown:      opsSrv.SetDraining,
			})
		}()
	}

	runErr := runner.Run(signalCtx)
	stop()

	var opsErr error
	if opsDone {
		opsErr = <-opsErrCh
	}
	return errors.Join(runErr, opsErr)
}
