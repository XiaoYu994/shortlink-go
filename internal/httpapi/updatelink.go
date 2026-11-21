package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/link"
)

/*
updateLinkRequest 用 RawMessage 保留「字段没传」和「传了 null」的区别。
*time.Time、*bool 区分不了这两种情况；语义由 ADR 0011、ADR 0016 定，
解析放在 decodeUpdateRequest。
*/
type updateLinkRequest struct {
	URL       json.RawMessage `json:"url"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	Disabled  json.RawMessage `json:"disabled"`
}

/*
handleUpdate 修改 url、expires_at 和/或停用状态。
具备 links:read 时返回 200 和修改后的链接；只有 links:write 时返回 204，
不把 url、点击数和停用时间带回。不能改 code、tenant_id、created_at、clicks、clicks_today；
未知字段、空对象和非法值返回 400。被封禁的链接返回 403，对租户只读。
*/
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	code, ok := s.validLinkCode(w, r)
	if !ok {
		return
	}

	body, ok := s.readSmallBody(w, r)
	if !ok {
		return
	}
	update, err := decodeUpdateRequest(body)
	if err != nil {
		s.writeDecodeError(w, r, err)
		return
	}

	updated, err := s.admin.Update(r.Context(), p.Tenant.ID, code, update)
	if err != nil {
		s.handleAdminStoreError(w, r, "update link", err, code)
		return
	}
	/*
		链接对象属于 links:read。只校验写权限的 PATCH 若把整行返回，
		没有读权限的密钥也能看到目标地址和点击数。
	*/
	if s.authz != nil && s.authz.Can(p, identity.PermLinksRead) {
		s.writeJSON(r.Context(), w, http.StatusOK, newLinkResponse(*updated))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/*
decodeUpdateRequest 解析 PATCH 语义：
  - url 出现时必须按创建规则校验；
  - expires_at 出现且为字符串时必须是将来时间；
  - expires_at 出现且为 null 时清除过期时间；
  - disabled 出现时必须是布尔值：true 停用，false 启用；
  - 三个字段都没出现、空对象和未知字段都返回 400。
*/
func decodeUpdateRequest(body []byte) (link.Update, error) {
	var req updateLinkRequest
	if err := decodeStrict(body, &req); err != nil {
		return link.Update{}, err
	}

	var update link.Update
	if req.URL != nil {
		var raw string
		if err := json.Unmarshal(req.URL, &raw); err != nil {
			return link.Update{}, badRequestError("invalid url")
		}
		valid, err := link.ValidateURL(raw)
		if err != nil {
			return link.Update{}, err
		}
		update.URL = &valid
	}

	if req.ExpiresAt != nil {
		if string(bytes.TrimSpace(req.ExpiresAt)) == "null" {
			update.ClearExpiresAt = true
		} else {
			var raw string
			if err := json.Unmarshal(req.ExpiresAt, &raw); err != nil {
				return link.Update{}, badRequestError("invalid expires_at")
			}
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return link.Update{}, badRequestError("invalid expires_at")
			}
			expires, err := link.ValidateExpiresAt(&at)
			if err != nil {
				return link.Update{}, err
			}
			update.ExpiresAt = expires
		}
	}

	if req.Disabled != nil {
		// encoding/json 把 null 解进 bool 时返回 nil 并保持零值，必须显式区分。
		if string(bytes.TrimSpace(req.Disabled)) == "null" {
			return link.Update{}, badRequestError("invalid disabled")
		}
		var raw bool
		if err := json.Unmarshal(req.Disabled, &raw); err != nil {
			return link.Update{}, badRequestError("invalid disabled")
		}
		update.Disabled = &raw
	}

	if update.Empty() {
		return link.Update{}, badRequestError("at least one of url, expires_at or disabled is required")
	}
	return update, nil
}

// writeDecodeError 把字段校验、语义错误和 JSON 语法错误分别映射成客户端短句。
func (s *Server) writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	if verr, ok := errors.AsType[*link.ValidationError](err); ok {
		s.writeError(r.Context(), w, http.StatusBadRequest, verr.Error())
		return
	}
	var bad badRequestError
	if errors.As(err, &bad) {
		s.writeError(r.Context(), w, http.StatusBadRequest, string(bad))
		return
	}
	s.writeError(r.Context(), w, http.StatusBadRequest, requestMessage(err))
}
