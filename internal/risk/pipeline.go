package risk

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// PipelineOptions 组装按代价递增分层的 URL 风控责任链（ADR 0034）。
type PipelineOptions struct {
	Rules   *RuleScanner
	GSB     *GSBClient
	LLM     *LLMJudge
	Metrics *Metrics
	Logger  *slog.Logger
}

/*
Pipeline 按 L0（本地黑白名单/URL 关键词）-> L1（可选 Safe Browsing）-> L2（安全抓取 + HTML 净文本 + 规则）
-> L3（可选 LLM 语义研判）分层串联风控检测并实现 Scanner 接口。
任意可降级外部情报层（GSB、LLM 非 429 错误）出错时 fail-open 继续或收尾，且把未命中的安全结果标为 Uncacheable。
*/
type Pipeline struct {
	rules   *RuleScanner
	gsb     *GSBClient
	llm     *LLMJudge
	metrics *Metrics
	logger  *slog.Logger
}

// NewPipeline 构造分层风控责任链。Rules 不可为 nil。
func NewPipeline(opts PipelineOptions) (*Pipeline, error) {
	if opts.Rules == nil {
		return nil, errors.New("risk pipeline rules scanner is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Pipeline{
		rules:   opts.Rules,
		gsb:     opts.GSB,
		llm:     opts.LLM,
		metrics: opts.Metrics,
		logger:  logger,
	}, nil
}

// Scan 依次经过 L0/L1/L2/L3 检测单条目标 URL。
func (p *Pipeline) Scan(ctx context.Context, targetURL string) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}

	// L0: 本地黑/白名单与 URL 关键词短路，绝不产生任何外部请求或令牌消耗。
	u, trimmed, localVerdict, done, err := p.rules.CheckLocal(targetURL)
	if err != nil {
		return Verdict{}, err
	}
	if done {
		return localVerdict, nil
	}

	degraded := false

	// L1: 可选 Google Safe Browsing 查验入口 URL；失败时 fail-open 降级继续走 L2。
	if p.gsb != nil {
		gv, gerr := p.gsb.Check(ctx, trimmed)
		if gerr != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			p.recordGSBFallback(ctx, gerr)
			degraded = true
		} else if gv.Unsafe {
			return gv, nil
		}
	}

	// L2: 安全抓取（含重定向黑名单、每跳限速与 HTML 净文本提取）与正文关键词匹配。
	fv, page, err := p.rules.FetchAndInspect(ctx, u, trimmed)
	if err != nil {
		return Verdict{}, err
	}
	if fv.Unsafe {
		return fv, nil
	}

	// 若目标站发生了 3xx 重定向，即使落地页返回 4xx 或二进制体，也应先对真实落地 URL 补做一次 GSB 查验。
	if p.gsb != nil && page.FinalURL != "" && page.FinalURL != trimmed {
		gv, gerr := p.gsb.Check(ctx, page.FinalURL)
		if gerr != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			p.recordGSBFallback(ctx, gerr)
			degraded = true
		} else if gv.Unsafe {
			return gv, nil
		}
	}

	if fv.Outcome == OutcomeHTTP4xx || fv.Outcome == OutcomeBinarySkipped {
		if degraded {
			fv.Uncacheable = true
		}
		return fv, nil
	}

	// L3: 可选 LLM 语义研判。仅对提取出的有效净文本运行。
	if p.llm != nil && page.CombinedText() != "" {
		lv, lerr := p.llm.Judge(ctx, trimmed, page)
		if lerr != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			// 上游 429 交由 handler 走 RetryAfter 退避重排（末次尝试再 fail-open）。
			if _, ok := errors.AsType[rateLimitError](lerr); ok {
				return Verdict{}, lerr
			}
			p.logger.WarnContext(ctx, "risk llm failed, degraded to rule verdict", "err", lerr)
			return Verdict{
				Unsafe:      false,
				Uncacheable: true,
				Stage:       StageLLM,
				Outcome:     OutcomeError,
			}, nil
		}
		if degraded && !lv.Unsafe {
			lv.Uncacheable = true
		}
		return lv, nil
	}

	if degraded {
		fv.Uncacheable = true
	}
	return fv, nil
}

func (p *Pipeline) recordGSBFallback(ctx context.Context, err error) {
	outcome := OutcomeError
	if _, ok := errors.AsType[rateLimitError](err); ok {
		outcome = OutcomeRateLimited
	}
	p.metrics.Record(StageGSB, outcome)
	p.logger.WarnContext(ctx, "risk gsb lookup failed, continuing with fetch", "outcome", outcome, "err", err)
}
