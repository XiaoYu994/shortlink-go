package console

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
)

/*
MemberStore 定义成员管理接口需要的存储能力。
接口放在使用方（3 个方法以内），生产由 postgres.Store 实现（ADR 0018）。
*/
type MemberStore interface {
	ListMembers(ctx context.Context, tenantID int64) ([]identity.Member, error)
	UpdateMemberRole(ctx context.Context, tenantID, actorUserID, targetUserID int64, newRole identity.Role) (identity.Member, error)
	RemoveMember(ctx context.Context, tenantID, actorUserID, targetUserID int64) error
}

/*
memberHandler 独立承接组织成员列表、改角色与移出成员（ADR 0019：不往 Server 堆成员字段）。
只持有日志和 MemberStore，不接触短链存储或 OIDC 客户端。
*/
type memberHandler struct {
	logger  *slog.Logger
	members MemberStore
}

func newMemberHandler(logger *slog.Logger, members MemberStore) *memberHandler {
	return &memberHandler{
		logger:  logger,
		members: members,
	}
}

type memberResponse struct {
	UserID   int64     `json:"user_id"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

type listMembersResponse struct {
	Items []memberResponse `json:"items"`
}

type updateMemberRoleRequest struct {
	Role string `json:"role"`
}

func newMemberResponse(m identity.Member) memberResponse {
	return memberResponse{
		UserID:   m.UserID,
		Email:    m.Email,
		Name:     m.Name,
		Role:     string(m.Role),
		JoinedAt: m.JoinedAt.UTC(),
	}
}

// handleList 返回当前组织下的成员列表，需要 members:read。
func (h *memberHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}

	items, err := h.members.ListMembers(r.Context(), p.Tenant.ID)
	if err != nil {
		httpx.HandleStoreError(h.logger, w, r, "list members", err)
		return
	}

	resp := listMembersResponse{Items: make([]memberResponse, 0, len(items))}
	for _, item := range items {
		resp.Items = append(resp.Items, newMemberResponse(item))
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, resp)
}

/*
handleUpdateRole 修改当前组织内目标成员的角色，需要 members:write（仅用户会话允许，API Key 在外层拒绝）。
在资源层按角色等级校验（admin 不能操作 owner，也不能把任何人设为 owner）；
最后一名 owner 不变量由存储层在同一事务里加行锁保证。
*/
func (h *memberHandler) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}
	/*
		纵深防御：即使外层更换了自定义 Authorizer（例如后续练习接入策略引擎）误放行 API Key，
		也不能把 api_keys.id 当作 users.id 传进成员存储层，防止与同编号用户冲突或越权。
	*/
	if p.ActorKind != identity.ActorUser {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	targetUserID, ok := parsePositiveInt64(r.PathValue("user_id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid user_id")
		return
	}

	body, ok := readSmallRequestBody(h.logger, w, r)
	if !ok {
		return
	}
	var req updateMemberRoleRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, memberRoleDecodeMessage(err))
		return
	}

	newRole := identity.Role(req.Role)
	if !identity.ValidRole(newRole) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid role")
		return
	}
	if !identity.CanAssignMemberRole(p.Role, newRole) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	updated, err := h.members.UpdateMemberRole(r.Context(), p.Tenant.ID, p.ActorID, targetUserID, newRole)
	if err != nil {
		h.handleMemberError(w, r, "update member role", err, targetUserID)
		return
	}
	httpx.WriteJSON(h.logger, r.Context(), w, http.StatusOK, newMemberResponse(updated))
}

/*
handleRemove 将目标成员移出当前组织，需要 members:write。
成功返回 204，不吊销该用户的会话（他在其他组织的访问不受影响）。
*/
func (h *memberHandler) handleRemove(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(h.logger, r) {
		return
	}
	p, ok := principalOrInternalError(h.logger, w, r)
	if !ok {
		return
	}
	// 同 handleUpdateRole：防止误配 Authorizer 把 API Key 的 ActorID 当成用户 ID 传给存储层。
	if p.ActorKind != identity.ActorUser || !identity.CanManageMembers(p.Role) {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, "forbidden")
		return
	}

	targetUserID, ok := parsePositiveInt64(r.PathValue("user_id"))
	if !ok {
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, "invalid user_id")
		return
	}

	if err := h.members.RemoveMember(r.Context(), p.Tenant.ID, p.ActorID, targetUserID); err != nil {
		h.handleMemberError(w, r, "remove member", err, targetUserID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func memberRoleDecodeMessage(err error) string {
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && (typeErr.Field == "role" || strings.HasSuffix(typeErr.Field, ".role")) {
		return "invalid role"
	}
	return requestMessage(err)
}

func (h *memberHandler) handleMemberError(w http.ResponseWriter, r *http.Request, op string, err error, targetUserID int64) {
	switch {
	case errors.Is(err, identity.ErrMemberNotFound):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusNotFound, identity.ErrMemberNotFound.Error())
	case errors.Is(err, identity.ErrOrgNotFound):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusNotFound, identity.ErrOrgNotFound.Error())
	case errors.Is(err, identity.ErrMemberForbidden):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusForbidden, identity.ErrMemberForbidden.Error())
	case errors.Is(err, identity.ErrLastOwner):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusConflict, identity.ErrLastOwner.Error())
	case errors.Is(err, identity.ErrInvalidRole):
		httpx.WriteError(h.logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidRole.Error())
	default:
		httpx.HandleStoreError(h.logger, w, r, op, err, "target_user_id", targetUserID)
	}
}
