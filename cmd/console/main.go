/*
Command console 是管理进程，也是租户、API Key、成员邀请和链接下架的运维入口。

console serve 提供短链增删改查、点击统计、/healthz、/readyz 和 /metrics；
console worker 启动异步任务与定时维护引擎；
console config dump 校验并脱敏输出当前生效配置；
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
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/audit"
	"shortlink/internal/cache"
	"shortlink/internal/config"
	"shortlink/internal/logx"
	"shortlink/internal/postgres"
)

func main() {
	globalConfigPath, args, err := stripLeadingConfigFlag(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
		writeUsage(os.Stderr)
		os.Exit(2)
	}

	/*
		serve / worker / config 在打开数据库连接前分派：key 和 tenant 命令只需要 DATABASE_URL，
		不能因为 HTTP 模式或 link 命令要连 Redis，就让它们也依赖 Redis。
	*/
	if len(args) > 0 && args[0] == "serve" {
		serveOpts, ok, err := parseServeArgs(args[1:], os.Stderr)
		if !ok {
			if err != nil {
				if !errors.Is(err, errUsage) {
					_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
				}
				os.Exit(2)
			}
			return
		}
		if serveOpts.configPath == "" {
			serveOpts.configPath = globalConfigPath
		}
		if err := runServe(serveOpts); err != nil {
			logx.NewFallback(os.Stderr, "console").Error("fatal", "err", err)
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
		if opts.configPath == "" {
			opts.configPath = globalConfigPath
		}
		if err := runWorker(opts); err != nil {
			logx.NewFallback(os.Stderr, "console-worker").Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "config" {
		opts, ok, err := parseConfigCommand(args[1:], os.Stderr)
		if !ok {
			if err != nil {
				if !errors.Is(err, errUsage) {
					_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
				}
				os.Exit(2)
			}
			return
		}
		if opts.configPath == "" {
			opts.configPath = globalConfigPath
		}
		if err := runConfigDump(opts, os.Stdout); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
			os.Exit(1)
		}
		return
	}

	open := func(ctx context.Context) (adminStore, io.Closer, error) {
		return openStore(ctx, globalConfigPath)
	}
	openLink := func(ctx context.Context) (linkStore, io.Closer, error) {
		return openLinkStore(ctx, globalConfigPath)
	}
	if err := run(args, os.Stdout, os.Stderr, open, openLink); err != nil {
		if !errors.Is(err, errUsage) {
			_, _ = fmt.Fprintf(os.Stderr, "console: %v\n", err)
		}
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func stripLeadingConfigFlag(args []string) (string, []string, error) {
	var configPath string
	for len(args) > 0 {
		switch {
		case args[0] == "--config" || args[0] == "-config":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", nil, fmt.Errorf("%w: --config requires a file path", errUsage)
			}
			configPath = strings.TrimSpace(args[1])
			args = args[2:]
		case strings.HasPrefix(args[0], "--config=") || strings.HasPrefix(args[0], "-config="):
			_, val, _ := strings.Cut(args[0], "=")
			val = strings.TrimSpace(val)
			if val == "" {
				return "", nil, fmt.Errorf("%w: --config requires a file path", errUsage)
			}
			configPath = val
			args = args[1:]
		default:
			return configPath, args, nil
		}
	}
	return configPath, args, nil
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
	ctx = audit.WithMeta(ctx, audit.OperatorMeta(operatorActorRef()))
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
	ctx = audit.WithMeta(ctx, audit.OperatorMeta(operatorActorRef()))
	links, closer, err := open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	return cmd(ctx, links, stdout)
}

func openStore(ctx context.Context, configPath string) (adminStore, io.Closer, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	features, err := loadAdminCLIStoreFeatures(configPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
	if err != nil {
		return nil, nil, err
	}
	return newConfiguredPostgresStore(db, features), db, nil
}

/*
openLinkStore 给 link 命令打开 PostgreSQL 与可选 Redis（ADR 0029）。
配置 REDIS_URL 时先建 Redis 客户端、再连数据库：格式错误要在写库之前报出来；
客户端不 Ping，Redis 不可用时封禁照样先落库，连接失败留到第一次写缓存时由 cache.Store 报告 cache.ErrNotUpdated。
REDIS_URL 留空时跳过缓存层，直接写库返回。
*/
func openLinkStore(ctx context.Context, configPath string) (linkStore, io.Closer, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	if _, err := config.LoadFile(config.ResolvePath(configPath)); err != nil {
		return nil, nil, err
	}
	features, err := loadLinkCLIStoreFeatures()
	if err != nil {
		return nil, nil, err
	}
	redisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if redisURL == "" {
		db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
		if err != nil {
			return nil, nil, err
		}
		return newConfiguredPostgresStore(db, features), db, nil
	}
	rdb, err := cache.NewClient(redisURL)
	if err != nil {
		return nil, nil, err
	}
	db, err := postgres.Open(ctx, dsn, postgres.DefaultPoolConfig())
	if err != nil {
		_ = rdb.Close()
		return nil, nil, err
	}
	store := cache.New(newConfiguredPostgresStore(db, features), rdb, prometheus.NewRegistry(), cache.Options{
		Logger: logx.NewFallback(os.Stderr, "console-cli"),
	})
	return store, multiCloser{db, rdb}, nil
}

// operatorActorRef 生成 CLI 运维操作的执行者标识，便于审计追溯是哪位运维在哪个节点执行。
func operatorActorRef() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return "cli:" + user + "@" + host
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
