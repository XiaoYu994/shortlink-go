package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

// OrgStore 定义自助创建组织需要的存储能力，由 postgres.Store 实现（ADR 0021）。
type OrgStore interface {
	CreateOrgForUser(ctx context.Context, userID int64, name string, maxOwned int) (identity.CreatedOrg, error)
}

/*
orgHandler 独立承接 POST /api/v1/orgs（ADR 0019、ADR 0021）。
与 /api/v1/me 一样只认唯一 sl_session 会话 Cookie，不要求 X-Org-ID（新用户尚未加入任何组织），
并且作为写请求必须经过 CrossOriginProtection，拒绝 API Key 调用。
*/
type orgHandler struct {
	logger   *slog.Logger
	sessions SessionManager
	orgs     OrgStore
	csrf     *http.CrossOriginProtection
	maxOwned int
}

func newOrgHandler(logger *slog.Logger, sessions SessionManager, orgs OrgStore, csrf *http.CrossOriginProtection) *orgHandler {
	return &orgHandler{
		logger:   logger,
		sessions: sessions,
		orgs:     orgs,
		csrf:     csrf,
		maxOwned: identity.DefaultMaxOwnedOrgs,
	}
}

type createOrgRequest struct {
	Name string `json:"name"`
}

type createOrgResponse struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *orgHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if h.sessions == nil || h.orgs == nil {
		h.logger.ErrorContext(r.Context(), "create org", "err", errors.New("org store is not configured"))
		httpx.WriteError(h.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}
	// API Key 属于已有组织，不允许用机器密钥自助新建组织。
	if r.Header.Get("Authorization") != "" {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}
	cookies := sessionCookies(r)
	if len(cookies) == 0 {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !checkCrossOrigin(h.logger, h.csrf, w, r) {
		return
	}
	if len(cookies) != 1 || !identity.SessionToken.Valid(cookies[0].Value) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	info, err := h.sessions.GetSessionInfo(r.Context(), cookies[0].Value)
	if errors.Is(err, identity.ErrUnauthenticated) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "authenticate create org", err)
		return
	}

	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req createOrgRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, orgDecodeMessage(err))
		return
	}

	auditCtx := audit.WithMeta(r.Context(), audit.UserMeta(info.User.ID, httpx.RequestIDFrom(r.Context())))
	created, err := h.orgs.CreateOrgForUser(auditCtx, info.User.ID, req.Name, h.maxOwned)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidOrgName):
			httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidOrgName.Error())
		case errors.Is(err, identity.ErrOrgLimitExceeded):
			httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, identity.ErrOrgLimitExceeded.Error())
		case errors.Is(err, identity.ErrUnauthenticated):
			httpx.WriteError(h.logger, r.Context(), w, http.StatusUnauthorized, "unauthorized")
		default:
			httpx.HandleStoreError(h.logger, w, r, "create org", err)
		}
		return
	}

	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusCreated, createOrgResponse{
		ID:        created.ID,
		Name:      created.Name,
		Role:      string(created.Role),
		CreatedAt: created.CreatedAt.UTC(),
	})
}

func orgDecodeMessage(err error) string {
	if isJSONFieldError(err, "name") {
		return identity.ErrInvalidOrgName.Error()
	}
	return decodeErrorMessage(err)
}
