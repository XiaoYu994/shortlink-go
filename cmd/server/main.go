/*
Command server 是短链接服务的入口。

目前创建短链和跳转都在这一个程序里。做到 SaaS 层时，
再按笔记第 1 节拆成 cmd/redirector、cmd/console、cmd/worker。
*/
package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"shortlink/internal/httpapi"
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

	// TODO(第 8 课): 换成 internal/config 里的 LoadConfig，启动时校验必需配置。
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := httpapi.NewServer(logger)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", addr)

	// TODO(第 8 课): 用 signal.NotifyContext 加 httpSrv.Shutdown 实现优雅退出。
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
