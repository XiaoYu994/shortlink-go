/*
serve 是 console 的 HTTP 模式：提供可选的 OIDC / GitHub OAuth 2.0 登录、短链增删改查、统计，
并按 CONSOLE_OPS_ADDR 决定 /healthz、/readyz 和 /metrics 是独立监听还是同端口挂载。
创建、修改、删除后维护 Redis 缓存，公开跳转在 redirector 进程里读同一份缓存。
密钥、租户和成员命令仍然走 admin.go 的路径，不加载 Redis。
*/
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"shortlink/internal/cache"
	"shortlink/internal/console"
	"shortlink/internal/httpx"
	"shortlink/internal/identity"
	"shortlink/internal/identity/oauth"
	"shortlink/internal/postgres"
	"shortlink/internal/safehttp"
	"shortlink/internal/storemetrics"
	"shortlink/internal/webhook"
)

func runServe(cliOpts serveCLIOptions) error {
	cfg, err := loadConsoleConfig()
	if err != nil {
		return err
	}
	logger, _ := cfg.NewLoggerWithService(os.Stdout, "console")

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

	// 每个进程各自建 Registry，/metrics 只反映这个进程的指标。
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	/*
		创建必须走 cache.Store：成功后写穿正缓存，盖掉 redirector 可能已经写入的负缓存。
		否则新短码会在负缓存 TTL 内继续 404。
		console 不跳转，不启动 ClickRecorder。
	*/
	pgStore := postgres.NewStore(db)
	store := cache.New(storemetrics.New(pgStore, registry), rdb, registry, cache.Options{
		MissConcurrency: cfg.CacheMissConcurrency,
		FetchTimeout:    cfg.CacheFetchTimeout,
		Logger:          logger,
	})

	oidcClient, githubClient, err := initAuthProviders(cfg, logger)
	if err != nil {
		return err
	}
	var (
		sessions      console.SessionStore
		members       console.MemberStore
		orgs          console.OrgStore
		invitations   console.InvitationStore
		apiKeys       console.APIKeyStore
		auditLogs     console.AuditLogStore
		webhooks      console.WebhookStore
		webhookCipher *webhook.SecretCipher
	)
	if oidcClient != nil || githubClient != nil {
		sessions = pgStore
		members = pgStore
		orgs = pgStore
		invitations = pgStore
		apiKeys = pgStore
		auditLogs = pgStore
	}
	if cfg.WebhookSecretKey != "" && sessions != nil {
		key, err := webhook.ParseMasterKeyHex(cfg.WebhookSecretKey)
		if err != nil {
			return fmt.Errorf("parse WEBHOOK_SECRET_KEY: %w", err)
		}
		webhookCipher, err = webhook.NewSecretCipher(key)
		if err != nil {
			return fmt.Errorf("init webhook cipher: %w", err)
		}
		webhooks = pgStore
		logger.Info("webhooks api enabled")
	} else if cfg.WebhookSecretKey == "" {
		logger.Info("webhooks api disabled: WEBHOOK_SECRET_KEY is not set")
	} else {
		logger.Info("webhooks api disabled: OIDC or GitHub session auth is required")
	}
	if cfg.WebDistDir != "" {
		info, err := os.Stat(cfg.WebDistDir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("WEB_DIST_DIR %q must be an existing directory", cfg.WebDistDir)
		}
		logger.Info("serving web console static files", "dir", cfg.WebDistDir)
	}

	// 鉴权、会话和成员管理都直接用 pgStore，缓存不参与身份与成员校验。
	srv := console.New(logger, store, cfg.RequestTimeout, registry, console.Options{
		Keys:          pgStore,
		Authorizer:    identity.NewMapAuthorizer(),
		OIDC:          oidcClient,
		GitHub:        githubClient,
		Sessions:      sessions,
		Members:       members,
		Orgs:          orgs,
		Invitations:   invitations,
		APIKeys:       apiKeys,
		AuditLogs:     auditLogs,
		Webhooks:      webhooks,
		WebhookCipher: webhookCipher,
		WebhookURLOpts: safehttp.URLValidationOptions{
			AllowedPorts:     cfg.WebhookAllowedPorts,
			AllowedHostPorts: cliOpts.allowTestHosts,
		},
		WebDistDir:        cfg.WebDistDir,
		PublicBaseURL:     cfg.PublicBaseURL,
		TrustProxyHeaders: cfg.TrustProxyHeaders,
		TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
		IPRateLimit:       cfg.IPRateLimit,
		IPRateBurst:       cfg.IPRateBurst,
	})
	/*
		管理进程的 /readyz 只探 PostgreSQL：每个管理请求都要查库鉴权，库不通就一个也处理不了。
		Redis 不探：写缓存失败只记日志，管理请求照常成功（ADR 0006 的 fail-open）。
	*/
	srv.SetReadiness(httpx.ReadyCheck{Name: "postgres", Ping: db.PingContext})

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

	return httpx.ListenAndServe(handler, logger, httpx.ServeConfig{
		Addr:            cfg.Addr,
		RequestTimeout:  cfg.RequestTimeout,
		DrainDelay:      cfg.DrainDelay,
		ShutdownTimeout: cfg.ShutdownTimeout,
		OnShutdown:      srv.SetDraining,
	})
}

/*
initAuthProviders 按配置初始化可选的 OIDC 与 GitHub 登录提供方。
返回值保持接口类型，只在启用时赋值，避免把带类型的 nil 指针放进 console.Options。
*/
func initAuthProviders(cfg ConsoleConfig, logger *slog.Logger) (console.OIDCClient, console.GitHubClient, error) {
	var (
		oidcClient   console.OIDCClient
		githubClient console.GitHubClient
	)
	if cfg.OIDC.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		provider, err := oauth.NewOIDCProvider(ctx, oauth.OIDCConfig{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			RedirectURL:  cfg.OIDC.RedirectURL,
		})
		cancel()
		if err != nil {
			return nil, nil, fmt.Errorf("init oidc provider: %w", err)
		}
		oidcClient = provider
		logger.Info("initialized oidc provider", "issuer", cfg.OIDC.Issuer)
	}
	if cfg.GitHub.Enabled() {
		provider, err := oauth.NewGitHubProvider(oauth.GitHubConfig{
			ClientID:     cfg.GitHub.ClientID,
			ClientSecret: cfg.GitHub.ClientSecret,
			RedirectURL:  cfg.GitHub.RedirectURL,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("init github provider: %w", err)
		}
		githubClient = provider
		logger.Info("initialized github oauth provider")
	}
	return oidcClient, githubClient, nil
}
