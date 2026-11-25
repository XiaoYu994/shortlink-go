package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadAdminCLIStoreFeatures(t *testing.T) {
	// 无关的坏 WEBHOOK_SECRET_KEY 不应阻塞 key/tenant/member 命令。
	t.Setenv("WEBHOOK_SECRET_KEY", "bad-hex")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("SMTP_HOST", "")
	t.Setenv("SMTP_FROM", "")

	got, err := loadAdminCLIStoreFeatures()
	if err != nil {
		t.Fatalf("loadAdminCLIStoreFeatures without smtp: %v", err)
	}
	if got != (StoreFeatures{}) {
		t.Fatalf("StoreFeatures = %+v, want all false", got)
	}

	t.Setenv("SMTP_HOST", "smtp.example.com")
	if _, err := loadAdminCLIStoreFeatures(); err == nil || !strings.Contains(err.Error(), "SMTP_FROM") {
		t.Fatalf("err = %v, want SMTP_FROM required error", err)
	}

	t.Setenv("SMTP_FROM", "noreply@example.com")
	got, err = loadAdminCLIStoreFeatures()
	if err != nil {
		t.Fatalf("loadAdminCLIStoreFeatures with smtp: %v", err)
	}
	if got != (StoreFeatures{Email: true}) {
		t.Fatalf("StoreFeatures = %+v, want Email=true only", got)
	}
}

func TestLoadLinkCLIStoreFeatures(t *testing.T) {
	// 无关的半残 SMTP 配置不应阻塞 link ban/unban 命令。
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("WEBHOOK_SECRET_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")

	got, err := loadLinkCLIStoreFeatures()
	if err != nil {
		t.Fatalf("loadLinkCLIStoreFeatures: %v", err)
	}
	want := StoreFeatures{Cache: true, Webhooks: true}
	if got != want {
		t.Fatalf("StoreFeatures = %+v, want %+v", got, want)
	}

	enabled, disabled := (StoreFeatures{Cache: true, RiskScan: true, Webhooks: true, Email: true}).enabledDisabled()
	if !slices.Equal(enabled, []string{"cache", "risk_scan", "webhooks", "email"}) {
		t.Fatalf("enabled = %v", enabled)
	}
	if !slices.Equal(disabled, []string{"analytics"}) {
		t.Fatalf("disabled = %v", disabled)
	}

	t.Setenv("WEBHOOK_SECRET_KEY", "bad-hex")
	if _, err := loadLinkCLIStoreFeatures(); err == nil {
		t.Fatal("expected error for invalid WEBHOOK_SECRET_KEY")
	}
}
