/*
Command redirector 是公开短链跳转进程。

只提供 GET /r/{code}，并按可选 OPS_ADDR 决定 /healthz、/readyz 和 /metrics 是独立监听还是同端口挂载。
读 Redis 缓存，未命中回源 PostgreSQL，成功的 GET 异步记录点击。设了 PPROF_ADDR 时另起一个回环监听提供 /debug/pprof/。
管理 API 和 API Key 鉴权在 cmd/console，本进程完全不引入管理、身份鉴权、风控与任务依赖，见 ADR 0009、ADR 0024。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/cache"
	"shortlink/internal/click"
	"shortlink/internal/httpx"
	"shortlink/internal/logx"
	"shortlink/internal/pgdb"
	"shortlink/internal/pgredirect"
	"shortlink/internal/redirector"
	"shortlink/internal/storemetrics"
)

func main() {
	// 逻辑都放在 run 里并返回 error，这样 run 里的 defer 才会执行。
	if err := run(); err != nil {
		logx.NewFallback(os.Stderr, "redirector").Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadRedirectorConfig()
	if err != nil {
		return err
	}
	logger, levelVar := cfg.NewLoggerWithService(os.Stdout, "redirector")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := pgdb.Open(ctx, cfg.DatabaseURL, cfg.Pool)
	cancel()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger.Info("connected to postgres")

	/*
		自建 Registry：每个进程自己暴露 /metrics，redirector 和 console 各自抓取。
		自建 Registry 默认是空的，Go 运行时、进程和连接池指标要单独注册。
	*/
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	pgStore := pgredirect.NewStore(db)
	var store redirector.LinkStore = storemetrics.NewReader(pgStore, registry)
	if cfg.RedisURL != "" {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		rdb, err := cache.Open(ctx, cfg.RedisURL)
		cancel()
		if err != nil {
			return fmt.Errorf("connect redis: %w", err)
		}
		defer func() { _ = rdb.Close() }()
		logger.Info("connected to redis")

		/*
			组合顺序：HTTP -> cache.Reader -> storemetrics.Reader -> pgredirect.Store（ADR 0024）。
			指标装饰器贴着数据库，缓存命中不会计入存储耗时。
		*/
		store = cache.NewReader(store, rdb, registry, cache.Options{
			MissConcurrency: cfg.CacheMissConcurrency,
			FetchTimeout:    cfg.CacheFetchTimeout,
			Logger:          logger,
		})
	} else {
		logger.Info("redis cache disabled: REDIS_URL is not set")
	}

	/*
		点击写出直接用 pgStore，不经过缓存，也不算进存储耗时。
		这个 defer 注册在 db.Close 和 rdb.Close 之后，run 返回时会先执行：
		先把缓冲里的点击写完，再关 Redis 和数据库。
		FEATURE_ANALYTICS=false 时完全不启动 ClickRecorder，传递未装箱 nil 接口给 redirector（ADR 0029）。
	*/
	var (
		clicks      redirector.ClickRecorder
		visitorSalt []byte
	)
	if cfg.Features.Analytics {
		rec := click.New(pgStore, logger, registry, click.Options{})
		clicks = rec
		visitorSalt = []byte(cfg.VisitorSalt)
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := rec.Close(closeCtx); err != nil {
				logger.Error("close click recorder", "err", err)
			}
		}()
	} else {
		logger.Info("click analytics disabled: FEATURE_ANALYTICS=false")
	}

	var enabledFeatures, disabledFeatures []string
	if cfg.RedisURL != "" {
		enabledFeatures = append(enabledFeatures, "cache")
	} else {
		disabledFeatures = append(disabledFeatures, "cache")
	}
	if cfg.Features.Analytics {
		enabledFeatures = append(enabledFeatures, "analytics")
	} else {
		disabledFeatures = append(disabledFeatures, "analytics")
	}
	logger.Info("redirector features configured",
		"enabled_features", enabledFeatures,
		"disabled_features", disabledFeatures,
	)

	sampleRate := cfg.AccessLogSampleRate
	srv := redirector.New(logger, store, cfg.RequestTimeout, registry, redirector.Options{
		Clicks:                 clicks,
		VisitorSalt:            visitorSalt,
		TrustProxyHeaders:      cfg.TrustProxyHeaders,
		TrustedProxyCIDRs:      cfg.TrustedProxyCIDRs,
		AccessLogSampleRate:    &sampleRate,
		AccessLogSlowThreshold: cfg.AccessLogSlowThreshold,
	})
	/*
		跳转进程的 /readyz 不探 PostgreSQL 和 Redis，走到这里（两边都连上了）就算就绪。
		Redis 挂了要降级查库，数据库挂了缓存命中的链接还能跳（ADR 0006）。
		这两个依赖是所有副本共用的，探针一失败就是全部副本同时被摘流量，
		降级会变成全站不可用。依赖故障看指标和告警。
	*/
	srv.SetReadiness()

	if cfg.PprofAddr != "" {
		stopPprof, err := servePprof(cfg.PprofAddr, logger, levelVar)
		if err != nil {
			return err
		}
		defer stopPprof()
	}

	handler := srv.Routes()
	if cfg.OpsAddr != "" {
		_, stopOps, err := httpx.StartOpsServer(cfg.OpsAddr, srv.OpsHandler(), logger)
		if err != nil {
			return err
		}
		defer func() {
			opsCtx, opsCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer opsCancel()
			if err := stopOps(opsCtx); err != nil {
				logger.Error("shutdown ops server", "err", err)
			}
		}()
		handler = srv.RoutesBusiness()
	}

	/*
		ListenAndServe 收到信号后先摘流等待再 Shutdown；返回后 defer 先关闭独立运维端口、
		再关停 pprof、排空点击缓冲，最后关闭 Redis 和数据库。
	*/
	return httpx.ListenAndServe(handler, logger, httpx.ServeConfig{
		Addr:            cfg.Addr,
		RequestTimeout:  cfg.RequestTimeout,
		DrainDelay:      cfg.DrainDelay,
		ShutdownTimeout: cfg.ShutdownTimeout,
		OnShutdown:      srv.SetDraining,
	})
}

const (
	// 阻塞每累计 100 微秒采一个样本。设成 1 会记下每一次阻塞，开销明显。
	blockProfileRate = 100_000
	// 每 100 次锁竞争采一个样本。
	mutexProfileFraction = 100
)

/*
servePprof 在 addr 上单独起一个只有剖析端点的 HTTP 服务，返回关闭它的函数（ADR 0013）。
addr 已经由 config 校验为回环地址。
*/
func servePprof(addr string, logger *slog.Logger, levelVar *slog.LevelVar) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen pprof: %w", err)
	}
	runtime.SetBlockProfileRate(blockProfileRate)
	runtime.SetMutexProfileFraction(mutexProfileFraction)

	srv := &http.Server{
		Handler:           httpx.DebugHandler(levelVar),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve pprof", "err", err)
		}
	}()
	logger.Info("pprof listening", "addr", addr)
	return func() { _ = srv.Close() }, nil
}
