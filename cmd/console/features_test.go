package main

import (
	"slices"
	"strings"
	"testing"

	"shortlink/internal/config"
)

func TestLoadAdminCLIStoreFeatures(t *testing.T) {
	// 无关的坏 WEBHOOK_SECRET_KEY 不应阻塞 key/tenant/member 命令。
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("WEBHOOK_SECRET_KEY", "bad-hex")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")

	got, err := loadAdminCLIStoreFeatures("")
	if err != nil {
		t.Fatalf("loadAdminCLIStoreFeatures without smtp: %v", err)
	}
	if got != (StoreFeatures{}) {
		t.Fatalf("StoreFeatures = %+v, want all false", got)
	}

	halfSMTP := writeTempYAML(t, "smtp:\n  host: smtp.example.com\n")
	if _, err := loadAdminCLIStoreFeatures(halfSMTP); err == nil || !strings.Contains(err.Error(), "smtp.from") {
		t.Fatalf("err = %v, want smtp.from required error", err)
	}

	fullSMTP := writeTempYAML(t, "smtp:\n  host: smtp.example.com\n  from: noreply@example.com\n")
	got, err = loadAdminCLIStoreFeatures(fullSMTP)
	if err != nil {
		t.Fatalf("loadAdminCLIStoreFeatures with smtp: %v", err)
	}
	if got != (StoreFeatures{Email: true}) {
		t.Fatalf("StoreFeatures = %+v, want Email=true only", got)
	}
}

func TestLoadLinkCLIStoreFeatures(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("WEBHOOK_SECRET_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")

	got, err := loadLinkCLIStoreFeatures()
	if err != nil {
		t.Fatalf("loadLinkCLIStoreFeatures: %v", err)
	}
	if got != (StoreFeatures{Webhooks: true}) {
		t.Fatalf("StoreFeatures = %+v, want Webhooks=true", got)
	}

	enabled, disabled := (StoreFeatures{RiskScan: true, Webhooks: true, Email: true}).enabledDisabled(true)
	if !slices.Equal(enabled, []string{"cache", "risk_scan", "webhooks", "email"}) {
		t.Fatalf("enabled = %v", enabled)
	}
	if !slices.Equal(disabled, []string{"analytics"}) {
		t.Fatalf("disabled = %v", disabled)
	}

	enabled, disabled = (StoreFeatures{Analytics: true}).consoleEnabledDisabled(false, true)
	if !slices.Equal(enabled, []string{"analytics", "web"}) {
		t.Fatalf("console enabled = %v", enabled)
	}
	if !slices.Equal(disabled, []string{"cache", "risk_scan", "webhooks", "email"}) {
		t.Fatalf("console disabled = %v", disabled)
	}

	t.Setenv("WEBHOOK_SECRET_KEY", "bad-hex")
	if _, err := loadLinkCLIStoreFeatures(); err == nil {
		t.Fatal("expected error for invalid WEBHOOK_SECRET_KEY")
	}
}
