package risk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"shortlink/internal/link"
	"shortlink/internal/safehttp"
)

const (
	StageCache        = "cache"
	StageLocalAllow   = "local_allow"
	StageLocalHost    = "local_host"
	StageLocalKeyword = "local_keyword"
	StageGSB          = "gsb"
	StageFetch        = "fetch"
	StageBodyKeyword  = "body_keyword"
	StageLLM          = "llm"

	OutcomeSafe             = "safe"
	OutcomeUnsafe           = "unsafe"
	OutcomeShadowUnsafe     = "shadow_unsafe"
	OutcomeLowConfidence    = "low_confidence"
	OutcomeFiltered         = "filtered"
	OutcomeBudgetLimited    = "budget_limited"
	OutcomeRateLimited      = "rate_limited"
	OutcomeBlockedAddress   = "blocked_address"
	OutcomeBlockedPort      = "blocked_port"
	OutcomeInvalidScheme    = "invalid_scheme"
	OutcomeTooManyRedirects = "too_many_redirects"
	OutcomeUnreachable      = "unreachable"
	OutcomeBinarySkipped    = "binary_skipped"
	OutcomeHTTP4xx          = "http_4xx"
	OutcomeError            = "error"
)

// Verdict 表示一次 URL 风控扫描的结论。
type Verdict struct {
	Unsafe      bool
	Reason      link.BanReason
	Detail      string
	Uncacheable bool   `json:"-"`
	Stage       string `json:"-"`
	Outcome     string `json:"-"`
}

// Scanner 抽象单条目标 URL 的风控检测能力，可通过 Pipeline 串联本地规则、威胁情报与 LLM 语义研判。
type Scanner interface {
	Scan(ctx context.Context, targetURL string) (Verdict, error)
}

// RuleScannerOptions 配置规则检测器的客户端、读取/提取上限、黑白名单规则与目标站限速器。
type RuleScannerOptions struct {
	HTTPClient      *http.Client
	MaxBodyBytes    int64
	MaxTextBytes    int64
	AllowedHosts    []string
	BlockedHosts    map[string]link.BanReason
	BlockedKeywords map[string]link.BanReason
	Limiter         *safehttp.HostLimiter
}

// DefaultKeywordRules 返回默认内置关键词规则，覆盖常见明显违规内容。
func DefaultKeywordRules() map[string]link.BanReason {
	return map[string]link.BanReason{
		"verify your password immediately": link.BanReasonPhishing,
		"malware-payload":                  link.BanReasonMalware,
	}
}

/*
ParseKeywordRules 解析环境变量中的关键词规则，格式为 "keyword:reason,keyword2:reason2"。
空串返回 nil；格式非法或 reason 不在 link.BanReason 白名单内返回错误。
*/
func ParseKeywordRules(raw string) (map[string]link.BanReason, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	rules := make(map[string]link.BanReason)
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kw, reasonStr, ok := strings.Cut(entry, ":")
		kw = strings.ToLower(strings.TrimSpace(kw))
		reason := link.BanReason(strings.TrimSpace(reasonStr))
		if !ok || kw == "" || !reason.Valid() {
			return nil, fmt.Errorf("invalid risk keyword rule %q: want keyword:reason", entry)
		}
		rules[kw] = reason
	}
	return rules, nil
}

/*
ParseHostRules 解析环境变量中的域名黑名单规则，格式为 "host:reason,host2:reason2"。
按最后一个冒号切分以兼容方括号包裹的 IPv6 字面量（如 "[2001:db8::1]:malware"），并自动剥离 FQDN 尾点。
*/
func ParseHostRules(raw string) (map[string]link.BanReason, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	rules := make(map[string]link.BanReason)
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.LastIndex(entry, ":")
		if idx <= 0 {
			return nil, fmt.Errorf("invalid risk host rule %q: want host:reason", entry)
		}
		hostPart := strings.TrimSpace(entry[:idx])
		if strings.HasPrefix(hostPart, "[") && strings.HasSuffix(hostPart, "]") {
			hostPart = hostPart[1 : len(hostPart)-1]
		}
		host := normalizeHost(hostPart)
		reason := link.BanReason(strings.TrimSpace(entry[idx+1:]))
		if host == "" || !reason.Valid() {
			return nil, fmt.Errorf("invalid risk host rule %q: want host:reason", entry)
		}
		if strings.Contains(host, ":") {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, fmt.Errorf("invalid risk host rule %q: want host:reason", entry)
			}
		}
		rules[host] = reason
	}
	return rules, nil
}

// ParseHostList 解析逗号分隔域名列表，统一小写、去尾点并去重排序（供白名单与指纹计算使用）。
func ParseHostList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	for entry := range strings.SplitSeq(raw, ",") {
		if host := normalizeHost(entry); host != "" {
			seen[host] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for host := range seen {
		out = append(out, host)
	}
	slices.Sort(out)
	return out
}

type ruleEntry struct {
	pattern string
	reason  link.BanReason
}

type blockedRedirectError struct {
	verdict Verdict
}

func (e blockedRedirectError) Error() string {
	return "blocked redirect: " + e.verdict.Detail
}

type rateLimitError struct {
	stage      string
	host       string
	retryAfter time.Duration
	deferOnly  bool
	err        error
}

func (e rateLimitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "rate limited"
}

func (e rateLimitError) Unwrap() error {
	return e.err
}

func normalizeHost(host string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
}

// RuleScanner 通过防 SSRF 的 HTTP 客户端抓取目标页面并按黑白名单与正文提取规则判定是否违规。
type RuleScanner struct {
	client          *http.Client
	maxBodyBytes    int64
	maxTextBytes    int64
	allowedHosts    []string
	blockedHosts    []ruleEntry
	blockedKeywords []ruleEntry
	limiter         *safehttp.HostLimiter
}

// NewRuleScanner 构造规则风控检测器。规则按模式排序存放，保证同时命中多条规则时判定结果确定。
func NewRuleScanner(opts RuleScannerOptions) *RuleScanner {
	baseClient := opts.HTTPClient
	if baseClient == nil {
		baseClient = NewSafeHTTPClient(safehttp.ClientOptions{})
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	maxText := opts.MaxTextBytes
	if maxText <= 0 {
		maxText = DefaultMaxExtractedTextBytes
	}

	hostsMap := make(map[string]link.BanReason, len(opts.BlockedHosts))
	for h, r := range opts.BlockedHosts {
		h = normalizeHost(h)
		if h != "" && r.Valid() {
			hostsMap[h] = r
		}
	}

	keywordsMap := DefaultKeywordRules()
	for kw, r := range opts.BlockedKeywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && r.Valid() {
			keywordsMap[kw] = r
		}
	}

	allowSet := make(map[string]struct{}, len(opts.AllowedHosts))
	for _, h := range opts.AllowedHosts {
		if norm := normalizeHost(h); norm != "" {
			allowSet[norm] = struct{}{}
		}
	}
	allowedHosts := make([]string, 0, len(allowSet))
	for h := range allowSet {
		allowedHosts = append(allowedHosts, h)
	}
	slices.Sort(allowedHosts)

	s := &RuleScanner{
		maxBodyBytes:    maxBody,
		maxTextBytes:    maxText,
		allowedHosts:    allowedHosts,
		blockedHosts:    sortedRules(hostsMap),
		blockedKeywords: sortedRules(keywordsMap),
		limiter:         opts.Limiter,
	}

	// 复制客户端值而不原地改写调用方传入的 *http.Client，共享底层 Transport 连接池。
	client := *baseClient
	prevCheckRedirect := baseClient.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if prevCheckRedirect != nil {
			if err := prevCheckRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if v, matched := s.matchURL(req.URL, true); matched {
			return blockedRedirectError{verdict: v}
		}
		// 对跳转到不同目标站点的重定向补做按 host 限速，防止多入口域汇聚打爆同一落地域名。
		if s.limiter != nil && req != nil && req.URL != nil {
			redirHost := normalizeHost(req.URL.Hostname())
			if redirHost != "" && !hostSeenInRequests(redirHost, via) {
				if delay, allowed := s.limiter.Allow(redirHost); !allowed {
					return rateLimitError{
						stage:      StageFetch,
						host:       redirHost,
						retryAfter: delay,
						deferOnly:  true,
						err:        fmt.Errorf("host rate limited: %s", redirHost),
					}
				}
			}
		}
		return nil
	}
	s.client = &client
	return s
}

func hostSeenInRequests(host string, reqs []*http.Request) bool {
	for _, r := range reqs {
		if r != nil && r.URL != nil && normalizeHost(r.URL.Hostname()) == host {
			return true
		}
	}
	return false
}

func sortedRules(m map[string]link.BanReason) []ruleEntry {
	entries := make([]ruleEntry, 0, len(m))
	for pattern, reason := range m {
		entries = append(entries, ruleEntry{pattern: pattern, reason: reason})
	}
	slices.SortFunc(entries, func(a, b ruleEntry) int {
		return strings.Compare(a.pattern, b.pattern)
	})
	return entries
}

/*
CheckLocal 在任何出站抓取和按 host 限速前执行 L0 本地检查：
1. 校验 URL 协议为 http/https 且 host 非空；
2. 先查域名黑名单（优先于白名单，避免误配白名单放行已拉黑子域）；
3. 再查域名白名单；
4. 最后查 URL 路径与查询参数中的关键词规则。
*/
func (s *RuleScanner) CheckLocal(targetURL string) (*url.URL, string, Verdict, bool, error) {
	trimmed := strings.TrimSpace(targetURL)
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || normalizeHost(u.Hostname()) == "" {
		return nil, "", Verdict{}, false, fmt.Errorf("%w: %q", safehttp.ErrInvalidScheme, targetURL)
	}

	host := normalizeHost(u.Hostname())
	if reason, matched := s.matchHost(host); matched {
		return u, trimmed, Verdict{
			Unsafe:  true,
			Reason:  reason,
			Detail:  "blocked host: " + host,
			Stage:   StageLocalHost,
			Outcome: OutcomeUnsafe,
		}, true, nil
	}
	if s.matchAllowedHost(host) {
		return u, trimmed, Verdict{
			Unsafe:  false,
			Stage:   StageLocalAllow,
			Outcome: OutcomeSafe,
		}, true, nil
	}
	if reason, kw, matched := s.matchText(trimmed); matched {
		return u, trimmed, Verdict{
			Unsafe:  true,
			Reason:  reason,
			Detail:  "matched url keyword: " + kw,
			Stage:   StageLocalKeyword,
			Outcome: OutcomeUnsafe,
		}, true, nil
	}
	return u, trimmed, Verdict{}, false, nil
}

/*
FetchAndInspect 对未在本地 L0 命中的 URL 依次执行：
按目标 host 限速 -> 防 SSRF 安全抓取与 3xx 中间/落地 URL 检查 -> 非文本响应跳过 -> HTML 流式净文本提取与正文规则匹配。
*/
func (s *RuleScanner) FetchAndInspect(ctx context.Context, u *url.URL, trimmed string) (Verdict, PageContent, error) {
	host := normalizeHost(u.Hostname())
	if s.limiter != nil && host != "" {
		if delay, allowed := s.limiter.Allow(host); !allowed {
			return Verdict{}, PageContent{}, rateLimitError{
				stage:      StageFetch,
				host:       host,
				retryAfter: delay,
				deferOnly:  true,
				err:        fmt.Errorf("host rate limited: %s", host),
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed, nil)
	if err != nil {
		return Verdict{}, PageContent{}, fmt.Errorf("build scan request: %w", err)
	}
	req.Header.Set("User-Agent", "shortlink-risk-scanner/1.0")

	resp, err := s.client.Do(req)
	if blocked, ok := errors.AsType[blockedRedirectError](err); ok {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return blocked.verdict, PageContent{}, nil
	}
	if rle, ok := errors.AsType[rateLimitError](err); ok {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return Verdict{}, PageContent{}, rle
	}
	if err != nil {
		return Verdict{}, PageContent{}, fmt.Errorf("fetch target url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	finalURL := trimmed
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
		// 纵深兜底：若自定义 RoundTripper 未经过 CheckRedirect 即改写了最终请求 URL，仍在状态码判断前复核。
		if finalURL != trimmed {
			if v, matched := s.matchURL(resp.Request.URL, true); matched {
				return v, PageContent{FinalURL: finalURL}, nil
			}
		}
	}

	if resp.StatusCode >= 500 {
		return Verdict{}, PageContent{FinalURL: finalURL}, fmt.Errorf("target returned server error %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// 4xx 可能是目标站识别扫描器后做 cloaking，本次不误封，但也不写入"安全"缓存。
		return Verdict{
			Unsafe:      false,
			Uncacheable: true,
			Stage:       StageFetch,
			Outcome:     OutcomeHTTP4xx,
		}, PageContent{FinalURL: finalURL}, nil
	}

	page, err := ExtractPageContent(resp.Header.Get("Content-Type"), resp.Body, s.maxBodyBytes, s.maxTextBytes)
	if err != nil {
		return Verdict{}, PageContent{FinalURL: finalURL}, fmt.Errorf("read target body: %w", err)
	}
	page.FinalURL = finalURL
	if page.SkippedBody {
		return Verdict{
			Unsafe:  false,
			Stage:   StageFetch,
			Outcome: OutcomeBinarySkipped,
		}, page, nil
	}

	if reason, kw, matched := s.matchText(page.CombinedText()); matched {
		return Verdict{
			Unsafe:  true,
			Reason:  reason,
			Detail:  "matched body keyword: " + kw,
			Stage:   StageBodyKeyword,
			Outcome: OutcomeUnsafe,
		}, page, nil
	}
	return Verdict{
		Unsafe:  false,
		Stage:   StageFetch,
		Outcome: OutcomeSafe,
	}, page, nil
}

/*
Scan 先检查入口 URL 自身（域名黑白名单和路径关键词），未命中时限速并通过防 SSRF 客户端抓取页面
提取净文本检查正文关键词。
*/
func (s *RuleScanner) Scan(ctx context.Context, targetURL string) (Verdict, error) {
	u, trimmed, localVerdict, done, err := s.CheckLocal(targetURL)
	if err != nil {
		return Verdict{}, err
	}
	if done {
		return localVerdict, nil
	}
	v, _, err := s.FetchAndInspect(ctx, u, trimmed)
	return v, err
}

func (s *RuleScanner) matchURL(u *url.URL, isRedirect bool) (Verdict, bool) {
	if u == nil {
		return Verdict{}, false
	}
	host := normalizeHost(u.Hostname())
	if host != "" {
		if reason, matched := s.matchHost(host); matched {
			prefix := "blocked host: "
			if isRedirect {
				prefix = "blocked redirect host: "
			}
			return Verdict{
				Unsafe:  true,
				Reason:  reason,
				Detail:  prefix + host,
				Stage:   StageLocalHost,
				Outcome: OutcomeUnsafe,
			}, true
		}
	}
	if reason, kw, matched := s.matchText(u.String()); matched {
		prefix := "matched url keyword: "
		if isRedirect {
			prefix = "matched redirect url keyword: "
		}
		return Verdict{
			Unsafe:  true,
			Reason:  reason,
			Detail:  prefix + kw,
			Stage:   StageLocalKeyword,
			Outcome: OutcomeUnsafe,
		}, true
	}
	return Verdict{}, false
}

func (s *RuleScanner) matchAllowedHost(host string) bool {
	for _, allow := range s.allowedHosts {
		if host == allow || strings.HasSuffix(host, "."+allow) {
			return true
		}
	}
	return false
}

func (s *RuleScanner) matchHost(host string) (link.BanReason, bool) {
	for _, rule := range s.blockedHosts {
		if host == rule.pattern || strings.HasSuffix(host, "."+rule.pattern) {
			return rule.reason, true
		}
	}
	return "", false
}

func (s *RuleScanner) matchText(text string) (link.BanReason, string, bool) {
	lower := strings.ToLower(text)
	for _, rule := range s.blockedKeywords {
		if strings.Contains(lower, rule.pattern) {
			return rule.reason, rule.pattern, true
		}
	}
	return "", "", false
}
