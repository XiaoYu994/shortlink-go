package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"shortlink/internal/link"
)

const (
	// LLMModeBan 表示高置信度违规时自动封禁短链。
	LLMModeBan = "ban"
	// LLMModeShadow 表示只记日志与指标，不自动封禁。
	LLMModeShadow = "shadow"

	// DefaultLLMTimeout 是单次 LLM 语义研判请求的默认超时。
	DefaultLLMTimeout = 8 * time.Second
	// DefaultLLMMinConfidence 是触发自动封禁的最低置信度门槛。
	DefaultLLMMinConfidence = 0.85
	// DefaultLLMMaxPerMin 是进程内 LLM 调用预算的默认每分钟请求上限。
	DefaultLLMMaxPerMin = 30

	maxLLMSummaryRunes = 120
)

// ValidLLMMode 报告 mode 是否为受支持的 AI 研判模式（ban 或 shadow）。
func ValidLLMMode(mode string) bool {
	return mode == LLMModeBan || mode == LLMModeShadow
}

// LLMOptions 配置可选的 LLM 语义风控器（L3），默认使用 OpenAICompatModel 与内置嵌入式策略。
type LLMOptions struct {
	BaseURL       string
	APIKey        string
	Model         string
	Mode          string
	MinConfidence float64
	Timeout       time.Duration
	MaxPerMin     int
	PolicyText    string
	HTTPClient    *http.Client
	ModelClient   ChatModel
	Policy        PolicyProvider
	Logger        *slog.Logger
}

/*
LLMJudge 组装「PolicyProvider（业务策略）+ Prompt 外壳（防注入与结构化约束）+ ChatModel（模型协议适配）」
对灰样本网页净文本做语义风控裁决。
*/
type LLMJudge struct {
	model  ChatModel
	policy PolicyProvider
	budget *rate.Limiter
	logger *slog.Logger
}

// NewLLMJudge 构造 LLM 风控裁决器。未显式传入 ModelClient/Policy 时使用 OpenAICompatModel 与内置策略快照。
func NewLLMJudge(opts LLMOptions) (*LLMJudge, error) {
	policy := opts.Policy
	if policy == nil {
		staticPolicy, err := NewStaticPolicyProvider(opts.PolicyText, opts.Mode, opts.MinConfidence)
		if err != nil {
			return nil, err
		}
		policy = staticPolicy
	}

	modelClient := opts.ModelClient
	if modelClient == nil {
		var err error
		modelClient, err = NewOpenAICompatModel(OpenAICompatOptions{
			BaseURL:    opts.BaseURL,
			APIKey:     opts.APIKey,
			Model:      opts.Model,
			Timeout:    opts.Timeout,
			HTTPClient: opts.HTTPClient,
		})
		if err != nil {
			return nil, err
		}
	}

	var budget *rate.Limiter
	if opts.MaxPerMin > 0 {
		perSec := float64(opts.MaxPerMin) / 60.0
		burst := max(1, min(opts.MaxPerMin, 5))
		budget = rate.NewLimiter(rate.Limit(perSec), burst)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &LLMJudge{
		model:  modelClient,
		policy: policy,
		budget: budget,
		logger: logger,
	}, nil
}

// PolicyVersion 返回当前策略文本版本号，供缓存规则指纹串联使用。
func (j *LLMJudge) PolicyVersion() string {
	if j == nil || j.policy == nil {
		return ""
	}
	return j.policy.Current(context.Background()).Version
}

// ModelDescriptor 返回底层模型驱动与模型 ID。
func (j *LLMJudge) ModelDescriptor() ModelDescriptor {
	if j == nil || j.model == nil {
		return ModelDescriptor{}
	}
	return j.model.Descriptor()
}

type llmVerdictOutput struct {
	Unsafe     bool    `json:"unsafe"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Summary    string  `json:"summary"`
}

/*
Judge 对抓取并净化的页面上下文执行语义研判：
- 本地预算耗尽 -> fail-open（OutcomeBudgetLimited，Uncacheable）；
- 上游 429 -> 透传 rateLimitError 交由任务队列按 RetryAfter 退避重试；
- 上游内容审核拦截 / 拒答 -> OutcomeFiltered，不重试、不误封、不写安全缓存；
- shadow 模式命中或低于置信度门槛 -> 保留 Reason/Detail 供 Handler 记录日志（含 code），保持 Unsafe: false, Uncacheable: true。
*/
func (j *LLMJudge) Judge(ctx context.Context, targetURL string, page PageContent) (Verdict, error) {
	desc := j.model.Descriptor()
	pol := j.policy.Current(ctx)

	if j.budget != nil && !j.budget.Allow() {
		j.logger.WarnContext(ctx, "risk llm skipped due to budget limit",
			"provider", desc.Provider, "model", desc.Model)
		return Verdict{
			Unsafe:      false,
			Uncacheable: true,
			Stage:       StageLLM,
			Outcome:     OutcomeBudgetLimited,
		}, nil
	}

	opCtx, span := otel.Tracer(riskTracerName).Start(ctx, "risk.llm",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("risk.llm.provider", desc.Provider),
			attribute.String("risk.llm.model", desc.Model),
			attribute.String("risk.llm.mode", pol.Mode),
			attribute.String("risk.llm.policy_version", pol.Version),
		),
	)
	defer span.End()

	chatResp, err := j.model.Complete(opCtx, ChatRequest{
		SystemPrompt: buildLLMSystemPrompt(pol.PolicyText),
		UserPrompt:   buildLLMUserPrompt(targetURL, page),
		ExpectJSON:   true,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Verdict{}, err
	}
	if chatResp.PromptTokens > 0 || chatResp.CompletionTokens > 0 {
		span.SetAttributes(
			attribute.Int("risk.llm.prompt_tokens", chatResp.PromptTokens),
			attribute.Int("risk.llm.completion_tokens", chatResp.CompletionTokens),
		)
	}
	if chatResp.ContentFiltered {
		j.logger.WarnContext(ctx, "risk llm blocked or refused by upstream policy",
			"provider", desc.Provider, "model", desc.Model, "finish_reason", chatResp.FinishReason)
		span.SetAttributes(attribute.String("risk.outcome", OutcomeFiltered))
		return Verdict{
			Unsafe:      false,
			Uncacheable: true,
			Stage:       StageLLM,
			Outcome:     OutcomeFiltered,
		}, nil
	}

	rawJSON := stripJSONFence(chatResp.Content)
	var out llmVerdictOutput
	if err := json.Unmarshal([]byte(rawJSON), &out); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decode llm verdict json")
		return Verdict{}, fmt.Errorf("decode llm verdict json: %w", err)
	}
	if math.IsNaN(out.Confidence) || math.IsInf(out.Confidence, 0) || out.Confidence < 0 || out.Confidence > 1 {
		err := fmt.Errorf("llm confidence out of range: %v", out.Confidence)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Verdict{}, err
	}

	span.SetAttributes(attribute.Float64("risk.llm.confidence", out.Confidence))
	if !out.Unsafe {
		return Verdict{Unsafe: false, Stage: StageLLM, Outcome: OutcomeSafe}, nil
	}

	reason := normalizeLLMBanReason(out.Reason)
	summary := cleanSingleLine(out.Summary, maxLLMSummaryRunes)
	detail := fmt.Sprintf("llm(%s,conf=%.2f)", desc.Model, out.Confidence)
	if summary != "" {
		detail += ": " + summary
	}

	if out.Confidence < pol.MinConfidence {
		return Verdict{
			Unsafe:      false,
			Reason:      reason,
			Detail:      detail,
			Uncacheable: true,
			Stage:       StageLLM,
			Outcome:     OutcomeLowConfidence,
		}, nil
	}
	if pol.Mode == LLMModeShadow {
		return Verdict{
			Unsafe:      false,
			Reason:      reason,
			Detail:      detail,
			Uncacheable: true,
			Stage:       StageLLM,
			Outcome:     OutcomeShadowUnsafe,
		}, nil
	}

	return Verdict{
		Unsafe:  true,
		Reason:  reason,
		Detail:  detail,
		Stage:   StageLLM,
		Outcome: OutcomeUnsafe,
	}, nil
}

func stripJSONFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 2 {
		return trimmed
	}
	lines = lines[1:]
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func normalizeLLMBanReason(raw string) link.BanReason {
	r := link.BanReason(strings.ToLower(strings.TrimSpace(raw)))
	if r.Valid() {
		return r
	}
	return link.BanReasonOther
}

func cleanSingleLine(s string, maxRunes int) string {
	joined := strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for i := range joined {
		if count == maxRunes {
			return joined[:i]
		}
		count++
	}
	return joined
}
