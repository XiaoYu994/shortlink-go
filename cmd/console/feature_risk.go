package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/risk"
	"shortlink/internal/safehttp"
)

/*
riskWorkerModule 自包含异步 URL 风控所需的分层责任链（L0 本地规则、可选 L1 Safe Browsing、
L2 防 SSRF 安全抓取与 HTML 净文本提取、可选 L3 LLM 语义研判）、带规则指纹的结果缓存与任务注册（ADR 0030、ADR 0034）。
FEATURE_RISK_SCAN=false 时仅注册 link.risk_scan 的 disabledSkipHandler。
*/
type riskWorkerModule struct {
	enabled         bool
	banner          risk.LinkBanner
	redisClient     *redis.Client
	registry        *prometheus.Registry
	fetchTimeout    time.Duration
	allowedPorts    []int
	allowTestHosts  []string
	allowedHosts    []string
	blockedHosts    map[string]link.BanReason
	blockedKeywords map[string]link.BanReason
	hostRateLimit   int
	gsb             RiskGSBConfig
	llm             RiskLLMConfig
	logger          *slog.Logger
}

func (m riskWorkerModule) Name() string { return "risk_scan" }

func (m riskWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.enabled {
		if m.logger != nil {
			m.logger.Info("risk scan worker disabled: FEATURE_RISK_SCAN=false")
		}
		r.HandleDisabled(job.KindLinkRiskScan, m.Name())
		return nil
	}
	if m.banner == nil {
		return errors.New("risk scan banner is nil")
	}

	metrics := risk.NewMetrics(m.registry)

	var gsbClient *risk.GSBClient
	if m.gsb.Enabled() {
		var err error
		gsbClient, err = risk.NewGSBClient(risk.GSBOptions{
			APIKey:  m.gsb.APIKey,
			BaseURL: m.gsb.BaseURL,
			Timeout: m.gsb.Timeout,
		})
		if err != nil {
			return fmt.Errorf("init risk gsb client: %w", err)
		}
	}

	var llmJudge *risk.LLMJudge
	if m.llm.Enabled() {
		var err error
		llmJudge, err = risk.NewLLMJudge(risk.LLMOptions{
			BaseURL:       m.llm.BaseURL,
			APIKey:        m.llm.APIKey,
			Model:         m.llm.Model,
			Mode:          m.llm.Mode,
			MinConfidence: m.llm.MinConfidence,
			Timeout:       m.llm.Timeout,
			MaxPerMin:     m.llm.MaxPerMin,
			Logger:        m.logger,
		})
		if err != nil {
			return fmt.Errorf("init risk llm judge: %w", err)
		}
	}

	var verdictCache *risk.VerdictCache
	if m.redisClient != nil {
		versionInput := risk.RuleVersionInput{
			AllowedHosts:    m.allowedHosts,
			BlockedHosts:    m.blockedHosts,
			BlockedKeywords: m.blockedKeywords,
			GSBEnabled:      gsbClient != nil,
		}
		if llmJudge != nil {
			versionInput.LLMModel = llmJudge.ModelDescriptor().Model
			versionInput.LLMMode = m.llm.Mode
			versionInput.LLMMinConfidence = m.llm.MinConfidence
			versionInput.PromptVersion = llmJudge.PolicyVersion()
		}
		verdictCache = risk.NewVerdictCache(m.redisClient, m.registry, risk.VerdictCacheOptions{
			RuleVersion: risk.ComputeRuleVersion(versionInput),
			Logger:      m.logger,
		})
	}

	httpClient := risk.NewSafeHTTPClient(safehttp.ClientOptions{
		Timeout:          m.fetchTimeout,
		AllowedPorts:     m.allowedPorts,
		AllowedHostPorts: m.allowTestHosts,
	})
	rules := risk.NewRuleScanner(risk.RuleScannerOptions{
		HTTPClient:      httpClient,
		AllowedHosts:    m.allowedHosts,
		BlockedHosts:    m.blockedHosts,
		BlockedKeywords: m.blockedKeywords,
		Limiter:         safehttp.NewHostLimiter(m.hostRateLimit),
	})
	pipeline, err := risk.NewPipeline(risk.PipelineOptions{
		Rules:   rules,
		GSB:     gsbClient,
		LLM:     llmJudge,
		Metrics: metrics,
		Logger:  m.logger,
	})
	if err != nil {
		return fmt.Errorf("init risk pipeline: %w", err)
	}

	if m.logger != nil {
		m.logger.Info("risk scan pipeline configured",
			"gsb_enabled", gsbClient != nil,
			"llm_enabled", llmJudge != nil,
			"llm_model", m.llm.Model,
			"llm_mode", m.llm.Mode,
		)
	}

	handler := risk.NewHandler(risk.HandlerOptions{
		Scanner: pipeline,
		Banner:  m.banner,
		Cache:   verdictCache,
		Metrics: metrics,
		Logger:  m.logger,
	})
	r.Handle(job.KindLinkRiskScan, handler)
	return nil
}
