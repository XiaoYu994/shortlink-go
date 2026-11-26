/*
Package sms 提供手机号短信验证码登录使用的短信发送通道抽象（ADR 0030）。

本包不依赖数据库、Redis 或认证域包，仅通过 Sender 接口隔离不同短信网关实现：
  - WebhookSender：经 safehttp.Client 向外部 HTTP 短信网关投递 JSON（防 SSRF、端口白名单、可选 Bearer 认证）；
  - LogSender：本地开发模式专用，仅通过 slog 输出验证码而不触发真实外网发信。
*/
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"shortlink/internal/logx"
	"shortlink/internal/safehttp"
)

const (
	ProviderWebhook = "webhook"
	ProviderLog     = "log"

	DefaultTimeout = 5 * time.Second

	maxErrorResponseBytes = 1024
)

var (
	// ErrInvalidConfig 表示短信通道配置缺失或非法（如未配置有效的 Webhook URL 或超时）。
	ErrInvalidConfig = errors.New("sms: invalid config")
	// ErrInvalidMessage 表示短信目标手机号、验证码或有效期非法。
	ErrInvalidMessage = errors.New("sms: invalid message")
	// ErrProviderUnavailable 表示短信网关返回非 2xx 响应或网络通信失败。
	ErrProviderUnavailable = errors.New("sms: provider unavailable")
)

// ValidProvider 报告 provider 是否为受支持的短信发送实现名。
func ValidProvider(provider string) bool {
	return provider == ProviderWebhook || provider == ProviderLog
}

// Message 描述一条待发送的一次性短信验证码。
type Message struct {
	To   string
	Code string
	TTL  time.Duration
}

// Sender 抽象最小短信验证码发送能力（单方法接口，对标 internal/mailer.Sender）。
type Sender interface {
	SendOTP(ctx context.Context, msg Message) error
}

// WebhookConfig 控制通用 HTTP 短信网关的请求地址、鉴权令牌、超时与安全白名单。
type WebhookConfig struct {
	URL              string
	Token            string
	Timeout          time.Duration
	AllowedPorts     []int
	AllowedHostPorts []string
}

// LogValue 掩码 Token，避免日志泄露短信网关凭据。
func (c WebhookConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("token", logx.RedactSecret(c.Token)),
		slog.Duration("timeout", c.Timeout),
		slog.Int("allowed_ports_count", len(c.AllowedPorts)),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出网关 Token。
func (c WebhookConfig) String() string {
	return c.LogValue().String()
}

// WebhookSender 通过 safehttp.Client 向外部 HTTP 短信网关同步投递 OTP JSON。
type WebhookSender struct {
	endpoint string
	token    string
	client   *http.Client
}

type webhookPayload struct {
	To         string `json:"to"`
	Code       string `json:"code"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// NewWebhookSender 校验 WebhookConfig 并构造防 SSRF 的短信发送器。
func NewWebhookSender(cfg WebhookConfig) (*WebhookSender, error) {
	u, err := safehttp.ValidateURL(cfg.URL, safehttp.URLValidationOptions{
		AllowedPorts:     cfg.AllowedPorts,
		AllowedHostPorts: cfg.AllowedHostPorts,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: webhook url: %w", ErrInvalidConfig, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := safehttp.NewClient(safehttp.ClientOptions{
		Timeout:          timeout,
		DisableRedirects: true,
		AllowedPorts:     cfg.AllowedPorts,
		AllowedHostPorts: cfg.AllowedHostPorts,
	})
	return &WebhookSender{
		endpoint: u.String(),
		token:    strings.TrimSpace(cfg.Token),
		client:   client,
	}, nil
}

// SendOTP 向短信网关 POST JSON 载荷；2xx 视为成功，非 2xx 包装为 ErrProviderUnavailable。
func (s *WebhookSender) SendOTP(ctx context.Context, msg Message) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("%w: nil webhook sender", ErrInvalidConfig)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMessage(msg); err != nil {
		return err
	}

	ttlSeconds := max(int64(msg.TTL/time.Second), 1)
	raw, err := json.Marshal(webhookPayload{
		To:         msg.To,
		Code:       msg.Code,
		TTLSeconds: ttlSeconds,
	})
	if err != nil {
		return fmt.Errorf("sms: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorResponseBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: gateway returned status %d", ErrProviderUnavailable, resp.StatusCode)
	}
	return nil
}

// LogSender 仅用于本地开发与调试：把验证码输出到 slog，不发真实短信。
type LogSender struct {
	logger *slog.Logger
}

// NewLogSender 构造本地日志模式短信发送器。
func NewLogSender(logger *slog.Logger) *LogSender {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &LogSender{logger: logger}
}

// SendOTP 在 ctx 未取消且消息合法时将验证码记录到开发日志。
func (s *LogSender) SendOTP(ctx context.Context, msg Message) error {
	if s == nil {
		return fmt.Errorf("%w: nil log sender", ErrInvalidConfig)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMessage(msg); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "dev sms otp dispatched",
		"to", logx.RedactPhone(msg.To),
		"code", msg.Code,
		"ttl", msg.TTL.String(),
	)
	return nil
}

func validateMessage(msg Message) error {
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("%w: recipient is required", ErrInvalidMessage)
	}
	if strings.TrimSpace(msg.Code) == "" {
		return fmt.Errorf("%w: code is required", ErrInvalidMessage)
	}
	if msg.TTL <= 0 {
		return fmt.Errorf("%w: ttl must be positive", ErrInvalidMessage)
	}
	return nil
}
