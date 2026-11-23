package risk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"shortlink/internal/link"
)

// Verdict 表示一次 URL 风控扫描的结论。
type Verdict struct {
	Unsafe bool
	Reason link.BanReason
	Detail string
}

// Scanner 抽象单条目标 URL 的风控检测能力，第一版为安全抓取加规则匹配，后续可替换外部风控或大模型。
type Scanner interface {
	Scan(ctx context.Context, targetURL string) (Verdict, error)
}

// RuleScannerOptions 配置规则检测器的客户端、正文读取上限和违规规则。
type RuleScannerOptions struct {
	HTTPClient      *http.Client
	MaxBodyBytes    int64
	BlockedHosts    map[string]link.BanReason
	BlockedKeywords map[string]link.BanReason
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

type ruleEntry struct {
	pattern string
	reason  link.BanReason
}

// RuleScanner 通过防 SSRF 的 HTTP 客户端抓取目标页面前缀并按规则判定是否违规。
type RuleScanner struct {
	client          *http.Client
	maxBodyBytes    int64
	blockedHosts    []ruleEntry
	blockedKeywords []ruleEntry
}

// NewRuleScanner 构造规则风控检测器。规则按模式排序存放，保证同时命中多条规则时判定结果确定。
func NewRuleScanner(opts RuleScannerOptions) *RuleScanner {
	client := opts.HTTPClient
	if client == nil {
		client = NewSafeHTTPClient(HTTPClientOptions{})
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	hostsMap := make(map[string]link.BanReason, len(opts.BlockedHosts))
	for h, r := range opts.BlockedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
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

	return &RuleScanner{
		client:          client,
		maxBodyBytes:    maxBody,
		blockedHosts:    sortedRules(hostsMap),
		blockedKeywords: sortedRules(keywordsMap),
	}
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
Scan 先检查 URL 自身（域名和路径关键词），未命中时再通过防 SSRF 客户端抓取页面前缀检查正文关键词。
HTTP 5xx 视为目标站临时故障返回错误以便退避重试；4xx 视为目标不可达但无违规证据，返回安全结论。
*/
func (s *RuleScanner) Scan(ctx context.Context, targetURL string) (Verdict, error) {
	u, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return Verdict{}, fmt.Errorf("%w: %q", ErrInvalidScheme, targetURL)
	}

	host := strings.ToLower(u.Hostname())
	if reason, matched := s.matchHost(host); matched {
		return Verdict{Unsafe: true, Reason: reason, Detail: "blocked host: " + host}, nil
	}
	if reason, kw, matched := s.matchText(targetURL); matched {
		return Verdict{Unsafe: true, Reason: reason, Detail: "matched url keyword: " + kw}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return Verdict{}, fmt.Errorf("build scan request: %w", err)
	}
	req.Header.Set("User-Agent", "shortlink-risk-scanner/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("fetch target url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		return Verdict{}, fmt.Errorf("target returned server error %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return Verdict{Unsafe: false}, nil
	}

	// 只读取前 maxBodyBytes 字节，防止恶意站点返回无限响应流打爆内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBodyBytes))
	if err != nil {
		return Verdict{}, fmt.Errorf("read target body: %w", err)
	}
	if reason, kw, matched := s.matchText(string(body)); matched {
		return Verdict{Unsafe: true, Reason: reason, Detail: "matched body keyword: " + kw}, nil
	}
	return Verdict{Unsafe: false}, nil
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
