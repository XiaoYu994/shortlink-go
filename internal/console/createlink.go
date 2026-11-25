package console

import (
	"errors"
	"net/http"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/link"
)

type createLinkRequest struct {
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at"`
}

/*
handleCreate 解析并校验 JSON 请求体，创建短链后返回 201 和 Location。
请求体先限制大小并整体解码，避免只读到第一个 JSON 值就放过尾部内容。
*/
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(s.logger, r) {
		return
	}

	body, ok := readSmallRequestBody(s.logger, w, r)
	if !ok {
		return
	}

	req, err := decodeCreateRequest(body)
	if err != nil {
		httpx.WriteError(s.logger, r.Context(), w, http.StatusBadRequest, expiresAtDecodeMessage(err))
		return
	}

	l, err := link.NewLink(req.URL, req.ExpiresAt)
	if err != nil {
		if verr, ok := errors.AsType[*link.ValidationError](err); ok {
			httpx.WriteError(s.logger, r.Context(), w, http.StatusBadRequest, verr.Error())
			return
		}
		s.logger.ErrorContext(r.Context(), "new link", "err", err)
		httpx.WriteError(s.logger, r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	// 租户来自鉴权中间件。请求 JSON 里没有这个字段，客户端传了会被当成未知字段拒绝。
	p, ok := principalOrInternalError(s.logger, w, r)
	if !ok {
		return
	}
	l.TenantID = p.Tenant.ID

	if err := s.store.Create(r.Context(), l); err != nil {
		/*
			配额是租户策略，不是存储故障：403 告诉调用方链接本身没问题，本月额度用完了，删除链接也换不回来。
			用哨兵错误的文本，不把存储层包了一层的内部前缀暴露给客户端。
		*/
		if errors.Is(err, link.ErrQuotaExceeded) {
			httpx.WriteError(s.logger, r.Context(), w, http.StatusForbidden, link.ErrQuotaExceeded.Error())
			return
		}
		httpx.HandleStoreError(s.logger, w, r, "create link", err)
		return
	}

	w.Header().Set("Location", redirectPrefix+l.Code)
	httpx.WriteJSON(s.logger, r.Context(), w, http.StatusCreated, newCreatedLinkResponse(l))
}

// decodeCreateRequest 按严格模式解码创建短链请求，未知字段（例如拼错的 expire_at）会被拒绝。
func decodeCreateRequest(body []byte) (createLinkRequest, error) {
	var req createLinkRequest
	if err := decodeStrict(body, &req); err != nil {
		return createLinkRequest{}, err
	}
	return req, nil
}
