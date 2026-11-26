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
	"shortlink/internal/httpx"
	"shortlink/internal/job"
	"shortlink/internal/mailer"
	"shortlink/internal/postgres"
	"shortlink/internal/risk"
	"shortlink/internal/safehttp"
	"shortlink/internal/storemetrics"
	"shortlink/internal/webhook"
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
	cfg, err := loadWorkerConfig()
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

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	storeFeatures := cfg.StoreFeatures()
	enabledFeatures, disabledFeatures := storeFeatures.enabledDisabled()
	logger.Info("worker features configured", "enabled_features", enabledFeatures, "disabled_features", disabledFeatures)

	pgStore := newConfiguredPostgresStore(db, storeFeatures)

	metricsStore := storemetrics.New(pgStore, registry)
	var (
		banner       risk.LinkBanner = metricsStore
		reconciler   cacheReconciler
		verdictCache *risk.VerdictCache
	)
	if cfg.RedisURL != "" {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		rdb, err := cache.Open(ctx, cfg.RedisURL)
		cancel()
		if err != nil {
			return fmt.Errorf("connect redis: %w", err)
		}
		defer func() { _ = rdb.Close() }()
		logger.Info("connected to redis")

		cachedStore := cache.New(metricsStore, rdb, registry, cache.Options{
			FetchTimeout: cfg.CacheFetchTimeout,
			Logger:       logger,
		})
		banner = cachedStore
		reconciler = cachedStore
		if cfg.Features.RiskScan {
			verdictCache = risk.NewVerdictCache(rdb, registry, risk.VerdictCacheOptions{Logger: logger})
		}
	} else {
		logger.Info("redis cache disabled: REDIS_URL is not set")
	}

	var riskHandler job.Handler
	if cfg.Features.RiskScan {
		httpClient := risk.NewSafeHTTPClient(safehttp.ClientOptions{
			Timeout:          cfg.RiskFetchTimeout,
			AllowedPorts:     cfg.RiskAllowedPorts,
			AllowedHostPorts: cliOpts.allowTestHosts,
		})
		scanner := risk.NewRuleScanner(risk.RuleScannerOptions{
			HTTPClient:      httpClient,
			BlockedHosts:    cfg.RiskBlockedHosts,
			BlockedKeywords: cfg.RiskBlockedKeywords,
		})
		hostLimiter := safehttp.NewHostLimiter(cfg.RiskHostRateLimit)
		riskHandler = risk.NewHandler(risk.HandlerOptions{
			Scanner: scanner,
			Banner:  banner,
			Cache:   verdictCache,
			Limiter: hostLimiter,
			Logger:  logger,
		})
	} else {
		logger.Info("risk scan worker disabled: FEATURE_RISK_SCAN=false")
	}

	var webhookHandler job.Handler
	if cfg.WebhookSecretKey != "" {
		key, err := webhook.ParseMasterKeyHex(cfg.WebhookSecretKey)
		if err != nil {
			return fmt.Errorf("parse WEBHOOK_SECRET_KEY: %w", err)
		}
		webhookCipher, err := webhook.NewSecretCipher(key)
		if err != nil {
			return fmt.Errorf("init webhook cipher: %w", err)
		}
		webhookClient := safehttp.NewClient(safehttp.ClientOptions{
			Timeout:          cfg.WebhookTimeout,
			DisableRedirects: true,
			AllowedPorts:     cfg.WebhookAllowedPorts,
			AllowedHostPorts: cliOpts.allowTestHosts,
		})
		webhookThreshold := cfg.WebhookMaxConsecutiveFailures
		if webhookThreshold == 0 {
			webhookThreshold = -1 // 显式配置 0 关闭自动熔断（区分未配置零值默认 10）。
		}
		webhookHandler = webhook.NewHandler(webhook.HandlerOptions{
			Store:                  pgStore,
			HTTPClient:             webhookClient,
			Limiter:                safehttp.NewHostLimiter(cfg.WebhookHostRateLimit),
			Cipher:                 webhookCipher,
			RetryBaseDelay:         cfg.WebhookRetryBaseDelay,
			RetryMaxDelay:          cfg.WebhookRetryMaxDelay,
			MaxConsecutiveFailures: webhookThreshold,
			Registry:               registry,
			Logger:                 logger,
		})
	} else {
		logger.Info("webhooks disabled: WEBHOOK_SECRET_KEY is not set")
	}

	var invitationEmailHandler job.Handler
	if cfg.SMTP.Enabled() {
		smtpSender, err := mailer.NewSMTPSender(cfg.SMTP)
		if err != nil {
			return fmt.Errorf("init smtp sender: %w", err)
		}
		invitationEmailHandler = newInvitationEmailHandler(pgStore, smtpSender, cfg.ConsolePublicURL, logger)
		logger.Info("invitation email sender enabled", "host", cfg.SMTP.Host, "port", cfg.SMTP.Port, "tls_mode", cfg.SMTP.TLSMode)
	} else {
		logger.Info("invitation email sender disabled: SMTP_HOST is not set")
	}

	assembled, err := assembleWorkerModules(
		logger,
		registry,
		coreWorkerModule{
			store:               pgStore,
			cleanupInterval:     cfg.CleanupInterval,
			sessionRetention:    cfg.SessionRetention,
			doneJobRetention:    cfg.DoneJobRetention,
			deadJobRetention:    cfg.DeadJobRetention,
			invitationRetention: cfg.InvitationRetention,
			auditRetentionDays:  cfg.AuditRetentionDays,
		},
		riskWorkerModule{enabled: cfg.Features.RiskScan, handler: riskHandler},
		webhookWorkerModule{
			enabled:         cfg.WebhookSecretKey != "",
			handler:         webhookHandler,
			cleaner:         pgStore,
			cleanupInterval: cfg.CleanupInterval,
			retention:       cfg.WebhookDeliveryRetention,
		},
		emailWorkerModule{enabled: cfg.SMTP.Enabled(), handler: invitationEmailHandler},
		cacheWorkerModule{reconciler: reconciler},
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
