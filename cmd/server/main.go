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

func main() {
	// 逻辑都放在 run 里并返回 error，这样 run 里的 defer 才会执行（第 8 课）。
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
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

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
		自建 Registry：测试会构造多个 Server，共用 DefaultRegisterer 时同名指标重复注册会 panic。
		自建 Registry 默认是空的，Go 运行时指标和连接池指标要单独注册。
	*/
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	// 组合顺序：HTTP -> cache -> storemetrics -> postgres。
	// 指标装饰器贴着数据库，缓存命中不会计入存储耗时。
	store := cache.New(storemetrics.New(postgres.NewStore(db), registry), rdb)
	srv := httpapi.NewServer(logger, store, cfg.RequestTimeout, registry)

	/*
		读写超时都比 REQUEST_TIMEOUT 多 5 秒，两边跟着请求时限一起变。
		ReadTimeout 从开始读请求算起，创建接口的 body 也算在里面；写死成更短的值时，
		把请求时限调大后，body 会先被掐断。
		WriteTimeout 从读完请求头算起，多出来的时间用来把 504 写回客户端。
	*/
	readWriteTimeout := cfg.RequestTimeout + 5*time.Second
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readWriteTimeout,
		WriteTimeout:      readWriteTimeout,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", cfg.Addr)

	/*
		收到 SIGINT（Ctrl+C）或 SIGTERM（容器停止）后停止接受新连接，
		并等待正在处理的请求结束，上限是 SHUTDOWN_TIMEOUT。
		正在处理的请求有自己的 REQUEST_TIMEOUT 兜底，慢查询不会把退出拖满。
		收到信号后马上 stop，第二次信号恢复默认行为，进程会立刻退出。
		Shutdown 使用新的超时 context：信号 context 此时已经取消。
		数据库在 Shutdown 返回、run 退出时由上面的 defer 关闭。
	*/
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ListenAndServe 在 Shutdown 关掉监听后返回 ErrServerClosed，这条 goroutine 随之退出。
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
