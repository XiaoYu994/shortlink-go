package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"shortlink/internal/link"
)

/*
updateLinkRequest 用 RawMessage 保留「字段没传」和「传了 null」的区别。
*time.Time 区分不了这两种情况；语义由 ADR 0011 定，解析放在 decodeUpdateRequest。
*/
type updateLinkRequest struct {
	URL       json.RawMessage `json:"url"`
	ExpiresAt json.RawMessage `json:"expires_at"`
}

/*
handleUpdate 修改 url 和/或 expires_at，成功返回 200 和修改后的链接。
不能改 code、tenant_id、created_at、clicks、clicks_today；
未知字段、空对象和非法值返回 400。
*/
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}
	tn, ok := s.principal(w, r)
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

	updated, err := s.admin.Update(r.Context(), tn.ID, code, update)
	if err != nil {
		s.handleAdminStoreError(w, r, "update link", err, code)
		return
	}
	s.writeJSON(r.Context(), w, http.StatusOK, newLinkResponse(*updated))
}

/*
decodeUpdateRequest 解析 PATCH 语义：
  - url 出现时必须按创建规则校验；
  - expires_at 出现且为字符串时必须是将来时间；
  - expires_at 出现且为 null 时清除过期时间；
  - 两个字段都没出现、空对象和未知字段都返回 400。
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

	if update.Empty() {
		return link.Update{}, badRequestError("at least one of url or expires_at is required")
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
