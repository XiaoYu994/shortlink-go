package risk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/link"
)

const (
	riskCacheKeyPrefix = "shortlink:risk:url:"
	defaultSafeTTL     = 10 * time.Minute
	defaultUnsafeTTL   = 5 * time.Minute
	cacheOpTimeout     = 500 * time.Millisecond

	cacheLookupsMetricName = "shortlink_risk_cache_lookups_total"
	cacheResultHit         = "hit"
	cacheResultMiss        = "miss"
	cacheResultFallback    = "fallback"
)

type cachedVerdict struct {
	Unsafe bool           `json:"unsafe"`
	Reason link.BanReason `json:"reason,omitempty"`
	Detail string         `json:"detail,omitempty"`
}

/*
VerdictCache 把最近扫描过的 URL 结论短时缓存在 Redis 里，减少热门目标地址的重复出站抓取。
严格遵循 ADR 0006 与 ADR 0020 的 fail-open 定位：Redis 读写出错只记日志与指标，降级为直接扫描。
*/
type VerdictCache struct {
	rdb         *redis.Client
	logger      *slog.Logger
	safeTTL     time.Duration
	unsafeTTL   time.Duration
	ruleVersion string
	lookups     *prometheus.CounterVec
}

// VerdictCacheOptions 控制缓存 TTL、规则指纹与日志。零值使用默认值。
type VerdictCacheOptions struct {
	SafeTTL     time.Duration
	UnsafeTTL   time.Duration
	RuleVersion string
	Logger      *slog.Logger
}

// RuleVersionInput 汇总会影响风控裁决结果的配置维度（绝不含任何外部 API 密钥）。
type RuleVersionInput struct {
	AllowedHosts     []string
	BlockedHosts     map[string]link.BanReason
	BlockedKeywords  map[string]link.BanReason
	GSBEnabled       bool
	LLMModel         string
	LLMMode          string
	LLMMinConfidence float64
	PromptVersion    string
}

// ComputeRuleVersion 为当前风控规则与模型参数计算稳定短指纹；改规则重启后自动切换缓存版本。
func ComputeRuleVersion(in RuleVersionInput) string {
	h := sha256.New()
	allow := slices.Clone(in.AllowedHosts)
	for i, host := range allow {
		allow[i] = normalizeHost(host)
	}
	slices.Sort(allow)
	for _, host := range allow {
		if host != "" {
			_, _ = fmt.Fprintf(h, "allow=%s\n", host)
		}
	}

	hosts := make(map[string]link.BanReason, len(in.BlockedHosts))
	for k, v := range in.BlockedHosts {
		if nk := normalizeHost(k); nk != "" && v.Valid() {
			hosts[nk] = v
		}
	}
	for _, r := range sortedRules(hosts) {
		_, _ = fmt.Fprintf(h, "block_host=%s:%s\n", r.pattern, r.reason)
	}

	kws := DefaultKeywordRules()
	for k, v := range in.BlockedKeywords {
		if nk := strings.ToLower(strings.TrimSpace(k)); nk != "" && v.Valid() {
			kws[nk] = v
		}
	}
	for _, r := range sortedRules(kws) {
		_, _ = fmt.Fprintf(h, "block_kw=%s:%s\n", r.pattern, r.reason)
	}

	_, _ = fmt.Fprintf(h, "gsb=%t\n", in.GSBEnabled)
	if model := strings.TrimSpace(in.LLMModel); model != "" {
		pv := strings.TrimSpace(in.PromptVersion)
		if pv == "" {
			if p, err := NewStaticPolicyProvider("", in.LLMMode, in.LLMMinConfidence); err == nil {
				pv = p.Current(context.Background()).Version
			}
		}
		_, _ = fmt.Fprintf(h, "llm=%s|%s|%.4f|%s\n", model, strings.TrimSpace(in.LLMMode), in.LLMMinConfidence, pv)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// NewVerdictCache 构造风控结果缓存并注册 shortlink_risk_cache_lookups_total 指标。
func NewVerdictCache(rdb *redis.Client, registry *prometheus.Registry, opts VerdictCacheOptions) *VerdictCache {
	if opts.SafeTTL <= 0 {
		opts.SafeTTL = defaultSafeTTL
	}
	if opts.UnsafeTTL <= 0 {
		opts.UnsafeTTL = defaultUnsafeTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c := &VerdictCache{
		rdb:         rdb,
		logger:      opts.Logger,
		safeTTL:     opts.SafeTTL,
		unsafeTTL:   opts.UnsafeTTL,
		ruleVersion: strings.TrimSpace(opts.RuleVersion),
		lookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: cacheLookupsMetricName,
			Help: "Total number of risk scan verdict cache lookups by result.",
		}, []string{"result"}),
	}
	c.lookups.WithLabelValues(cacheResultHit)
	c.lookups.WithLabelValues(cacheResultMiss)
	c.lookups.WithLabelValues(cacheResultFallback)
	if registry != nil {
		registry.MustRegister(c.lookups)
	}
	return c
}

/*
NormalizeURL 归一化目标 URL 以便去重：协议与主机名转小写、去掉默认端口和仅客户端使用的 fragment。
路径和查询参数保留原样，避免把同一域名下的不同页面误合为一个结论。
*/
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("invalid url")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := normalizeHost(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = host + ":" + port
	} else {
		u.Host = host
	}
	u.Fragment = ""
	return u.String(), nil
}

func (c *VerdictCache) cacheKey(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	digest := hex.EncodeToString(sum[:])
	if c.ruleVersion != "" {
		return riskCacheKeyPrefix + c.ruleVersion + ":" + digest
	}
	return riskCacheKeyPrefix + digest
}

// Get 查询缓存中的扫描结论。未命中或 Redis 异常返回 (Verdict{}, false)。
func (c *VerdictCache) Get(ctx context.Context, rawURL string) (Verdict, bool) {
	if c == nil || c.rdb == nil {
		return Verdict{}, false
	}
	norm, err := NormalizeURL(rawURL)
	if err != nil {
		return Verdict{}, false
	}

	opCtx, cancel := context.WithTimeout(ctx, cacheOpTimeout)
	defer cancel()

	key := c.cacheKey(norm)
	raw, err := c.rdb.Get(opCtx, key).Bytes()
	switch {
	case err == nil:
		var cv cachedVerdict
		if uerr := json.Unmarshal(raw, &cv); uerr != nil || (cv.Unsafe && !cv.Reason.Valid()) {
			_ = c.rdb.Del(opCtx, key).Err()
			c.lookups.WithLabelValues(cacheResultMiss).Inc()
			return Verdict{}, false
		}
		c.lookups.WithLabelValues(cacheResultHit).Inc()
		return Verdict{Unsafe: cv.Unsafe, Reason: cv.Reason, Detail: cv.Detail}, true
	case errors.Is(err, redis.Nil):
		c.lookups.WithLabelValues(cacheResultMiss).Inc()
		return Verdict{}, false
	default:
		c.lookups.WithLabelValues(cacheResultFallback).Inc()
		c.logger.WarnContext(ctx, "read risk verdict cache", "err", err)
		return Verdict{}, false
	}
}

// Set 尽力写入扫描结论；写失败只记日志，不影响当前风控任务。
func (c *VerdictCache) Set(ctx context.Context, rawURL string, v Verdict) {
	if c == nil || c.rdb == nil {
		return
	}
	if v.Uncacheable {
		return
	}
	norm, err := NormalizeURL(rawURL)
	if err != nil {
		return
	}
	data, err := json.Marshal(cachedVerdict{Unsafe: v.Unsafe, Reason: v.Reason, Detail: v.Detail})
	if err != nil {
		return
	}
	ttl := c.safeTTL
	if v.Unsafe {
		ttl = c.unsafeTTL
	}

	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheOpTimeout)
	defer cancel()

	if err := c.rdb.Set(opCtx, c.cacheKey(norm), data, ttl).Err(); err != nil {
		c.logger.WarnContext(ctx, "write risk verdict cache", "err", err)
	}
}
