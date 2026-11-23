/*
Command console 是管理进程，也是租户、API Key、成员邀请和链接下架的运维入口。

console serve 提供短链增删改查、点击统计、/healthz、/readyz 和 /metrics；
console key issue/list/revoke 签发密钥、列出状态和吊销，明文只写到标准输出，不进日志；
console tenant show/set 查看和调整租户的链接配额与管理 API 限流；
console member invite 预先登记组织成员邀请，用户携带已验证的同一邮箱通过 OIDC 登录时自动兑现；
console link ban/unban 封禁和解封短链，先写 PostgreSQL 再写 Redis。
key、tenant 和 member 命令只连 PostgreSQL；link 命令还需要 Redis。进程拆分见 ADR 0009。
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/cache"
	"shortlink/internal/postgres"
)

func main() {
	args := os.Args[1:]
	/*
		serve 在加载配置前分派：key 和 tenant 命令只需要 DATABASE_URL，
		不能因为 HTTP 模式或 link 命令要连 Redis，就让它们也依赖 Redis。
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
	if len(args) > 0 && args[0] == "worker" {
		opts, ok, err := parseWorkerArgs(args[1:], os.Stderr)
		if !ok {
			if err != nil {
				if !errors.Is(err, errUsage) {
					_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
				}
				os.Exit(2)
			}
			return
		}
		if err := runWorker(opts); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(args, os.Stdout, os.Stderr, openStore, openLinkStore); err != nil {
		if !errors.Is(err, errUsage) {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
		}
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

/*
run 按子命令选择存储。key 和 tenant 只打开 PostgreSQL，link 打开 PostgreSQL 和 Redis；
两条路径各用各的打开函数，测试可以分别断言对方没有被调用。
*/
func run(args []string, stdout, stderr io.Writer,
	open func(context.Context) (adminStore, io.Closer, error),
	openLink func(context.Context) (linkStore, io.Closer, error),
) error {
	if len(args) > 0 && args[0] == "link" {
		return runLink(args[1:], stdout, stderr, openLink)
	}

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

func runLink(args []string, stdout, stderr io.Writer, open func(context.Context) (linkStore, io.Closer, error)) error {
	cmd, err := parseLinkCommand(args, stderr)
	if err != nil || cmd == nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	links, closer, err := open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	return cmd(ctx, links, stdout)
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

/*
openLinkStore 给 link 命令打开 PostgreSQL 和 Redis。
先建 Redis 客户端、再连数据库：REDIS_URL 缺失或格式不对属于配置错误，
要在写库之前报出来。客户端不 Ping，Redis 不可用时封禁照样先落库，
连接失败留到第一次写缓存时暴露，由 cache.Store 返回 cache.ErrNotUpdated。

CLI 进程没有 /metrics，指标注册到一个临时 Registry 即可，也不套 storemetrics。
*/
func openLinkStore(ctx context.Context) (linkStore, io.Closer, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	rdb, err := cache.NewClient(os.Getenv("REDIS_URL"))
	if err != nil {
		return nil, nil, err
	}
	db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
	if err != nil {
		_ = rdb.Close()
		return nil, nil, err
	}
	store := cache.New(postgres.NewStore(db), rdb, prometheus.NewRegistry(), cache.Options{
		// 复查读库失败要留一条日志；标准错误和命令结果分开。
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	return store, multiCloser{db, rdb}, nil
}

// multiCloser 依次关闭一组资源，把错误合并返回。
type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var errs []error
	for _, c := range m {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
