package httpapi

import (
	"errors"
	"net/http"

	"shortlink/internal/link"
)

/*
handleDelete 硬删除链接和它的统计行，成功返回 204，无响应体。
不存在和其他租户的链接都返回 404。
删除不是强撤销：已经在途的跳转仍可能返回 302，之后的新请求才会 404。
*/
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
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

	if err := s.admin.Delete(r.Context(), tn.ID, code); err != nil {
		if errors.Is(err, link.ErrNotFound) {
			s.writeLinkNotFound(r, w)
			return
		}
		s.handleStoreError(w, r, "delete link", err, "code", code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
