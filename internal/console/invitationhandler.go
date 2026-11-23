package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

// InvitationStore 定义组织邀请管理接口所需的存储能力（3 个方法），由 postgres.Store 实现（ADR 0021）。
type InvitationStore interface {
	ListInvitations(ctx context.Context, tenantID int64) ([]identity.Invitation, error)
	CreateOrgInvitation(ctx context.Context, p identity.CreateInvitationParams) (identity.Invitation, error)
	RevokeInvitation(ctx context.Context, p identity.RevokeInvitationParams) error
}

// invitationHandler 独立承接 /api/v1/invitations 的列表、发起与撤销。
type invitationHandler struct {
	logger      *slog.Logger
	invitations InvitationStore
}

func newInvitationHandler(logger *slog.Logger, invitations InvitationStore) *invitationHandler {
	return &invitationHandler{
		logger:      logger,
		invitations: invitations,
	}
}

type invitationResponse struct {
	ID         int64      `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	InvitedBy  *int64     `json:"invited_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type listInvitationsResponse struct {
	Items []invitationResponse `json:"items"`
}

type createInvitationRequest struct {
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func newInvitationResponse(inv identity.Invitation, now time.Time) invitationResponse {
	return invitationResponse{
		ID:         inv.ID,
		Email:      inv.Email,
		Role:       string(inv.Role),
		Status:     inv.Status(now),
		InvitedBy:  inv.InvitedBy,
		CreatedAt:  inv.CreatedAt.UTC(),
		ExpiresAt:  inv.ExpiresAt,
		AcceptedAt: inv.AcceptedAt,
		RevokedAt:  inv.RevokedAt,
	}
}

func (h *invitationHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}

	items, err := h.invitations.ListInvitations(r.Context(), p.Tenant.ID)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "list invitations", err)
		return
	}
	now := time.Now().UTC()
	resp := listInvitationsResponse{Items: make([]invitationResponse, 0, len(items))}
	for _, inv := range items {
		resp.Items = append(resp.Items, newInvitationResponse(inv, now))
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

func (h *invitationHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}
	if p.ActorKind != identity.ActorUser {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req createInvitationRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, requestMessage(err))
		return
	}

	email, err := identity.NormalizeEmail(req.Email)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidEmail.Error())
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidRole.Error())
		return
	}
	if !identity.CanAssignMemberRole(p.Role, role) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	created, err := h.invitations.CreateOrgInvitation(r.Context(), identity.CreateInvitationParams{
		TenantID:  p.Tenant.ID,
		Email:     email,
		Role:      role,
		InvitedBy: p.ActorID,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		h.handleInvitationError(w, r, "create invitation", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusCreated, newInvitationResponse(created, time.Now().UTC()))
}

func (h *invitationHandler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}
	if p.ActorKind != identity.ActorUser || !identity.CanManageMembers(p.Role) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	invID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid invitation id")
		return
	}

	if err := h.invitations.RevokeInvitation(r.Context(), identity.RevokeInvitationParams{
		TenantID:     p.Tenant.ID,
		InvitationID: invID,
		ActorRole:    p.Role,
	}); err != nil {
		h.handleInvitationError(w, r, "revoke invitation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *invitationHandler) handleInvitationError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidEmail):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidEmail.Error())
	case errors.Is(err, identity.ErrInvalidRole):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidRole.Error())
	case errors.Is(err, identity.ErrInvitationExpiresNotFuture):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvitationExpiresNotFuture.Error())
	case errors.Is(err, identity.ErrMemberForbidden):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
	case errors.Is(err, identity.ErrInvitationNotFound), errors.Is(err, identity.ErrOrgNotFound):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusNotFound, identity.ErrInvitationNotFound.Error())
	case errors.Is(err, identity.ErrInvitationPending):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, identity.ErrInvitationPending.Error())
	case errors.Is(err, identity.ErrInvitationNotPending):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, identity.ErrInvitationNotPending.Error())
	default:
		httpx.HandleStoreError(h.logger, w, r, op, err)
	}
}
