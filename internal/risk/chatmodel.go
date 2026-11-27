package risk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLLMMaxTokens 为单次风控裁决保留足够的 JSON 输出余量（兼容带轻量推理过程的模型）。
	DefaultLLMMaxTokens = 512

	llmMaxResponseBytes  = 1 << 20
	llmMaxErrorBodyBytes = 64 * 1024
	defaultLLMRetryAfter = 10 * time.Second
	maxLLMRetryAfter     = 5 * time.Minute
)

// ModelDescriptor 标识模型适配层驱动与模型名称，供追踪与日志使用（不含密钥）。
type ModelDescriptor struct {
	Provider string
	Model    string
}

// ChatRequest 表示一次与具体厂商无关的单轮结构化分类请求。
type ChatRequest struct {
	SystemPrompt string
	UserPrompt   string
	ExpectJSON   bool
}

// ChatResponse 表示模型适配层返回的标准化响应，显式区分厂商安全审核拦截。
type ChatResponse struct {
	Content          string
	Refusal          string
	FinishReason     string
	ContentFiltered  bool
	PromptTokens     int
	CompletionTokens int
}

// ChatModel 是大模型调用防腐层，屏蔽不同模型网关与直连厂商的协议及方言差异。
type ChatModel interface {
	Descriptor() ModelDescriptor
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// OpenAICompatOptions 配置 OpenAI 兼容 /chat/completions 模型客户端及常见方言开关。
type OpenAICompatOptions struct {
	BaseURL         string
	APIKey          string
	Model           string
	Timeout         time.Duration
	MaxTokens       int
	DisableJSONMode bool
	OmitTemperature bool
	HTTPClient      *http.Client
}

// OpenAICompatModel 基于 OpenAI 兼容 HTTP 协议调用 AI 网关（如 Magpie/Higress）或兼容模型端点。
type OpenAICompatModel struct {
	endpoint        string
	apiKey          string
	model           string
	timeout         time.Duration
	maxTokens       int
	disableJSONMode bool
	omitTemperature bool
	client          *http.Client
}

// NewOpenAICompatModel 构造 OpenAI 兼容模型客户端。BaseURL 与 Model 必填。
func NewOpenAICompatModel(opts OpenAICompatOptions) (*OpenAICompatModel, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("llm base url is required")
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return nil, errors.New("llm model is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultLLMTimeout
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultLLMMaxTokens
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &OpenAICompatModel{
		endpoint:        baseURL + "/chat/completions",
		apiKey:          strings.TrimSpace(opts.APIKey),
		model:           model,
		timeout:         timeout,
		maxTokens:       maxTokens,
		disableJSONMode: opts.DisableJSONMode,
		omitTemperature: opts.OmitTemperature,
		client:          client,
	}, nil
}

// Descriptor 返回适配层名称与模型 ID。
func (m *OpenAICompatModel) Descriptor() ModelDescriptor {
	return ModelDescriptor{
		Provider: "openai_compat",
		Model:    m.model,
	}
}

type openAIChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openAIChatMessage   `json:"messages"`
	Temperature    *float64              `json:"temperature,omitempty"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete 发送单轮 OpenAI 兼容对话请求并归类 429 限流与厂商内容审核拦截。
func (m *OpenAICompatModel) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	opCtx := ctx
	var cancel context.CancelFunc
	if m.timeout > 0 {
		opCtx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}

	wireReq := openAIChatRequest{
		Model: m.model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: req.SystemPrompt},
			{Role: "user", Content: req.UserPrompt},
		},
		MaxTokens: m.maxTokens,
	}
	if !m.omitTemperature {
		zero := 0.0
		wireReq.Temperature = &zero
	}
	if req.ExpectJSON && !m.disableJSONMode {
		wireReq.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
	}

	reqPayload, err := json.Marshal(wireReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal llm request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(opCtx, http.MethodPost, m.endpoint, bytes.NewReader(reqPayload))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build llm request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return ChatResponse{}, rateLimitError{
			stage:      StageLLM,
			retryAfter: retryAfter,
			deferOnly:  false,
			err:        fmt.Errorf("llm rate limited (429), retry after %s", retryAfter),
		}
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, llmMaxErrorBodyBytes))
		if isContentFilterHTTPResponse(resp.StatusCode, errBody) {
			return ChatResponse{
				FinishReason:    fmt.Sprintf("http_%d_filtered", resp.StatusCode),
				ContentFiltered: true,
			}, nil
		}
		return ChatResponse{}, fmt.Errorf("llm returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, llmMaxResponseBytes))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read llm response: %w", err)
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return ChatResponse{}, fmt.Errorf("decode llm response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return ChatResponse{}, errors.New("llm returned empty choices")
	}

	choice := chatResp.Choices[0]
	finishReason := strings.ToLower(strings.TrimSpace(choice.FinishReason))
	filtered := finishReason == "content_filter" || finishReason == "safety" || strings.TrimSpace(choice.Message.Refusal) != ""
	return ChatResponse{
		Content:          choice.Message.Content,
		Refusal:          choice.Message.Refusal,
		FinishReason:     finishReason,
		ContentFiltered:  filtered,
		PromptTokens:     chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
	}, nil
}

func isContentFilterHTTPResponse(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"content_filter",
		"data_inspection_failed",
		"prohibited_content",
		"safety",
		"moderation",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultLLMRetryAfter
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return clampRetryAfter(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampRetryAfter(t.Sub(now))
	}
	return defaultLLMRetryAfter
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	if d > maxLLMRetryAfter {
		return maxLLMRetryAfter
	}
	return d
}
