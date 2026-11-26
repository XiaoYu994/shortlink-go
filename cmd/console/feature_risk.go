package main

import (
	"errors"
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
riskWorkerModule 自包含异步 URL 风控所需的防 SSRF 客户端、规则扫描器、结果缓存与任务注册（ADR 0030）。
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
	blockedHosts    map[string]link.BanReason
	blockedKeywords map[string]link.BanReason
	hostRateLimit   int
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
	var verdictCache *risk.VerdictCache
	if m.redisClient != nil {
		verdictCache = risk.NewVerdictCache(m.redisClient, m.registry, risk.VerdictCacheOptions{Logger: m.logger})
	}
	httpClient := risk.NewSafeHTTPClient(safehttp.ClientOptions{
		Timeout:          m.fetchTimeout,
		AllowedPorts:     m.allowedPorts,
		AllowedHostPorts: m.allowTestHosts,
	})
	scanner := risk.NewRuleScanner(risk.RuleScannerOptions{
		HTTPClient:      httpClient,
		BlockedHosts:    m.blockedHosts,
		BlockedKeywords: m.blockedKeywords,
	})
	handler := risk.NewHandler(risk.HandlerOptions{
		Scanner: scanner,
		Banner:  m.banner,
		Cache:   verdictCache,
		Limiter: safehttp.NewHostLimiter(m.hostRateLimit),
		Logger:  m.logger,
	})
	r.Handle(job.KindLinkRiskScan, handler)
	return nil
}
