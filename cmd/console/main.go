/*
Command console 是租户和 API Key 的运维入口。

HTTP 管理接口仍在 cmd/server。这里只签发密钥、列出状态和吊销。
明文只写到标准输出，不进日志。进程拆分见 ADR 0009。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"shortlink/internal/postgres"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, openStore); err != nil {
		if !errors.Is(err, errUsage) {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
		}
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, open func(context.Context) (adminStore, io.Closer, error)) error {
	cmd, err := parseCommand(args, stderr)
	if err != nil || cmd == nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, closer, err := open(ctx)
	if err != nil {
		return err
	}
	// 密钥已经提交后再关闭连接。关闭失败不能让调用方以为签发没有成功。
	defer func() { _ = closer.Close() }()
	return cmd(ctx, store, stdout)
}

func openStore(ctx context.Context) (adminStore, io.Closer, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
	if err != nil {
		return nil, nil, err
	}
	return postgres.NewStore(db), db, nil
}
