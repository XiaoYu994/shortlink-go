package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
	"shortlink/internal/safehttp"
	"shortlink/internal/webhook"
)

// WebhookReader 定义 Webhook 与投递记录只读查询能力。
type WebhookReader interface {
	ListWebhooks(ctx context.Context, tenantID int64) ([]webhook.Subscription, error)
	ListWebhookDeliveries(ctx context.Context, opts webhook.DeliveryListOptions) ([]webhook.Delivery, error)
}

// WebhookWriter 定义 Webhook 创建、更新、轮换与删除存储能力。
type WebhookWriter interface {
	CreateWebhook(ctx context.Context, p webhook.CreateParams) (webhook.Subscription, error)
	UpdateWebhook(ctx context.Context, p webhook.UpdateParams) (webhook.Subscription, error)
	DeleteWebhook(ctx context.Context, tenantID, webhookID int64) error
	RotateWebhookSecret(ctx context.Context, tenantID, webhookID int64, ciphertext []byte, prefix string) (webhook.Subscription, error)
}

// WebhookTester 定义手动触发测试 Ping 的入队能力。
type WebhookTester interface {
	EnqueueWebhookTest(ctx context.Context, tenantID, webhookID int64) (webhook.TestEnqueueResult, error)
}

// WebhookStore 汇总 Webhook 管理接口所需全部存储能力，由 postgres.Store 实现（ADR 0027）。
type WebhookStore interface {
	WebhookReader
	WebhookWriter
	WebhookTester
}

/*
webhookHandler 承接 /api/v1/webhooks* 的订阅管理、密钥轮换、异步测试 Ping 与投递记录查询。
仅允许拥有 webhooks:read / webhooks:write 的会话用户（owner、admin）调用，并在 handler 内做 ActorUser 纵深防御。
*/
type webhookHandler struct {
	logger   *slog.Logger
	webhooks WebhookStore
	cipher   *webhook.SecretCipher
	urlOpts  safehttp.URLValidationOptions
}

func newWebhookHandler(logger *slog.Logger, webhooks WebhookStore, cipher *webhook.SecretCipher, urlOpts safehttp.URLValidationOptions) *webhookHandler {
	return &webhookHandler{
		logger:   logger,
		webhooks: webhooks,
		cipher:   cipher,
		urlOpts:  urlOpts,
	}
}

type webhookResponse struct {
	ID                  int64      `json:"id"`
	URL                 string     `json:"url"`
	Description         string     `json:"description,omitempty"`
	Events              []string   `json:"events"`
	SecretPrefix        string     `json:"secret_prefix"`
	Enabled             bool       `json:"enabled"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledReason      string     `json:"disabled_reason,omitempty"`
	DisabledAt          *time.Time `json:"disabled_at,omitempty"`
	CreatedBy           *int64     `json:"created_by,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type listWebhooksResponse struct {
	Items []webhookResponse `json:"items"`
}

type webhookWithSecretResponse struct {
	webhookResponse
	Secret string `json:"secret"`
}

type createWebhookRequest struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	Events      []string `json:"events"`
}

type updateWebhookRequest struct {
	URL         *string   `json:"url,omitempty"`
	Description *string   `json:"description,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

type testWebhookResponse struct {
	JobID      int64  `json:"job_id"`
	DeliveryID string `json:"delivery_id"`
}

type webhookDeliveryResponse struct {
	ID              int64     `json:"id"`
	WebhookID       int64     `json:"webhook_id"`
	JobID           int64     `json:"job_id"`
	DeliveryID      string    `json:"delivery_id"`
	Event           string    `json:"event"`
	Attempt         int       `json:"attempt"`
	Status          string    `json:"status"`
	HTTPStatus      *int      `json:"http_status,omitempty"`
	DurationMs      int       `json:"duration_ms"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	ResponseSnippet string    `json:"response_snippet,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type listWebhookDeliveriesResponse struct {
	Items      []webhookDeliveryResponse `json:"items"`
	NextCursor string                    `json:"next_cursor,omitempty"`
}

func newWebhookResponse(sub webhook.Subscription) webhookResponse {
	events := make([]string, len(sub.Events))
	for i, ev := range sub.Events {
		events[i] = string(ev)
	}
	return webhookResponse{
		ID:                  sub.ID,
		URL:                 sub.URL,
		Description:         sub.Description,
		Events:              events,
		SecretPrefix:        sub.SecretPrefix,
		Enabled:             sub.Enabled,
		ConsecutiveFailures: sub.ConsecutiveFailures,
		DisabledReason:      string(sub.DisabledReason),
		DisabledAt:          sub.DisabledAt,
		CreatedBy:           sub.CreatedBy,
		CreatedAt:           sub.CreatedAt.UTC(),
		UpdatedAt:           sub.UpdatedAt.UTC(),
	}
}

func newWebhookDeliveryResponse(d webhook.Delivery) webhookDeliveryResponse {
	return webhookDeliveryResponse{
		ID:              d.ID,
		WebhookID:       d.WebhookID,
		JobID:           d.JobID,
		DeliveryID:      d.DeliveryID,
		Event:           string(d.Event),
		Attempt:         d.Attempt,
		Status:          string(d.Status),
		HTTPStatus:      d.HTTPStatus,
		DurationMs:      d.DurationMs,
		ErrorMessage:    d.ErrorMessage,
		ResponseSnippet: d.ResponseSnippet,
		CreatedAt:       d.CreatedAt.UTC(),
	}
}

func (h *webhookHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	items, err := h.webhooks.ListWebhooks(r.Context(), p.Tenant.ID)
	if err != nil {
		h.handleWebhookError(w, r, "list webhooks", err)
		return
	}
	resp := listWebhooksResponse{Items: make([]webhookResponse, 0, len(items))}
	for _, sub := range items {
		resp.Items = append(resp.Items, newWebhookResponse(sub))
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

func (h *webhookHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req createWebhookRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, requestMessage(err))
		return
	}

	normURL, ok := h.validateWebhookURL(w, r, req.URL)
	if !ok {
		return
	}
	desc, err := webhook.NormalizeDescription(req.Description)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidDescription.Error())
		return
	}
	events, err := normalizeRequestEvents(req.Events)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidEvents.Error())
		return
	}

	plainSecret, prefix, err := webhook.GenerateSecret()
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "generate webhook secret", err)
		return
	}
	ct, err := h.cipher.Encrypt(p.Tenant.ID, plainSecret)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "encrypt webhook secret", err)
		return
	}

	createdBy := p.ActorID
	created, err := h.webhooks.CreateWebhook(r.Context(), webhook.CreateParams{
		TenantID:         p.Tenant.ID,
		URL:              normURL,
		Description:      desc,
		Events:           events,
		SecretCiphertext: ct,
		SecretPrefix:     prefix,
		CreatedBy:        &createdBy,
	})
	if err != nil {
		h.handleWebhookError(w, r, "create webhook", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusCreated, webhookWithSecretResponse{
		webhookResponse: newWebhookResponse(created),
		Secret:          plainSecret,
	})
}

func (h *webhookHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	webhookID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid webhook id")
		return
	}
	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req updateWebhookRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, requestMessage(err))
		return
	}
	if req.URL == nil && req.Description == nil && req.Events == nil && req.Enabled == nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "at least one field is required")
		return
	}

	params := webhook.UpdateParams{
		TenantID:  p.Tenant.ID,
		WebhookID: webhookID,
		Enabled:   req.Enabled,
	}
	if req.URL != nil {
		normURL, ok := h.validateWebhookURL(w, r, *req.URL)
		if !ok {
			return
		}
		params.URL = &normURL
	}
	if req.Description != nil {
		desc, err := webhook.NormalizeDescription(*req.Description)
		if err != nil {
			httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidDescription.Error())
			return
		}
		params.Description = &desc
	}
	if req.Events != nil {
		events, err := normalizeRequestEvents(*req.Events)
		if err != nil {
			httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidEvents.Error())
			return
		}
		params.Events = events
	}

	updated, err := h.webhooks.UpdateWebhook(r.Context(), params)
	if err != nil {
		h.handleWebhookError(w, r, "update webhook", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, newWebhookResponse(updated))
}

func (h *webhookHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	webhookID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid webhook id")
		return
	}
	if err := h.webhooks.DeleteWebhook(r.Context(), p.Tenant.ID, webhookID); err != nil {
		h.handleWebhookError(w, r, "delete webhook", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *webhookHandler) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	webhookID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid webhook id")
		return
	}
	plainSecret, prefix, err := webhook.GenerateSecret()
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "generate webhook secret", err)
		return
	}
	ct, err := h.cipher.Encrypt(p.Tenant.ID, plainSecret)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "encrypt webhook secret", err)
		return
	}
	updated, err := h.webhooks.RotateWebhookSecret(r.Context(), p.Tenant.ID, webhookID, ct, prefix)
	if err != nil {
		h.handleWebhookError(w, r, "rotate webhook secret", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, webhookWithSecretResponse{
		webhookResponse: newWebhookResponse(updated),
		Secret:          plainSecret,
	})
}

func (h *webhookHandler) handleTest(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	webhookID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid webhook id")
		return
	}
	res, err := h.webhooks.EnqueueWebhookTest(r.Context(), p.Tenant.ID, webhookID)
	if err != nil {
		h.handleWebhookError(w, r, "test webhook", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusAccepted, testWebhookResponse{
		JobID:      res.JobID,
		DeliveryID: res.DeliveryID,
	})
}

func (h *webhookHandler) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := h.requireUserPrincipal(w, r)
	if !ok {
		return
	}

	webhookID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid webhook id")
		return
	}
	opts, err := parseWebhookDeliveryListOptions(r, p.Tenant.ID, webhookID)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, err.Error())
		return
	}
	pageLimit := opts.Limit - 1
	items, err := h.webhooks.ListWebhookDeliveries(r.Context(), opts)
	if err != nil {
		h.handleWebhookError(w, r, "list webhook deliveries", err)
		return
	}
	hasNext := len(items) > pageLimit
	if hasNext {
		items = items[:pageLimit]
	}
	resp := listWebhookDeliveriesResponse{
		Items: make([]webhookDeliveryResponse, 0, len(items)),
	}
	for _, d := range items {
		resp.Items = append(resp.Items, newWebhookDeliveryResponse(d))
	}
	if hasNext {
		resp.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

func (h *webhookHandler) requireUserPrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return identity.Principal{}, false
	}
	if p.ActorKind != identity.ActorUser {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return identity.Principal{}, false
	}
	return p, true
}

func (h *webhookHandler) validateWebhookURL(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	if _, err := safehttp.ValidateURL(raw, h.urlOpts); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidURL.Error())
		return "", false
	}
	return strings.TrimSpace(raw), true
}

func (h *webhookHandler) handleWebhookError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, webhook.ErrInvalidURL):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidURL.Error())
	case errors.Is(err, webhook.ErrInvalidDescription):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidDescription.Error())
	case errors.Is(err, webhook.ErrInvalidEvents):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, webhook.ErrInvalidEvents.Error())
	case errors.Is(err, webhook.ErrNotFound), errors.Is(err, webhook.ErrTenantNotFound):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusNotFound, webhook.ErrNotFound.Error())
	case errors.Is(err, webhook.ErrLimitExceeded):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, webhook.ErrLimitExceeded.Error())
	default:
		httpx.HandleStoreError(h.logger, w, r, op, err)
	}
}

func normalizeRequestEvents(raw []string) ([]webhook.EventType, error) {
	events := make([]webhook.EventType, len(raw))
	for i, ev := range raw {
		events[i] = webhook.EventType(ev)
	}
	return webhook.NormalizeSubscribableEvents(events)
}

func parseWebhookDeliveryListOptions(r *http.Request, tenantID, webhookID int64) (webhook.DeliveryListOptions, error) {
	query := r.URL.Query()
	pageLimit := defaultListLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxListLimit {
			return webhook.DeliveryListOptions{}, badRequestError("invalid limit")
		}
		pageLimit = n
	}

	var cursor *webhook.DeliveryListCursor
	if raw := query.Get("cursor"); raw != "" {
		id, ok := parsePositiveInt64(raw)
		if !ok {
			return webhook.DeliveryListOptions{}, badRequestError("invalid cursor")
		}
		cursor = &webhook.DeliveryListCursor{ID: id}
	}

	return webhook.DeliveryListOptions{
		TenantID:  tenantID,
		WebhookID: webhookID,
		Limit:     pageLimit + 1,
		Cursor:    cursor,
	}, nil
}
