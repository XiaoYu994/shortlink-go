/*
Command redirector 是公开短链跳转进程。

只提供 GET /r/{code}、/healthz、/readyz 和 /metrics。读 Redis 缓存，未命中回源 PostgreSQL，
成功的 GET 异步记录点击。设了 PPROF_ADDR 时另起一个回环监听提供 /debug/pprof/。管理 API 和 API Key 鉴权在 cmd/console，进程之间不互相调用。
拆分边界见 ADR 0009。
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
	"shortlink/internal/config"
	"shortlink/internal/httpx"
	"shortlink/internal/postgres"
	"shortlink/internal/redirector"
	"shortlink/internal/storemetrics"
)

func main() {
	// 逻辑都放在 run 里并返回 error，这样 run 里的 defer 才会执行。
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.VisitorSalt == "" {
		return errors.New("VISITOR_SALT is required")
	}
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

	/*
		点击写出直接用 pgStore，不经过缓存，也不算进存储耗时。
		这个 defer 注册在 db.Close 和 rdb.Close 之后，run 返回时会先执行：
		先把缓冲里的点击写完，再关 Redis 和数据库。
		排空共用这一份 SHUTDOWN_TIMEOUT。已经开始的普通批次不跟着取消，
		最多再占一次写入超时。进程在排空结束前被杀掉的话，缓冲区里的点击会丢掉。
	*/
	pgStore := postgres.NewStore(db)
	clicks := click.New(pgStore, logger, registry, click.Options{})
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := clicks.Close(closeCtx); err != nil {
			logger.Error("close click recorder", "err", err)
		}
	}()

	/*
		组合顺序：HTTP -> cache -> storemetrics -> postgres。
		指标装饰器贴着数据库，缓存命中不会计入存储耗时。
	*/
	store := cache.New(storemetrics.New(pgStore, registry), rdb, registry, cache.Options{
		MissConcurrency: cfg.CacheMissConcurrency,
		FetchTimeout:    cfg.CacheFetchTimeout,
		Logger:          logger,
	})
	srv := redirector.New(logger, store, cfg.RequestTimeout, registry, redirector.Options{
		Clicks:            clicks,
		VisitorSalt:       []byte(cfg.VisitorSalt),
		TrustProxyHeaders: cfg.TrustProxyHeaders,
	})
	/*
		跳转进程的 /readyz 不探 PostgreSQL 和 Redis，走到这里（两边都连上了）就算就绪。
		Redis 挂了要降级查库，数据库挂了缓存命中的链接还能跳（ADR 0006）。
		这两个依赖是所有副本共用的，探针一失败就是全部副本同时被摘流量，
		降级会变成全站不可用。依赖故障看指标和告警。
	*/
	srv.SetReadiness()

	if cfg.PprofAddr != "" {
		stopPprof, err := servePprof(cfg.PprofAddr, logger)
		if err != nil {
			return err
		}
		defer stopPprof()
	}

	/*
		ListenAndServe 收到信号后先摘流等待再 Shutdown；返回后 run 退出，
		defer 依次关停 pprof、排空点击缓冲，最后关闭 Redis 和数据库。
	*/
	return httpx.ListenAndServe(srv.Routes(), logger, httpx.ServeConfig{
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

先 Listen 再起 goroutine：端口被占用时启动直接失败，不会留下一个没人发现的坏监听。
block 和 mutex profile 默认关闭，不打开的话对应端点返回的是空数据；
只在开了剖析端口时才采样，平时没有这份开销。

不设 WriteTimeout：CPU profile 和 trace 要采几十秒，响应在采样结束后才写完。
goroutine 的退出点：返回的函数调用 Close，Serve 随之返回 ErrServerClosed。
这里不等在途的采样，进程退出时剖析请求直接断开。
*/
func servePprof(addr string, logger *slog.Logger) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen pprof: %w", err)
	}
	runtime.SetBlockProfileRate(blockProfileRate)
	runtime.SetMutexProfileFraction(mutexProfileFraction)

	srv := &http.Server{
		Handler:           httpx.PprofHandler(),
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
