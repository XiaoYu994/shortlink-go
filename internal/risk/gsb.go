package risk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shortlink/internal/link"
)

const (
	// DefaultGSBBaseURL 是 Google Safe Browsing v4 API 的默认基地址。
	DefaultGSBBaseURL = "https://safebrowsing.googleapis.com"
	// DefaultGSBTimeout 是单次 Safe Browsing 查找请求的超时上限。
	DefaultGSBTimeout = 3 * time.Second

	gsbMaxResponseBytes = 1 << 20
)

// GSBOptions 配置可选的 Google Safe Browsing v4 威胁情报客户端（L1）。
type GSBOptions struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// GSBClient 调用 Google Safe Browsing v4 threatMatches:find 接口查验已知钓鱼与恶意 URL。
type GSBClient struct {
	apiKey   string
	endpoint string // 不含查询参数，确保错误消息、日志和追踪绝不泄露 API Key
	timeout  time.Duration
	client   *http.Client
}

// NewGSBClient 构造 Safe Browsing v4 客户端。APIKey 为空时返回错误；调用方按需启用。
func NewGSBClient(opts GSBOptions) (*GSBClient, error) {
	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		return nil, errors.New("gsb api key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultGSBBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultGSBTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &GSBClient{
		apiKey:   apiKey,
		endpoint: baseURL + "/v4/threatMatches:find",
		timeout:  timeout,
		client:   client,
	}, nil
}

type gsbRequest struct {
	Client     gsbClientInfo `json:"client"`
	ThreatInfo gsbThreatInfo `json:"threatInfo"`
}

type gsbClientInfo struct {
	ClientID      string `json:"clientId"`
	ClientVersion string `json:"clientVersion"`
}

type gsbThreatInfo struct {
	ThreatTypes      []string         `json:"threatTypes"`
	PlatformTypes    []string         `json:"platformTypes"`
	ThreatEntryTypes []string         `json:"threatEntryTypes"`
	ThreatEntries    []gsbThreatEntry `json:"threatEntries"`
}

type gsbThreatEntry struct {
	URL string `json:"url"`
}

type gsbResponse struct {
	Matches []struct {
		ThreatType string `json:"threatType"`
	} `json:"matches"`
}

// Check 查询 Safe Browsing 威胁库。请求或响应错误时剥离 URL 查询参数，防止 API Key 泄露到错误链。
func (c *GSBClient) Check(ctx context.Context, rawURL string) (Verdict, error) {
	opCtx := ctx
	var cancel context.CancelFunc
	if c.timeout > 0 {
		opCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	opCtx, span := otel.Tracer(riskTracerName).Start(opCtx, "risk.gsb",
		trace.WithSpanKind(trace.SpanKindClient),
	)
	defer span.End()

	reqBody := gsbRequest{
		Client: gsbClientInfo{ClientID: "shortlink-go", ClientVersion: "1.0"},
		ThreatInfo: gsbThreatInfo{
			ThreatTypes:      []string{"MALWARE", "SOCIAL_ENGINEERING", "UNWANTED_SOFTWARE", "POTENTIALLY_HARMFUL_APPLICATION"},
			PlatformTypes:    []string{"ANY_PLATFORM"},
			ThreatEntryTypes: []string{"URL"},
			ThreatEntries:    []gsbThreatEntry{{URL: rawURL}},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal gsb request")
		return Verdict{}, fmt.Errorf("marshal gsb request: %w", err)
	}

	reqURL := c.endpoint + "?key=" + url.QueryEscape(c.apiKey)
	req, err := http.NewRequestWithContext(opCtx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, fmt.Errorf("build gsb request: %w", sanitizeGSBURLError(err, c.endpoint))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		sanitized := sanitizeGSBURLError(err, c.endpoint)
		span.RecordError(sanitized)
		span.SetStatus(codes.Error, "gsb request failed")
		return Verdict{}, fmt.Errorf("gsb request: %w", sanitized)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		err := rateLimitError{
			stage: StageGSB,
			err:   errors.New("gsb returned status 429"),
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Verdict{}, err
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("gsb returned status %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Verdict{}, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, gsbMaxResponseBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read gsb response")
		return Verdict{}, fmt.Errorf("read gsb response: %w", err)
	}
	var out gsbResponse
	if err := json.Unmarshal(body, &out); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decode gsb response")
		return Verdict{}, fmt.Errorf("decode gsb response: %w", err)
	}
	if len(out.Matches) == 0 {
		span.SetAttributes(attribute.Bool("risk.gsb.matched", false))
		return Verdict{Unsafe: false, Stage: StageGSB, Outcome: OutcomeSafe}, nil
	}

	threatType := strings.ToUpper(strings.TrimSpace(out.Matches[0].ThreatType))
	reason := mapGSBThreatReason(threatType)
	span.SetAttributes(
		attribute.Bool("risk.gsb.matched", true),
		attribute.String("risk.gsb.threat_type", threatType),
	)
	return Verdict{
		Unsafe:  true,
		Reason:  reason,
		Detail:  "safe browsing: " + strings.ToLower(threatType),
		Stage:   StageGSB,
		Outcome: OutcomeUnsafe,
	}, nil
}

func mapGSBThreatReason(threatType string) link.BanReason {
	switch threatType {
	case "SOCIAL_ENGINEERING":
		return link.BanReasonPhishing
	case "MALWARE", "UNWANTED_SOFTWARE", "POTENTIALLY_HARMFUL_APPLICATION":
		return link.BanReasonMalware
	default:
		return link.BanReasonOther
	}
}

func sanitizeGSBURLError(err error, safeEndpoint string) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		copied := *uerr
		copied.URL = safeEndpoint
		return &copied
	}
	return err
}
