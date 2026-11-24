/*
worker 是 console 的后台任务模式（阶段 12、ADR 0020）：
运行 PostgreSQL Outbox Worker Pool，处理异步 URL 风控（link.risk_scan）、
回收过期租约，并定期分批清理过期会话与已完成任务。
配置了 WORKER_ADDR 时同时暴露独立的 /healthz、/readyz 和 /metrics 运维端口。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
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

const cleanupBatchLimit = 500

type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return fmt.Sprint([]string(*f)) }

func (f *stringSliceFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

type workerCLIOptions struct {
	allowTestHosts []string
}

func parseWorkerArgs(args []string, stderr io.Writer) (workerCLIOptions, bool, error) {
	fs := newFlagSet("worker", stderr)
	var allowHosts stringSliceFlag
	fs.Var(&allowHosts, "allow-test-host", "仅测试/冒烟使用：放行指定的回环 host:port 供本地风控联调")
	ok, err := parseFlags(fs, args, stderr)
	if !ok {
		return workerCLIOptions{}, false, err
	}
	for _, hp := range allowHosts {
		if err := validateLoopbackHostPort(hp); err != nil {
			return workerCLIOptions{}, false, fmt.Errorf("--allow-test-host %q: %w", hp, err)
		}
	}
	return workerCLIOptions{allowTestHosts: allowHosts}, true, nil
}

func validateLoopbackHostPort(hp string) error {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return errors.New("must be a valid host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be in 1..65535")
	}
	if host == "localhost" {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return errors.New("must be a loopback address such as 127.0.0.1:18089")
	}
	return nil
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
		AllowedHostPorts: cliOpts.allowTestHosts,
	})
	scanner := risk.NewRuleScanner(risk.RuleScannerOptions{
		HTTPClient:      httpClient,
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
				Name:     "sessions.cleanup",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return pgStore.CleanupExpiredSessions(ctx, cfg.SessionRetention, cleanupBatchLimit)
				},
			},
			{
				Name:     "jobs.cleanup_done",
				Interval: cfg.CleanupInterval,
				Run: func(ctx context.Context) (int64, error) {
					return pgStore.CleanupDoneJobs(ctx, cfg.DoneJobRetention, cleanupBatchLimit)
				},
			},
		},
	})

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
