/*
worker 是 console 的后台任务模式（阶段 12、ADR 0020、ADR 0030、ADR 0031）：
作为纯组合根按垂直特性装配各 WorkerModule（核心清理、手机号 OTP 清理、异步风控、Webhook、
异步邀请邮件与点击统计维护），并按 WORKER_ADDR 可选暴露运维端口。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/logx"
	"shortlink/internal/postgres"
	"shortlink/internal/risk"
	"shortlink/internal/storemetrics"
	"shortlink/internal/tracex"
)

const (
	cleanupBatchLimit          = 500
	maxCleanupBatchesPerTick   = 40
	purgeStatsBatchLimit       = 500
	maxPurgeStatsBatchesPerRun = 20
	purgeStatsDeferDelay       = 100 * time.Millisecond
	partitionMonthsAhead       = 2
	workerStartupTimeout       = 10 * time.Second
)

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
	cfg, err := loadWorkerConfig(cliOpts.configPath)
	if err != nil {
		return err
	}
	logger, _ := cfg.NewLoggerWithService(os.Stdout, "console-worker")

	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownTracing, tracingEnabled, err := tracex.Init(initCtx, tracex.Options{
		ServiceName: "console-worker",
		Endpoint:    cfg.Tracing.Endpoint,
		SampleRate:  cfg.Tracing.SampleRate,
		Logger:      logger,
	})
	initCancel()
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		traceCtx, traceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer traceCancel()
		if err := shutdownTracing(traceCtx); err != nil {
			logger.Error("shutdown tracing", "err", err)
		}
	}()
	if tracingEnabled {
		logger.Info("tracing enabled", "endpoint", logx.RedactURL(cfg.Tracing.Endpoint), "sample_rate", cfg.Tracing.SampleRate)
	} else {
		logger.Info("tracing disabled: tracing.endpoint / OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.Pool)
	cancel()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger.Info("connected to postgres")

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	storeFeatures := cfg.StoreFeatures()
	enabledFeatures, disabledFeatures := storeFeatures.enabledDisabled(cfg.RedisURL != "")
	logger.Info("worker features configured", "enabled_features", enabledFeatures, "disabled_features", disabledFeatures)

	pgStore := newConfiguredPostgresStore(db, storeFeatures)
	metricsStore := storemetrics.New(pgStore, registry)
	var banner risk.LinkBanner = metricsStore
	cachedStore, rdb, closeRedis, err := openRedisCache(cfg.RedisURL, metricsStore, registry, cfg.CacheFetchTimeout, logger)
	if err != nil {
		return err
	}
	defer closeRedis()
	if cachedStore != nil {
		banner = cachedStore
	}

	assembled, err := assembleWorkerModules(
		logger,
		coreWorkerModule{
			store:               pgStore,
			cleanupInterval:     cfg.CleanupInterval,
			sessionRetention:    cfg.RetentionShort,
			doneJobRetention:    cfg.RetentionShort,
			deadJobRetention:    cfg.RetentionLong,
			invitationRetention: cfg.RetentionLong,
			auditRetentionDays:  cfg.AuditRetentionDays,
		},
		phoneOTPWorkerModule{
			cleaner:         pgStore,
			cleanupInterval: cfg.CleanupInterval,
			retention:       identity.DefaultOTPRetention,
		},
		riskWorkerModule{
			enabled:         cfg.Features.RiskScan,
			banner:          banner,
			redisClient:     rdb,
			registry:        registry,
			fetchTimeout:    cfg.RiskFetchTimeout,
			allowedPorts:    cfg.OutboundAllowedPorts,
			allowTestHosts:  cliOpts.allowTestHosts,
			allowedHosts:    cfg.RiskAllowedHosts,
			blockedHosts:    cfg.RiskBlockedHosts,
			blockedKeywords: cfg.RiskBlockedKeywords,
			hostRateLimit:   cfg.RiskHostRateLimit,
			gsb:             cfg.RiskGSB,
			llm:             cfg.RiskLLM,
			logger:          logger,
		},
		webhookWorkerModule{
			secretKey:              cfg.WebhookSecretKey,
			store:                  pgStore,
			timeout:                cfg.WebhookTimeout,
			allowedPorts:           cfg.OutboundAllowedPorts,
			allowTestHosts:         cliOpts.allowTestHosts,
			hostRateLimit:          cfg.WebhookHostRateLimit,
			retryBaseDelay:         cfg.WebhookRetryBaseDelay,
			retryMaxDelay:          cfg.WebhookRetryMaxDelay,
			maxConsecutiveFailures: cfg.WebhookMaxConsecutiveFailures,
			cleanupInterval:        cfg.CleanupInterval,
			retention:              cfg.RetentionShort,
			registry:               registry,
			logger:                 logger,
		},
		emailWorkerModule{
			smtp:             cfg.SMTP,
			store:            pgStore,
			consolePublicURL: cfg.ConsolePublicURL,
			logger:           logger,
		},
		analyticsWorkerModule{
			enabled:         cfg.Features.Analytics,
			store:           pgStore,
			cleanupInterval: cfg.CleanupInterval,
			retentionDays:   cfg.StatsRetentionDays,
		},
	)
	if err != nil {
		return fmt.Errorf("assemble worker modules: %w", err)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), workerStartupTimeout)
	startupErr := assembled.runStartup(startupCtx)
	cancelStartup()
	if startupErr != nil {
		return fmt.Errorf("run worker startup: %w", startupErr)
	}

	runner := job.New(pgStore, assembled.handlers, logger, registry, job.Options{
		Concurrency:     cfg.Concurrency,
		MaxPerTenant:    cfg.MaxPerTenant,
		PollInterval:    cfg.PollInterval,
		LeaseDuration:   cfg.LeaseDuration,
		JobTimeout:      cfg.JobTimeout,
		ReapInterval:    cfg.ReapInterval,
		ShutdownTimeout: cfg.ShutdownTimeout,
		PeriodicTasks:   assembled.periodic,
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
