package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/console"
	"shortlink/internal/job"
	"shortlink/internal/safehttp"
	"shortlink/internal/webhook"
)

type webhookWorkerStore interface {
	webhook.DeliveryStore
	CleanupWebhookDeliveries(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

/*
initWebhooksConsoleModule 按配置装配管理进程 Webhook HTTP 模块（ADR 0029、ADR 0030）。
需同时配置 WEBHOOK_SECRET_KEY 与至少一种用户登录方式才会挂载管理 API。
*/
func initWebhooksConsoleModule(
	cfg ConsoleConfig,
	allowTestHosts []string,
	store console.WebhookStore,
	sessionsEnabled bool,
	logger *slog.Logger,
) (console.Module, error) {
	if cfg.WebhookSecretKey == "" {
		if logger != nil {
			logger.Info("webhooks api disabled: WEBHOOK_SECRET_KEY is not set")
		}
		return nil, nil
	}
	if !sessionsEnabled {
		if logger != nil {
			logger.Info("webhooks api disabled: at least one user auth method is required")
		}
		return nil, nil
	}
	cipher, err := newWebhookCipherFromHex(cfg.WebhookSecretKey)
	if err != nil {
		return nil, err
	}
	if logger != nil {
		logger.Info("webhooks api enabled")
	}
	return console.NewWebhooksModule(store, cipher, safehttp.URLValidationOptions{
		AllowedPorts:     cfg.OutboundAllowedPorts,
		AllowedHostPorts: allowTestHosts,
	}), nil
}

func newWebhookCipherFromHex(hexKey string) (*webhook.SecretCipher, error) {
	key, err := webhook.ParseMasterKeyHex(hexKey)
	if err != nil {
		return nil, fmt.Errorf("parse WEBHOOK_SECRET_KEY: %w", err)
	}
	cipher, err := webhook.NewSecretCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init webhook cipher: %w", err)
	}
	return cipher, nil
}

/*
webhookWorkerModule 自包含 Webhook 后台投递处理器与历史投递清理的构造与注册（ADR 0030）。
未配置 WEBHOOK_SECRET_KEY 时仅注册 webhook.deliver 的 disabledSkipHandler。
*/
type webhookWorkerModule struct {
	secretKey              string
	store                  webhookWorkerStore
	timeout                time.Duration
	allowedPorts           []int
	allowTestHosts         []string
	hostRateLimit          int
	retryBaseDelay         time.Duration
	retryMaxDelay          time.Duration
	maxConsecutiveFailures int
	cleanupInterval        time.Duration
	retention              time.Duration
	registry               *prometheus.Registry
	logger                 *slog.Logger
}

func (m webhookWorkerModule) Name() string { return "webhooks" }

func (m webhookWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if m.secretKey == "" {
		if m.logger != nil {
			m.logger.Info("webhooks disabled: WEBHOOK_SECRET_KEY is not set")
		}
		r.HandleDisabled(job.KindWebhookDeliver, m.Name())
		return nil
	}
	if m.store == nil {
		return errors.New("webhook worker store is nil")
	}
	cipher, err := newWebhookCipherFromHex(m.secretKey)
	if err != nil {
		return err
	}
	webhookClient := safehttp.NewClient(safehttp.ClientOptions{
		Timeout:          m.timeout,
		DisableRedirects: true,
		AllowedPorts:     m.allowedPorts,
		AllowedHostPorts: m.allowTestHosts,
	})
	threshold := m.maxConsecutiveFailures
	if threshold == 0 {
		threshold = -1 // 显式配置 0 关闭自动熔断（区分未配置零值默认 10）。
	}
	handler := webhook.NewHandler(webhook.HandlerOptions{
		Store:                  m.store,
		HTTPClient:             webhookClient,
		Limiter:                safehttp.NewHostLimiter(m.hostRateLimit),
		Cipher:                 cipher,
		RetryBaseDelay:         m.retryBaseDelay,
		RetryMaxDelay:          m.retryMaxDelay,
		MaxConsecutiveFailures: threshold,
		Registry:               m.registry,
		Logger:                 m.logger,
	})
	r.Handle(job.KindWebhookDeliver, handler)
	r.Periodic(cleanupBatchTask("webhooks.cleanup_deliveries", m.cleanupInterval, m.retention, m.store.CleanupWebhookDeliveries))
	return nil
}
