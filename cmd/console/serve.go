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

	// 每个进程各自建 Registry，/metrics 只反映这个进程的指标。
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "shortlink"),
	)

	storeFeatures := cfg.StoreFeatures()

	/*
		配置 REDIS_URL 时创建必须走 cache.Store：成功后写穿正缓存，盖掉 redirector 可能已经写入的负缓存。
		REDIS_URL 为空时跳过缓存层直接经 storemetrics 访问 PostgreSQL（ADR 0029）。
		console 不跳转，不启动 ClickRecorder。
	*/
	pgStore := newConfiguredPostgresStore(db, storeFeatures)
	metricsStore := storemetrics.New(pgStore, registry)
	var store console.Store = metricsStore
	if cfg.RedisURL != "" {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		rdb, err := cache.Open(ctx, cfg.RedisURL)
		cancel()
		if err != nil {
			return fmt.Errorf("connect redis: %w", err)
		}
		defer func() { _ = rdb.Close() }()
		logger.Info("connected to redis")

		store = cache.New(metricsStore, rdb, registry, cache.Options{
			FetchTimeout: cfg.CacheFetchTimeout,
			Logger:       logger,
		})
	} else {
		logger.Info("redis cache disabled: REDIS_URL is not set")
	}

	authMethods, err := initAuthMethods(cfg, logger)
	if err != nil {
		return err
	}
	if err := console.ValidateAuthMethods(authMethods); err != nil {
		return fmt.Errorf("validate auth methods: %w", err)
	}
	var (
		sessions console.SessionStore
		modules  []console.Module
	)
	if cfg.Features.Analytics {
		modules = append(modules, console.NewStatsModule(store))
	}
	/*
		以下均为仅限浏览器会话用户（ActorUser）调用的组织自助与治理模块（ADR 0018/0021/0023/0029）：
		API Key 的 scope 白名单仅含 links/stats，且各 handler 均做 ActorUser 纵深防御。
		未配置任何用户登录方式时无法产生会话，不挂载这些模块以保持最小暴露面。
	*/
	if len(authMethods) > 0 {
		sessions = pgStore
		modules = append(modules,
			console.NewOrgsModule(pgStore, pgStore),
			console.NewMembersModule(pgStore),
			console.NewInvitationsModule(pgStore),
			console.NewAPIKeysModule(pgStore),
			console.NewAuditModule(pgStore),
		)
	}
	webhooksAPIEnabled := false
	if cfg.WebhookSecretKey != "" && sessions != nil {
		key, err := webhook.ParseMasterKeyHex(cfg.WebhookSecretKey)
		if err != nil {
			return fmt.Errorf("parse WEBHOOK_SECRET_KEY: %w", err)
		}
		webhookCipher, err := webhook.NewSecretCipher(key)
		if err != nil {
			return fmt.Errorf("init webhook cipher: %w", err)
		}
		modules = append(modules, console.NewWebhooksModule(pgStore, webhookCipher, safehttp.URLValidationOptions{
			AllowedPorts:     cfg.WebhookAllowedPorts,
			AllowedHostPorts: cliOpts.allowTestHosts,
		}))
		webhooksAPIEnabled = true
		logger.Info("webhooks api enabled")
	} else if cfg.WebhookSecretKey == "" {
		logger.Info("webhooks api disabled: WEBHOOK_SECRET_KEY is not set")
	} else {
		logger.Info("webhooks api disabled: at least one user auth method is required")
	}
	if err := console.ValidateModules(modules); err != nil {
		return fmt.Errorf("validate modules: %w", err)
	}
	if cfg.WebDistDir != "" {
		info, err := os.Stat(cfg.WebDistDir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("WEB_DIST_DIR %q must be an existing directory", cfg.WebDistDir)
		}
		logger.Info("serving web console static files", "dir", cfg.WebDistDir)
	}

	enabledFeatures, disabledFeatures := storeFeatures.consoleEnabledDisabled(webhooksAPIEnabled, cfg.WebDistDir != "")
	enabledAuthMethods := make([]string, 0, len(authMethods))
	for _, m := range authMethods {
		enabledAuthMethods = append(enabledAuthMethods, m.Descriptor().ID)
	}
	logger.Info("console features configured",
		"enabled_features", enabledFeatures,
		"disabled_features", disabledFeatures,
		"enabled_auth_methods", enabledAuthMethods,
	)

	// 鉴权、会话和成员管理都直接用 pgStore，缓存不参与身份与成员校验。
	srv := console.New(logger, store, cfg.RequestTimeout, registry, console.Options{
		Keys:              pgStore,
		Authorizer:        identity.NewMapAuthorizer(),
		Sessions:          sessions,
		AuthMethods:       authMethods,
		Modules:           modules,
		RiskScanEnabled:   cfg.Features.RiskScan,
		EmailEnabled:      cfg.EmailEnabled,
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
initAuthMethods 按配置初始化已启用的登录方式插件（OIDC / GitHub / 未来手机号 OTP 等，ADR 0029）。
*/
func initAuthMethods(cfg ConsoleConfig, logger *slog.Logger) ([]console.AuthMethod, error) {
	var methods []console.AuthMethod
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
			return nil, fmt.Errorf("init oidc provider: %w", err)
		}
		m, err := console.NewOAuthAuthMethod("oidc", provider)
		if err != nil {
			return nil, fmt.Errorf("init oidc auth method: %w", err)
		}
		methods = append(methods, m)
		logger.Info("initialized oidc auth method", "issuer", cfg.OIDC.Issuer)
	}
	if cfg.GitHub.Enabled() {
		provider, err := oauth.NewGitHubProvider(oauth.GitHubConfig{
			ClientID:     cfg.GitHub.ClientID,
			ClientSecret: cfg.GitHub.ClientSecret,
			RedirectURL:  cfg.GitHub.RedirectURL,
		})
		if err != nil {
			return nil, fmt.Errorf("init github provider: %w", err)
		}
		m, err := console.NewOAuthAuthMethod("github", provider)
		if err != nil {
			return nil, fmt.Errorf("init github auth method: %w", err)
		}
		methods = append(methods, m)
		logger.Info("initialized github auth method")
	}
	return methods, nil
}
