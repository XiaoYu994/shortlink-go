package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	// readWriteMargin 加在 RequestTimeout 之外：留出读请求体与把 504 写回客户端的时间。
	readWriteMargin = 5 * time.Second
)

/*
ServeConfig 控制 HTTP 服务的监听地址、超时和优雅关停行为。
OnShutdown 在收到退出信号后、等待 DrainDelay 之前调用（通常传 srv.SetDraining）。
*/
type ServeConfig struct {
	Addr            string
	RequestTimeout  time.Duration
	DrainDelay      time.Duration
	ShutdownTimeout time.Duration
	OnShutdown      func()
}

/*
ListenAndServe 监听 cfg.Addr 并运行 HTTP 服务，直到监听出错或收到 SIGINT/SIGTERM。

先 Listen 再启动服务：端口冲突在启动阶段立刻返回，不打印误导性的 listening 日志。
收到第一次信号后立即恢复默认信号处理，第二次信号会直接打断摘流等待和 Shutdown。
*/
func ListenAndServe(handler http.Handler, logger *slog.Logger, cfg ServeConfig) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(signalCtx, stop, ln, handler, logger, cfg)
}

/*
Serve 在已打开的监听器 ln 上运行 HTTP 服务，由 ctx 取消触发摘流与优雅关停。
供测试直接注入本地回环监听器和可取消的 context，不依赖进程信号。
*/
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, logger *slog.Logger, cfg ServeConfig) error {
	return serve(ctx, nil, ln, handler, logger, cfg)
}

func serve(
	ctx context.Context,
	stopSignal func(),
	ln net.Listener,
	handler http.Handler,
	logger *slog.Logger,
	cfg ServeConfig,
) error {
	readWriteTimeout := cfg.RequestTimeout + readWriteMargin
	httpSrv := &http.Server{
		Handler:           handler,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       readWriteTimeout,
		WriteTimeout:      readWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}

	addr := cfg.Addr
	if addr == "" {
		addr = ln.Addr().String()
	}
	logger.Info("listening", "addr", addr)

	// Serve 在 Shutdown 关掉监听后返回 ErrServerClosed，这条 goroutine 随之退出。
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down", "cause", context.Cause(ctx))
	}

	if stopSignal != nil {
		stopSignal()
	}

	/*
		先触发 OnShutdown（把 /readyz 打成 503），等 DrainDelay，再关监听。
		监听还开着，这段时间进来的请求照常处理；/healthz 保持 200，避免编排器重启正在退出的进程。
	*/
	if cfg.OnShutdown != nil {
		cfg.OnShutdown()
	}
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
