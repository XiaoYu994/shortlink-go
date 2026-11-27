package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shortlink/internal/audit"
	"shortlink/internal/cache"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/safehttp"
)

const (
	riskTracerName  = "shortlink/risk"
	scansMetricName = "shortlink_risk_scans_total"
)

// Metrics 统计风控扫描各阶段（cache / local_* / gsb / fetch / body_keyword / llm）与裁决结果的分布计数。
type Metrics struct {
	scans *prometheus.CounterVec
}

// NewMetrics 构造风控指标并注册 shortlink_risk_scans_total{stage,outcome}。
func NewMetrics(registry *prometheus.Registry) *Metrics {
	m := &Metrics{
		scans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: scansMetricName,
			Help: "Total number of risk scan evaluations by pipeline stage and outcome.",
		}, []string{"stage", "outcome"}),
	}
	// 预置上游风控降级/限流/内容审核序列，防止告警规则在首次故障前缺少时间序列。
	for _, pair := range [][2]string{
		{StageGSB, OutcomeError},
		{StageGSB, OutcomeRateLimited},
		{StageLLM, OutcomeError},
		{StageLLM, OutcomeRateLimited},
		{StageLLM, OutcomeFiltered},
		{StageLLM, OutcomeBudgetLimited},
	} {
		m.scans.WithLabelValues(pair[0], pair[1])
	}
	if registry != nil {
		registry.MustRegister(m.scans)
	}
	return m
}

// Record 累加指定阶段与结果的扫描计数；m 或标签为空时安全忽略。
func (m *Metrics) Record(stage, outcome string) {
	if m == nil || m.scans == nil || stage == "" || outcome == "" {
		return
	}
	m.scans.WithLabelValues(stage, outcome).Inc()
}

// LinkBanner 是风控处理器需要的最小封禁能力，由 cache.Store 实现。
type LinkBanner interface {
	BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
}

// ScanPayload 是 link.risk_scan 任务的 JSON 载荷，和 postgres 入队 CTE 的字段一致。
type ScanPayload struct {
	LinkID int64  `json:"link_id"`
	Code   string `json:"code"`
	URL    string `json:"url"`
}

// HandlerOptions 组装风控处理器需要的依赖。
type HandlerOptions struct {
	Scanner Scanner
	Banner  LinkBanner
	Cache   *VerdictCache
	Metrics *Metrics
	Logger  *slog.Logger
}

/*
NewHandler 返回处理 link.risk_scan 任务的 job.Handler（ADR 0020、ADR 0034）：
1. 解析载荷，损坏时标为 Permanent 直接进入 dead；
2. 查 Redis 结果缓存（fail-open），命中时免出站抓取；
3. 调用 Scanner 扫描（按 host 限速已沉降到抓取前，L0 本地规则免受限速拖累）：
  - HostLimiter 限流 -> job.Defer（不扣尝试次数）；
  - LLM 429 限流 -> 末次尝试前按 RetryAfter 重排，末次尝试 fail-open；
  - SSRF / 非法端口 / 非法协议 / 超最大跳转 -> 记专项指标后返回 job.Skip，不污染 dead 告警；
  - 外部站点超时/5xx -> 重试耗尽前交由队列指数退避，耗尽后记 unreachable 并 job.Skip；

4. 违规时调用 BanIfURL(code, expectedURL, reason, risk)：
  - link.ErrNotFound / link.ErrURLChanged 标为 job.Skip（链接已删或已改成新 URL）；
  - cache.ErrNotUpdated 记 Warn 日志返回 nil：数据库封禁已提交，不重抓外网 URL（ADR 0031）。
*/
func NewHandler(opts HandlerOptions) job.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return func(ctx context.Context, j job.Job) error {
		var p ScanPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode risk payload: %w", err))
		}
		if p.Code == "" || p.URL == "" {
			return job.Permanent(errors.New("risk payload missing code or url"))
		}

		verdict, hit := opts.Cache.Get(ctx, p.URL)
		if hit {
			outcome := OutcomeSafe
			if verdict.Unsafe {
				outcome = OutcomeUnsafe
			}
			opts.Metrics.Record(StageCache, outcome)
			trace.SpanFromContext(ctx).SetAttributes(
				attribute.Bool("risk.cache.hit", true),
				attribute.String("risk.stage", StageCache),
				attribute.String("risk.outcome", outcome),
			)
		} else {
			scanCtx, span := otel.Tracer(riskTracerName).Start(ctx, "risk.scan",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(attribute.Bool("risk.cache.hit", false)),
			)
			var err error
			verdict, err = opts.Scanner.Scan(scanCtx, p.URL)
			if rle, ok := errors.AsType[rateLimitError](err); ok {
				span.RecordError(rle)
				span.SetStatus(codes.Error, rle.Error())
				span.SetAttributes(
					attribute.String("risk.stage", rle.stage),
					attribute.String("risk.outcome", OutcomeRateLimited),
				)
				span.End()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				opts.Metrics.Record(rle.stage, OutcomeRateLimited)
				if rle.deferOnly {
					return job.Defer(rle.retryAfter, "host rate limited: "+rle.host)
				}
				if ctx.Err() == nil && j.MaxAttempts > 0 && j.Attempts >= j.MaxAttempts {
					logger.WarnContext(ctx, "risk scan upstream rate limited after max attempts; failing open",
						"code", p.Code, "stage", rle.stage, "err", rle)
					return nil
				}
				return job.RetryAfter(rle.retryAfter, rle.err)
			}
			if skipOutcome, reason, ok := classifyUnscannableError(err); ok {
				span.RecordError(err)
				span.SetStatus(codes.Error, reason)
				span.SetAttributes(
					attribute.String("risk.stage", StageFetch),
					attribute.String("risk.outcome", skipOutcome),
				)
				span.End()
				opts.Metrics.Record(StageFetch, skipOutcome)
				logger.InfoContext(ctx, "risk scan skipped unscannable target url",
					"code", p.Code, "outcome", skipOutcome, "err", err)
				return job.Skip(reason)
			}
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				if ctx.Err() == nil && j.MaxAttempts > 0 && j.Attempts >= j.MaxAttempts {
					span.SetAttributes(
						attribute.String("risk.stage", StageFetch),
						attribute.String("risk.outcome", OutcomeUnreachable),
					)
					span.End()
					opts.Metrics.Record(StageFetch, OutcomeUnreachable)
					logger.WarnContext(ctx, "risk scan skipped unreachable target after retries",
						"code", p.Code, "attempts", j.Attempts, "err", err)
					return job.Skip("target unreachable after retries")
				}
				span.SetAttributes(
					attribute.String("risk.stage", StageFetch),
					attribute.String("risk.outcome", OutcomeError),
				)
				span.End()
				if ctx.Err() != nil {
					return err
				}
				opts.Metrics.Record(StageFetch, OutcomeError)
				return err
			}

			stage, outcome := defaultVerdictLabels(verdict)
			opts.Metrics.Record(stage, outcome)
			span.SetAttributes(
				attribute.Bool("risk.verdict.unsafe", verdict.Unsafe),
				attribute.String("risk.stage", stage),
				attribute.String("risk.outcome", outcome),
			)
			if verdict.Reason != "" {
				span.SetAttributes(attribute.String("risk.verdict.reason", string(verdict.Reason)))
			}
			span.End()
			if !verdict.Uncacheable {
				opts.Cache.Set(ctx, p.URL, verdict)
			}
		}

		if !verdict.Unsafe {
			switch verdict.Outcome {
			case OutcomeShadowUnsafe:
				logger.WarnContext(ctx, "risk scan shadow verdict",
					"code", p.Code, "reason", verdict.Reason, "detail", verdict.Detail)
			case OutcomeLowConfidence:
				logger.InfoContext(ctx, "risk scan low-confidence verdict",
					"code", p.Code, "reason", verdict.Reason, "detail", verdict.Detail)
			}
			return nil
		}

		banCtx := audit.WithMeta(ctx, audit.SystemMeta(fmt.Sprintf("job:%d", j.ID)))
		_, err := opts.Banner.BanIfURL(banCtx, p.Code, p.URL, verdict.Reason, link.BanSourceRisk)
		if err == nil {
			logger.InfoContext(ctx, "risk scan banned link",
				"code", p.Code, "reason", verdict.Reason, "detail", verdict.Detail)
			return nil
		}
		if errors.Is(err, link.ErrNotFound) {
			return job.Skip("link deleted before ban")
		}
		if errors.Is(err, link.ErrURLChanged) {
			return job.Skip("link url changed before ban")
		}
		if errors.Is(err, cache.ErrNotUpdated) {
			logger.WarnContext(ctx, "risk ban committed; cache write failed and will expire within ttl",
				"code", p.Code, "reason", verdict.Reason, "err", err)
			return nil
		}
		return err
	}
}

func classifyUnscannableError(err error) (outcome, reason string, ok bool) {
	switch {
	case err == nil:
		return "", "", false
	case errors.Is(err, safehttp.ErrBlockedAddress):
		return OutcomeBlockedAddress, "target address blocked by ssrf guard", true
	case errors.Is(err, safehttp.ErrBlockedPort):
		return OutcomeBlockedPort, "target port blocked by egress allowlist", true
	case errors.Is(err, safehttp.ErrInvalidScheme):
		return OutcomeInvalidScheme, "target scheme or host invalid", true
	case errors.Is(err, safehttp.ErrTooManyRedirects):
		return OutcomeTooManyRedirects, "target exceeded max redirects", true
	default:
		return "", "", false
	}
}

func defaultVerdictLabels(v Verdict) (stage, outcome string) {
	stage = v.Stage
	if stage == "" {
		stage = StageFetch
	}
	outcome = v.Outcome
	if outcome == "" {
		if v.Unsafe {
			outcome = OutcomeUnsafe
		} else {
			outcome = OutcomeSafe
		}
	}
	return stage, outcome
}
