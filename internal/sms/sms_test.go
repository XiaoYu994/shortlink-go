package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookConfigRedactsToken(t *testing.T) {
	cfg := WebhookConfig{
		URL:     "https://sms.example.com/send",
		Token:   "super-secret-sms-token",
		Timeout: 3 * time.Second,
	}
	if strings.Contains(cfg.String(), "super-secret-sms-token") {
		t.Fatalf("WebhookConfig.String() leaked token: %s", cfg.String())
	}
	if !strings.Contains(cfg.String(), "[REDACTED]") {
		t.Fatalf("WebhookConfig.String() missing redacted marker: %s", cfg.String())
	}
}

func TestNewWebhookSenderValidation(t *testing.T) {
	if _, err := NewWebhookSender(WebhookConfig{URL: ""}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty url err = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewWebhookSender(WebhookConfig{URL: "http://127.0.0.1:8080/sms"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("loopback url without allowlist err = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewWebhookSender(WebhookConfig{URL: "https://sms.example.com:8443/sms"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("non-standard port without allowlist err = %v, want ErrInvalidConfig", err)
	}
}

func TestWebhookSenderSendOTP(t *testing.T) {
	var (
		calls      atomic.Int32
		gotAuth    string
		gotPayload webhookPayload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	sender, err := NewWebhookSender(WebhookConfig{
		URL:              srv.URL + "/otp",
		Token:            "tok-123",
		Timeout:          2 * time.Second,
		AllowedHostPorts: []string{hostPort},
	})
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}

	msg := Message{To: "+8613800138000", Code: "482910", TTL: 5 * time.Minute}
	if err := sender.SendOTP(context.Background(), msg); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
	if gotPayload.To != msg.To || gotPayload.Code != msg.Code || gotPayload.TTLSeconds != 300 {
		t.Errorf("payload = %+v, want {To:%s Code:%s TTLSeconds:300}", gotPayload, msg.To, msg.Code)
	}

	// 已取消的 ctx 必须在发网络请求前短路，零下游调用。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sender.SendOTP(canceled, msg); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendOTP(canceled) = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("canceled ctx still called gateway: calls = %d, want 1", calls.Load())
	}
}

func TestWebhookSenderMapsNon2xxToProviderUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	sender, err := NewWebhookSender(WebhookConfig{
		URL:              srv.URL,
		Timeout:          2 * time.Second,
		AllowedHostPorts: []string{hostPort},
	})
	if err != nil {
		t.Fatalf("NewWebhookSender: %v", err)
	}

	err = sender.SendOTP(context.Background(), Message{To: "+8613800138000", Code: "123456", TTL: time.Minute})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("SendOTP(503) = %v, want ErrProviderUnavailable", err)
	}
}

func TestLogSender(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sender := NewLogSender(logger)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sender.SendOTP(canceled, Message{To: "+8613800138000", Code: "654321", TTL: 5 * time.Minute}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendOTP(canceled) = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("canceled ctx wrote dev log: %s", buf.String())
	}

	if err := sender.SendOTP(context.Background(), Message{To: "+8613800138000", Code: "654321", TTL: 5 * time.Minute}); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "code=654321") || !strings.Contains(out, "+86****8000") || strings.Contains(out, "+8613800138000") {
		t.Fatalf("unexpected dev sms log output: %s", out)
	}
}
