package main

import (
	"database/sql"
	"errors"
	"os"
	"strings"

	"shortlink/internal/postgres"
	"shortlink/internal/webhook"
)

/*
StoreFeatures 汇总 console serve、console worker 与运维 CLI 共用的存储级外围特性开关（ADR 0029、ADR 0031）。
三类入口共用 newConfiguredPostgresStore 构造 postgres.Store，确保无论通过 HTTP、后台风控
还是 CLI（link ban/unban、member invite）写库，挂载的事务钩子完全一致。
*/
type StoreFeatures struct {
	Analytics bool // FEATURE_ANALYTICS
	RiskScan  bool // FEATURE_RISK_SCAN
	Webhooks  bool // WEBHOOK_SECRET_KEY 非空
	Email     bool // SMTP_HOST 非空
}

type featureStatus struct {
	name string
	on   bool
}

func partitionFeatures(items []featureStatus) (enabled, disabled []string) {
	for _, item := range items {
		if item.on {
			enabled = append(enabled, item.name)
		} else {
			disabled = append(disabled, item.name)
		}
	}
	return enabled, disabled
}

func (f StoreFeatures) enabledDisabled(cacheEnabled bool) (enabled, disabled []string) {
	return partitionFeatures([]featureStatus{
		{"cache", cacheEnabled},
		{"analytics", f.Analytics},
		{"risk_scan", f.RiskScan},
		{"webhooks", f.Webhooks},
		{"email", f.Email},
	})
}

func (f StoreFeatures) consoleEnabledDisabled(cacheEnabled, webEnabled bool) (enabled, disabled []string) {
	return partitionFeatures([]featureStatus{
		{"cache", cacheEnabled},
		{"analytics", f.Analytics},
		{"risk_scan", f.RiskScan},
		{"webhooks", f.Webhooks},
		{"email", f.Email},
		{"web", webEnabled},
	})
}

/*
loadEmailEnabledFromEnv 与 worker 保持一致的 SMTP 启用口径：
SMTP_HOST 为空表示关闭异步发信；非空时要求 SMTP_FROM 同时非空，避免 serve 入队而 worker 启动失败。
*/
func loadEmailEnabledFromEnv() (bool, error) {
	if strings.TrimSpace(os.Getenv("SMTP_HOST")) == "" {
		return false, nil
	}
	if strings.TrimSpace(os.Getenv("SMTP_FROM")) == "" {
		return false, errors.New("SMTP_FROM is required when SMTP_HOST is set")
	}
	return true, nil
}

// loadAdminCLIStoreFeatures 只加载 key/tenant/member 运维命令实际可能触发的钩子（仅 member invite 涉及 Email），
// 避免无关的 WEBHOOK_SECRET_KEY 或 REDIS_URL 影响 key/tenant 命令执行。
func loadAdminCLIStoreFeatures() (StoreFeatures, error) {
	emailEnabled, err := loadEmailEnabledFromEnv()
	if err != nil {
		return StoreFeatures{}, err
	}
	return StoreFeatures{
		Email: emailEnabled,
	}, nil
}

// loadLinkCLIStoreFeatures 加载 link ban/unban 命令实际可能触发的 Webhook 钩子。
func loadLinkCLIStoreFeatures() (StoreFeatures, error) {
	webhookKey := strings.TrimSpace(os.Getenv("WEBHOOK_SECRET_KEY"))
	if webhookKey != "" {
		if _, err := webhook.ParseMasterKeyHex(webhookKey); err != nil {
			return StoreFeatures{}, err
		}
	}
	return StoreFeatures{
		Webhooks: webhookKey != "",
	}, nil
}

func newConfiguredPostgresStore(db *sql.DB, f StoreFeatures) *postgres.Store {
	var hooks []postgres.TxHook
	if f.RiskScan {
		hooks = append(hooks, postgres.RiskScanHook())
	}
	if f.Webhooks {
		hooks = append(hooks, postgres.WebhookFanoutHook())
	}
	if f.Email {
		hooks = append(hooks, postgres.InvitationEmailHook())
	}
	if f.Analytics {
		hooks = append(hooks, postgres.StatsPurgeHook())
	}
	return postgres.NewStore(db,
		postgres.WithAnalytics(f.Analytics),
		postgres.WithTxHooks(hooks...),
	)
}
