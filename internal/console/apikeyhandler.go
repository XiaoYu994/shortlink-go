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

// APIKeyStore 定义组织内自助管理 API Key 所需的存储能力（3 个方法），由 postgres.Store 实现（ADR 0021）。
type APIKeyStore interface {
	ListOrgAPIKeys(ctx context.Context, tenantID int64) ([]identity.APIKeyInfo, error)
	IssueOrgAPIKey(ctx context.Context, p identity.IssueOrgAPIKeyParams) (identity.IssuedOrgAPIKey, error)
	RevokeOrgAPIKey(ctx context.Context, tenantID, keyID int64) error
}

/*
apiKeyHandler 独立承接 /api/v1/api-keys 的列表、签发与吊销（ADR 0021）。
仅允许拥有 apikeys:read / apikeys:write 的会话用户（owner、admin）调用；
handler 内做 ActorUser 纵深防御，杜绝 API Key 签发或吊销其他 API Key。
*/
type apiKeyHandler struct {
	logger  *slog.Logger
	apiKeys APIKeyStore
}

func newAPIKeyHandler(logger *slog.Logger, apiKeys APIKeyStore) *apiKeyHandler {
	return &apiKeyHandler{
		logger:  logger,
		apiKeys: apiKeys,
	}
}

type apiKeyResponse struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name,omitempty"`
	Scopes     []string   `json:"scopes"`
	Status     string     `json:"status"`
	CreatedBy  *int64     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type listAPIKeysResponse struct {
	Items []apiKeyResponse `json:"items"`
}

type issueAPIKeyRequest struct {
	Name      string     `json:"name,omitempty"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type issueAPIKeyResponse struct {
	apiKeyResponse
	APIKey string `json:"api_key"`
}

func newAPIKeyResponse(k identity.APIKeyInfo, now time.Time) apiKeyResponse {
	scopes := make([]string, len(k.Scopes))
	for i, s := range k.Scopes {
		scopes[i] = string(s)
	}
	return apiKeyResponse{
		ID:         k.ID,
		Name:       k.Name,
		Scopes:     scopes,
		Status:     k.Status(now),
		CreatedBy:  k.CreatedBy,
		CreatedAt:  k.CreatedAt.UTC(),
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		RevokedAt:  k.RevokedAt,
	}
}

func (h *apiKeyHandler) handleList(w http.ResponseWriter, r *http.Request) {
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

	keys, err := h.apiKeys.ListOrgAPIKeys(r.Context(), p.Tenant.ID)
	if err != nil {
		h.handleAPIKeyError(w, r, "list api keys", err)
		return
	}
	now := time.Now().UTC()
	resp := listAPIKeysResponse{Items: make([]apiKeyResponse, 0, len(keys))}
	for _, k := range keys {
		resp.Items = append(resp.Items, newAPIKeyResponse(k, now))
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

func (h *apiKeyHandler) handleIssue(w http.ResponseWriter, r *http.Request) {
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

	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req issueAPIKeyRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, requestMessage(err))
		return
	}

	keyName, err := identity.NormalizeAPIKeyName(req.Name)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidAPIKeyName.Error())
		return
	}

	rawScopes := make([]identity.Perm, len(req.Scopes))
	for i, s := range req.Scopes {
		rawScopes[i] = identity.Perm(s)
	}
	scopes, err := identity.NormalizeAPIKeyScopes(rawScopes)
	if err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidAPIKeyScope.Error())
		return
	}

	issued, err := h.apiKeys.IssueOrgAPIKey(r.Context(), identity.IssueOrgAPIKeyParams{
		TenantID:  p.Tenant.ID,
		CreatedBy: p.ActorID,
		Name:      keyName,
		Scopes:    scopes,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		h.handleAPIKeyError(w, r, "issue api key", err)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusCreated, issueAPIKeyResponse{
		apiKeyResponse: newAPIKeyResponse(issued.Key, time.Now().UTC()),
		APIKey:         issued.Plaintext,
	})
}

func (h *apiKeyHandler) handleRevoke(w http.ResponseWriter, r *http.Request) {
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

	keyID, ok := parsePositiveInt64(r.PathValue("id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid api key id")
		return
	}
	if err := h.apiKeys.RevokeOrgAPIKey(r.Context(), p.Tenant.ID, keyID); err != nil {
		h.handleAPIKeyError(w, r, "revoke api key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiKeyHandler) handleAPIKeyError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidAPIKeyName):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidAPIKeyName.Error())
	case errors.Is(err, identity.ErrInvalidAPIKeyScope):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidAPIKeyScope.Error())
	case errors.Is(err, identity.ErrExpiresNotFuture):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrExpiresNotFuture.Error())
	case errors.Is(err, identity.ErrAPIKeyNotFound), errors.Is(err, identity.ErrOrgNotFound):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusNotFound, identity.ErrAPIKeyNotFound.Error())
	case errors.Is(err, identity.ErrAPIKeyRevoked):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, identity.ErrAPIKeyRevoked.Error())
	default:
		httpx.HandleStoreError(h.logger, w, r, op, err)
	}
}
