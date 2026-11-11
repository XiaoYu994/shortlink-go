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
	"time"

	"shortlink/internal/config"
	"shortlink/internal/httpapi"
	"shortlink/internal/postgres"
)

func main() {
	// 逻辑都放在 run 里并返回 error，这样 run 里的 defer 才会执行（第 8 课）。
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.Pool)
	cancel()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger.Info("connected to postgres")

	srv := httpapi.NewServer(logger, postgres.NewStore(db))

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", cfg.Addr)

	// TODO(第 8 课): 用 signal.NotifyContext 加 httpSrv.Shutdown 实现优雅退出。
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
