/*
Command console 是管理进程，也是租户和 API Key 的运维入口。

console serve 提供 POST /api/v1/links、/healthz 和 /metrics；
console key issue/list/revoke 签发密钥、列出状态和吊销，明文只写到标准输出，不进日志。
密钥命令只连 PostgreSQL，不加载 Redis。进程拆分见 ADR 0009。
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
	args := os.Args[1:]
	/*
		serve 在加载配置前分派：密钥命令只需要 DATABASE_URL，
		不能因为 HTTP 模式要连 Redis，就让 key issue/list/revoke 也依赖 Redis。
	*/
	if len(args) > 0 && args[0] == "serve" {
		// 和 console help / key issue -h 一致：help 是成功路径。
		if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
			writeUsage(os.Stderr)
			return
		}
		if len(args) > 1 {
			writeUsage(os.Stderr)
			os.Exit(2)
		}
		if err := runServe(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(args, os.Stdout, os.Stderr, openStore); err != nil {
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
