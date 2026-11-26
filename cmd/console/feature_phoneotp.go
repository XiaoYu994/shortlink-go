package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shortlink/internal/console"
	"shortlink/internal/sms"
)

type phoneOTPCleaner interface {
	CleanupExpiredPhoneOTPChallenges(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

/*
initPhoneOTPAuthMethod 按 SMS_PROVIDER 构造短信发送通道与 PhoneOTPAuthMethod（ADR 0030）。
SMS_PROVIDER 为空时返回 nil，表示关闭手机号验证码登录。
*/
func initPhoneOTPAuthMethod(
	cfg PhoneOTPConfig,
	store console.PhoneOTPStore,
	allowTestHosts []string,
	logger *slog.Logger,
) (console.AuthMethod, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	var (
		sender sms.Sender
		err    error
	)
	switch cfg.Provider {
	case sms.ProviderWebhook:
		sender, err = sms.NewWebhookSender(sms.WebhookConfig{
			URL:              cfg.WebhookURL,
			Token:            cfg.WebhookToken,
			Timeout:          cfg.Timeout,
			AllowedPorts:     cfg.AllowedPorts,
			AllowedHostPorts: allowTestHosts,
		})
		if err != nil {
			return nil, fmt.Errorf("init sms webhook sender: %w", err)
		}
	case sms.ProviderLog:
		if logger != nil {
			logger.Warn("phone otp sms provider is set to log mode; do not use in production")
		}
		sender = sms.NewLogSender(logger)
	default:
		return nil, fmt.Errorf("unsupported SMS_PROVIDER %q", cfg.Provider)
	}

	method, err := console.NewPhoneOTPAuthMethod("phone", console.PhoneOTPOptions{
		Store:         store,
		Sender:        sender,
		TTL:           cfg.TTL,
		Cooldown:      cfg.Cooldown,
		MaxPerHour:    cfg.MaxPerHour,
		MaxAttempts:   cfg.MaxAttempts,
		SecureCookies: cfg.SecureCookies,
	})
	if err != nil {
		return nil, fmt.Errorf("init phone otp auth method: %w", err)
	}
	if logger != nil {
		logger.Info("initialized phone otp auth method", "provider", cfg.Provider)
	}
	return method, nil
}

/*
phoneOTPWorkerModule 负责定期分批清理过期的手机号验证码挑战记录（ADR 0030）。
与 sessions 清理一致，不要求 worker 重复配置 SMS_* 环境变量，也能清理“先开后关”后的存量挑战行。
*/
type phoneOTPWorkerModule struct {
	cleaner         phoneOTPCleaner
	cleanupInterval time.Duration
	retention       time.Duration
}

func (m phoneOTPWorkerModule) Name() string { return "phone_otp" }

func (m phoneOTPWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if m.cleaner == nil {
		return errors.New("phone otp cleaner is nil")
	}
	r.Periodic(cleanupBatchTask("phone_otp.cleanup", m.cleanupInterval, m.retention, m.cleaner.CleanupExpiredPhoneOTPChallenges))
	return nil
}
