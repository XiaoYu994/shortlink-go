package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/audit"
	"shortlink/internal/job"
	"shortlink/internal/safehttp"
)

const (
	deliveriesMetricName   = "shortlink_webhook_deliveries_total"
	deliveryDurationName   = "shortlink_webhook_delivery_duration_seconds"
	autoDisabledMetricName = "shortlink_webhook_auto_disabled_total"

	missingKeyDeferDelay = 30 * time.Second
)

var webhookDurationBuckets = prometheus.ExponentialBuckets(0.005, 2, 12)

// DeliveryStore 定义 webhook.deliver 处理器依赖的最小持久化能力，由 postgres.Store 实现。
type DeliveryStore interface {
	GetWebhookForDelivery(ctx context.Context, webhookID int64) (DeliveryTarget, error)
	RecordWebhookDelivery(ctx context.Context, p RecordDeliveryParams) (autoDisabled bool, err error)
}

/*
HandlerOptions 组装 webhook.deliver 任务处理器依赖。
MaxConsecutiveFailures > 0 使用给定阈值，0 使用 DefaultMaxConsecutiveFailures（10），< 0 关闭自动熔断。
*/
type HandlerOptions struct {
	Store                  DeliveryStore
	HTTPClient             *http.Client
	Limiter                *safehttp.HostLimiter
	Cipher                 *SecretCipher
	RetryBaseDelay         time.Duration
	RetryMaxDelay          time.Duration
	MaxConsecutiveFailures int
	Registry               *prometheus.Registry
	Logger                 *slog.Logger
	Now                    func() time.Time
}

type metrics struct {
	deliveries   *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	autoDisabled prometheus.Counter
}

func newMetrics(registry *prometheus.Registry) *metrics {
	m := &metrics{
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: deliveriesMetricName,
			Help: "Total number of webhook delivery attempts by event and result.",
		}, []string{"event", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    deliveryDurationName,
			Help:    "Outbound webhook HTTP delivery duration in seconds by event.",
			Buckets: webhookDurationBuckets,
		}, []string{"event"}),
		autoDisabled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: autoDisabledMetricName,
			Help: "Total number of webhooks automatically disabled due to consecutive terminal failures.",
		}),
	}
	if registry != nil {
		registry.MustRegister(m.deliveries, m.duration, m.autoDisabled)
	}
	return m
}

/*
NewHandler 返回消费 webhook.deliver 任务的 job.Handler（ADR 0027）：
1. 未配置 WEBHOOK_SECRET_KEY（Cipher == nil）时返回 job.Defer，不消耗重试次数；
2. 回表查询订阅当前 URL/启用状态/加密密钥（已删或已停用的非 ping 任务返回 job.Skip）；
3. 按目标 host 限速（超限时 job.Defer），构造统一 JSON 信封并带新鲜 HMAC 签名 POST 投递（禁重定向）；
4. 2xx 记成功；3xx / 非 408·429 4xx / SSRF 拦截标记 job.Permanent；网络/408/429/5xx 走独立退避重试；
5. 非 ping 终态失败达到连续阈值时自动停用订阅并写系统审计日志。
*/
func NewHandler(opts HandlerOptions) job.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	client := opts.HTTPClient
	if client == nil {
		client = safehttp.NewClient(safehttp.ClientOptions{
			Timeout:          DefaultTimeout,
			DisableRedirects: true,
		})
	}
	retryBase := opts.RetryBaseDelay
	if retryBase <= 0 {
		retryBase = DefaultRetryBaseDelay
	}
	retryMax := opts.RetryMaxDelay
	if retryMax < retryBase {
		retryMax = max(DefaultRetryMaxDelay, retryBase)
	}
	failureThreshold := opts.MaxConsecutiveFailures
	switch {
	case failureThreshold == 0:
		failureThreshold = DefaultMaxConsecutiveFailures
	case failureThreshold < 0:
		failureThreshold = 0
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	m := newMetrics(opts.Registry)

	return func(ctx context.Context, j job.Job) error {
		if opts.Cipher == nil {
			return job.Defer(missingKeyDeferDelay, "webhook secret key not configured")
		}

		var p DeliverPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode webhook payload: %w", err))
		}
		if p.WebhookID <= 0 || p.EventID == "" || !p.Event.Valid() {
			return job.Permanent(errors.New("webhook payload missing required fields or invalid event"))
		}
		isPing := p.Event == EventPing

		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}

		target, err := opts.Store.GetWebhookForDelivery(ctx, p.WebhookID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				m.deliveries.WithLabelValues(string(p.Event), "skipped").Inc()
				return job.Skip("webhook deleted before delivery")
			}
			return err
		}
		if !target.Enabled && !isPing {
			m.deliveries.WithLabelValues(string(p.Event), "skipped").Inc()
			return job.Skip("webhook disabled before delivery")
		}

		secret, err := opts.Cipher.Decrypt(target.TenantID, target.SecretCiphertext)
		if err != nil {
			return job.Permanent(err)
		}
		u, err := url.Parse(target.URL)
		if err != nil || u.Hostname() == "" {
			return job.Permanent(fmt.Errorf("%w: %q", ErrInvalidURL, target.URL))
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		if opts.Limiter != nil {
			if delay, allowed := opts.Limiter.Allow(u.Hostname()); !allowed {
				m.deliveries.WithLabelValues(string(p.Event), "deferred").Inc()
				return job.Defer(delay, "webhook host rate limited: "+u.Hostname())
			}
		}

		data := p.Data
		if len(data) == 0 {
			data = json.RawMessage("{}")
		}
		createdAt := p.OccurredAt
		if createdAt.IsZero() {
			createdAt = nowFn().UTC()
		}
		body, err := json.Marshal(Envelope{
			ID:         p.EventID,
			Event:      p.Event,
			APIVersion: "v1",
			CreatedAt:  createdAt.UTC(),
			TenantID:   target.TenantID,
			Data:       data,
		})
		if err != nil {
			return job.Permanent(fmt.Errorf("marshal webhook envelope: %w", err))
		}

		deliveryID := FormatDeliveryID(j.ID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
		if err != nil {
			return job.Permanent(fmt.Errorf("build webhook request: %w", err))
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set(HeaderEvent, string(p.Event))
		req.Header.Set(HeaderDelivery, deliveryID)
		req.Header.Set(HeaderSignature, Sign(secret, nowFn().UTC(), body))

		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		start := nowFn()
		resp, reqErr := client.Do(req)
		if reqErr != nil && errors.Is(ctx.Err(), context.Canceled) {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			// Worker 停机主动取消时由 job.Runner 原样重入队（ConsumeAttempt=false），
			// 不写失败投递记录、也不累计连续失败触发误熔断。
			return ctx.Err()
		}
		durationMs := int(max(nowFn().Sub(start).Milliseconds(), 0))
		m.duration.WithLabelValues(string(p.Event)).Observe(float64(durationMs) / 1000.0)

		outcome := classifyOutcome(resp, reqErr)
		maxAttempts := j.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = DefaultMaxAttempts
		}
		attempt := max(j.Attempts, 1)
		terminalFailure := !outcome.succeeded && (outcome.permanent || attempt >= maxAttempts)

		recStatus := StatusSuccess
		if !outcome.succeeded {
			if terminalFailure {
				recStatus = StatusFailed
			} else {
				recStatus = StatusRetry
			}
		}

		// 如果恰在 worker 关闭取消 context 时收到瞬断，不覆盖正常状态落库超时（用 WithoutCancel 保留 trace 但独立短超时）。
		recordBaseCtx := audit.WithMeta(context.WithoutCancel(ctx), audit.SystemMeta(fmt.Sprintf("job:%d", j.ID)))
		recordCtx, cancelRecord := context.WithTimeout(recordBaseCtx, 5*time.Second)
		defer cancelRecord()

		autoDisabled, recErr := opts.Store.RecordWebhookDelivery(recordCtx, RecordDeliveryParams{
			TenantID:               target.TenantID,
			WebhookID:              target.ID,
			JobID:                  j.ID,
			DeliveryID:             deliveryID,
			Event:                  p.Event,
			Attempt:                attempt,
			Status:                 recStatus,
			HTTPStatus:             outcome.httpStatus,
			DurationMs:             durationMs,
			ErrorMessage:           sanitizeAndTruncate(outcome.errMsg, MaxErrorBytes),
			ResponseSnippet:        sanitizeAndTruncate(outcome.snippet, MaxSnippetBytes),
			TerminalFailure:        terminalFailure,
			IsPing:                 isPing,
			MaxConsecutiveFailures: failureThreshold,
		})
		if recErr != nil {
			// 投递记录落库失败只记日志，仍须继续执行指标计数与 Permanent / RetryAfter 分类，
			// 避免把 3xx/4xx 误打回默认重试，或丢失 5xx 的 Webhook 专用退避节拍。
			logger.ErrorContext(ctx, "record webhook delivery", "webhook_id", target.ID, "err", recErr)
		} else if autoDisabled {
			m.autoDisabled.Inc()
			logger.WarnContext(ctx, "webhook auto-disabled after consecutive failures",
				"webhook_id", target.ID, "tenant_id", target.TenantID, "threshold", failureThreshold)
		}

		if outcome.succeeded {
			m.deliveries.WithLabelValues(string(p.Event), "success").Inc()
			return nil
		}
		if terminalFailure {
			m.deliveries.WithLabelValues(string(p.Event), "failed").Inc()
			if outcome.permanent {
				return job.Permanent(outcome.err)
			}
			return outcome.err
		}
		m.deliveries.WithLabelValues(string(p.Event), "retry").Inc()
		return job.RetryAfter(job.Backoff(attempt, retryBase, retryMax), outcome.err)
	}
}

type deliveryOutcome struct {
	succeeded  bool
	permanent  bool
	httpStatus *int
	snippet    string
	errMsg     string
	err        error
}

func classifyOutcome(resp *http.Response, reqErr error) deliveryOutcome {
	if reqErr != nil {
		permanent := errors.Is(reqErr, safehttp.ErrBlockedAddress) ||
			errors.Is(reqErr, safehttp.ErrBlockedPort) ||
			errors.Is(reqErr, safehttp.ErrInvalidScheme)
		return deliveryOutcome{
			permanent: permanent,
			errMsg:    reqErr.Error(),
			err:       reqErr,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	rawSnippet, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseReadBytes))
	snippet := string(rawSnippet)
	if readErr != nil && snippet == "" {
		snippet = "(read response failed: " + readErr.Error() + ")"
	}
	code := resp.StatusCode
	statusPtr := &code

	if code >= 200 && code < 300 {
		return deliveryOutcome{
			succeeded:  true,
			httpStatus: statusPtr,
			snippet:    snippet,
		}
	}

	err := fmt.Errorf("webhook endpoint returned HTTP %d", code)
	if code >= 300 && code < 400 {
		err = fmt.Errorf("webhook endpoint returned redirect HTTP %d (redirects are disabled)", code)
		return deliveryOutcome{
			permanent:  true,
			httpStatus: statusPtr,
			snippet:    snippet,
			errMsg:     err.Error(),
			err:        err,
		}
	}
	if code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests {
		return deliveryOutcome{
			permanent:  true,
			httpStatus: statusPtr,
			snippet:    snippet,
			errMsg:     err.Error(),
			err:        err,
		}
	}
	return deliveryOutcome{
		permanent:  false,
		httpStatus: statusPtr,
		snippet:    snippet,
		errMsg:     err.Error(),
		err:        err,
	}
}

func sanitizeAndTruncate(s string, maxBytes int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
