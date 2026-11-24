package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

// AuditLogStore 定义查询组织审计日志所需的存储能力（1 个方法），由 postgres.Store 实现（ADR 0023）。
type AuditLogStore interface {
	ListAuditLogs(ctx context.Context, opts audit.ListOptions) ([]audit.Event, error)
}

/*
auditHandler 独立承接 GET /api/v1/audit-logs（ADR 0019、ADR 0023）。
仅允许拥有 audit:read 的会话用户（owner、admin）调用，并在 handler 内做 ActorUser 纵深防御；
响应一律带 Cache-Control: no-store。
*/
type auditHandler struct {
	logger *slog.Logger
	logs   AuditLogStore
}

func newAuditHandler(logger *slog.Logger, logs AuditLogStore) *auditHandler {
	return &auditHandler{
		logger: logger,
		logs:   logs,
	}
}

type auditLogResponse struct {
	ID         int64           `json:"id"`
	ActorKind  string          `json:"actor_kind"`
	ActorID    *int64          `json:"actor_id,omitempty"`
	ActorEmail string          `json:"actor_email,omitempty"`
	ActorName  string          `json:"actor_name,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  time.Time       `json:"created_at"`
}

type listAuditLogsResponse struct {
	Items      []auditLogResponse `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func newAuditLogResponse(ev audit.Event) auditLogResponse {
	meta := ev.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	return auditLogResponse{
		ID:         ev.ID,
		ActorKind:  string(ev.ActorKind),
		ActorID:    ev.ActorID,
		ActorEmail: ev.ActorEmail,
		ActorName:  ev.ActorName,
		Action:     string(ev.Action),
		TargetType: string(ev.TargetType),
		TargetID:   ev.TargetID,
		RequestID:  ev.RequestID,
		Metadata:   meta,
		CreatedAt:  ev.CreatedAt.UTC(),
	}
}

func (h *auditHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}
	if p.ActorKind != identity.ActorUser {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	opts, err := parseAuditListOptions(r, p.Tenant.ID)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, err.Error())
		return
	}
	pageLimit := opts.Limit - 1

	items, err := h.logs.ListAuditLogs(r.Context(), opts)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "list audit logs", err)
		return
	}

	hasNext := len(items) > pageLimit
	if hasNext {
		items = items[:pageLimit]
	}
	resp := listAuditLogsResponse{
		Items: make([]auditLogResponse, 0, len(items)),
	}
	for _, item := range items {
		resp.Items = append(resp.Items, newAuditLogResponse(item))
	}
	if hasNext {
		resp.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

func parseAuditListOptions(r *http.Request, tenantID int64) (audit.ListOptions, error) {
	query := r.URL.Query()

	pageLimit := defaultListLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxListLimit {
			return audit.ListOptions{}, badRequestError("invalid limit")
		}
		pageLimit = n
	}

	var action audit.Action
	if raw := query.Get("action"); raw != "" {
		action = audit.Action(raw)
		if !action.Valid() {
			return audit.ListOptions{}, badRequestError("invalid action")
		}
	}

	var cursor *audit.ListCursor
	if raw := query.Get("cursor"); raw != "" {
		id, ok := parsePositiveInt64(raw)
		if !ok {
			return audit.ListOptions{}, badRequestError("invalid cursor")
		}
		cursor = &audit.ListCursor{ID: id}
	}

	return audit.ListOptions{
		TenantID: tenantID,
		Action:   action,
		Limit:    pageLimit + 1,
		Cursor:   cursor,
	}, nil
}
